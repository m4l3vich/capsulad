"""PA speaker: play one WAV file on the framework speaker.

Trigger      the ``Play`` RPC of ``org.capsulad.pa.v1.SpeakerService``, reached
             by another app through ``AppBusService.Call``.
Feedback     one ``PlayResult`` event on success; ``PlayCredit`` events that
             bound in-flight request frames; a non-OK gRPC status carrying the
             reason when the file is rejected, truncated or cancelled; and one
             ``pa:`` log line per call.
Ownership    the app opens exactly one ``AudioService.Playback`` stream per
             ``Play`` call and releases it on every terminal path. It holds no
             resource between calls and does not claim the microphone, the ring
             or the touch panel.
Interruption the caller cancels the call: the queued audio is discarded, the
             call ends, and the speaker is freed. A run stop, a lost bus or a
             daemon restart tears the stream down through the framework's own
             cleanup.
Dismissal    ``ApplicationService.Stop`` (``capsulad-cli app stop``) ends the
             run; the app is idle between calls, so there is nothing else to
             dismiss.
Final state  no playback stream, no buffered file, no pending call and no
             playback error state: the app is ready for the next ``Play``.

Play is bidirectional and credit-based. The app grants an initial two request
frames, then one more after it has fully consumed each frame. The caller must
not send ahead of its credits; this keeps the native SDK's 32-frame inbound
queue from overflowing when the app is slow or another call is playing.

Interaction limits, all enforced here and documented in docs/speaker.md: the
request stream is a client stream of WAV slices, each at most 128 KiB; the WAV
must be RIFF/WAVE with 16-bit PCM or 32-bit IEEE float samples, 1 or 2
channels, 8000..48000 Hz; the audio may not exceed the ``max_seconds`` setting
(1..60, default 30). Anything else is rejected before or during playback with
an explicit status.

The script host dispatches one call at a time and has no timers, so a ``Play``
in progress delays the next call; the app therefore has no separate stop and
cancelling the call is the stop.
"""

import capsulad

SERVICE = "org.capsulad.pa.v1.SpeakerService"
AUDIO_SERVICE = "capsulad.framework.v1.AudioService"
CONFIG_SERVICE = "capsulad.framework.v1.ApplicationConfigurationService"
SPEAKER_ENDPOINT = "audio.output.speaker"

# Bounds (see docs/speaker.md).
MAX_FRAME_BYTES = 128 * 1024  # one request message; also the 4 MiB heap guard
PLAY_CHUNK = 128 * 1024  # one Playback write
MAX_TRAILER_BYTES = 64 * 1024
MAX_HEADER_BYTES = 1024 * 1024
MAX_CHUNKS = 64
MAX_FMT_BYTES = 64
MIN_RATE = 8000
MAX_RATE = 48000
MAX_CHANNELS = 2
READ_TIMEOUT_MS = 15000
POLL_MS = 50
BUSY_RETRIES = 20
DEFAULT_MAX_SECONDS = 30
MAX_SECONDS_LIMIT = 60
INITIAL_CREDITS = 2  # request frames granted before the first read

# PLAY_CHUNK also bounds the number of Playback writes. The largest accepted
# audio is MAX_SECONDS_LIMIT s at MAX_RATE Hz, MAX_CHANNELS channels and 32-bit
# samples: 60 * 48000 * 2 * 4 = 23,040,000 bytes, or 176 writes of 128 KiB.
# That stays below the shared engine's 256-message outbound call queue, so a
# Playback write - including the final half-close that starts the drain - can
# never be rejected with BUSY and lost. Keep PLAY_CHUNK >= 96 KiB if these
# limits are ever raised.

# Sample formats as AudioFormat.sample_format.
SAMPLE_PCM16 = 1
SAMPLE_FLOAT32 = 2

# gRPC status codes used in replies.
OK = 0
CANCELLED = 1
INVALID_ARGUMENT = 3
DEADLINE_EXCEEDED = 4
RESOURCE_EXHAUSTED = 8
UNIMPLEMENTED = 12
INTERNAL = 13
UNAVAILABLE = 14

# ABI statuses surfaced as OSError.args[0].
STATUS_TIMEOUT = 10
STATUS_CANCELLED = 11
STATUS_BUSY = 13

# Local ABI statuses -> the gRPC code a caller sees. The two spaces overlap
# numerically but mean different things, so every value is mapped explicitly;
# anything unknown becomes INTERNAL rather than an unrelated code.
ABI_TO_GRPC = {
    1: INVALID_ARGUMENT,  # malformed argument
    3: UNAVAILABLE,  # client or bus not connected
    4: UNAVAILABLE,  # handle closed or call already finished
    5: INTERNAL,  # descriptor not found
    6: INTERNAL,  # invalid message
    7: UNAVAILABLE,  # transport failure
    STATUS_TIMEOUT: DEADLINE_EXCEEDED,  # a bounded read timed out
    STATUS_CANCELLED: CANCELLED,  # cancelled, deadline expired, dependency closed
    STATUS_BUSY: INTERNAL,  # still busy after the bounded retry
    14: INTERNAL,  # engine invariant
    15: RESOURCE_EXHAUSTED,  # payload above a cap
    16: INTERNAL,  # unsupported runtime capability
}


def log(message):
    capsulad.log("pa: " + str(message))


class WaveError(ValueError):
    """A rejected WAV or a failed playback: args are (status, detail).

    A ValueError subclass is used because the MicroPython embed port does not
    support custom attributes on exception objects, while the built-in args
    tuple is supported and lets one raise site carry its status.
    """


def status_of(err):
    """The ABI/gRPC status of an OSError raised by the script host."""
    args = getattr(err, "args", None)
    if args and isinstance(args[0], int):
        return args[0]
    return 0


def text_of(err):
    """The human-readable half of an OSError raised by the script host."""
    args = getattr(err, "args", None)
    if args and len(args) > 1:
        return str(args[1])
    return str(err)


def status_of_call(call):
    """The terminal gRPC status of a call, as (code, detail)."""
    try:
        info = call.status()
    except Exception:
        return (0, "")
    return (info.get("code", 0) or 0, info.get("detail", "") or "")


def call_failure(call, err):
    """The (code, detail) a caller should see for `err` on `call`.

    A stream that the framework ended carries the canonical code in the call's
    terminal status, so that always wins. A purely local ABI failure is then
    translated explicitly: the two status spaces are different and overlap
    numerically, so passing an ABI value through would report an unrelated gRPC
    condition (ABI CLOSED is not DEADLINE_EXCEEDED, ABI NOT_CONNECTED is not
    INVALID_ARGUMENT).
    """
    code, detail = status_of_call(call)
    if code > 0:
        return (code, detail or text_of(err))
    return (ABI_TO_GRPC.get(status_of(err), INTERNAL), text_of(err))


# --- protobuf --------------------------------------------------------------


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


def length_field(number, payload):
    return varint((number << 3) | 2) + varint(len(payload)) + payload


def varint_field(number, value):
    return varint(number << 3) + varint(value)


def read_varint(data, pos):
    value = 0
    shift = 0
    size = len(data)
    while pos < size:
        byte = data[pos]
        pos += 1
        value |= (byte & 0x7F) << shift
        if not byte & 0x80:
            return value, pos
        shift += 7
        if shift > 35:
            break
    raise WaveError(INVALID_ARGUMENT, "malformed protobuf varint")


def walk_fields(data, strict):
    """Decode one protobuf message into ``[(number, wire, value)]``.

    ``value`` is bytes for a length-delimited field and an integer for a
    varint. Fixed-width fields are skipped. With ``strict`` a malformed message
    raises; otherwise the fields decoded so far are returned, which is what
    reading a framework event wants.
    """
    out = []
    pos = 0
    size = len(data)
    try:
        while pos < size:
            if len(out) >= 64:
                raise WaveError(INVALID_ARGUMENT, "message has more than 64 fields")
            key, pos = read_varint(data, pos)
            number = key >> 3
            wire = key & 7
            if wire == 2:
                length, pos = read_varint(data, pos)
                if length > size - pos:
                    raise WaveError(INVALID_ARGUMENT, "truncated message")
                out.append((number, 2, bytes(data[pos:pos + length])))
                pos += length
            elif wire == 0:
                value, pos = read_varint(data, pos)
                out.append((number, 0, value))
            elif wire == 1:
                if pos + 8 > size:
                    raise WaveError(INVALID_ARGUMENT, "truncated message")
                pos += 8
            elif wire == 5:
                if pos + 4 > size:
                    raise WaveError(INVALID_ARGUMENT, "truncated message")
                pos += 4
            else:
                raise WaveError(INVALID_ARGUMENT, "unsupported protobuf wire type %d" % wire)
    except WaveError:
        if strict:
            raise
    return out


def last_bytes(data, number, strict):
    """Last length-delimited payload of one field number, or None."""
    found = None
    for field, wire, value in walk_fields(data, strict):
        if field == number and wire == 2:
            found = value
    return found


def varint_value(data, number):
    value = 0
    for field, wire, item in walk_fields(data, False):
        if field == number and wire == 0:
            value = item
    return value


def chunk_request(pcm):
    # PlaybackRequest.chunk = 2, AudioChunk.pcm = 5.
    return length_field(2, length_field(5, pcm))


def open_request(sample_format, rate, channels):
    # PlaybackRequest.open = 1, OpenPlayback{endpoint_id = 1, format = 2},
    # AudioFormat{sample_format = 1, sample_rate_hz = 2, channels = 3}.
    audio_format = varint_field(1, sample_format)
    audio_format += varint_field(2, rate)
    audio_format += varint_field(3, channels)
    open_playback = length_field(1, SPEAKER_ENDPOINT.encode())
    open_playback += length_field(2, audio_format)
    return length_field(1, open_playback)


def play_result(played, frames, rate, channels, detail):
    out = varint_field(1, 1 if played else 0)
    out += varint_field(2, frames)
    out += varint_field(3, rate)
    out += varint_field(4, channels)
    out += length_field(5, detail.encode())
    return out


def play_event_credit(frames):
    # PlayEvent.credit = 1; PlayCredit.frames = 1.
    return length_field(1, varint_field(1, frames))


def play_event_result(result):
    # PlayEvent.result = 2.
    return length_field(2, result)


# --- small helpers ---------------------------------------------------------


def u16(data, offset):
    return data[offset] | (data[offset + 1] << 8)


def u32(data, offset):
    return (data[offset] | (data[offset + 1] << 8)
            | (data[offset + 2] << 16) | (data[offset + 3] << 24))


def matches(data, offset, literal):
    index = 0
    while index < len(literal):
        if data[offset + index] != literal[index]:
            return False
        index += 1
    return True


# --- WAV reader ------------------------------------------------------------


class WaveReader:
    """Incremental RIFF/WAVE reader for one PCM_S16LE or PCM_F32LE file.

    ``feed`` consumes the next slice of the file and returns the PCM that is
    ready for playback, always a whole number of frames. Only the current RIFF
    header and a partial frame are held, so a long file is never buffered.
    """

    def __init__(self, max_seconds):
        self.max_seconds = max_seconds
        self.buffer = bytearray()
        self.phase = "riff"
        self.chunk_left = 0
        self.chunks = 0
        self.fmt = bytearray()
        self.data_left = 0
        self.unbounded = False
        self.trailer = 0
        self.header_bytes = 0
        self.declared = 0
        self.sample_format = 0
        self.rate = 0
        self.channels = 0
        self.frame_bytes = 0
        self.pcm_bytes = 0
        self.limit_bytes = 0
        self.complete = False

    def frames(self):
        if self.frame_bytes == 0:
            return 0
        return self.pcm_bytes // self.frame_bytes

    def has_audio(self):
        return self.pcm_bytes > 0

    def feed(self, data):
        if self.phase == "done":
            self._trailer(len(data))
            return b""
        self.buffer.extend(data)
        out = bytearray()
        while True:
            if self.phase == "riff":
                if len(self.buffer) < 12:
                    break
                if not matches(self.buffer, 0, b"RIFF") or not matches(self.buffer, 8, b"WAVE"):
                    raise WaveError(INVALID_ARGUMENT, "not a RIFF/WAVE file")
                self.buffer = self.buffer[12:]
                self.phase = "chunk"
            elif self.phase == "chunk":
                if len(self.buffer) < 8:
                    break
                self.chunks += 1
                if self.chunks > MAX_CHUNKS:
                    raise WaveError(INVALID_ARGUMENT, "more than %d RIFF chunks" % MAX_CHUNKS)
                size = u32(self.buffer, 4)
                is_fmt = matches(self.buffer, 0, b"fmt ")
                is_data = matches(self.buffer, 0, b"data")
                self.buffer = self.buffer[8:]
                if is_fmt:
                    if size < 16 or size > MAX_FMT_BYTES:
                        raise WaveError(INVALID_ARGUMENT, "fmt chunk is %d bytes" % size)
                    self._header(size)
                    self.chunk_left = size
                    self.phase = "fmt"
                elif is_data:
                    if self.frame_bytes == 0:
                        raise WaveError(INVALID_ARGUMENT, "the data chunk precedes the fmt chunk")
                    self.declared = size
                    if size == 0 or size > 0xFFFFFFF0:
                        # Streaming WAV: the audio runs to the end of the file.
                        self.unbounded = True
                        self.data_left = 0
                    else:
                        self.data_left = size
                        self._check_declared()
                    self.phase = "data"
                else:
                    # Unknown chunks (LIST, fact, ...) are skipped whole. Their
                    # declared sizes are charged against one header budget, so a
                    # hostile size cannot make the app stream gigabytes of junk.
                    self._header(size + (size & 1))
                    self.chunk_left = size + (size & 1)
                    self.phase = "skip"
            elif self.phase == "fmt":
                take = self.chunk_left
                if take > len(self.buffer):
                    take = len(self.buffer)
                if take:
                    self.fmt.extend(self.buffer[0:take])
                    self.buffer = self.buffer[take:]
                    self.chunk_left -= take
                if self.chunk_left:
                    break
                self._parse_fmt()
                if len(self.fmt) & 1:
                    self.chunk_left = 1
                    self.phase = "skip"
                else:
                    self.phase = "chunk"
            elif self.phase == "skip":
                take = self.chunk_left
                if take > len(self.buffer):
                    take = len(self.buffer)
                if take:
                    self.buffer = self.buffer[take:]
                    self.chunk_left -= take
                if self.chunk_left:
                    break
                self.phase = "chunk"
            else:  # data
                room = len(self.buffer)
                if not self.unbounded and room > self.data_left:
                    room = self.data_left
                take = room - (room % self.frame_bytes)
                if take:
                    out.extend(self.buffer[0:take])
                    self.buffer = self.buffer[take:]
                    self.pcm_bytes += take
                    if self.pcm_bytes > self.limit_bytes:
                        raise WaveError(RESOURCE_EXHAUSTED, "audio is longer than max_seconds=%d" % self.max_seconds)
                    if not self.unbounded:
                        self.data_left -= take
                        if self.data_left == 0:
                            self.complete = True
                            self.phase = "done"
                            self._trailer(len(self.buffer))
                            self.buffer = bytearray()
                break
        return bytes(out)

    def finish(self):
        """Settle the file at the end of the request stream.

        Returns True only when the whole declared audio arrived, or when an
        unknown-length data chunk ran cleanly to the end of the stream.
        """
        if self.unbounded and self.phase == "data":
            self.complete = self.pcm_bytes > 0
            self.phase = "done"
        return self.complete

    def _parse_fmt(self):
        fmt = self.fmt
        tag = u16(fmt, 0)
        channels = u16(fmt, 2)
        rate = u32(fmt, 4)
        bits = u16(fmt, 14)
        if tag == 1 and bits == 16:
            self.sample_format = SAMPLE_PCM16
        elif tag == 3 and bits == 32:
            self.sample_format = SAMPLE_FLOAT32
        else:
            raise WaveError(INVALID_ARGUMENT, "unsupported WAV encoding: format tag %d with %d bits (need 16-bit PCM or 32-bit IEEE float)" % (tag, bits))
        if channels < 1 or channels > MAX_CHANNELS:
            raise WaveError(INVALID_ARGUMENT, "%d channels; the speaker accepts 1 or 2" % channels)
        if rate < MIN_RATE or rate > MAX_RATE:
            raise WaveError(INVALID_ARGUMENT, "%d Hz; the speaker accepts %d..%d Hz" % (rate, MIN_RATE, MAX_RATE))
        self.channels = channels
        self.rate = rate
        self.frame_bytes = channels * (bits // 8)
        self.limit_bytes = self.max_seconds * rate * self.frame_bytes

    def _header(self, size):
        self.header_bytes += size
        if self.header_bytes > MAX_HEADER_BYTES:
            raise WaveError(INVALID_ARGUMENT, "more than %d bytes of RIFF chunks before the audio" % MAX_HEADER_BYTES)

    def _check_declared(self):
        if self.declared % self.frame_bytes:
            raise WaveError(INVALID_ARGUMENT, "the data chunk is %d bytes, not a whole number of %d-byte frames" % (self.declared, self.frame_bytes))
        if self.declared > self.limit_bytes:
            raise WaveError(RESOURCE_EXHAUSTED, "the WAV holds %d bytes of audio; max_seconds=%d allows %d" % (self.declared, self.max_seconds, self.limit_bytes))

    def _trailer(self, size):
        self.trailer += size
        if self.trailer > MAX_TRAILER_BYTES:
            raise WaveError(INVALID_ARGUMENT, "more than %d bytes follow the audio chunk" % MAX_TRAILER_BYTES)


# --- playback --------------------------------------------------------------


class Speaker:
    """One AudioService.Playback stream and its event bookkeeping."""

    def __init__(self, call):
        self.call = call
        self.eos = False
        self.pending = bytearray()
        self.started = None
        self.notice = ""

    def write(self, pcm):
        # PCM is sent as complete PLAY_CHUNK messages; the tail of a clip
        # smaller than one chunk is written by flush() when the request stream
        # ends. A rejected or truncated stream therefore plays nothing that was
        # still buffered, and already-sent chunks are discarded by release().
        self.pending.extend(pcm)
        while len(self.pending) >= PLAY_CHUNK:
            self._chunk(bytes(self.pending[0:PLAY_CHUNK]))
            self.pending = self.pending[PLAY_CHUNK:]

    def flush(self):
        if self.pending:
            self._chunk(bytes(self.pending))
            self.pending = bytearray()

    def _chunk(self, pcm):
        message = chunk_request(pcm)
        attempts = 0
        while True:
            if self.eos:
                raise WaveError(INTERNAL, "the speaker stream ended before the WAV finished")
            try:
                self.call.send_bytes(message)
                return
            except OSError as err:
                if status_of(err) != STATUS_BUSY:
                    raise
                # The engine's outbound queue is full. That is backpressure,
                # not a failure: wait out one poll interval and retry.
                attempts += 1
                if attempts > BUSY_RETRIES:
                    raise WaveError(INTERNAL, "speaker transport stayed full: %s" % text_of(err))
                self.pump(POLL_MS)

    def pump(self, timeout_ms):
        """Read at most one playback event. False on a bounded timeout or end."""
        try:
            event = self.call.recv_bytes(timeout_ms)
        except OSError as err:
            if status_of(err) == STATUS_TIMEOUT:
                return False
            raise
        if event is None:
            self.eos = True
            return False
        for field, wire, value in walk_fields(event, False):
            if wire != 2:
                continue
            if field == 1:
                self.started = _started_format(value)
            elif field == 3:
                self.notice = _notice_text(value)
        return True

    def drain(self):
        while self.pump(0):
            pass

    def finish(self, polls, inbound):
        """Half-close and wait for the framework to drain and end the stream.

        The whole clip is queued by now, so the drain lasts about as long as
        the audio. The inbound call is polled between waits: a caller that
        cancels during the drain must discard that audio instead of letting it
        play out.
        """
        self.call.half_close()
        while not self.eos and polls > 0:
            polls -= 1
            poll_inbound(inbound)
            self.pump(POLL_MS)
        if not self.eos:
            raise WaveError(DEADLINE_EXCEEDED, "the speaker did not finish draining%s" % self.notice)

    def release(self):
        """Abort the stream and drop the handle; safe on every path."""
        try:
            self.call.cancel()
        except Exception as err:
            log("playback release failed: %s" % err)


def _started_format(started):
    for field, wire, value in walk_fields(started, False):
        if field == 2 and wire == 2:
            return (varint_value(value, 2), varint_value(value, 3))
    return None


def _notice_text(notice):
    code = ""
    detail = ""
    for field, wire, value in walk_fields(notice, False):
        if wire != 2:
            continue
        if field == 1:
            code = value.decode()
        elif field == 2:
            detail = value.decode()
    if detail:
        return " (%s: %s)" % (code, detail)
    if code:
        return " (%s)" % code
    return ""


def open_speaker(reader, max_seconds):
    # No call deadline: a caller may legitimately take up to READ_TIMEOUT_MS per
    # request frame, so only its own routed deadline and the app's own bounds
    # (per-frame timeout, bounded drain, caller cancellation) may end this
    # stream. A fixed deadline would cut off a slow but conforming upload.
    call = capsulad.open(AUDIO_SERVICE, "Playback", 3, 0)
    try:
        call.send_bytes(open_request(reader.sample_format, reader.rate, reader.channels))
    except OSError as err:
        code, detail = call_failure(call, err)
        try:
            call.cancel()
        except Exception:
            pass
        raise WaveError(code, "the speaker refused the stream: %s" % detail)
    return Speaker(call)


def drain_polls(max_seconds):
    return (max_seconds + 5) * 1000 // POLL_MS


def poll_inbound(call):
    """Surface a cancel that arrived while the app was waiting for a drain.

    The request stream is already half-closed here, so an empty poll raises a
    timeout (nothing pending) or the call's terminal status (cancelled).
    """
    try:
        call.recv_bytes(0)
    except OSError as err:
        if status_of(err) == STATUS_TIMEOUT:
            return
        raise


# --- call handling ---------------------------------------------------------


def end_call(call, code, detail):
    try:
        call.end(code, detail)
    except Exception as err:
        log("end failed: %s" % err)


def send_credit(call, frames):
    """Grant request-frame credit to the caller.

    A caller that will not read the credit cannot safely be given more audio:
    the native SDK's inbound queue would fill and end the call with
    RESOURCE_EXHAUSTED. Ending it explicitly with UNAVAILABLE is the honest
    terminal state.
    """
    try:
        call.send_bytes(play_event_credit(frames))
    except OSError as err:
        if status_of(err) == STATUS_BUSY:
            raise WaveError(UNAVAILABLE, "caller is not reading playback credits")
        raise


def serve_play(call, call_id, max_seconds):
    """Serve one Play call. Always ends the call; never leaks the stream."""
    reader = WaveReader(max_seconds)
    speaker = None
    # Request messages received; distinct from the PCM frame count below.
    requests = 0
    try:
        # The caller starts with no credit. Grant the bounded initial window
        # before reading; each processed frame grants one more.
        send_credit(call, INITIAL_CREDITS)
        while True:
            frame = call.recv_bytes(READ_TIMEOUT_MS)
            if frame is None:
                break
            requests += 1
            if len(frame) > MAX_FRAME_BYTES:
                raise WaveError(RESOURCE_EXHAUSTED, "request frame %d is %d bytes; the limit is %d" % (requests, len(frame), MAX_FRAME_BYTES))
            data = last_bytes(frame, 1, True)
            # Every request frame must carry WAV bytes. A data-less frame would
            # otherwise only reset the read timeout without making progress,
            # letting one call hold the app - and any queued second call - for
            # the caller's whole routed deadline.
            if not data:
                raise WaveError(INVALID_ARGUMENT, "request frame %d carries no WAV bytes" % requests)
            pcm = reader.feed(data)
            if pcm:
                if speaker is None:
                    speaker = open_speaker(reader, max_seconds)
                speaker.write(pcm)
            elif speaker is not None:
                speaker.drain()
            # The frame is fully consumed here: its bytes are parsed, and any
            # PCM is handed to the playback stream. Only now may the caller
            # replace it.
            send_credit(call, 1)
        if not reader.has_audio():
            raise WaveError(INVALID_ARGUMENT, "no WAV audio data in the request stream")
        if not reader.finish():
            raise WaveError(INVALID_ARGUMENT, "the WAV is truncated: %d of %d audio bytes arrived" % (reader.pcm_bytes, reader.declared))
        speaker.flush()
        speaker.finish(drain_polls(max_seconds), call)
        frames = reader.frames()
        detail = "played %d frames at %d Hz/%d ch" % (frames, reader.rate, reader.channels)
        # PlaybackStarted.actual_format is the device mixer format, not the WAV's
        # own format; report both so neither is mistaken for the other.
        mixer = ""
        if speaker.started:
            mixer = " mixer=%d Hz/%d ch" % speaker.started
        log("played call=%s %d frames wav=%d Hz/%d ch%s%s" % (call_id, frames, reader.rate, reader.channels, mixer, speaker.notice))
        try:
            call.send_bytes(play_event_result(
                play_result(True, frames, reader.rate, reader.channels, detail)))
        except OSError as err:
            code, detail = call_failure(call, err)
            end_call(call, code, "reply failed: %s" % detail)
            return
        end_call(call, OK, detail)
    except WaveError as err:
        log("rejected call=%s status=%d %s" % (call_id, err.args[0], err.args[1]))
        end_call(call, err.args[0], err.args[1])
    except OSError as err:
        if status_of(err) == STATUS_TIMEOUT:
            log("stalled call=%s: no WAV frame for %d ms" % (call_id, READ_TIMEOUT_MS))
            end_call(call, DEADLINE_EXCEEDED, "no WAV frame for %d ms" % READ_TIMEOUT_MS)
            return
        code, detail = call_failure(speaker.call if speaker is not None else call, err)
        if code == CANCELLED:
            log("cancelled call=%s" % call_id)
            end_call(call, CANCELLED, "cancelled by the caller")
        else:
            log("failed call=%s status=%d %s" % (call_id, code, detail))
            end_call(call, code, detail)
    finally:
        if speaker is not None:
            speaker.release()


def on_call(call):
    info = call.info or {}
    if info.get("grpc_service") != SERVICE or info.get("rpc_name") != "Play":
        end_call(call, UNIMPLEMENTED, "only %s/Play is served" % SERVICE)
        return
    call_id = info.get("call_id", "?")
    try:
        serve_play(call, call_id, settings["max_seconds"])
    except Exception as err:
        log("call=%s failed unexpectedly: %s" % (call_id, err))
        end_call(call, INTERNAL, "internal error")


# --- settings --------------------------------------------------------------

settings = {"max_seconds": DEFAULT_MAX_SECONDS}


def parse_max_seconds(raw):
    try:
        seconds = int(raw)
    except Exception:
        return 0
    if seconds < 1 or seconds > MAX_SECONDS_LIMIT:
        return 0
    return seconds


def apply_values(values):
    """Apply one settings snapshot. Both shapes the host can deliver are valid.

    ``ApplicationConfigurationService.Get`` reports
    ``{key, valueJson, hasValue}``, where ``hasValue: false`` means an
    administrator cleared the setting. The app-private ``AppSettingsChanged``
    event carries ``{key, valueJson}`` only, with an unset key simply absent,
    so a value is applied whenever ``valueJson`` is present and ``hasValue``
    is not explicitly false.
    """
    for value in values or []:
        if value.get("key") != "max_seconds":
            continue
        if value.get("hasValue") is False:
            settings["max_seconds"] = DEFAULT_MAX_SECONDS
            return
        raw = value.get("valueJson")
        if raw is None:
            return
        seconds = parse_max_seconds(raw)
        if seconds:
            settings["max_seconds"] = seconds
        else:
            log("max_seconds=%s is not a value in 1..%d; keeping %d" % (raw, MAX_SECONDS_LIMIT, settings["max_seconds"]))
        return


def on_settings(event):
    revision = (event or {}).get("revision", "?")
    apply_values((event or {}).get("values"))
    log("settings revision=%s max_seconds=%d" % (revision, settings["max_seconds"]))


def read_settings():
    try:
        snapshot = capsulad.call(CONFIG_SERVICE, "Get", {"appId": capsulad.env["appId"]}, 5000)
    except Exception as err:
        log("settings read failed (%s); keeping max_seconds=%d" % (err, settings["max_seconds"]))
        return
    apply_values(snapshot.get("values"))


# --- lifecycle -------------------------------------------------------------


def on_disconnect(event):
    event = event or {}
    log("bus ended status=%s detail=%s" % (event.get("status", 0), event.get("detail", "")))


def startup():
    capsulad.on("call", on_call)
    capsulad.on("settings_changed", on_settings)
    capsulad.on("disconnect", on_disconnect)
    capsulad.start()
    log("ready app=%s run=%s service=%s max_seconds=%d" % (
        capsulad.env["appId"], capsulad.env["runId"], SERVICE, settings["max_seconds"]))
    read_settings()
    capsulad.run()


startup()
