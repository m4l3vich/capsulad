"""Independent WAV and protobuf fixtures for the PA speaker tests.

These are deliberately *not* the app's own helpers: the tests must not agree
with the app by construction. `wav` writes files the way the RIFF container
describes, and `walk` decodes protobuf messages with its own reader, so a bug
in `apps/pa/main.py`'s codec cannot hide behind the fixture.
"""

import struct


def chunk(ident, payload):
    """One RIFF chunk, padded to a word boundary like the container requires."""
    out = ident + struct.pack("<I", len(payload)) + payload
    if len(payload) % 2:
        out += b"\x00"
    return out


def wav(tag=1, channels=1, rate=16000, bits=16, pcm=b"\x00\x00",
        data_size=None, extra_before=b"", extra_after=b""):
    """Build one RIFF/WAVE file; returns (file bytes, bytes per frame).

    `data_size` overrides the declared data-chunk length, which is how the
    tests build truncated and streaming files. `extra_before`/`extra_after`
    insert raw chunk bytes before and after the data chunk.
    """
    block = channels * bits // 8
    fmt = struct.pack("<HHIIHH", tag, channels, rate, rate * block, block, bits)
    size = len(pcm) if data_size is None else data_size
    body = (b"WAVE" + extra_before + b"fmt " + struct.pack("<I", len(fmt)) + fmt
            + b"data" + struct.pack("<I", size) + pcm + extra_after)
    return b"RIFF" + struct.pack("<I", len(body)) + body, block


def square(frames, amplitude=0x2000, channels=1):
    """Deterministic 16-bit interleaved PCM with a whole number of frames."""
    sample = struct.pack("<h", amplitude)
    return sample * channels * frames


def varint_bytes(value):
    out = bytearray()
    while True:
        byte = value & 0x7F
        value >>= 7
        if value:
            out.append(byte | 0x80)
        else:
            out.append(byte)
            return bytes(out)


def varint(data, pos):
    value = 0
    shift = 0
    while pos < len(data):
        byte = data[pos]
        pos += 1
        value |= (byte & 0x7F) << shift
        if not byte & 0x80:
            return value, pos
        shift += 7
    raise ValueError("truncated varint")


def walk(data):
    """[(field, wire, value)] with an independent protobuf reader."""
    out = []
    pos = 0
    while pos < len(data):
        key, pos = varint(data, pos)
        number = key >> 3
        wire = key & 7
        if wire == 2:
            length, pos = varint(data, pos)
            out.append((number, 2, bytes(data[pos:pos + length])))
            pos += length
        elif wire == 0:
            value, pos = varint(data, pos)
            out.append((number, 0, value))
        else:
            raise ValueError("unexpected wire type %d" % wire)
    return out


def field(data, number, default=None):
    """The last payload of one field: bytes for wire 2, an int for wire 0."""
    found = default
    for index, _wire, value in walk(data):
        if index == number:
            found = value
    return found


def play_request(payload):
    """org.capsulad.pa.v1.PlayRequest{bytes data = 1}."""
    return b"\x0a" + varint_bytes(len(payload)) + payload
