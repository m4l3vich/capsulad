"""Minimal protobuf wire helpers for the radio tests.

Deliberately independent of the code in ``main.py``: the tests build and parse
the raw wire format themselves so a bug in the app's own codec cannot hide
behind a symmetric helper.
"""


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


def message(field, payload):
    return varint((field << 3) | 2) + varint(len(payload)) + payload


def parse(data):
    """Return {field: value}: varints as int, others as bytes (last wins)."""
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
            raise ValueError("bad wire type %d" % wire)
        fields[field] = value
    return fields


def push_request(session, pcm=b"", rate=48000, channels=2, sample_format=1,
                 sequence=1, first_frame=0, discontinuity=False, dropped=0,
                 title="", artist="", finished=False, failed=False, detail=""):
    """Build an org.capsulad.radio.v1.PushRequest (see proto/radio.proto)."""
    return (uint(1, sequence) + text(2, session) + raw(3, pcm)
            + uint(4, rate) + uint(5, channels) + uint(6, sample_format)
            + uint(7, first_frame) + boolean(8, discontinuity) + uint(9, dropped)
            + text(10, title) + text(11, artist) + boolean(12, finished)
            + boolean(13, failed) + text(14, detail))


def tune_request(session, url, name, player):
    """Build an org.capsulad.radio.v1.TuneRequest."""
    return text(1, session) + text(2, url) + text(3, name) + text(4, player)


def tune_result(session, accepted=True, detail="", content_type="audio/mpeg"):
    """Build an org.capsulad.radio.v1.TuneResult."""
    return (boolean(1, accepted) + text(2, detail) + text(3, session)
            + text(4, content_type))


def push_result(accepted, detail=""):
    return boolean(1, accepted) + text(2, detail)


def playback_started(stream_id="pbs_test", rate=48000, channels=2, sample_format=1):
    """Build a capsulad.framework.v1.PlaybackEvent carrying PlaybackStarted."""
    started = (text(1, stream_id) + message(2, uint(1, sample_format)
               + uint(2, rate) + uint(3, channels)))
    return message(1, started)


def playback_progress(played, queued):
    """Build a capsulad.framework.v1.PlaybackEvent carrying PlaybackProgress."""
    return message(2, uint(1, played) + uint(2, queued))


def playback_notice(code, detail=""):
    """Build a capsulad.framework.v1.PlaybackEvent carrying AudioNotice."""
    return message(3, text(1, code) + text(2, detail))


def playback_audio_chunk(payload):
    """Build a capsulad.framework.v1.PlaybackRequest carrying one AudioChunk."""
    return message(2, payload)
