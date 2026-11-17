"""Internet radio for the Capsula Mini (MicroPython runtime).

The app owns the station list, the user-visible radio state, the segment
display and the framework speaker stream. It cannot open an outbound
connection and the platform has no audio decoder for script apps, so a
companion tuner app fetches and decodes the station and pushes decoded PCM
here: one routed ``Push`` call per chunk (see ``proto/radio.proto``). The
chunk-by-chunk shape matters: this host has no threads, so audio is streamed
one event per chunk, and repeating work (the stall watchdog) runs on the host's
run-loop callbacks (``capsulad.every``).

Interaction
  trigger      : the ``play``/``pause``/``stop``/``next``/``previous``/
                 ``dismiss`` actions, the ``station`` launch argument, the
                 ``station`` setting, or the standard media contract
                 (circle key, swipes, Web UI "Now playing").
  feedback     : the segment display shows the station number, a colon while
                 audio is flowing, a lit decimal point while paused and four
                 dashes after a failure; ``MediaPlayerService`` reports the
                 full state and the tuner's now-playing metadata.
  ownership    : one segment presentation and one speaker playback stream,
                 both owned by this run.
  interruption : ``pause`` stops the tuner session; ``stop``/``dismiss`` abort
                 the playback stream, drop the tuner session and release the
                 display so the configured baseline returns.
  dismissal    : ``dismiss`` is the action the device's circle key uses.
  final state  : idle with no owned presentation or audio stream; a failure
                 keeps the dash pattern on the display until the user stops or
                 dismisses it.
"""

import capsulad

CONFIG = "capsulad.framework.v1.ApplicationConfigurationService"
DEVICE = "capsulad.framework.v1.DeviceService"
AUDIO = "capsulad.framework.v1.AudioService"
PRESENTATION = "capsulad.framework.v1.PresentationService"
APP_IFACES = "capsulad.framework.v1.AppInterfaceService"
ROUTING = "capsulad.framework.v1.RoutingService"

MEDIA = "capsulad.contracts.media.v1.MediaPlayerService"
PLAYER = "org.capsulad.radio.v1.RadioPlayerService"
TUNER = "org.capsulad.radio.v1.RadioTunerService"

SEGMENT = "display.segment.front"
SPEAKER = "audio.output.speaker"
PLAYER_ID = "radio"
APP_NAME = "Internet radio"

CALL_TIMEOUT = 5000
# A long-lived stream (presentation, playback, capture) must have no overall
# deadline: call_start's timeout is the whole RPC deadline, so a non-zero value
# would make the daemon cancel the stream while the radio is still using it.
STREAM_DEADLINE = 0
TUNE_TIMEOUT = 25000  # the tuner dials, resolves playlists and decodes the first frame
STOP_TIMEOUT = 2000
PLAYBACK_OPEN_TIMEOUT = 5000
MAX_STATIONS = 99
MAX_CHUNK = 1 << 20
MAX_DRAIN = 8
STREAM_FORMATS = (1, 2)  # capsulad PCM_S16LE, PCM_F32LE
# Autonomous watchdog. The script host's run loop fires repeating callbacks
# (capsulad.every) even when no event arrives, so a tuner that dies without a
# final Push is detected without any user interaction. Two independent signals:
#   * liveness: the run this session tuned must still be the ready tuner app;
#     a killed or crashed tuner disappears from ListInterfaces within ~2 s.
#   * dry queue: a live but hung tuner stops queuing audio; the tuner's own
#     reconnect backoff (1/2/4 s) plus a fresh dial is why the bound is long.
WATCHDOG_INTERVAL_MS = 500
LIVENESS_TICKS = 4                       # ~2 s between liveness lookups
LIVENESS_TIMEOUT = 800                   # a watchdog lookup must not stall the loop
AUTHORIZE_TIMEOUT = 2000                 # push authorization lookup, inside a Push call
# One bound for "no audio reached the speaker", used both while buffering and
# while playing. The tuner's own budget is 3 reconnects in a 30 s window with
# 1/2/4 s backoff and a 12 s open timeout each, and its first chunk read after
# TuneResult can idle-time-out at 10 s: (1+12)+(2+12)+(4+12) = 43 s worst case.
# 60 s sits above that, so a legitimate retry window is never pre-empted, while
# a dead or hung tuner is still bounded.
NO_AUDIO_TIMEOUT_MS = 60000
# A run id's app mapping is immutable, so resolved run ids are cached: a foreign
# caller costs one lookup ever, and the tuned entry is never evicted by it.
PUSH_CALLER_CACHE = 16
# A successful half-close drains the queued tail (up to the 32-chunk queue).
# Draining is serviced by the pump/watchdog, never by blocking the loop.
DRAIN_TIMEOUT_MS = 30000

DIGITS = (0x3F, 0x06, 0x5B, 0x4F, 0x66, 0x6D, 0x7D, 0x07, 0x7F, 0x6F)
DASH = 0x40

MEDIA_PLAY = "MEDIA_PLAY"
MEDIA_PAUSE = "MEDIA_PAUSE"
MEDIA_SKIP_NEXT = "MEDIA_SKIP_NEXT"
MEDIA_SKIP_PREVIOUS = "MEDIA_SKIP_PREVIOUS"
MEDIA_STATES = {
    "stopped": "PLAYBACK_STOPPED",
    "buffering": "PLAYBACK_BUFFERING",
    "playing": "PLAYBACK_PLAYING",
    "paused": "PLAYBACK_PAUSED",
    "error": "PLAYBACK_STOPPED",
}

# --- state ------------------------------------------------------------------

stations = []           # [{"name": ..., "url": ...}]
station_number = 1      # 1-based effective selection
autoplay = False
tuner_setting = ""

phase = "stopped"       # stopped | buffering | playing | paused | error
detail = ""             # last status or failure detail
revision = 1
now_title = ""
now_artist = ""

settings_revision = 0
have_snapshot = False
settings_seen = False
pending_invocations = []

display_available = False
speaker_available = False

presentation = None
presentation_id = ""
capture = None
capture_id = ""
playback = None
playback_format = None
watchers = []

session_id = ""
session_url = ""
session_counter = 0
session_chunks = 0
tuner_app = ""
tuner_run = ""           # the run id that is actually serving this session
push_caller_apps = {}    # run id -> app id the daemon resolved (bounded)
push_caller_order = []   # insertion order for the bounded cache
last_queued = 0          # frames_queued from the last speaker progress event
dry_since_ms = 0         # monotonic time the speaker queue went dry while playing
buffering_since_ms = -1  # monotonic time the session entered `buffering`
playback_draining = False  # half-closed, draining the queued tail
drain_since_ms = 0         # monotonic time the drain started
watchdog_ticks = 0       # watchdog ticks since the last liveness lookup
watchdog_token = 0       # capsulad.every() token for the watchdog


def log(message):
    capsulad.log("radio: " + str(message))


# --- protobuf helpers (raw wire format, no descriptors) ---------------------


def _varint(value):
    out = bytearray()
    while True:
        byte = value & 0x7F
        value >>= 7
        if value:
            out.append(byte | 0x80)
        else:
            out.append(byte)
            return bytes(out)


def _read_varint(data, pos):
    result = 0
    shift = 0
    length = len(data)
    while pos < length:
        byte = data[pos]
        pos += 1
        result |= (byte & 0x7F) << shift
        if not byte & 0x80:
            return result, pos
        shift += 7
    raise ValueError("truncated varint")


def _field(field, wire):
    return _varint((field << 3) | wire)


def _uint(field, value):
    if not value:
        return b""
    return _field(field, 0) + _varint(int(value))


def _flag(field, value):
    if not value:
        return b""
    return _field(field, 0) + b"\x01"


def _raw(field, data):
    if not data:
        return b""
    return _field(field, 2) + _varint(len(data)) + data


def _text(field, text):
    return _raw(field, str(text).encode())


def _message(field, payload):
    return _field(field, 2) + _varint(len(payload)) + payload


def _scan(data):
    """Split one protobuf message into (field, wire, value) triples."""
    fields = []
    pos = 0
    end = len(data)
    while pos < end:
        key, pos = _read_varint(data, pos)
        field = key >> 3
        wire = key & 7
        if wire == 0:
            value, pos = _read_varint(data, pos)
        elif wire == 2:
            size, pos = _read_varint(data, pos)
            if pos + size > end:
                raise ValueError("truncated message")
            value = data[pos:pos + size]
            pos += size
        elif wire == 5:
            if pos + 4 > end:
                raise ValueError("truncated message")
            value = data[pos:pos + 4]
            pos += 4
        elif wire == 1:
            if pos + 8 > end:
                raise ValueError("truncated message")
            value = data[pos:pos + 8]
            pos += 8
        else:
            raise ValueError("unsupported wire type")
        fields.append((field, wire, value))
    return fields


def _value(fields, field, fallback=None):
    for item in fields:
        if item[0] == field:
            return item[2]
    return fallback


# --- settings value decoding ------------------------------------------------


def _json_text(raw):
    """Decode the JSON document of a text setting into its string content."""
    if raw is None:
        return ""
    text = raw.strip()
    if len(text) < 2 or text[0] != '"':
        return text
    out = []
    pos = 1
    end = len(text)
    while pos < end:
        ch = text[pos]
        if ch == '"':
            break
        if ch != "\\":
            out.append(ch)
            pos += 1
            continue
        pos += 1
        if pos >= end:
            break
        esc = text[pos]
        pos += 1
        if esc == "n":
            out.append("\n")
        elif esc == "r":
            out.append("\r")
        elif esc == "t":
            out.append("\t")
        elif esc == "u" and pos + 4 <= end:
            out.append(chr(int(text[pos:pos + 4], 16)))
            pos += 4
        else:
            out.append(esc)
    return "".join(out)


def _json_int(raw):
    text = (raw or "").strip()
    try:
        return int(text)
    except ValueError:
        return None


def _json_bool(raw):
    return (raw or "").strip() == "true"


# --- station list -----------------------------------------------------------


def parse_stations(text):
    """Parse the station setting: one 'Name | URL' per line, # comments."""
    found = []
    for raw_line in (text or "").split("\n"):
        line = raw_line.strip()
        if not line or line[0] == "#":
            continue
        name = ""
        url = line
        split = line.find("|")
        if split > 0:
            candidate = line[split + 1:].strip()
            if candidate.startswith("http://") or candidate.startswith("https://"):
                name = line[:split].strip()
                url = candidate
        if not (url.startswith("http://") or url.startswith("https://")):
            log("stations: line is neither 'Name | URL' nor an http(s) URL: " + line)
            continue
        found.append({"name": name or url, "url": url})
        if len(found) >= MAX_STATIONS:
            log("stations: list truncated at %d entries" % MAX_STATIONS)
            break
    return found


def station_index(number):
    """1-based number -> list index, or -1 when it is not selectable."""
    if number is None or not stations or number < 1 or number > len(stations):
        return -1
    return number - 1


def missing_station_reason():
    if not stations:
        return "no stations configured"
    return "station %s is not in the %d configured stations" % (
        station_number, len(stations))


def current_station():
    index = station_index(station_number)
    if index < 0:
        return None
    return stations[index]


def select_number(number):
    global station_number
    if not stations:
        return None
    if number < 1:
        number = 1
    if number > len(stations):
        number = len(stations)
    station_number = number
    return current_station()


def select_text(text):
    """Select by 1-based number, exact name (case-insensitive) or URL."""
    wanted = (text or "").strip()
    if not wanted or not stations:
        return None
    number = _json_int(wanted)
    if number is not None and 1 <= number <= len(stations):
        return select_number(number)
    lowered = wanted.lower()
    for index in range(len(stations)):
        if stations[index]["name"].lower() == lowered:
            return select_number(index + 1)
    for index in range(len(stations)):
        if stations[index]["url"] == wanted:
            return select_number(index + 1)
    return None


def step_station(delta):
    if not stations:
        return None
    count = len(stations)
    number = station_number + delta
    while number < 1:
        number += count
    while number > count:
        number -= count
    return select_number(number)


def persist_station():
    """Compare-and-swap the station setting after the app moved the selection."""
    global settings_revision
    try:
        snapshot = capsulad.call(CONFIG, "Get", {"appId": capsulad.env["appId"]},
                                 CALL_TIMEOUT)
        settings_revision = snapshot.get("revision") or 0
        stored = None
        for item in snapshot.get("values") or []:
            if item.get("key") == "station" and item.get("hasValue"):
                stored = _json_int(item.get("valueJson"))
        if stored == station_number:
            return
        updated = capsulad.call(CONFIG, "Update", {
            "appId": capsulad.env["appId"],
            "expectedRevision": str(settings_revision),
            "changes": [{"key": "station", "valueJson": str(station_number)}],
        }, CALL_TIMEOUT)
        settings_revision = updated.get("revision") or settings_revision
    except Exception as err:
        log("station setting not persisted: " + str(err))


# --- segment display --------------------------------------------------------


def draw():
    """Render the current state on the segment presentation."""
    if not display_available:
        return
    if phase == "stopped":
        close_presentation()
        return
    if not ensure_presentation():
        return
    if phase == "error":
        cells = []
        for _ in range(4):
            cells.append({"mask": DASH, "decimalPoint": False})
        colon = False
    else:
        number = station_number
        if number < 1:
            number = 1
        tens = number // 10
        ones = number % 10
        cells = [
            {"mask": 0, "decimalPoint": False},
            {"mask": 0, "decimalPoint": False},
            {"mask": DIGITS[tens] if tens else 0, "decimalPoint": False},
            {"mask": DIGITS[ones], "decimalPoint": phase == "paused"},
        ]
        colon = phase == "playing"
    try:
        presentation.send({"frame": {"segment": {"cells": cells, "colon": colon}}})
    except Exception as err:
        log("display frame failed: " + str(err))
        close_presentation()
        return
    drain_presentation()
    ensure_capture()


def ensure_presentation():
    """Open the segment presentation if it is not currently open.

    ``display_available`` is the capability the device advertised at startup;
    an Open failure is a transient condition, not a capability change, so it is
    only logged and the next draw retries (the display must recover without an
    app restart).
    """
    global presentation, presentation_id
    if presentation is not None:
        return True
    call = None
    try:
        call = capsulad.open(PRESENTATION, "Open", 3, STREAM_DEADLINE)
        call.send({"open": {"resourceId": SEGMENT}})
        event = call.recv(CALL_TIMEOUT)
    except Exception as err:
        log("display open failed (will retry): " + str(err))
        if call is not None:
            try:
                call.cancel()
            except Exception:
                pass
        return False
    if not event or not event.get("ready"):
        log("display open returned no ready event (will retry)")
        try:
            call.end(0, "")
        except Exception:
            pass
        return False
    presentation = call
    presentation_id = event["ready"].get("presentationId") or ""
    return True


def drain_presentation():
    for _ in range(MAX_DRAIN):
        try:
            event = presentation.recv(0)
        except OSError as err:
            if err.args[0] != 10:  # CAPSULAD_STATUS_TIMEOUT
                log("display stream ended: " + str(err))
                close_presentation()
            return
        if event is None:
            close_presentation()
            return
        if event.get("visibility") is not None:
            visible = event["visibility"].get("visible")
            log("display visible=%s reason=%s" % (visible, event["visibility"].get("reason", "")))


def ensure_capture():
    """Take the display from the configured default app while the radio is active.

    Without a capture the configured default app's baseline (the clock) wins, so
    the station display would never be seen on a device that selects one.
    """
    global capture, capture_id
    if capture is not None or not presentation_id:
        return
    try:
        call = capsulad.open(ROUTING, "Capture", 1, STREAM_DEADLINE)
        call.send({"resourceId": SEGMENT, "presentationId": presentation_id})
        call.half_close()
        event = call.recv(CALL_TIMEOUT)
    except Exception as err:
        log("display capture failed: " + str(err))
        try:
            call.cancel()
        except Exception:
            pass
        return
    granted = (event or {}).get("granted") or {}
    info = granted.get("capture") or {}
    capture_id = info.get("captureId") or ""
    if not capture_id:
        log("display capture was not granted")
        try:
            call.cancel()
        except Exception:
            pass
        return
    capture = call
    log("display capture %s selected=%s" % (capture_id, info.get("selected")))
    drain_capture()


def drain_capture():
    global capture, capture_id
    for _ in range(MAX_DRAIN):
        if capture is None:
            return
        try:
            event = capture.recv(0)
        except OSError as err:
            if err.args[0] != 10:
                log("display capture ended: " + str(err))
                capture = None
                capture_id = ""
            return
        if event is None:
            capture = None
            capture_id = ""
            return
        changed = event.get("changed")
        if changed is not None:
            log("display capture selected=%s reason=%s" % (
                changed.get("selected"), changed.get("reason", "")))


def release_capture():
    global capture, capture_id
    call = capture
    owned = capture_id
    capture = None
    capture_id = ""
    if owned:
        try:
            capsulad.call(ROUTING, "ReleaseCapture", {"captureId": owned},
                          CALL_TIMEOUT)
        except Exception as err:
            log("display capture release failed: " + str(err))
    if call is not None:
        try:
            call.cancel()
        except Exception:
            pass


def close_presentation():
    global presentation, presentation_id
    if presentation is None:
        return
    release_capture()
    call = presentation
    presentation = None
    presentation_id = ""
    try:
        call.send({"close": {}})
    except Exception:
        pass
    try:
        call.end(0, "")
    except Exception:
        try:
            call.cancel()
        except Exception:
            pass


# --- speaker stream ---------------------------------------------------------


def open_playback(sample_format, rate, channels):
    global playback, playback_format
    try:
        call = capsulad.open(AUDIO, "Playback", 3, STREAM_DEADLINE)
    except Exception as err:
        log("playback open failed: " + str(err))
        return None
    open_request = _message(1, _text(1, SPEAKER) + _message(
        2, _uint(1, sample_format) + _uint(2, rate) + _uint(3, channels)))
    try:
        call.send_bytes(open_request)
        started = call.recv_bytes(PLAYBACK_OPEN_TIMEOUT)
    except Exception as err:
        log("playback open failed: " + str(err))
        try:
            call.cancel()
        except Exception:
            pass
        return None
    if started is None:
        log("playback open was refused by the framework")
        try:
            call.cancel()
        except Exception:
            pass
        return None
    fields = _scan(started)
    info = _scan(_value(fields, 1, b""))
    stream_id = _as_text(_value(info, 1, b""))
    actual = _scan(_value(info, 2, b""))
    playback = call
    playback_format = (sample_format, rate, channels)
    log("speaker stream %s open %d Hz %d ch format=%d actual=%s/%s/%s" % (
        stream_id, rate, channels, sample_format,
        _value(actual, 1, 0), _value(actual, 2, 0), _value(actual, 3, 0)))
    return call


def _as_text(data):
    if isinstance(data, bytes):
        try:
            return data.decode()
        except Exception:
            return "?"
    return str(data)


def write_chunk(sample_format, rate, channels, pcm, frames, first_frame,
                discontinuity, dropped):
    """Write one chunk to the speaker; returns "" or a refusal reason."""
    global playback, playback_format
    if playback is None or playback_format != (sample_format, rate, channels):
        abort_playback()
        if open_playback(sample_format, rate, channels) is None:
            return "no playback stream"
    payload = _uint(2, first_frame) + _uint(4, frames) + _raw(5, pcm)
    payload += _flag(6, discontinuity) + _uint(7, dropped)
    global dry_since_ms
    try:
        playback.send_bytes(_message(2, payload))
    except Exception as err:
        log("playback write failed: " + str(err))
        abort_playback()
        return "no playback stream"
    dry_since_ms = 0
    pump_playback()
    return ""


def playback_failed(why):
    """A dead or stalled speaker/tuner stream must not keep claiming PLAYING."""
    log("playback failed: " + why)
    end_session(why)
    fail(why)


def pump_playback():
    """Read speaker events and notice a stream that has gone dry or ended.

    This runs after each chunk write, at the start of every inbound event or
    call, and from the watchdog callback. `frames_played` counts mixed output
    frames (silence included), so it is not evidence that the tuner is feeding
    us; `frames_queued` is this stream's own backlog, and the watchdog compares
    the time since audio was last queued against NO_AUDIO_TIMEOUT_MS.
    """
    global playback, playback_format, last_queued, dry_since_ms, playback_draining
    for _ in range(MAX_DRAIN):
        if playback is None:
            return
        try:
            event = playback.recv_bytes(0)
        except OSError as err:
            if err.args[0] != 10:  # CAPSULAD_STATUS_TIMEOUT
                log("playback stream ended: " + str(err))
                playback = None
                playback_format = None
                if playback_draining:
                    playback_draining = False
                    set_phase("stopped", "station stream ended")
                else:
                    playback_failed("speaker stream ended")
            return
        if event is None:
            playback = None
            playback_format = None
            if playback_draining:
                playback_draining = False
                set_phase("stopped", "station stream ended")
            else:
                playback_failed("speaker stream ended")
            return
        if playback_draining:
            # The tail is draining; progress and notices carry no new decision.
            continue
        fields = _scan(event)
        notice_bytes = _value(fields, 3, None)
        if notice_bytes is not None:
            notice = _scan(notice_bytes)
            code = _as_text(_value(notice, 1, b""))
            log("playback notice %s %s" % (
                code, _as_text(_value(notice, 2, b""))))
            if code in ("STREAM_CLOSED", "DAEMON_SHUTDOWN"):
                playback = None
                playback_format = None
                playback_failed("speaker stream " + code.lower())
                return
            continue
        progress_bytes = _value(fields, 2, None)
        if progress_bytes is None:
            continue
        progress = _scan(progress_bytes)
        # frames_played counts mixed output frames (silence included), so it is
        # not evidence that the tuner is feeding us. frames_queued is this
        # stream's own backlog: any queued audio means the stream is fed.
        last_queued = _value(progress, 2, 0)
        if last_queued > 0:
            dry_since_ms = 0


def tuner_alive():
    """Whether the run this session tuned is still the ready tuner app.

    The check is by **run**, not by app: a tuner restarted by the supervisor is
    ready again as the same app id within a second, and only the run id tells
    the session that the process serving it is gone. A live tuner with a
    temporarily dry stream (backoff, reconnect) keeps its run. A framework
    lookup failure is not evidence of anything, so it never fails the session,
    and the budget is short because this runs on the event thread.
    """
    if not tuner_app or not session_id or not tuner_run:
        return True
    try:
        response = capsulad.call(APP_IFACES, "ListInterfaces",
                                 {"runningOnly": True}, LIVENESS_TIMEOUT)
    except Exception as err:
        log("tuner liveness lookup failed: " + str(err))
        return True
    for item in response.get("interfaces") or []:
        if item.get("grpcService") != TUNER:
            continue
        if item.get("appId") != tuner_app or not item.get("available"):
            continue
        if item.get("runId") == tuner_run:
            return True
    return False


def watchdog():
    """Autonomous stall check, driven by the run loop's repeating callback.

    This is the path that runs when nothing else happens: a dead tuner sends no
    Push and no framework event, so without it the app would report PLAYING
    forever. Two signals: the serving run must still be ready (liveness) and
    audio must have been queued recently (dry queue).
    """
    global dry_since_ms, watchdog_ticks, buffering_since_ms, drain_since_ms
    pump_playback()
    if playback_draining:
        if capsulad.monotonic_ms() - drain_since_ms >= DRAIN_TIMEOUT_MS:
            log("drain timed out; discarding the tail")
            abort_playback()
            set_phase("stopped", "drain timeout")
        return
    if phase == "buffering":
        dry_since_ms = 0
        watchdog_ticks = 0
        now = capsulad.monotonic_ms()
        if buffering_since_ms >= 0 and \
                now - buffering_since_ms >= NO_AUDIO_TIMEOUT_MS:
            buffering_since_ms = -1
            playback_failed("tuner did not start (no audio for %d ms)" %
                            NO_AUDIO_TIMEOUT_MS)
        return
    buffering_since_ms = -1
    if phase != "playing":
        dry_since_ms = 0
        watchdog_ticks = 0
        return
    now = capsulad.monotonic_ms()
    if dry_since_ms == 0:
        dry_since_ms = now
    elif now - dry_since_ms >= NO_AUDIO_TIMEOUT_MS:
        dry_since_ms = 0
        playback_failed("tuner stalled (no queued audio for %d ms)" %
                        NO_AUDIO_TIMEOUT_MS)
        return
    watchdog_ticks += 1
    if watchdog_ticks >= LIVENESS_TICKS:
        watchdog_ticks = 0
        if not tuner_alive():
            playback_failed("tuner stopped (run is no longer ready)")


def control_playback(command):
    # A control frame after half-close is not legal (the request stream is
    # closed), and the drain must never be aborted behind the state machine.
    if playback is None or playback_draining:
        return
    try:
        playback.send_bytes(_uint(3, command))
    except Exception as err:
        log("playback control failed: " + str(err))
        abort_playback()


def finish_playback():
    """Half-close so the framework drains the queued tail, without blocking.

    The tail is serviced by pump_playback/watchdog, so control stays responsive
    while it plays out; the session ends when the daemon ends the event stream.
    """
    global playback_draining, drain_since_ms, buffering_since_ms
    if playback_draining:
        return
    if playback is None:
        # Nothing was ever queued (a terminal finished without audio): there is
        # no tail to drain, so end the session instead of waiting for the
        # buffering bound to report a missing tuner.
        buffering_since_ms = -1
        set_phase("stopped", "station stream ended")
        return
    try:
        playback.half_close()
    except Exception as err:
        log("playback half close failed: " + str(err))
        abort_playback()
        set_phase("stopped", "station stream ended")
        return
    playback_draining = True
    drain_since_ms = capsulad.monotonic_ms()
    log("speaker stream half-closed, draining the queued tail")


def abort_playback():
    global playback, playback_format, playback_draining
    playback_draining = False
    if playback is None:
        return
    call = playback
    playback = None
    playback_format = None
    try:
        call.cancel()
    except Exception:
        pass


# --- tuner session ----------------------------------------------------------


def find_tuner():
    if tuner_setting:
        return tuner_setting
    try:
        response = capsulad.call(APP_IFACES, "ListInterfaces",
                                 {"runningOnly": True}, CALL_TIMEOUT)
    except Exception as err:
        log("tuner discovery failed: " + str(err))
        return None
    for item in response.get("interfaces") or []:
        if item.get("grpcService") == TUNER and item.get("available"):
            return item.get("appId")
    return None


def tuner_tune(app_id, session, station):
    """Ask the tuner to fetch and start pushing a station."""
    call = None
    request = (_text(1, session) + _text(2, station["url"])
               + _text(3, station["name"]) + _text(4, capsulad.env["appId"]))
    try:
        call = capsulad.open_routed(app_id, TUNER, "Tune", TUNE_TIMEOUT)
        call.send_bytes(request)
        call.half_close()
        reply = call.recv_bytes(TUNE_TIMEOUT)
    except Exception as err:
        if call is not None:
            call.cancel()
        return False, "tuner unavailable: " + str(err)
    if reply is None:
        call.end(0, "")
        return False, "tuner returned no result"
    call.end(0, "")
    fields = _scan(reply)
    accepted = bool(_value(fields, 1, 0))
    reason = _as_text(_value(fields, 2, b""))
    echoed = _as_text(_value(fields, 3, b""))
    if not accepted:
        return False, reason or "tuner refused the station"
    if echoed and echoed != session:
        return False, "tuner answered another session"
    return True, reason


def tuner_stop(app_id, session, why):
    """Best effort: tell the tuner to drop the session."""
    if not app_id or not session:
        return
    call = None
    try:
        call = capsulad.open_routed(app_id, TUNER, "Stop", STOP_TIMEOUT)
        call.send_bytes(_text(1, session) + _text(2, why))
        call.half_close()
        call.recv_bytes(STOP_TIMEOUT)
        call.end(0, "")
    except Exception as err:
        if call is not None:
            try:
                call.cancel()
            except Exception:
                pass
        log("tuner stop not acknowledged: " + str(err))


# --- radio state machine ----------------------------------------------------


def set_phase(next_phase, why=""):
    global phase, detail, revision
    phase = next_phase
    detail = why
    revision += 1
    log("state %s %s" % (next_phase, why))
    notify_watchers()
    draw()


def fail(why):
    log("error: " + why)
    set_phase("error", why)


def bump_state():
    global revision
    revision += 1
    notify_watchers()
    draw()


def clear_session():
    global session_id, session_url
    session_id = ""
    session_url = ""


def start_session(why):
    """Tune the selected station; returns (ok, detail)."""
    global session_id, session_url, session_counter, tuner_app, tuner_run
    global now_title, now_artist, session_chunks, dry_since_ms, buffering_since_ms
    station = current_station()
    if station is None:
        reason = missing_station_reason()
        fail(reason)
        return False, reason
    if not speaker_available:
        fail("no speaker endpoint on this device")
        return False, "no speaker endpoint on this device"
    app_id = find_tuner()
    if not app_id:
        fail("no tuner provider running (install a " + TUNER + " provider)")
        return False, "no tuner provider is running"
    session_counter += 1
    session_id = "radio-%d-%s" % (session_counter, str(capsulad.env["runId"])[-6:])
    session_url = station["url"]
    tuner_app = app_id
    tuner_run = ""
    now_title = ""
    now_artist = ""
    session_chunks = 0
    dry_since_ms = 0
    # Show the station while the tuner dials: Tune blocks for as long as the
    # tuner needs to open the stream, and the user must see progress.
    set_phase("buffering", why)
    ok, reason = tuner_tune(app_id, session_id, station)
    if not ok:
        clear_session()
        fail(reason)
        return False, reason
    log("tuning %s (%s) via %s" % (station["name"], why, app_id))
    buffering_since_ms = capsulad.monotonic_ms()
    bump_state()
    return True, reason


def end_session(why):
    """Tear the session down; abort discards queued audio."""
    if session_id:
        log("session %s ended after %d chunks (%s)" % (
            session_id, session_chunks, why))
        tuner_stop(tuner_app, session_id, why)
    clear_session()
    abort_playback()


def stop_playback(why):
    end_session(why)
    set_phase("stopped", why)
    return True, reason_text(why)


def reason_text(why):
    if why == "stop":
        return "stopped"
    if why == "dismiss":
        return "dismissed"
    return why or "stopped"


def start_playback(why):
    if phase == "paused":
        return resume_playback()
    if phase in ("playing", "buffering"):
        station = current_station()
        if station is not None and station["url"] == session_url:
            return True, "already playing"
        end_session("station changed")
    return start_session(why)


def pause_playback():
    if playback_draining:
        # The session is already ending: the queued tail is playing out and the
        # stream cannot be paused any more. Treat the pause as a stop.
        abort_playback()
        set_phase("stopped", "station stream ended")
        return True, "stream ended"
    if phase not in ("playing", "buffering"):
        return False, "nothing is playing"
    control_playback(2)  # PLAYBACK_PAUSE
    set_phase("paused", "paused")
    return True, "paused"


def resume_playback():
    if phase != "paused":
        return False, "nothing is paused"
    # Resume is a fresh session: discard the parked tail instead of replaying
    # it (and never let the previous station's tail play after a switch).
    abort_playback()
    return start_session("resumed")


def retune_selection():
    """Re-tune only when an active session no longer matches the selection."""
    station = current_station()
    active = phase in ("playing", "buffering", "paused")
    if station is None:
        if active:
            end_session("no stations")
            fail("no stations configured")
        else:
            bump_state()
        return False, "no stations configured"
    if active and station["url"] != session_url:
        end_session("station changed")
        return start_session("station changed")
    bump_state()
    return True, "selected"


def step_and_play(delta):
    """next/previous: move the selection and play it when a session was live."""
    if not stations:
        return False, "no stations configured"
    step_station(delta)
    persist_station()
    if phase in ("playing", "buffering", "paused", "error"):
        end_session("station changed")
        return start_session("station changed")
    bump_state()
    return True, "selected"


def control(action):
    """Apply one user control; returns (ok, detail)."""
    if action == "play":
        return start_playback("play")
    if action == "pause":
        return pause_playback()
    if action == "stop" or action == "dismiss":
        return stop_playback(action)
    if action == "next":
        return step_and_play(1)
    if action == "previous":
        return step_and_play(-1)
    return False, "unknown action " + str(action)


# --- media contract ---------------------------------------------------------


def media_status():
    station = current_station()
    source = station["name"] if station else ""
    actions = [MEDIA_SKIP_NEXT, MEDIA_SKIP_PREVIOUS]
    if phase in ("playing", "buffering"):
        actions.insert(0, MEDIA_PAUSE)
    else:
        actions.insert(0, MEDIA_PLAY)
    status = {
        "playerId": PLAYER_ID,
        "playbackState": MEDIA_STATES.get(phase, "PLAYBACK_STOPPED"),
        "supportedActions": actions,
        "appName": APP_NAME,
        "sourceName": source,
        "mediaName": now_title or source,
        "revision": str(revision),
    }
    if now_title:
        status["title"] = now_title
    if now_artist:
        status["artist"] = now_artist
    return status


def notify_watchers():
    if not watchers:
        return
    status = media_status()
    alive = []
    for call in watchers:
        try:
            call.send({"updated": status})
            alive.append(call)
        except Exception:
            try:
                call.cancel()
            except Exception:
                pass
    watchers[:] = alive


def handle_media(call):
    info = call.info or {}
    method = info.get("rpc_name") or ""
    # Inbound calls also pump the speaker stream (the watchdog covers the idle
    # case), so a dead tuner is noticed before this answer is built.
    pump_playback()
    if method == "WatchPlayers":
        try:
            call.recv(CALL_TIMEOUT)
            call.send({"snapshot": {"players": [media_status()]}})
        except Exception as err:
            log("WatchPlayers failed: " + str(err))
            call.cancel()
            return
        watchers.append(call)
        return
    try:
        request = call.recv(CALL_TIMEOUT) or {}
    except Exception as err:
        call.end(13, str(err))
        return
    if method == "ListPlayers":
        call.send({"players": [media_status()]})
        call.end(0, "")
        return
    if method not in ("GetStatus", "Play", "Pause", "SkipNext", "SkipPrevious"):
        call.end(12, "unimplemented method " + method)
        return
    player_id = request.get("playerId") or ""
    if not player_id:
        call.end(3, "playerId is required")
        return
    if player_id != PLAYER_ID:
        call.end(5, "unknown player " + player_id)
        return
    if method == "GetStatus":
        call.send(media_status())
        call.end(0, "")
        return
    if method == "Play":
        ok, why = start_playback("media play")
    elif method == "Pause":
        ok, why = pause_playback()
    elif method == "SkipNext":
        ok, why = control("next")
    else:  # SkipPrevious
        ok, why = control("previous")
    call.send({"accepted": bool(ok), "detail": why or ""})
    call.end(0, "")


# --- player interface (tuner -> app) ----------------------------------------


def handle_push(call):
    try:
        data = call.recv_bytes(CALL_TIMEOUT)
    except Exception as err:
        call.end(13, str(err))
        return
    if data is None:
        call.end(0, "empty push")
        return
    accepted, why, finished, failed = accept_chunk(
        data, (call.info or {}).get("caller_run_id") or "")
    try:
        call.send_bytes(_flag(1, accepted) + _text(2, why))
        call.end(0, "")
    except Exception as err:
        log("push reply failed: " + str(err))
        return
    if finished and not failed:
        clear_session()
        finish_playback()


def authorize_pusher(caller_run):
    """Only the tuner run this player tuned may push audio.

    The daemon resolves ``caller_run_id`` from peer credentials and the
    supervised PID, so matching it against the running tuner app's run id is an
    identity check, not a claim check. The result is cached per caller run id
    (one extra unary call per session, not per chunk).

    Returns True when authorized, False when the caller is refused, and None
    when the lookup itself failed: a refusal leaves the session untouched, while
    a failed lookup means the push is lost either way and the caller must not be
    left claiming PLAYING.
    """
    global tuner_run
    if not caller_run:
        return False
    if caller_run in push_caller_apps:
        if push_caller_apps[caller_run] == tuner_app and tuner_app != "":
            tuner_run = caller_run
            return True
        return False
    try:
        response = capsulad.call(APP_IFACES, "ListInterfaces",
                                 {"runningOnly": True}, AUTHORIZE_TIMEOUT)
    except Exception as err:
        log("push authorization lookup failed: " + str(err))
        return None
    resolved = ""
    for item in response.get("interfaces") or []:
        if item.get("grpcService") != TUNER:
            continue
        if item.get("runId") != caller_run or not item.get("available"):
            continue
        resolved = item.get("appId") or ""
        break
    if not resolved:
        log("push from unidentifiable run %s refused" % caller_run)
        return False
    cache_caller(caller_run, resolved)
    if resolved != tuner_app:
        log("push from %s (run %s) refused: not the tuned app %s" % (
            resolved, caller_run, tuner_app))
        return False
    tuner_run = caller_run
    log("push authorized app=%s run=%s" % (resolved, caller_run))
    return True


def cache_caller(caller_run, app_id):
    """Remember one immutable run-id -> app-id resolution (bounded).

    An empty resolution is not cached: a run id can be unknown because the
    registry has not caught up yet, and a permanent negative would be wrong.
    """
    if not app_id:
        return
    if caller_run in push_caller_apps:
        return
    if len(push_caller_order) >= PUSH_CALLER_CACHE:
        oldest = push_caller_order.pop(0)
        push_caller_apps.pop(oldest, None)
    push_caller_order.append(caller_run)
    push_caller_apps[caller_run] = app_id


def accept_chunk(data, caller_run):
    """Validate one pushed chunk; returns (accepted, detail, finished, failed)."""
    global session_chunks
    try:
        fields = _scan(data)
    except ValueError as err:
        return False, "malformed chunk: " + str(err), False, False
    session = _as_text(_value(fields, 2, b""))
    finished = bool(_value(fields, 12, 0))
    failed = bool(_value(fields, 13, 0))
    reason = _as_text(_value(fields, 14, b""))
    # Ownership and identity first. The session echo is required and exact (the
    # contract sends TuneResult.session_id), and a superseded or foreign message
    # is never terminal for the current session.
    if not session or session != session_id:
        return False, "stale session", False, False
    authorized = authorize_pusher(caller_run)
    if authorized is None:
        # The lookup failed. This chunk is lost either way (a refusal makes the
        # tuner drop the session), so when the caller is the run that is serving
        # this session, fail now instead of claiming PLAYING in silence until
        # the dry-queue bound. An unknown or foreign caller must never be able
        # to end playback through a lookup failure: it is only refused, and
        # liveness/dry-queue still bound the real session.
        if tuner_run and caller_run == tuner_run:
            clear_session()
            abort_playback()
            fail("push authorization unavailable")
            return False, "push authorization unavailable", False, False
        return False, "unauthorized caller", False, False
    if not authorized:
        return False, "unauthorized caller", False, False
    if failed:
        fail(reason or "tuner reported a failure")
        end_session("tuner failed")
        return False, "session failed", True, True
    if finished:
        return True, "session finished", True, False
    if phase == "paused":
        return False, "paused", False, False
    if phase not in ("buffering", "playing"):
        return False, "stopped", False, False
    pcm = _value(fields, 3, b"")
    if not pcm:
        return False, "empty chunk", False, False
    if len(pcm) > MAX_CHUNK:
        return False, "over capacity", False, False
    sample_format = _value(fields, 6, 0)
    rate = _value(fields, 4, 0)
    channels = _value(fields, 5, 0)
    if sample_format not in STREAM_FORMATS or rate < 8000 or rate > 192000 or \
            channels < 1 or channels > 2:
        return False, "unsupported format", False, False
    first_frame = _value(fields, 7, 0)
    discontinuity = bool(_value(fields, 8, 0))
    dropped = _value(fields, 9, 0)
    frame_bytes = channels * (2 if sample_format == 1 else 4)
    if len(pcm) % frame_bytes:
        return False, "malformed chunk: partial frame", False, False
    title = _as_text(_value(fields, 10, b""))
    artist = _as_text(_value(fields, 11, b""))
    # An empty pair means "unchanged" (the tuner only sends ICY metadata when
    # it changes), so never clear the now-playing line with an empty chunk.
    if (title or artist) and (title != now_title or artist != now_artist):
        update_metadata(title, artist)
    frames = len(pcm) // frame_bytes
    refusal = write_chunk(sample_format, rate, channels, pcm, frames,
                          first_frame, discontinuity, dropped)
    if refusal:
        fail("speaker stream failed")
        end_session("speaker failure")
        return False, refusal, False, False
    if phase == "error":
        # pump_playback detected a dead/stalled stream during the write.
        return False, "session ended", False, False
    session_chunks += 1
    if session_chunks % 50 == 0:
        log("session %s playing %d chunks" % (session_id, session_chunks))
    if phase == "buffering":
        set_phase("playing", "station audio")
    return True, "", False, False


def update_metadata(title, artist):
    global now_title, now_artist, revision
    now_title = title
    now_artist = artist
    revision += 1
    notify_watchers()
    log("now playing: %s%s" % (title, (" - " + artist) if artist else ""))


# --- events -----------------------------------------------------------------


def on_call(call):
    info = call.info or {}
    service = info.get("grpc_service") or ""
    if service == MEDIA:
        handle_media(call)
        return
    if service == PLAYER:
        if (info.get("rpc_name") or "") == "Push":
            handle_push(call)
        else:
            call.end(12, "unimplemented method")
        return
    call.end(12, "unknown service " + service)


def apply_values(values):
    """Apply a settings snapshot: both shapes are accepted.

    ``ApplicationConfigurationService.Get`` reports ``{key, valueJson, hasValue}``
    while the app-private ``AppSettingsChanged`` carries ``{key, valueJson}``
    with unset keys simply absent, so a value is applied whenever valueJson is
    present and hasValue is not explicitly false.
    """
    global stations, station_number, autoplay, tuner_setting
    new_stations = None
    for item in values:
        key = item.get("key")
        if not key or item.get("hasValue") is False:
            continue
        raw = item.get("valueJson")
        if raw is None:
            continue
        if key == "stations":
            new_stations = parse_stations(_json_text(raw))
        elif key == "station":
            number = _json_int(raw)
            if number is not None:
                station_number = number
        elif key == "autoplay":
            autoplay = _json_bool(raw)
        elif key == "tuner":
            tuner_setting = _json_text(raw)
    if new_stations is not None:
        stations = new_stations
    if stations and (station_number < 1 or station_number > len(stations)):
        log("station %s is outside the %d configured stations" % (
            station_number, len(stations)))


def read_snapshot():
    """Read the stored settings once, before the event stream starts."""
    global settings_revision, have_snapshot
    try:
        snapshot = capsulad.call(CONFIG, "Get", {"appId": capsulad.env["appId"]},
                                 CALL_TIMEOUT)
    except Exception as err:
        log("settings read failed: " + str(err))
        return
    settings_revision = snapshot.get("revision") or 0
    apply_values(snapshot.get("values") or [])
    have_snapshot = True
    log("settings snapshot revision=%s stations=%d station=%s autoplay=%s tuner=%s" % (
        settings_revision, len(stations), station_number, autoplay, tuner_setting))


def on_settings(event):
    global settings_revision, have_snapshot, settings_seen, pending_invocations
    event = event or {}
    settings_revision = event.get("revision") or settings_revision
    apply_values(event.get("values") or [])
    log("settings revision=%s stations=%d station=%s autoplay=%s" % (
        settings_revision, len(stations), station_number, autoplay))
    if not have_snapshot:
        have_snapshot = True
        queued = pending_invocations
        pending_invocations = []
        for item in queued:
            apply_invocation(item)
    if not settings_seen:
        settings_seen = True
        if autoplay and phase == "stopped":
            start_playback("autoplay")
        return
    retune_selection()


def on_action(event):
    event = event or {}
    action = event.get("actionId") or ""
    log("action " + action)
    ok, why = control(action)
    capsulad.notify_action(event.get("actionInvocationId"), ok, why or "ok")


def on_invocation(event):
    if not have_snapshot:
        if len(pending_invocations) < 4:
            pending_invocations.append(event)
        else:
            log("invocation dropped: still waiting for the settings snapshot")
        return
    apply_invocation(event)


def apply_invocation(event):
    event = event or {}
    ok = True
    why = "radio ready"
    wanted = None
    for argument in event.get("arguments") or []:
        if argument.get("name") == "station" and argument.get("value"):
            wanted = argument.get("value")
    if wanted is not None:
        if stations and select_text(wanted) is None:
            ok = False
            why = "unknown station " + str(wanted)
        elif stations:
            persist_station()
    if ok and wanted is not None:
        # A bare launch only ensures the app is running; it never starts audio
        # on its own (a boot start also delivers an invocation with no
        # arguments). Naming a station is the explicit "play this" request.
        ok, why = start_playback("invocation")
    log("invocation %s ok=%s %s" % (event.get("invocationId", "?"), ok, why))
    capsulad.notify_invocation(event.get("invocationId"), ok, why or "ok")


def on_disconnect(event):
    event = event or {}
    log("bus ended status=%s detail=%s" % (event.get("status"), event.get("detail")))
    shutdown("bus ended")


# --- shutdown ---------------------------------------------------------------


def shutdown(why):
    global phase, detail, revision
    for call in watchers:
        try:
            call.cancel()
        except Exception:
            pass
    watchers[:] = []
    end_session(why)
    abort_playback()
    close_presentation()
    phase = "stopped"
    detail = why
    revision += 1
    log("shutdown: " + why)


# --- startup ----------------------------------------------------------------


def probe_capabilities():
    global display_available, speaker_available
    try:
        response = capsulad.call(DEVICE, "ListResources", {}, CALL_TIMEOUT)
        for item in response.get("resources") or []:
            if item.get("resourceId") == SEGMENT:
                display_available = True
    except Exception as err:
        log("resource probe failed: " + str(err))
    try:
        response = capsulad.call(AUDIO, "ListEndpoints", {}, CALL_TIMEOUT)
        for item in response.get("endpoints") or []:
            if item.get("endpointId") == SPEAKER:
                speaker_available = True
    except Exception as err:
        log("endpoint probe failed: " + str(err))


def guard(name, handler):
    """Event handlers must never unwind into the host run loop.

    The prelude protects the inbound-call handler only; an exception from an
    event handler would end the script (and restart the run).
    """
    def wrapped(event):
        try:
            # Inbound events pump the speaker stream too, so a dead producer
            # is noticed promptly in addition to the watchdog tick.
            pump_playback()
        except Exception as err:
            log("playback pump failed: %s" % (err,))
        try:
            handler(event)
        except Exception as err:
            log("%s handler failed: %s" % (name, err))
    return wrapped


def startup():
    capsulad.on("call", on_call)
    capsulad.on("invocation", guard("invocation", on_invocation))
    capsulad.on("settings_changed", guard("settings", on_settings))
    capsulad.on("action", guard("action", on_action))
    capsulad.on("disconnect", guard("disconnect", on_disconnect))
    capsulad.start()
    log("ready app=%s run=%s sdk=%s" % (
        capsulad.env["appId"], capsulad.env["runId"], capsulad.env["sdk"]))
    probe_capabilities()
    log("capabilities display=%s speaker=%s" % (display_available, speaker_available))
    read_snapshot()
    global watchdog_token
    watchdog_token = capsulad.every(WATCHDOG_INTERVAL_MS, watchdog)
    log("watchdog every %d ms, liveness %d ticks, dry queue %d ms" % (
        WATCHDOG_INTERVAL_MS, LIVENESS_TICKS, NO_AUDIO_TIMEOUT_MS))


startup()
capsulad.run()
shutdown("bus ended")
