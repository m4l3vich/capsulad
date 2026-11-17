"""Unit tests for the PA speaker's settings, status mapping and call handler.

`apps/pa/main.py` is imported with a scripted stand-in for the injected
MicroPython host module (fake_capsulad), so `serve_play` can be driven against
the documented host contract: both settings shapes, partial input across
request frames, a drain that never finishes, a caller that cancels mid-drain
and a playback stream the framework refuses.

    python3 apps/pa/tests/test_app.py -v
"""

import os
import sys
import unittest

# Importing the app (and these helpers) must not leave a __pycache__ in the
# package directory.
sys.dont_write_bytecode = True

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import fake_capsulad
import fixtures


def play_info(call_id="call_test"):
    return {"call_id": call_id, "grpc_service": "org.capsulad.pa.v1.SpeakerService", "rpc_name": "Play"}


def inbound(byte_in, call_id="call_test"):
    return fake_capsulad.ScriptedCall(info=play_info(call_id), byte_in=byte_in)


def playback_chunks(call):
    """The PCM the app sent, decoded from its own Playback messages."""
    pcm = bytearray()
    for message in call.sent_bytes[1:]:  # message 0 is the open request
        chunk = fixtures.field(message, 2)  # PlaybackRequest.chunk
        pcm.extend(fixtures.field(chunk, 5))  # AudioChunk.pcm
    return bytes(pcm)


def opened_endpoint(call):
    open_playback = fixtures.field(call.sent_bytes[0], 1)  # PlaybackRequest.open
    return fixtures.field(open_playback, 1).decode()


def opened_format(call):
    audio_format = fixtures.field(fixtures.field(call.sent_bytes[0], 1), 2)
    return (fixtures.field(audio_format, 1), fixtures.field(audio_format, 2), fixtures.field(audio_format, 3))


class AppTestCase(unittest.TestCase):
    def setUp(self):
        self.host, self.app = fake_capsulad.load_app()

class SettingsTest(AppTestCase):
    def test_snapshot_shape_applies_a_value(self):
        self.app.apply_values([{"key": "max_seconds", "valueJson": "5", "hasValue": True}])
        self.assertEqual(self.app.settings["max_seconds"], 5)

    def test_snapshot_shape_cleared_falls_back_to_the_default(self):
        self.app.settings["max_seconds"] = 7
        self.app.apply_values([{"key": "max_seconds", "valueJson": "7", "hasValue": False}])
        self.assertEqual(self.app.settings["max_seconds"], self.app.DEFAULT_MAX_SECONDS)

    def test_event_shape_without_has_value_applies_the_value(self):
        # AppSettingsChanged carries {key, valueJson} only: an absent hasValue
        # means "stored value present", never "cleared".
        self.app.apply_values([{"key": "max_seconds", "valueJson": "1"}])
        self.assertEqual(self.app.settings["max_seconds"], 1)

    def test_settings_event_updates_the_effective_bound(self):
        self.host.emit("settings_changed", {"revision": "3", "values": [{"key": "max_seconds", "valueJson": "2"}]})
        self.assertEqual(self.app.settings["max_seconds"], 2)
        self.assertIn("max_seconds=2", self.host.texts())

    def test_absent_or_unset_key_keeps_the_current_value(self):
        self.app.settings["max_seconds"] = 9
        self.app.apply_values([])
        self.app.apply_values([{"key": "other", "valueJson": "1"}])
        self.app.apply_values([{"key": "max_seconds"}])
        self.assertEqual(self.app.settings["max_seconds"], 9)

    def test_invalid_values_keep_the_current_value_and_log(self):
        for raw in ('"abc"', "0", "-1", str(self.app.MAX_SECONDS_LIMIT + 1), "null"):
            with self.subTest(raw=raw):
                self.app.settings["max_seconds"] = 9
                self.app.apply_values([{"key": "max_seconds", "valueJson": raw}])
                self.assertEqual(self.app.settings["max_seconds"], 9)
        self.assertIn("not a value in 1..%d" % self.app.MAX_SECONDS_LIMIT, self.host.texts())

    def test_read_settings_uses_the_snapshot_shape(self):
        self.host.answers["Get"] = {"values": [{"key": "max_seconds", "valueJson": "4", "hasValue": True}]}
        self.app.read_settings()
        self.assertEqual(self.app.settings["max_seconds"], 4)
        service, method, request, timeout = self.host.calls[-1]
        self.assertEqual((service, method), (self.app.CONFIG_SERVICE, "Get"))
        self.assertEqual(request["appId"], "org.capsulad.pa")
        self.assertEqual(timeout, 5000)

    def test_read_settings_failure_keeps_the_current_value(self):
        self.host.answers["Get"] = OSError(7, "transport failure")
        self.app.settings["max_seconds"] = 3
        self.app.read_settings()
        self.assertEqual(self.app.settings["max_seconds"], 3)
        self.assertIn("settings read failed", self.host.texts())


class StatusMappingTest(AppTestCase):
    def test_abi_statuses_map_to_their_own_gRPC_codes(self):
        expected = {
            1: self.app.INVALID_ARGUMENT,
            3: self.app.UNAVAILABLE,
            4: self.app.UNAVAILABLE,
            7: self.app.UNAVAILABLE,
            10: self.app.DEADLINE_EXCEEDED,
            11: self.app.CANCELLED,
            13: self.app.INTERNAL,
            14: self.app.INTERNAL,
            15: self.app.RESOURCE_EXHAUSTED,
            16: self.app.INTERNAL,
            99: self.app.INTERNAL,  # unknown ABI status is never passed through
        }
        for status, code in expected.items():
            with self.subTest(status=status):
                call = fake_capsulad.ScriptedCall()
                self.assertEqual(self.app.call_failure(call, OSError(status, "detail"))[0], code)

    def test_call_terminal_status_wins_over_the_local_failure(self):
        call = fake_capsulad.ScriptedCall(terminal=(9, "no output endpoint"))
        code, detail = self.app.call_failure(call, OSError(4, "call already finished"))
        self.assertEqual(code, 9)
        self.assertEqual(detail, "no output endpoint")

    def test_local_close_is_not_reported_as_a_deadline(self):
        call = fake_capsulad.ScriptedCall()
        code, detail = self.app.call_failure(call, OSError(4, "call already finished"))
        self.assertEqual(code, self.app.UNAVAILABLE)
        self.assertNotEqual(code, self.app.DEADLINE_EXCEEDED)
        self.assertEqual(detail, "call already finished")

    def test_status_helpers_tolerate_a_host_without_args(self):
        self.assertEqual(self.app.status_of(OSError()), 0)
        self.assertEqual(self.app.status_of_call(fake_capsulad.ScriptedCall())[0], 0)


class ServePlayTest(AppTestCase):
    def test_plays_one_wav_and_reports_it(self):
        pcm = fixtures.square(400)
        data, _ = fixtures.wav(pcm=pcm)
        playback = fake_capsulad.ScriptedCall(byte_in=[None])
        self.host.outbound.append(playback)
        call = inbound([fixtures.play_request(data), None])

        self.app.on_call(call)

        # The playback stream is opened without a deadline (a caller may take up
        # to the frame timeout per message), and it is drained, not aborted.
        self.assertEqual(self.host.opened, [(self.app.AUDIO_SERVICE, "Playback", 3, 0)])
        self.assertEqual(opened_endpoint(playback), self.app.SPEAKER_ENDPOINT)
        self.assertEqual(opened_format(playback), (self.app.SAMPLE_PCM16, 16000, 1))
        self.assertEqual(playback_chunks(playback), pcm)
        self.assertTrue(playback.half_closed)
        self.assertTrue(playback.cancelled)  # released after the drain
        self.assertEqual(call.ended[0], 0)
        reply = call.sent_bytes[0]
        self.assertEqual(fixtures.field(reply, 1), 1)
        self.assertEqual(fixtures.field(reply, 2), 400)
        self.assertEqual(fixtures.field(reply, 3), 16000)
        self.assertEqual(fixtures.field(reply, 4), 1)
        self.assertIn(b"played 400 frames", fixtures.field(reply, 5))

    def test_plays_a_wav_split_across_request_frames(self):
        pcm = fixtures.square(300)
        data, _ = fixtures.wav(pcm=pcm)
        playback = fake_capsulad.ScriptedCall(byte_in=[None])
        self.host.outbound.append(playback)
        frames = [fixtures.play_request(data[0:3]), fixtures.play_request(data[3:40]),
                  fixtures.play_request(data[40:])]

        self.app.on_call(inbound(frames + [None]))

        self.assertEqual(playback_chunks(playback), pcm)
        self.assertEqual(self.host.opened[0][3], 0)

    def test_partial_header_then_end_of_stream_is_rejected(self):
        data, _ = fixtures.wav()
        call = inbound([fixtures.play_request(data[:20]), None])

        self.app.on_call(call)

        self.assertEqual(self.host.opened, [])
        self.assertEqual(call.ended[0], self.app.INVALID_ARGUMENT)
        self.assertIn("no WAV audio data", call.ended[1])

    def test_truncated_audio_is_rejected_after_it_played(self):
        pcm = fixtures.square(300)
        data, _ = fixtures.wav(pcm=pcm, data_size=len(pcm))
        playback = fake_capsulad.ScriptedCall(byte_in=[None])
        self.host.outbound.append(playback)
        call = inbound([fixtures.play_request(data[:44 + 100]), None])

        self.app.on_call(call)

        self.assertEqual(call.ended[0], self.app.INVALID_ARGUMENT)
        self.assertIn("truncated", call.ended[1])
        # The audio never played: a chunk smaller than PLAY_CHUNK is written
        # when the request stream ends, and a truncated stream is rejected
        # first, so the partial clip is discarded rather than half played.
        self.assertEqual(playback_chunks(playback), b"")
        self.assertFalse(playback.half_closed)
        self.assertTrue(playback.cancelled)

    def test_frames_without_wav_bytes_are_rejected(self):
        # A frame with no WAV bytes would only reset the read timer, so one call
        # could hold the app - and a queued second call - for the caller's whole
        # routed deadline without making progress.
        cases = {
            "empty message": b"",
            "empty data field": fixtures.play_request(b""),
            "no data field": b"\x08\x01",
        }
        for name, frame in cases.items():
            with self.subTest(name=name):
                host, app = fake_capsulad.load_app()
                call = inbound([frame, None])
                app.on_call(call)
                self.assertEqual(call.ended[0], app.INVALID_ARGUMENT)
                self.assertIn("carries no WAV bytes", call.ended[1])
                self.assertEqual(host.opened, [])

    def test_a_one_byte_frame_is_progress_not_an_empty_frame(self):
        data, _ = fixtures.wav(pcm=fixtures.square(2))
        playback = fake_capsulad.ScriptedCall(byte_in=[None])
        self.host.outbound.append(playback)
        call = inbound([fixtures.play_request(data[0:1]), fixtures.play_request(data[1:]), None])

        self.app.on_call(call)

        self.assertEqual(call.ended[0], 0)
        self.assertEqual(playback_chunks(playback), fixtures.square(2))

    def test_oversized_frame_is_rejected_before_playback(self):
        call = inbound([fixtures.play_request(b"\x00" * (self.app.MAX_FRAME_BYTES + 1)), None])

        self.app.on_call(call)

        self.assertEqual(self.host.opened, [])
        self.assertEqual(call.ended[0], self.app.RESOURCE_EXHAUSTED)

    def test_unsupported_rate_is_rejected_before_playback(self):
        data, _ = fixtures.wav(rate=96000)
        call = inbound([fixtures.play_request(data), None])

        self.app.on_call(call)

        self.assertEqual(self.host.opened, [])
        self.assertEqual(call.ended[0], self.app.INVALID_ARGUMENT)
        self.assertIn("96000 Hz", call.ended[1])

    def test_frame_timeout_ends_the_call_and_discards_the_audio(self):
        data, _ = fixtures.wav(pcm=fixtures.square(400))
        playback = fake_capsulad.ScriptedCall()  # nothing pending: always a timeout
        self.host.outbound.append(playback)
        call = inbound([fixtures.play_request(data)])  # then the stream stalls

        self.app.on_call(call)

        self.assertEqual(call.ended[0], self.app.DEADLINE_EXCEEDED)
        self.assertIn("no WAV frame", call.ended[1])
        self.assertFalse(playback.half_closed)
        self.assertTrue(playback.cancelled)
        self.assertIn("stalled call=call_test", self.host.texts())

    def test_cancel_while_reading_frames_is_reported(self):
        call = inbound([OSError(fake_capsulad.STATUS_CANCELLED, "caller cancelled the call")])

        self.app.on_call(call)

        self.assertEqual(self.host.opened, [])
        self.assertEqual(call.ended[0], self.app.CANCELLED)
        self.assertIn("cancelled call=call_test", self.host.texts())

    def test_cancel_during_the_drain_discards_the_queued_audio(self):
        data, _ = fixtures.wav(pcm=fixtures.square(400))
        playback = fake_capsulad.ScriptedCall()  # the drain never completes
        self.host.outbound.append(playback)
        call = inbound([fixtures.play_request(data), None,
                        OSError(fake_capsulad.STATUS_CANCELLED, "caller cancelled the call")])

        self.app.on_call(call)

        self.assertEqual(call.ended[0], self.app.CANCELLED)
        # The drain had already started, so the stream is half-closed and then
        # discarded rather than drained.
        self.assertTrue(playback.half_closed)
        self.assertTrue(playback.cancelled)
        self.assertEqual(call.sent_bytes, [])  # no reply: the audio was cut off
        self.assertIn("cancelled call=call_test", self.host.texts())

    def test_drain_that_never_ends_is_a_deadline(self):
        self.app.settings["max_seconds"] = 1
        data, _ = fixtures.wav(pcm=fixtures.square(16000))
        playback = fake_capsulad.ScriptedCall()
        self.host.outbound.append(playback)
        call = inbound([fixtures.play_request(data), None])

        self.app.on_call(call)

        self.assertEqual(call.ended[0], self.app.DEADLINE_EXCEEDED)
        self.assertIn("did not finish draining", call.ended[1])
        self.assertTrue(playback.cancelled)

    def test_refused_playback_stream_is_reported_from_its_terminal_status(self):
        data, _ = fixtures.wav(pcm=fixtures.square(400))
        playback = fake_capsulad.ScriptedCall(
            send_failures=[OSError(4, "call already finished")],
            terminal=(9, "no output endpoint"))  # FAILED_PRECONDITION
        self.host.outbound.append(playback)
        call = inbound([fixtures.play_request(data), None])

        self.app.on_call(call)

        self.assertEqual(call.ended[0], 9)  # FAILED_PRECONDITION from the daemon
        self.assertIn("no output endpoint", call.ended[1])
        self.assertTrue(playback.cancelled)

    def test_local_write_failure_is_not_reported_as_a_deadline(self):
        data, _ = fixtures.wav(pcm=fixtures.square(400))
        playback = fake_capsulad.ScriptedCall(send_failures=[OSError(4, "call already finished")])
        self.host.outbound.append(playback)
        call = inbound([fixtures.play_request(data), None])

        self.app.on_call(call)

        self.assertEqual(call.ended[0], self.app.UNAVAILABLE)
        self.assertNotEqual(call.ended[0], self.app.DEADLINE_EXCEEDED)

    def test_unknown_method_is_unimplemented(self):
        call = fake_capsulad.ScriptedCall(info={"call_id": "c", "grpc_service": self.app.SERVICE,
                                               "rpc_name": "Stop"})
        self.app.on_call(call)
        self.assertEqual(call.ended[0], self.app.UNIMPLEMENTED)

    def test_other_services_are_unimplemented(self):
        call = fake_capsulad.ScriptedCall(info={"call_id": "c", "grpc_service": "other.Service",
                                               "rpc_name": "Play"})
        self.app.on_call(call)
        self.assertEqual(call.ended[0], self.app.UNIMPLEMENTED)


if __name__ == "__main__":
    unittest.main()
