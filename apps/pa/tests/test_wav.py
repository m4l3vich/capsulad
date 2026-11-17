"""Unit tests for the PA speaker script's WAV parser and protobuf codec.

The app itself runs on the embedded MicroPython host; its parser and codec are
plain Python, so CPython exercises them directly. Files are built and decoded
by the independent fixtures module, not by the app's own helpers. The handler
and settings behaviour is covered by test_app.py.

    python3 apps/pa/tests/test_wav.py -v
"""

import os
import struct
import sys
import unittest

# Importing the app (and these helpers) must not leave a __pycache__ in the
# package directory.
sys.dont_write_bytecode = True

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import fake_capsulad
import fixtures
from fixtures import chunk, wav


def load_app():
    """Import the app under the script-host stand-in; return the app module."""
    _host, module = fake_capsulad.load_app()
    return module


def feed_all(reader, data, sizes):
    """Feed `data` in the given slice sizes; return the concatenated PCM."""
    out = bytearray()
    offset = 0
    index = 0
    while offset < len(data):
        size = sizes[index % len(sizes)]
        index += 1
        out.extend(reader.feed(data[offset:offset + size]))
        offset += size
    return bytes(out)


class WaveReaderTest(unittest.TestCase):
    def setUp(self):
        self.app = load_app()
        self.WaveError = self.app.WaveError

    def reader(self, max_seconds=30):
        return self.app.WaveReader(max_seconds)

    # --- accepted files ---------------------------------------------------

    def test_mono16_every_split_matches_the_data_chunk(self):
        pcm = bytes(range(256)) * 8  # 2048 bytes, 1024 frames
        data, block = wav(pcm=pcm)
        self.assertEqual(block, 2)
        for sizes in ([len(data)], [1], [3], [7, 5, 11], [13]):
            with self.subTest(sizes=sizes):
                reader = self.reader()
                self.assertEqual(feed_all(reader, data, sizes), pcm)
                self.assertTrue(reader.has_audio())
                self.assertTrue(reader.complete)
                self.assertTrue(reader.finish())
                self.assertEqual(reader.rate, 16000)
                self.assertEqual(reader.channels, 1)
                self.assertEqual(reader.frame_bytes, 2)
                self.assertEqual(reader.frames(), 1024)
                self.assertEqual(reader.sample_format, self.app.SAMPLE_PCM16)

    def test_stereo16_and_float32(self):
        cases = [
            dict(tag=1, channels=2, rate=44100, bits=16, block=4,
                 sample_format=self.app.SAMPLE_PCM16),
            dict(tag=3, channels=1, rate=48000, bits=32, block=4,
                 sample_format=self.app.SAMPLE_FLOAT32),
            dict(tag=3, channels=2, rate=8000, bits=32, block=8,
                 sample_format=self.app.SAMPLE_FLOAT32),
        ]
        for case in cases:
            with self.subTest(case=case):
                pcm = b"\x11" * (case["block"] * 50)
                data, block = wav(tag=case["tag"], channels=case["channels"],
                                  rate=case["rate"], bits=case["bits"], pcm=pcm)
                self.assertEqual(block, case["block"])
                reader = self.reader()
                self.assertEqual(feed_all(reader, data, [5, 64]), pcm)
                self.assertTrue(reader.complete)
                self.assertEqual(reader.sample_format, case["sample_format"])
                self.assertEqual(reader.frames(), 50)

    def test_unknown_chunks_and_odd_padding_are_skipped(self):
        before = chunk(b"LIST", b"odd")  # odd payload, padded to 4 bytes
        before += chunk(b"fact", b"\x00\x00\x00\x00")
        after = chunk(b"LIST", b"INFOxxxx")
        pcm = b"\x01\x02" * 8
        data, _ = wav(pcm=pcm, extra_before=before, extra_after=after)
        reader = self.reader()
        self.assertEqual(feed_all(reader, data, [9]), pcm)
        self.assertTrue(reader.complete)

    def test_unknown_length_data_chunk_runs_to_the_end(self):
        pcm = b"\x03\x04" * 6
        for declared in (0, 0xFFFFFFFF):
            with self.subTest(declared=declared):
                data, _ = wav(pcm=pcm, data_size=declared)
                reader = self.reader()
                self.assertEqual(feed_all(reader, data, [4]), pcm)
                self.assertFalse(reader.complete)  # settled by finish()
                self.assertTrue(reader.finish())
                self.assertEqual(reader.frames(), 6)

    def test_max_seconds_allows_a_file_at_the_bound(self):
        # 1 s of 16 kHz mono; the bound is inclusive.
        pcm = b"\x00\x00" * 16000
        data, _ = wav(pcm=pcm)
        reader = self.reader(max_seconds=1)
        self.assertEqual(len(feed_all(reader, data, [4096])), len(pcm))
        self.assertTrue(reader.finish())

    # --- rejected files ---------------------------------------------------

    def assertRejected(self, data, code=None, max_seconds=30, sizes=(7,)):
        reader = self.reader(max_seconds)
        with self.assertRaises(self.WaveError) as caught:
            feed_all(reader, data, list(sizes))
        if code is not None:
            self.assertEqual(caught.exception.args[0], code, caught.exception.args[1])
        return caught.exception

    def test_rejects_malformed_containers(self):
        for data in (b"not a wav at all", b"RIFF\x00\x00\x00\x00AVI "):
            with self.subTest(data=data):
                self.assertRejected(data, self.app.INVALID_ARGUMENT)

    def test_empty_stream_has_no_audio(self):
        reader = self.reader()
        self.assertEqual(reader.feed(b""), b"")
        self.assertFalse(reader.has_audio())
        self.assertFalse(reader.finish())

    def test_header_without_a_data_chunk_has_no_audio(self):
        data, _ = wav()
        data = data[:data.index(b"data")]
        reader = self.reader()
        self.assertEqual(feed_all(reader, data, [3]), b"")
        self.assertFalse(reader.has_audio())
        self.assertFalse(reader.finish())

    def test_zero_length_data_chunk_is_streaming(self):
        # Size 0 is the streaming-WAV convention (audio to end of file); with
        # no bytes it simply carries no audio.
        data, _ = wav(pcm=b"")
        reader = self.reader()
        self.assertEqual(feed_all(reader, data, [5]), b"")
        self.assertFalse(reader.finish())
        self.assertFalse(reader.has_audio())

    def test_rejects_data_before_fmt(self):
        pcm = b"\x00\x00" * 4
        body = b"WAVE" + b"data" + struct.pack("<I", len(pcm)) + pcm + \
            b"fmt " + struct.pack("<I", 0)
        data = b"RIFF" + struct.pack("<I", len(body)) + body
        self.assertRejected(data, self.app.INVALID_ARGUMENT)

    def test_rejects_unsupported_encodings(self):
        cases = {
            "8-bit pcm": dict(tag=1, bits=8, pcm=b"\x01" * 8),
            "24-bit pcm": dict(tag=1, bits=24, pcm=b"\x01" * 24),
            "32-bit int": dict(tag=1, bits=32, pcm=b"\x01" * 16),
            "extensible": dict(tag=0xFFFE, bits=16, pcm=b"\x01" * 8),
            "a-law": dict(tag=6, bits=8, pcm=b"\x01" * 8),
            "float16": dict(tag=3, bits=16, pcm=b"\x01" * 8),
        }
        for name, case in cases.items():
            with self.subTest(name=name):
                data, _ = wav(**case)
                self.assertRejected(data, self.app.INVALID_ARGUMENT)

    def test_rejects_unsupported_shape(self):
        cases = {
            "no channels": dict(channels=0, pcm=b"\x00\x00"),
            "three channels": dict(channels=3, bits=16, pcm=b"\x00" * 6),
            "low rate": dict(rate=4000, pcm=b"\x00\x00"),
            "high rate": dict(rate=96000, pcm=b"\x00\x00"),
            "zero rate": dict(rate=0, pcm=b"\x00\x00"),
        }
        for name, case in cases.items():
            with self.subTest(name=name):
                data, _ = wav(**case)
                self.assertRejected(data, self.app.INVALID_ARGUMENT)

    def test_rejects_misaligned_data(self):
        self.assertRejected(wav(channels=2, pcm=b"\x00\x00\x01")[0],
                            self.app.INVALID_ARGUMENT)

    def test_rejects_a_too_long_file_before_any_audio(self):
        pcm = b"\x00\x00" * 48000  # 3 s at 16 kHz
        data, _ = wav(pcm=pcm)
        reader = self.reader(max_seconds=1)
        with self.assertRaises(self.WaveError) as caught:
            # Only the header arrives: the declared size is enough to reject.
            self.assertEqual(reader.feed(data[:44]), b"")
            reader.feed(data[44:])
        self.assertEqual(caught.exception.args[0], self.app.RESOURCE_EXHAUSTED)
        self.assertEqual(reader.pcm_bytes, 0)

    def test_truncated_audio_is_not_complete(self):
        pcm = b"\x00\x00" * 100
        data, _ = wav(pcm=pcm, data_size=len(pcm))
        reader = self.reader()
        out = feed_all(reader, data[:44 + 64], [16])
        self.assertEqual(out, pcm[:64])
        self.assertFalse(reader.complete)
        self.assertFalse(reader.finish())
        self.assertEqual(reader.pcm_bytes, 64)

    def test_rejects_trailing_junk_over_the_bound(self):
        pcm = b"\x00\x00" * 4
        trailer = b"\x00" * (self.app.MAX_TRAILER_BYTES + 2)
        data, _ = wav(pcm=pcm, extra_after=trailer)
        self.assertRejected(data, self.app.INVALID_ARGUMENT, sizes=[1024])

    def test_rejects_a_header_over_the_byte_budget(self):
        # Unknown chunks are skipped whole and their declared sizes are charged
        # to one budget, so a hostile size is rejected from its header alone.
        before = b"junk" + struct.pack("<I", self.app.MAX_HEADER_BYTES + 1)
        data, _ = wav(pcm=b"\x00\x00", extra_before=before)
        error = self.assertRejected(data, self.app.INVALID_ARGUMENT, sizes=[64])
        self.assertIn("RIFF chunks", error.args[1])
        # A within-budget header still parses.
        data, _ = wav(pcm=b"\x00\x00", extra_before=b"junk" + struct.pack("<I", 16) + b"x" * 16)
        reader = self.reader()
        self.assertEqual(feed_all(reader, data, [8]), b"\x00\x00")
        self.assertTrue(reader.finish())

    def test_rejects_too_many_chunks(self):
        before = b"".join(chunk(b"JUNK", b"") for _ in range(self.app.MAX_CHUNKS))
        data, _ = wav(pcm=b"\x00\x00", extra_before=before)
        self.assertRejected(data, self.app.INVALID_ARGUMENT)

    # --- the Playback write budget ---------------------------------------

    def test_play_chunk_keeps_the_playback_queue_from_filling(self):
        # Half-close is unrecoverable if the engine's outbound queue is full:
        # a 256-entry queue must never be reached by one accepted clip.
        worst_bytes = (self.app.MAX_SECONDS_LIMIT * self.app.MAX_RATE
                       * self.app.MAX_CHANNELS * 4)
        writes = worst_bytes // self.app.PLAY_CHUNK + 1
        self.assertLess(writes, 256)
        self.assertGreaterEqual(self.app.PLAY_CHUNK, 96 * 1024)

    def test_limits_are_consistent(self):
        self.assertLessEqual(self.app.MIN_RATE, self.app.MAX_RATE)
        self.assertGreaterEqual(self.app.MAX_SECONDS_LIMIT, 1)
        self.assertLessEqual(self.app.DEFAULT_MAX_SECONDS, self.app.MAX_SECONDS_LIMIT)
        self.assertLessEqual(self.app.MAX_FRAME_BYTES, self.app.PLAY_CHUNK * 2)


class ProtobufTest(unittest.TestCase):
    def setUp(self):
        self.app = load_app()

    def varint(self, value, expected):
        self.assertEqual(self.app.varint(value), expected)

    def test_varint(self):
        self.varint(0, b"\x00")
        self.varint(1, b"\x01")
        self.varint(127, b"\x7f")
        self.varint(128, b"\x80\x01")
        self.varint(300, b"\xac\x02")
        self.varint(16384, b"\x80\x80\x01")

    def test_open_request_layout(self):
        message = self.app.open_request(self.app.SAMPLE_PCM16, 16000, 1)
        fields = self.app.walk_fields(message, True)
        self.assertEqual(len(fields), 1)
        self.assertEqual(fields[0][0], 1)
        self.assertEqual(fields[0][1], 2)
        open_playback = fields[0][2]
        inner = dict((number, value) for number, wire, value in
                     self.app.walk_fields(open_playback, True))
        self.assertEqual(inner[1], b"audio.output.speaker")
        audio_format = inner[2]
        values = dict((number, value) for number, wire, value in
                      self.app.walk_fields(audio_format, True))
        self.assertEqual(values[1], self.app.SAMPLE_PCM16)
        self.assertEqual(values[2], 16000)
        self.assertEqual(values[3], 1)

    def test_chunk_request_round_trip(self):
        pcm = b"\x5a" * 1000
        message = self.app.chunk_request(pcm)
        outer = self.app.walk_fields(message, True)
        self.assertEqual(len(outer), 1)
        self.assertEqual(outer[0][0], 2)
        inner = self.app.walk_fields(outer[0][2], True)
        self.assertEqual(inner[0][0], 5)
        self.assertEqual(inner[0][2], pcm)

    def test_play_result_fields(self):
        message = self.app.play_result(True, 4410, 44100, 2, "played")
        values = {}
        for number, wire, value in self.app.walk_fields(message, True):
            values[number] = value
        self.assertEqual(values[1], 1)
        self.assertEqual(values[2], 4410)
        self.assertEqual(values[3], 44100)
        self.assertEqual(values[4], 2)
        self.assertEqual(values[5], b"played")

    def test_last_bytes_field_handling(self):
        message = self.app.length_field(1, b"first") + \
            self.app.length_field(3, b"other") + \
            self.app.length_field(1, b"last")
        self.assertEqual(self.app.last_bytes(message, 1, True), b"last")
        self.assertEqual(self.app.last_bytes(message, 2, True), None)

    def test_request_frame_decoding(self):
        payload = b"\x00" * 32
        frame = self.app.length_field(1, payload)
        self.assertEqual(self.app.last_bytes(frame, 1, True), payload)
        self.assertEqual(self.app.last_bytes(b"", 1, True), None)
        # A length past the end of the message is a malformed request.
        with self.assertRaises(self.app.WaveError):
            self.app.last_bytes(b"\x0a\x05ab", 1, True)

    def test_strict_rejects_malformed_messages(self):
        bad = [b"\x0a", b"\x0a\x05abc", b"\x0f", b"\x0a\x80"]
        for message in bad:
            with self.subTest(message=message):
                with self.assertRaises(self.app.WaveError):
                    self.app.walk_fields(message, True)
                # Event decoding must never abort playback on a bad frame.
                self.assertIsInstance(self.app.walk_fields(message, False), list)

    def test_fields_have_a_bounded_count(self):
        # The walker stops at 64 fields, which bounds work on a hostile message.
        message = b"\x08\x01" * (self.app.MAX_CHUNKS + 1)
        with self.assertRaises(self.app.WaveError):
            self.app.walk_fields(message, True)

    def test_settings_values_are_validated(self):
        parse = self.app.parse_max_seconds
        self.assertEqual(parse(30), 30)
        self.assertEqual(parse("1"), 1)
        self.assertEqual(parse(self.app.MAX_SECONDS_LIMIT), self.app.MAX_SECONDS_LIMIT)
        for bad in (0, -1, self.app.MAX_SECONDS_LIMIT + 1, "abc", "", None):
            with self.subTest(bad=bad):
                self.assertEqual(parse(bad), 0)


if __name__ == "__main__":
    unittest.main()
