"""Test-only tuner fixture for the Internet Radio app.

It serves ``org.capsulad.radio.v1.RadioTunerService`` over raw protobuf exactly
like the real companion tuner, but instead of fetching a station it synthesizes
a square-wave tone and pushes it to the player's ``RadioPlayerService.Push``.
``apps/radio/tests/e2e.sh`` uses it to prove the shipped app against a real
simulator daemon and the real MicroPython host. It never opens a network
connection and is not part of the radio package.
"""

import capsulad

PLAYER = "org.capsulad.radio.v1.RadioPlayerService"
TUNER = "org.capsulad.radio.v1.RadioTunerService"
PLAYER_APP = "org.capsulad.radio"

RATE = 48000
CHANNELS = 2
FRAMES = 960          # 20 ms at 48 kHz
CHUNK_FRAMES = 9600   # 200 ms

player_app = PLAYER_APP
chunks = 25
frequency = 220

session_id = ""
chunk_pcm = b""
pending = 0
sequence = 0
tone_cache = {}


def log(message):
    capsulad.log("testtuner: " + str(message))


# --- protobuf helpers (raw wire format) -------------------------------------


def varint(value):
    out = bytearray()
    while True:
        byte = value & 0x7F
        value >>= 7
        if value:
            out.append(byte | 0x80)
        else:
            out.append(byte)
            return bytes(out)


def read_varint(data, pos):
    result = 0
    shift = 0
    while pos < len(data):
        byte = data[pos]
        pos += 1
        result |= (byte & 0x7F) << shift
        if not byte & 0x80:
            return result, pos
        shift += 7
    raise ValueError("truncated varint")


def uint(field, value):
    if not value:
        return b""
    return varint((field << 3) | 0) + varint(int(value))


def boolean(field, value):
    if not value:
        return b""
    return varint((field << 3) | 0) + b"\x01"


def raw(field, data):
    if not data:
        return b""
    return varint((field << 3) | 2) + varint(len(data)) + data


def text(field, value):
    return raw(field, str(value).encode())


def scan(data):
    fields = {}
    pos = 0
    end = len(data)
    while pos < end:
        key, pos = read_varint(data, pos)
        field = key >> 3
        wire = key & 7
        if wire == 0:
            value, pos = read_varint(data, pos)
        elif wire == 2:
            size, pos = read_varint(data, pos)
            if pos + size > end:
                raise ValueError("truncated message")
            value = data[pos:pos + size]
            pos += size
        elif wire == 5:
            value = data[pos:pos + 4]
            pos += 4
        elif wire == 1:
            value = data[pos:pos + 8]
            pos += 8
        else:
            raise ValueError("bad wire type")
        fields[field] = value
    return fields


def as_text(value):
    if isinstance(value, bytes):
        try:
            return value.decode()
        except Exception:
            return ""
    return ""


# --- synthetic audio --------------------------------------------------------


def tone_block(hz):
    """One 20 ms interleaved PCM_S16LE stereo block of a square wave."""
    half = RATE // (2 * hz)
    if half < 1:
        half = 1
    out = bytearray()
    for frame in range(FRAMES):
        sample = 8000 if (frame // half) % 2 == 0 else -8000
        low = sample & 0xFF
        high = (sample >> 8) & 0xFF
        out.append(low)
        out.append(high)
        out.append(low)
        out.append(high)
    return bytes(out)


def push(session, pcm, sequence, first_frame, finished=False, detail="",
         title="", artist=""):
    request = (uint(1, sequence) + text(2, session) + raw(3, pcm)
               + uint(4, RATE) + uint(5, CHANNELS) + uint(6, 1)
               + uint(7, first_frame) + text(10, title) + text(11, artist)
               + boolean(12, finished) + text(14, detail))
    call = None
    try:
        call = capsulad.open_routed(player_app, PLAYER, "Push", 15000)
        call.send_bytes(request)
        call.half_close()
        reply = call.recv_bytes(15000)
    except Exception as err:
        if call is not None:
            call.cancel()
        return False, "push failed: " + str(err)
    if reply is None:
        call.end(0, "")
        return False, "push returned no result"
    fields = scan(reply)
    accepted = bool(fields.get(1, 0))
    why = as_text(fields.get(2, b""))
    call.end(0, "")
    return accepted, why


def tone_pcm():
    """200 ms of the synthetic tone, cached per frequency."""
    pcm = tone_cache.get(frequency)
    if pcm is None:
        pcm = tone_block(frequency) * (CHUNK_FRAMES // FRAMES)
        tone_cache[frequency] = pcm
    return pcm


def start_session(session, url, name):
    """Reply-then-push, like the real tuner: Tune returns and audio follows."""
    global session_id, chunk_pcm, pending, sequence
    session_id = session
    chunk_pcm = tone_pcm()
    pending = chunks
    sequence = 0
    log("tune url=%s name=%s session=%s chunks=%d" % (url, name, session, chunks))


def clear_session(why):
    global session_id, pending
    if session_id:
        log("session %s stopped (%s)" % (session_id, why))
    session_id = ""
    pending = 0


def tick():
    """One chunk per 200 ms, from the host's repeating callback."""
    global pending, sequence
    if not session_id or pending <= 0:
        return
    # One metadata frame, like an ICY StreamTitle on the first chunk.
    title = "Fixture Tone" if sequence == 0 else ""
    artist = "Test Tuner" if sequence == 0 else ""
    accepted, why = push(session_id, chunk_pcm, sequence + 1,
                         sequence * CHUNK_FRAMES, title=title, artist=artist)
    if not accepted:
        log("push refused after %d chunks: %s" % (sequence, why))
        clear_session("refused")
        return
    sequence += 1
    pending -= 1
    if pending == 0:
        push(session_id, b"", sequence + 1, sequence * CHUNK_FRAMES, True,
             "fixture end")
        log("pushed %d chunks then finished" % sequence)
        clear_session("finished")


# --- events -----------------------------------------------------------------


def on_call(call):
    info = call.info or {}
    service = info.get("grpc_service") or ""
    method = info.get("rpc_name") or ""
    if service != TUNER:
        call.end(12, "testtuner only serves " + TUNER)
        return
    try:
        request = call.recv_bytes(5000)
    except Exception as err:
        call.end(13, str(err))
        return
    if request is None:
        call.end(0, "empty request")
        return
    try:
        fields = scan(request)
    except ValueError as err:
        call.end(3, str(err))
        return
    session = as_text(fields.get(1, b""))
    if method == "Stop":
        detail = as_text(fields.get(2, b""))
        call.send_bytes(boolean(1, True))
        call.end(0, "")
        if session and session == session_id:
            clear_session(detail or "stop")
        return
    if method != "Tune":
        call.end(12, "unimplemented method " + method)
        return
    url = as_text(fields.get(2, b""))
    name = as_text(fields.get(3, b""))
    target = as_text(fields.get(4, b""))
    if target and target != player_app:
        detail = "player app " + target + " is not configured"
        log("refused " + detail)
        call.send_bytes(boolean(1, False) + text(2, detail))
        call.end(0, "")
        return
    call.send_bytes(boolean(1, True) + text(3, session) + text(4, "audio/x-fixture-tone"))
    call.end(0, "")
    start_session(session, url, name)


def on_settings(event):
    global player_app, chunks, frequency
    for item in (event or {}).get("values") or []:
        key = item.get("key")
        # AppSettingsChanged carries {key, valueJson} without hasValue; a
        # value is applied whenever valueJson is present.
        if not key or item.get("hasValue") is False:
            continue
        raw_value = item.get("valueJson")
        if raw_value is None:
            continue
        if key == "player":
            player_app = decode_text(raw_value)
        elif key == "chunks":
            value = decode_int(raw_value)
            if value is not None and value >= 1:
                chunks = value
        elif key == "frequency":
            value = decode_int(raw_value)
            if value is not None and value >= 40:
                frequency = value
    log("settings player=%s chunks=%d frequency=%d" % (player_app, chunks, frequency))


def decode_text(raw_value):
    text_value = raw_value.strip()
    if len(text_value) >= 2 and text_value[0] == '"':
        return text_value[1:-1]
    return text_value


def decode_int(raw_value):
    try:
        return int((raw_value or "").strip())
    except ValueError:
        return None


capsulad.on("call", on_call)
capsulad.on("settings_changed", on_settings)
capsulad.start()
log("ready app=%s run=%s" % (capsulad.env["appId"], capsulad.env["runId"]))
capsulad.every(200, tick)
capsulad.run()
log("bus ended")
