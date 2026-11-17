"""Unit tests for the MicroPython Internet Radio app.

They import the shipped ``main.py`` unchanged with a scripted stand-in for the
injected ``capsulad`` host module (fake_capsulad.py) and an independent
protobuf codec (pb.py), then drive the app through the same events the
framework delivers: settings snapshots, actions, launch invocations, inbound
media-contract calls and tuner pushes.

Run with: python3 -m unittest discover -s apps/radio/tests  (or tests/run.sh)
"""

import importlib
import json
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import fake_capsulad  # noqa: E402
import pb  # noqa: E402

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
TUNER_APP = "org.capsulad.radio.tuner"
TUNER_RUN = "run_tuner"

APP_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "micropython")
DIGITS = (0x3F, 0x06, 0x5B, 0x4F, 0x66, 0x6D, 0x7D, 0x07, 0x7F, 0x6F)
DASH = 0x40

TWO_STATIONS = "Radio One | https://example.test/one\nRadio Two | https://example.test/two"


def text_setting(value):
    """JSON document of a text setting value."""
    return json.dumps(value)


def chunk_bytes(session, pcm, rate=48000, channels=2, sample_format=1,
                finished=False, failed=False, detail="", title="", artist="",
                discontinuity=False):
    return pb.push_request(session, pcm=pcm, rate=rate, channels=channels,
                           sample_format=sample_format, finished=finished,
                           failed=failed, detail=detail, title=title,
                           artist=artist, discontinuity=discontinuity)


def pcm_of(frames, channels=2, sample_format=1, value=0):
    width = 2 if sample_format == 1 else 4
    return bytes([value]) * (frames * channels * width)


def accepted(result_call):
    """Whether a PushResult the app sent accepted the chunk."""
    return bool(pb.parse(result_call.sent_bytes[0]).get(1, 0))


def refusal(result_call):
    fields = pb.parse(result_call.sent_bytes[0])
    detail = fields.get(2, b"")
    return detail.decode() if isinstance(detail, bytes) else ""


class RadioTest(unittest.TestCase):
    maxDiff = None

    # --- fixture -----------------------------------------------------------

    def setUp(self):
        self.host = fake_capsulad.install()
        self.host.reset()
        self.host.env["runId"] = "run_radio"
        self.host.answer(DEVICE, "ListResources", {
            "resources": [{"resourceId": SEGMENT, "kind": "RESOURCE_SEGMENT_DISPLAY"}]})
        self.host.answer(AUDIO, "ListEndpoints", {
            "endpoints": [{"endpointId": SPEAKER, "direction": "AUDIO_PLAYBACK"}]})
        self.host.answer(ROUTING, "ReleaseCapture", {"released": True})
        self.host.answer(APP_IFACES, "ListInterfaces", {
            "interfaces": [{"appId": TUNER_APP, "runId": TUNER_RUN,
                            "grpcService": TUNER, "available": True}]})
        self.host.answer(CONFIG, "Get", lambda request: {
            "appId": request["appId"], "revision": "3",
            "values": [{"key": "station", "valueJson": "1", "hasValue": True}]})
        self.host.answer(CONFIG, "Update", lambda request: {
            "appId": request["appId"], "revision": "4",
            "values": [{"key": "station",
                        "valueJson": request["changes"][0]["valueJson"],
                        "hasValue": True}]})
        self.radio = self.load_app()

    def load_app(self):
        if APP_DIR not in sys.path:
            sys.path.insert(0, APP_DIR)
        if "main" in sys.modules:
            return importlib.reload(sys.modules["main"])
        return importlib.import_module("main")

    # --- helpers -----------------------------------------------------------

    def script_presentation(self):
        call = fake_capsulad.ScriptedCall(json_messages=[
            {"ready": {"presentationId": "prs_1", "resourceId": SEGMENT,
                       "hasValidFrame": False}}])
        self.host.answer_open(PRESENTATION, "Open", call)
        capture = fake_capsulad.ScriptedCall(json_messages=[
            {"granted": {"capture": {"captureId": "cap_1", "resourceId": SEGMENT,
                                     "presentationId": "prs_1",
                                     "selected": True, "ready": True}}}])
        self.host.answer_open(ROUTING, "Capture", capture)
        return call

    def script_playback(self, count=1):
        calls = [fake_capsulad.ScriptedCall(
            byte_messages=[pb.playback_started("pbs_%d" % (index + 1))])
            for index in range(count)]
        self.host.answer_open(AUDIO, "Playback", *calls)
        return calls

    def script_tuner(self, tune=None, stop=True):
        peer = tune or fake_capsulad.TunerPeer()
        self.host.answer_routed(TUNER_APP, TUNER, "Tune", peer)
        if stop:
            self.host.answer_routed(TUNER_APP, TUNER, "Stop",
                                    fake_capsulad.ScriptedCall(
                                        byte_messages=[b"\x08\x01"]))
        return peer

    def snapshot(self, stations=TWO_STATIONS, station=1, autoplay=False, tuner="",
                 revision="1"):
        values = [
            {"key": "stations", "valueJson": text_setting(stations), "hasValue": True},
            {"key": "station", "valueJson": str(station), "hasValue": True},
            {"key": "autoplay", "valueJson": "true" if autoplay else "false",
             "hasValue": True},
            {"key": "tuner", "valueJson": text_setting(tuner), "hasValue": True},
        ]
        self.host.emit("settings_changed", {"revision": revision, "values": values})

    def by_role(self, role):
        return [call for call in self.host.live if getattr(call, "role", None) == role]

    def presentation(self):
        return self.by_role((PRESENTATION, "Open"))[-1]

    def playback(self, index=-1):
        return self.by_role((AUDIO, "Playback"))[index]

    def tuner_calls(self, method):
        return [call for call in self.host.live
                if getattr(call, "role", None) is not None
                and call.role[1] == TUNER and call.role[2] == method]

    def frames(self):
        return [item["frame"]["segment"] for item in self.presentation().sent
                if "frame" in item]

    def last_frame(self):
        return self.frames()[-1]

    def push(self, data, caller=TUNER_RUN):
        return self.host.inbound(PLAYER, "Push", byte_messages=[data],
                                 caller_run_id=caller)

    def start_playing(self, pcm=None, **kwargs):
        self.script_presentation()
        self.script_playback()
        peer = self.script_tuner()
        self.snapshot(**kwargs)
        ok, detail = self.radio.control("play")
        self.assertTrue(ok, detail)
        pushed = pcm if pcm is not None else pcm_of(480)
        push = self.push(chunk_bytes(self.radio.session_id, pushed))
        return peer, push, pushed

    # --- startup -----------------------------------------------------------

    def test_startup_registers_handlers_and_probes_capabilities(self):
        self.assertTrue(self.host.started)
        self.assertTrue(self.host.ran)
        for kind in ("call", "invocation", "settings_changed", "action", "disconnect"):
            self.assertIn(kind, self.host.handlers)
        self.assertTrue(self.radio.display_available)
        self.assertTrue(self.radio.speaker_available)
        called = [(item[0], item[1]) for item in self.host.calls]
        self.assertIn((DEVICE, "ListResources"), called)
        self.assertIn((AUDIO, "ListEndpoints"), called)

    def test_station_parsing_rules(self):
        parse = self.radio.parse_stations
        self.assertEqual(parse(""), [])
        self.assertEqual(parse("# just a comment\n\n   \n"), [])
        self.assertEqual(parse("A | https://a.test/x\nB|https://b.test"), [
            {"name": "A", "url": "https://a.test/x"},
            {"name": "B", "url": "https://b.test"}])
        # a bare URL is accepted, and a URL keeps its own separator
        self.assertEqual(parse("https://a.test/plain"),
                         [{"name": "https://a.test/plain",
                           "url": "https://a.test/plain"}])
        self.assertEqual(parse("https://a.test/x?y=1|z"),
                         [{"name": "https://a.test/x?y=1|z",
                           "url": "https://a.test/x?y=1|z"}])
        # no separator, empty URL and non-http URLs are ignored
        self.assertEqual(parse("No separator\nA | \nftp://nope | ftp://x"), [])
        many = "\n".join(["S%d | https://s%d.test" % (n, n) for n in range(120)])
        self.assertEqual(len(parse(many)), 99)

    def test_settings_snapshot_is_applied_without_starting_playback(self):
        self.script_tuner()
        self.snapshot()
        self.assertEqual(len(self.radio.stations), 2)
        self.assertEqual(self.radio.station_number, 1)
        self.assertEqual(self.radio.phase, "stopped")
        self.assertFalse(self.host.open_calls(PRESENTATION, "Open"))
        self.assertFalse(self.host.routed_calls)
        status = self.radio.media_status()
        self.assertEqual(status["playbackState"], "PLAYBACK_STOPPED")
        self.assertEqual(status["sourceName"], "Radio One")

    # --- playback ----------------------------------------------------------

    def test_play_tunes_station_and_writes_pushed_audio(self):
        peer, push, pcm = self.start_playing()
        # one Tune request for the selected station, carrying the player app id
        self.assertEqual(len(self.host.routed_calls), 1)
        app_id, service, method, _ = self.host.routed_calls[0]
        self.assertEqual((app_id, service, method), (TUNER_APP, TUNER, "Tune"))
        request = pb.parse(peer.sent_bytes[0])
        self.assertEqual(request[2].decode(), "https://example.test/one")
        self.assertEqual(request[3].decode(), "Radio One")
        self.assertEqual(request[4].decode(), "org.capsulad.radio")
        self.assertEqual(request[1].decode(), self.radio.session_id)
        # the speaker stream was opened with the pushed format
        playback = self.playback()
        opened = pb.parse(playback.sent_bytes[0])
        open_message = pb.parse(opened[1])
        self.assertEqual(open_message[1].decode(), SPEAKER)
        self.assertEqual(pb.parse(open_message[2]), {1: 1, 2: 48000, 3: 2})
        # the chunk was relayed as an AudioChunk with the same PCM
        chunk = pb.parse(pb.parse(playback.sent_bytes[1])[2])
        self.assertEqual(chunk[5], pcm)
        self.assertEqual(chunk[4], 480)          # frames = 1920 bytes / 4
        # the tuner got one accepted reply, and the app is playing
        self.assertTrue(accepted(push))
        self.assertEqual(push.ended, (0, ""))
        self.assertEqual(self.radio.phase, "playing")
        status = self.radio.media_status()
        self.assertEqual(status["playbackState"], "PLAYBACK_PLAYING")
        self.assertEqual(status["sourceName"], "Radio One")
        self.assertIn("MEDIA_PAUSE", status["supportedActions"])
        # display: station digit, colon on while audio flows
        self.assertTrue(self.last_frame()["colon"])
        self.assertEqual(self.last_frame()["cells"][3]["mask"], DIGITS[1])

    def test_pushed_metadata_reaches_the_media_contract(self):
        self.start_playing()
        self.push(chunk_bytes(
            self.radio.session_id, pcm_of(480), title="Scarlet Fire",
            artist="Otis McDonald"))
        status = self.radio.media_status()
        self.assertEqual(status["title"], "Scarlet Fire")
        self.assertEqual(status["artist"], "Otis McDonald")
        self.assertEqual(status["mediaName"], "Scarlet Fire")
        # an empty metadata pair means "unchanged"
        self.push(chunk_bytes(self.radio.session_id, pcm_of(480)))
        self.assertEqual(self.radio.media_status()["title"], "Scarlet Fire")

    def test_stale_and_malformed_pushes_are_refused(self):
        self.start_playing()
        playback = self.playback()
        writes = len(playback.sent_bytes)
        stale = self.push(chunk_bytes("radio-999-old", pcm_of(480)))
        self.assertFalse(accepted(stale))
        self.assertEqual(refusal(stale), "stale session")
        broken = self.push(b"\xff\xff\xff")
        self.assertFalse(accepted(broken))
        self.assertTrue(refusal(broken).startswith("malformed"))
        oversized = self.push(chunk_bytes(
            self.radio.session_id, b"\x00" * ((1 << 20) + 1)))
        self.assertEqual(refusal(oversized), "over capacity")
        bad_format = self.push(chunk_bytes(
            self.radio.session_id, pcm_of(10), rate=0))
        self.assertEqual(refusal(bad_format), "unsupported format")
        self.assertEqual(len(playback.sent_bytes), writes)  # nothing was written
        self.assertEqual(self.radio.phase, "playing")

    def test_stale_terminal_push_cannot_tear_down_a_new_session(self):
        self.start_playing()
        playback = self.playback()
        stale = self.push(chunk_bytes(
            "radio-999-old", b"", finished=True, failed=True, detail="boom"))
        self.assertFalse(accepted(stale))
        self.assertEqual(refusal(stale), "stale session")
        self.assertFalse(playback.cancelled)
        self.assertEqual(self.radio.phase, "playing")
        self.assertNotEqual(self.radio.session_id, "")
        self.assertEqual(self.radio.detail, "station audio")
        # the same holds for a clean end from a superseded session
        stale_end = self.push(chunk_bytes("radio-999-old", b"", finished=True))
        self.assertEqual(refusal(stale_end), "stale session")
        self.assertFalse(playback.half_closed)
        self.assertEqual(self.radio.phase, "playing")

    def test_format_change_reopens_the_speaker_stream(self):
        self.script_presentation()
        self.script_playback(2)
        self.script_tuner()
        self.snapshot()
        self.radio.control("play")
        session = self.radio.session_id
        self.push(chunk_bytes(session, pcm_of(480)))
        first = self.playback(0)
        self.push(chunk_bytes(
            session, pcm_of(480, channels=1), rate=16000, channels=1))
        self.assertTrue(first.cancelled)
        second = self.playback(1)
        opened = pb.parse(second.sent_bytes[0])
        self.assertEqual(pb.parse(pb.parse(opened[1])[2]), {1: 1, 2: 16000, 3: 1})
        self.assertEqual(self.radio.phase, "playing")

    def test_finished_session_drains_without_blocking_then_stops(self):
        self.start_playing()
        playback = self.playback()
        end = self.push(chunk_bytes(
            self.radio.session_id, b"", finished=True, detail="stream ended"))
        self.assertTrue(accepted(end))
        self.assertTrue(playback.half_closed)       # drain, do not discard
        self.assertFalse(playback.cancelled)
        self.assertEqual(self.radio.session_id, "")
        # the tail plays out while the loop stays responsive (the drain is
        # serviced by the pump/watchdog, not by a blocking loop)
        self.assertEqual(self.radio.phase, "playing")
        playback.byte_in.append(None)
        self.host.advance(500)
        self.assertEqual(self.radio.phase, "stopped")
        self.assertEqual(self.radio.media_status()["playbackState"], "PLAYBACK_STOPPED")
        # the presentation was released, so the configured baseline returns
        self.assertEqual(self.presentation().sent[-1], {"close": {}})

    def test_pause_during_the_drain_discards_the_tail_and_stops(self):
        self.start_playing()
        playback = self.playback()
        self.push(chunk_bytes(self.radio.session_id, b"", finished=True))
        self.assertTrue(playback.half_closed)
        # A control frame after half-close is not legal, so the pause must end
        # the session instead of stranding it in `paused` with no stream.
        pause = self.host.inbound(MEDIA, "Pause", json_messages=[{"playerId": "radio"}])
        self.assertTrue(pause.sent[0]["accepted"])
        self.assertEqual(pause.sent[0]["detail"], "stream ended")
        self.assertEqual(self.radio.phase, "stopped")
        self.assertTrue(playback.cancelled)          # the tail is discarded
        self.assertFalse(self.radio.playback_draining)
        # nothing can flip it back: the watchdog leaves a stopped radio alone
        self.host.advance(120000)
        self.assertEqual(self.radio.phase, "stopped")

    def test_finished_without_audio_returns_to_stopped(self):
        self.script_presentation()
        self.script_playback()
        self.script_tuner()
        self.snapshot()
        ok, detail = self.radio.control("play")
        self.assertTrue(ok, detail)
        self.assertEqual(self.radio.phase, "buffering")
        # a conforming tuner may finish a session before pushing any chunk
        end = self.push(chunk_bytes(self.radio.session_id, b"", finished=True))
        self.assertTrue(accepted(end))
        self.assertEqual(self.radio.phase, "stopped")
        self.assertEqual(self.radio.session_id, "")
        self.host.advance(120000)                    # no stale buffering error
        self.assertEqual(self.radio.phase, "stopped")

    def test_no_audio_bound_covers_the_tuner_retry_window(self):
        self.assertGreaterEqual(self.radio.NO_AUDIO_TIMEOUT_MS, 60000)
        self.script_presentation()
        self.script_playback()
        self.script_tuner()
        self.snapshot()
        self.radio.control("play")
        self.host.advance(45000)                     # inside the retry window
        self.assertEqual(self.radio.phase, "buffering")
        self.host.advance(16000)
        self.assertEqual(self.radio.phase, "error")
        self.assertIn("tuner did not start", self.radio.detail)

    def test_foreign_callers_cost_one_lookup_each(self):
        self.start_playing()
        lookups = len([item for item in self.host.calls
                       if item[0] == APP_IFACES and item[1] == "ListInterfaces"])
        self.host.answer(APP_IFACES, "ListInterfaces", {"interfaces": [
            {"appId": "org.example.other", "runId": "run_foreign",
             "grpcService": TUNER, "available": True}]})
        first = self.push(chunk_bytes(self.radio.session_id, pcm_of(480)),
                          caller="run_foreign")
        second = self.push(chunk_bytes(self.radio.session_id, pcm_of(480)),
                           caller="run_foreign")
        self.assertEqual(refusal(first), "unauthorized caller")
        self.assertEqual(refusal(second), "unauthorized caller")
        after = len([item for item in self.host.calls
                     if item[0] == APP_IFACES and item[1] == "ListInterfaces"])
        self.assertEqual(after - lookups, 1)          # cached after the first
        # the tuned run still pushes without a lookup
        ok_push = self.push(chunk_bytes(self.radio.session_id, pcm_of(480)))
        self.assertTrue(accepted(ok_push))
        final = len([item for item in self.host.calls
                     if item[0] == APP_IFACES and item[1] == "ListInterfaces"])
        self.assertEqual(final, after)

    def test_drain_timeout_discards_the_tail(self):
        self.start_playing()
        playback = self.playback()
        self.push(chunk_bytes(self.radio.session_id, b"", finished=True))
        self.assertTrue(playback.half_closed)
        self.host.advance(31000)
        self.assertEqual(self.radio.phase, "stopped")
        self.assertTrue(playback.cancelled)

    def test_failed_session_reports_a_distinct_error_state(self):
        self.start_playing()
        playback = self.playback()
        end = self.push(chunk_bytes(
            self.radio.session_id, b"", finished=True, failed=True, detail="404"))
        self.assertFalse(accepted(end))
        self.assertTrue(playback.cancelled)          # abort discards queued audio
        self.assertEqual(self.radio.phase, "error")
        self.assertEqual(self.radio.detail, "404")
        self.assertEqual([cell["mask"] for cell in self.last_frame()["cells"]],
                         [DASH] * 4)

    def test_pause_refuses_pushes_and_resume_starts_a_new_session(self):
        self.script_presentation()
        self.script_playback(2)
        self.script_tuner()
        self.snapshot()
        self.radio.control("play")
        self.push(chunk_bytes(self.radio.session_id, pcm_of(480)))
        playback = self.playback(0)
        ok, _ = self.radio.control("pause")
        self.assertTrue(ok)
        self.assertEqual(pb.parse(playback.sent_bytes[-1]), {3: 2})  # PLAYBACK_PAUSE
        self.assertEqual(self.radio.phase, "paused")
        self.assertTrue(self.last_frame()["cells"][3]["decimalPoint"])
        refused = self.push(chunk_bytes(self.radio.session_id, pcm_of(480)))
        self.assertEqual(refusal(refused), "paused")
        first_session = self.radio.session_id
        self.script_tuner()
        ok, detail = self.radio.control("play")   # resume
        self.assertTrue(ok, detail)
        # the parked tail is discarded, not resumed and replayed
        self.assertTrue(playback.cancelled)
        self.assertNotEqual(self.radio.session_id, first_session)
        self.assertEqual(self.radio.phase, "buffering")
        self.push(chunk_bytes(self.radio.session_id, pcm_of(480)))
        self.assertEqual(len(self.by_role((AUDIO, "Playback"))), 2)  # fresh stream
        self.assertEqual(self.radio.phase, "playing")

    def test_stop_drops_the_session_and_releases_the_display(self):
        self.start_playing()
        playback = self.playback()
        session = self.radio.session_id
        ok, _ = self.radio.control("stop")
        self.assertTrue(ok)
        stops = self.tuner_calls("Stop")
        self.assertEqual(len(stops), 1)
        stop_request = pb.parse(stops[0].sent_bytes[0])
        self.assertEqual(stop_request[1].decode(), session)
        self.assertEqual(stop_request[2].decode(), "stop")
        self.assertTrue(playback.cancelled)          # abort, not drain
        self.assertFalse(playback.half_closed)
        self.assertEqual(self.presentation().sent[-1], {"close": {}})
        self.assertEqual(self.presentation().ended, (0, ""))
        self.assertEqual(self.radio.phase, "stopped")
        self.assertEqual(self.radio.session_id, "")

    def test_dismiss_maps_to_a_stopped_radio(self):
        self.start_playing()
        ok, detail = self.radio.control("dismiss")
        self.assertTrue(ok)
        self.assertEqual(detail, "dismissed")
        self.assertEqual(self.radio.phase, "stopped")
        self.assertEqual(self.presentation().sent[-1], {"close": {}})

    def test_actions_are_acknowledged(self):
        self.script_presentation()
        self.script_playback()
        self.script_tuner()
        self.snapshot()
        self.host.emit("action", {"actionInvocationId": "act_1", "actionId": "play"})
        self.assertIn(("action", "act_1", True, "ok"), self.host.notifications)
        self.host.emit("action", {"actionInvocationId": "act_2", "actionId": "pause"})
        self.assertEqual(self.radio.phase, "paused")
        self.host.emit("action", {"actionInvocationId": "act_3", "actionId": "pause"})
        refused = [item for item in self.host.notifications if item[1] == "act_3"][0]
        self.assertFalse(refused[2])
        self.assertIn("nothing is playing", refused[3])
        self.host.emit("action", {"actionInvocationId": "act_4", "actionId": "warp"})
        unknown = [item for item in self.host.notifications if item[1] == "act_4"][0]
        self.assertFalse(unknown[2])

    def test_next_previous_step_selection_and_persist_it(self):
        self.script_presentation()
        self.script_playback(3)
        self.host.answer_routed(TUNER_APP, TUNER, "Tune",
                                fake_capsulad.TunerPeer(),
                                fake_capsulad.TunerPeer(),
                                fake_capsulad.TunerPeer())
        self.host.answer_routed(TUNER_APP, TUNER, "Stop",
                                fake_capsulad.ScriptedCall(byte_messages=[b"\x08\x01"]),
                                fake_capsulad.ScriptedCall(byte_messages=[b"\x08\x01"]))
        self.snapshot(stations=TWO_STATIONS + "\nRadio Three | https://example.test/three")
        self.radio.control("play")
        updates = []
        self.host.answer(CONFIG, "Update", lambda request: updates.append(request) or {
            "appId": request["appId"], "revision": "4", "values": request["changes"]})
        ok, detail = self.radio.control("next")
        self.assertTrue(ok, detail)
        self.assertEqual(self.radio.station_number, 2)
        self.assertEqual(updates[0]["changes"], [{"key": "station", "valueJson": "2"}])
        self.assertEqual(updates[0]["expectedRevision"], "3")
        self.assertEqual(pb.parse(self.tuner_calls("Tune")[-1].sent_bytes[0])[2].decode(),
                         "https://example.test/two")
        # previous from the first station wraps to the last one
        self.radio.control("previous")   # 2 -> 1
        self.radio.control("previous")   # 1 -> 3
        self.assertEqual(self.radio.station_number, 3)
        self.assertEqual(updates[-1]["changes"], [{"key": "station", "valueJson": "3"}])

    # --- failures and capability gaps -------------------------------------

    def test_no_tuner_provider_is_a_distinct_error_state(self):
        self.host.answer(ROUTING, "ReleaseCapture", {"released": True})
        self.host.answer(APP_IFACES, "ListInterfaces", {"interfaces": []})
        self.script_presentation()
        self.snapshot()
        ok, detail = self.radio.control("play")
        self.assertFalse(ok)
        self.assertIn("tuner", detail)
        self.assertEqual(self.radio.phase, "error")
        self.assertFalse(self.host.routed_calls)
        self.assertEqual([cell["mask"] for cell in self.last_frame()["cells"]],
                         [DASH] * 4)
        status = self.radio.media_status()
        self.assertEqual(status["playbackState"], "PLAYBACK_STOPPED")
        self.assertEqual(status["sourceName"], "Radio One")

    def test_configured_tuner_setting_skips_discovery(self):
        self.host.answer(ROUTING, "ReleaseCapture", {"released": True})
        self.host.answer(APP_IFACES, "ListInterfaces", {"interfaces": []})
        self.script_tuner()
        self.script_presentation()
        self.script_playback()
        self.snapshot(tuner=TUNER_APP)
        ok, detail = self.radio.control("play")
        self.assertTrue(ok, detail)
        self.assertEqual(self.host.routed_calls[0][0], TUNER_APP)

    def test_tuner_refusal_and_tuner_failure_are_reported(self):
        self.script_presentation()
        self.snapshot()
        self.host.answer_routed(TUNER_APP, TUNER, "Tune",
                                fake_capsulad.TunerPeer(accepted=False,
                                                        detail="station is offline"))
        ok, detail = self.radio.control("play")
        self.assertFalse(ok)
        self.assertEqual(detail, "station is offline")
        self.assertEqual(self.radio.phase, "error")
        self.assertEqual(self.radio.detail, "station is offline")
        # a tuner that is gone surfaces as an unavailable provider
        self.host.answer_routed(TUNER_APP, TUNER, "Tune", RuntimeError("bus is gone"))
        ok, detail = self.radio.control("play")
        self.assertFalse(ok)
        self.assertTrue(detail.startswith("tuner unavailable"))
        self.assertEqual(self.radio.phase, "error")

    def test_stop_clears_an_error_state(self):
        self.host.answer(ROUTING, "ReleaseCapture", {"released": True})
        self.host.answer(APP_IFACES, "ListInterfaces", {"interfaces": []})
        self.script_presentation()
        self.snapshot()
        self.radio.control("play")
        self.assertEqual(self.radio.phase, "error")
        ok, _ = self.radio.control("stop")
        self.assertTrue(ok)
        self.assertEqual(self.radio.phase, "stopped")
        self.assertEqual(self.presentation().sent[-1], {"close": {}})

    def test_missing_display_or_speaker_is_handled(self):
        self.host.answer(DEVICE, "ListResources", {"resources": []})
        self.host.answer(AUDIO, "ListEndpoints", {"endpoints": []})
        self.radio = self.load_app()
        self.assertFalse(self.radio.display_available)
        self.assertFalse(self.radio.speaker_available)
        self.script_tuner()
        self.snapshot()
        ok, detail = self.radio.control("play")
        self.assertFalse(ok)
        self.assertIn("speaker", detail)
        self.assertEqual(self.radio.phase, "error")
        self.assertFalse(self.host.open_calls(PRESENTATION, "Open"))

    def test_displayless_device_still_plays(self):
        self.host.answer(DEVICE, "ListResources", {"resources": []})
        self.radio = self.load_app()
        self.assertFalse(self.radio.display_available)
        self.script_tuner()
        self.script_playback()
        self.snapshot()
        ok, detail = self.radio.control("play")
        self.assertTrue(ok, detail)
        self.push(chunk_bytes(self.radio.session_id, pcm_of(480)))
        self.assertEqual(self.radio.phase, "playing")
        self.assertFalse(self.host.open_calls(PRESENTATION, "Open"))

    def test_settings_without_stations_does_not_start_playback(self):
        self.script_tuner()
        self.snapshot(stations="")
        self.assertEqual(self.radio.stations, [])
        self.assertEqual(self.radio.phase, "stopped")
        self.assertFalse(self.host.routed_calls)

    # --- invocations -------------------------------------------------------

    def test_invocation_selects_by_name_and_plays(self):
        self.script_presentation()
        self.script_playback()
        peer = self.script_tuner()
        self.snapshot()
        self.host.emit("invocation", {"invocationId": "inv_1", "arguments": [
            {"name": "station", "value": "radio two"}]})
        self.assertEqual(self.radio.station_number, 2)
        self.assertEqual(pb.parse(peer.sent_bytes[0])[2].decode(),
                         "https://example.test/two")
        self.assertIn(("invocation", "inv_1", True, "ok"), self.host.notifications)

    def test_invocation_is_held_when_the_startup_settings_read_fails(self):
        self.host.answer_error(CONFIG, "Get", RuntimeError("framework not ready"))
        self.radio = self.load_app()
        self.assertFalse(self.radio.have_snapshot)
        self.script_presentation()
        self.script_playback()
        self.script_tuner()
        self.host.emit("invocation", {"invocationId": "inv_early", "arguments": [
            {"name": "station", "value": "2"}]})
        self.assertEqual(self.host.notifications, [])
        self.assertEqual(self.host.routed_calls, [])
        self.snapshot()
        self.assertEqual(self.radio.station_number, 2)
        self.assertEqual(len(self.host.routed_calls), 1)
        self.assertEqual(self.host.notifications[-1][0], "invocation")

    def test_startup_snapshot_is_applied_before_any_event(self):
        # Get is answered by setUp with the station list default.
        self.host.answer(CONFIG, "Get", lambda request: {
            "appId": request["appId"], "revision": "7",
            "values": [
                {"key": "stations", "valueJson": text_setting(TWO_STATIONS),
                 "hasValue": True},
                {"key": "station", "valueJson": "2", "hasValue": True},
                {"key": "autoplay", "valueJson": "false", "hasValue": True},
                {"key": "tuner", "valueJson": text_setting(TUNER_APP),
                 "hasValue": True}]})
        self.radio = self.load_app()
        self.assertTrue(self.radio.have_snapshot)
        self.assertEqual(len(self.radio.stations), 2)
        self.assertEqual(self.radio.station_number, 2)
        self.assertEqual(self.radio.tuner_setting, TUNER_APP)
        self.assertFalse(self.host.routed_calls)

    def test_private_settings_event_shape_without_has_value(self):
        # AppSettingsChanged carries {key, valueJson} with no hasValue field.
        self.host.emit("settings_changed", {"revision": "2", "values": [
            {"key": "stations", "valueJson": text_setting(TWO_STATIONS)},
            {"key": "station", "valueJson": "2"},
            {"key": "autoplay", "valueJson": "false"},
            {"key": "tuner", "valueJson": text_setting("")}]})
        self.assertEqual(len(self.radio.stations), 2)
        self.assertEqual(self.radio.station_number, 2)
        self.assertEqual(self.radio.phase, "stopped")

    def test_bare_invocation_only_ensures_the_app_is_running(self):
        self.script_tuner()
        self.snapshot()
        self.host.emit("invocation", {"invocationId": "inv_boot", "arguments": []})
        self.assertEqual(self.radio.phase, "stopped")
        self.assertEqual(self.host.routed_calls, [])
        self.assertIn(("invocation", "inv_boot", True, "radio ready"),
                      self.host.notifications)

    def test_unknown_invocation_station_is_refused(self):
        self.script_tuner()
        self.snapshot()
        self.host.emit("invocation", {"invocationId": "inv_x", "arguments": [
            {"name": "station", "value": "nope"}]})
        kind, _, ok, detail = self.host.notifications[-1]
        self.assertEqual(kind, "invocation")
        self.assertFalse(ok)
        self.assertIn("unknown station", detail)

    # --- settings driven state --------------------------------------------

    def test_autoplay_plays_once_on_the_first_snapshot(self):
        self.script_presentation()
        self.script_playback()
        self.script_tuner()
        self.snapshot(autoplay=True)
        self.assertEqual(len(self.host.routed_calls), 1)
        self.assertEqual(self.radio.phase, "buffering")
        # a later snapshot (for example our own station write) must not restart it
        self.snapshot(autoplay=True, revision="2")
        self.assertEqual(len(self.host.routed_calls), 1)

    def test_settings_station_change_switches_a_playing_station(self):
        self.start_playing()
        self.host.answer_routed(TUNER_APP, TUNER, "Tune", fake_capsulad.TunerPeer())
        self.host.answer_routed(TUNER_APP, TUNER, "Stop",
                                fake_capsulad.ScriptedCall(byte_messages=[b"\x08\x01"]))
        self.snapshot(station=2, revision="2")
        self.assertEqual(self.radio.station_number, 2)
        self.assertEqual(len(self.tuner_calls("Tune")), 2)
        self.assertEqual(pb.parse(self.tuner_calls("Tune")[-1].sent_bytes[0])[2].decode(),
                         "https://example.test/two")

    def test_push_requires_an_exact_non_empty_session(self):
        self.start_playing()
        playback = self.playback()
        writes = len(playback.sent_bytes)
        empty = self.push(chunk_bytes("", pcm_of(480)))
        self.assertFalse(accepted(empty))
        self.assertEqual(refusal(empty), "stale session")
        empty_failed = self.push(chunk_bytes("", b"", finished=True, failed=True,
                                             detail="boom"))
        self.assertEqual(refusal(empty_failed), "stale session")
        self.assertEqual(self.radio.phase, "playing")
        self.assertEqual(len(playback.sent_bytes), writes)

    def test_push_requires_the_authorized_tuner_run(self):
        self.start_playing()
        playback = self.playback()
        writes = len(playback.sent_bytes)
        # a run id the daemon cannot resolve to the tuned app
        unknown = self.push(chunk_bytes(self.radio.session_id, pcm_of(480)),
                            caller="run_other")
        self.assertFalse(accepted(unknown))
        self.assertEqual(refusal(unknown), "unauthorized caller")
        # the same, for an app that is not the one this session tuned
        self.host.answer(APP_IFACES, "ListInterfaces", {"interfaces": [
            {"appId": "org.example.other", "runId": "run_other",
             "grpcService": TUNER, "available": True}]})
        self.radio.push_caller_apps.clear()
        self.radio.push_caller_order[:] = []
        foreign = self.push(chunk_bytes(self.radio.session_id, pcm_of(480)),
                            caller="run_other")
        self.assertEqual(refusal(foreign), "unauthorized caller")
        self.assertEqual(self.radio.phase, "playing")
        self.assertEqual(len(playback.sent_bytes), writes)
        # the tuned tuner's own run is accepted
        self.host.answer(APP_IFACES, "ListInterfaces", {"interfaces": [
            {"appId": TUNER_APP, "runId": TUNER_RUN, "grpcService": TUNER,
             "available": True}]})
        self.radio.push_caller_apps.clear()
        self.radio.push_caller_order[:] = []
        ok_push = self.push(chunk_bytes(self.radio.session_id, pcm_of(480)))
        self.assertTrue(accepted(ok_push))
        self.assertGreater(len(playback.sent_bytes), writes)

    def test_watchdog_is_registered_at_startup(self):
        self.assertTrue(self.host.timers)
        token, interval, _, _ = self.host.timers[0]
        self.assertEqual(interval, 500)
        self.assertTrue(token)

    def test_repeating_callback_limit_is_enforced(self):
        # Mirrors the host contract documented in docs/app_development/sdk.md:
        # at most 16 repeating callbacks per run, minimum interval 1 ms.
        registered = len(self.host.timers)
        self.assertGreaterEqual(registered, 1)   # the app's own watchdog
        for _ in range(self.host.MAX_TIMERS - registered):
            self.host.every(1000, lambda: None)
        with self.assertRaises(ValueError):
            self.host.every(1000, lambda: None)
        with self.assertRaises(ValueError):
            self.host.every(0, lambda: None)

    def test_push_authorization_lookup_failure_ends_the_serving_session(self):
        self.start_playing()
        playback = self.playback()
        writes = len(playback.sent_bytes)
        # cold cache + a failing lookup, from the run that is serving us
        self.radio.push_caller_apps.clear()
        self.radio.push_caller_order[:] = []
        self.host.answer_error(APP_IFACES, "ListInterfaces",
                               RuntimeError("daemon busy"))
        failed = self.push(chunk_bytes(self.radio.session_id, pcm_of(480)),
                           caller=TUNER_RUN)
        self.assertFalse(accepted(failed))
        self.assertEqual(refusal(failed), "push authorization unavailable")
        self.assertEqual(self.radio.phase, "error")
        self.assertIn("push authorization unavailable", self.radio.detail)
        self.assertEqual(self.radio.session_id, "")
        self.assertEqual(len(playback.sent_bytes), writes)
        self.assertTrue(playback.cancelled)

    def test_a_foreign_caller_cannot_end_the_session_on_a_lookup_failure(self):
        self.start_playing()
        playback = self.playback()
        writes = len(playback.sent_bytes)
        self.radio.push_caller_apps.clear()
        self.radio.push_caller_order[:] = []
        self.host.answer_error(APP_IFACES, "ListInterfaces",
                               RuntimeError("daemon busy"))
        foreign = self.push(chunk_bytes(self.radio.session_id, pcm_of(480)),
                            caller="run_foreign")
        self.assertFalse(accepted(foreign))
        self.assertEqual(refusal(foreign), "unauthorized caller")
        self.assertEqual(self.radio.phase, "playing")
        self.assertEqual(self.radio.session_id != "", True)
        self.assertFalse(playback.cancelled)
        self.assertEqual(len(playback.sent_bytes), writes)

    def test_dead_tuner_is_detected_by_the_watchdog_without_any_activity(self):
        self.start_playing()
        self.assertEqual(self.radio.phase, "playing")
        # The tuner process dies: no Push, no user input, no framework event.
        # Only the run loop's repeating callback runs, and the liveness lookup
        # must notice the run is gone and leave PLAYING.
        self.host.answer(APP_IFACES, "ListInterfaces", {"interfaces": []})
        self.host.advance(2000)
        self.assertEqual(self.radio.phase, "error")
        self.assertIn("tuner stopped", self.radio.detail)
        self.assertEqual(self.radio.media_status()["playbackState"],
                         "PLAYBACK_STOPPED")
        self.assertEqual([cell["mask"] for cell in self.last_frame()["cells"]],
                         [DASH] * 4)

    def test_liveness_lookup_failure_does_not_kill_playback(self):
        self.start_playing()
        self.host.answer_error(APP_IFACES, "ListInterfaces",
                               RuntimeError("framework busy"))
        self.host.advance(8000)
        self.assertEqual(self.radio.phase, "playing")

    def test_watchdog_detects_a_dry_queue_on_a_live_tuner(self):
        self.start_playing()
        # The tuner is alive (ListInterfaces still lists its run) but stops
        # queuing. The bound is above the tuner's 43 s reconnect worst case.
        self.host.advance(50000)
        self.assertEqual(self.radio.phase, "playing")
        self.host.advance(12000)
        self.assertEqual(self.radio.phase, "error")
        self.assertIn("tuner stalled", self.radio.detail)

    def test_a_written_chunk_restarts_the_dry_queue_clock(self):
        self.start_playing()
        self.host.advance(50000)
        self.assertEqual(self.radio.phase, "playing")
        self.push(chunk_bytes(self.radio.session_id, pcm_of(480)))
        self.host.advance(50000)
        self.assertEqual(self.radio.phase, "playing")
        self.host.advance(12000)
        self.assertEqual(self.radio.phase, "error")
        self.assertIn("tuner stalled", self.radio.detail)

    def test_liveness_is_by_run_not_by_app(self):
        self.start_playing()
        self.assertEqual(self.radio.tuner_run, TUNER_RUN)
        # the tuner restarted: same app id and interface, a different run
        self.host.answer(APP_IFACES, "ListInterfaces", {"interfaces": [
            {"appId": TUNER_APP, "runId": "run_restarted", "grpcService": TUNER,
             "available": True}]})
        self.host.advance(2000)
        self.assertEqual(self.radio.phase, "error")
        self.assertIn("tuner stopped", self.radio.detail)

    def test_liveness_accepts_the_same_run(self):
        self.start_playing()
        self.host.advance(2000)
        self.assertEqual(self.radio.phase, "playing")
        self.host.advance(4000)
        self.assertEqual(self.radio.phase, "playing")

    def test_liveness_lookup_uses_a_short_budget(self):
        self.start_playing()
        self.host.advance(2000)
        lookups = [item for item in self.host.calls
                   if item[0] == APP_IFACES and item[1] == "ListInterfaces"]
        self.assertTrue(lookups)
        self.assertLessEqual(lookups[-1][3], 1000)

    def test_speaker_stream_end_becomes_error(self):
        self.start_playing()
        playback = self.playback()
        # the daemon ends the stream with a notice, not a Push
        playback.byte_in.append(pb.playback_notice("STREAM_CLOSED", "done"))
        self.host.inbound(MEDIA, "GetStatus", json_messages=[{"playerId": "radio"}])
        self.assertEqual(self.radio.phase, "error")
        self.assertIn("stream_closed", self.radio.detail)

    def test_display_recovers_after_a_failed_frame(self):
        def failing_presentation(kind, timeout):
            call = fake_capsulad.ScriptedCall(json_messages=[
                {"ready": {"presentationId": "prs_dead", "resourceId": SEGMENT,
                           "hasValidFrame": False}}])
            call.fail_frame = True
            return call
        self.host.answer_open(PRESENTATION, "Open", failing_presentation)
        self.script_playback()
        self.script_tuner()
        self.snapshot()
        ok, detail = self.radio.control("play")
        self.assertTrue(ok, detail)
        first = self.by_role((PRESENTATION, "Open"))[0]
        self.assertIsNotNone(first.ended)          # the dead stream was closed
        self.assertIsNone(self.radio.presentation)
        self.script_presentation()
        self.host.inbound(MEDIA, "Pause", json_messages=[{"playerId": "radio"}])
        self.assertEqual(len(self.by_role((PRESENTATION, "Open"))), 2)
        self.assertIsNotNone(self.radio.presentation)
        self.assertTrue(self.last_frame()["cells"][3]["decimalPoint"])

    # --- media contract ----------------------------------------------------

    def test_media_contract_methods(self):
        self.snapshot()
        listed = self.host.inbound(MEDIA, "ListPlayers", json_messages=[{}])
        player = listed.sent[0]["players"][0]
        self.assertEqual(player["playerId"], "radio")
        self.assertEqual(player["playbackState"], "PLAYBACK_STOPPED")
        self.assertEqual(player["appName"], "Internet radio")
        self.assertEqual(player["sourceName"], "Radio One")
        self.assertEqual(player["supportedActions"][0], "MEDIA_PLAY")
        self.assertEqual(listed.ended, (0, ""))
        status = self.host.inbound(MEDIA, "GetStatus",
                                   json_messages=[{"playerId": "radio"}])
        self.assertEqual(status.sent[0]["playerId"], "radio")
        foreign = self.host.inbound(MEDIA, "GetStatus",
                                    json_messages=[{"playerId": "other"}])
        self.assertEqual(foreign.ended[0], 5)
        missing = self.host.inbound(MEDIA, "GetStatus", json_messages=[{}])
        self.assertEqual(missing.ended[0], 3)
        unknown = self.host.inbound(MEDIA, "Nope", json_messages=[{}])
        self.assertEqual(unknown.ended[0], 12)
        # commands drive the same state machine as the actions
        self.script_presentation()
        self.script_playback()
        self.script_tuner()
        played = self.host.inbound(MEDIA, "Play", json_messages=[{"playerId": "radio"}])
        self.assertEqual(played.sent[0], {"accepted": True, "detail": ""})
        self.assertEqual(self.radio.phase, "buffering")
        paused = self.host.inbound(MEDIA, "Pause", json_messages=[{"playerId": "radio"}])
        self.assertTrue(paused.sent[0]["accepted"])
        self.assertEqual(self.radio.phase, "paused")

    def test_watch_players_snapshot_then_updates(self):
        self.snapshot()
        watch = self.host.inbound(MEDIA, "WatchPlayers", json_messages=[{}])
        self.assertEqual(watch.sent[0]["snapshot"]["players"][0]["playerId"], "radio")
        self.assertIsNone(watch.ended)
        self.script_presentation()
        self.script_playback()
        self.script_tuner()
        self.radio.control("play")
        self.push(chunk_bytes(self.radio.session_id, pcm_of(480)))
        updated = [item for item in watch.sent if "updated" in item]
        self.assertTrue(updated)
        self.assertEqual(updated[-1]["updated"]["playbackState"], "PLAYBACK_PLAYING")
        self.radio.shutdown("test")
        self.assertTrue(watch.cancelled)

    def test_long_lived_streams_have_no_call_deadline(self):
        # call_start's timeout is the whole RPC deadline: a non-zero value would
        # make the daemon cancel the stream while the radio still uses it.
        self.start_playing()
        wanted = {(PRESENTATION, "Open"): 3, (AUDIO, "Playback"): 3,
                  (ROUTING, "Capture"): 1}
        seen = set()
        for service, method, kind, timeout in self.host.opens:
            if (service, method) in wanted:
                seen.add((service, method))
                self.assertEqual(timeout, 0,
                                 "%s/%s used a whole-call deadline of %s" % (
                                     service, method, timeout))
                self.assertEqual(kind, wanted[(service, method)])
        self.assertEqual(seen, set(wanted))
        # one-shot calls stay bounded
        self.assertTrue(self.host.routed_calls)
        for _, _, _, timeout in self.host.routed_calls:
            self.assertGreater(timeout, 0)

    def test_event_handler_failure_does_not_end_the_run(self):
        def explode(event):
            raise RuntimeError("boom")
        self.host.handlers["settings_changed"] = self.radio.guard(
            "settings", explode)
        self.host.emit("settings_changed", {"revision": "9", "values": []})
        self.assertTrue(any("settings handler failed" in line
                            for line in self.host.logs))

    def test_display_capture_is_taken_and_released(self):
        self.start_playing()
        captures = self.by_role((ROUTING, "Capture"))
        self.assertEqual(len(captures), 1)
        request = captures[0].sent[0]
        self.assertEqual(request, {"resourceId": SEGMENT,
                                   "presentationId": "prs_1"})
        self.assertEqual(self.radio.capture_id, "cap_1")
        # the capture is not retaken while the presentation lives
        self.radio.control("pause")
        self.assertEqual(len(self.by_role((ROUTING, "Capture"))), 1)
        self.radio.control("stop")
        releases = [item for item in self.host.calls
                    if item[0] == ROUTING and item[1] == "ReleaseCapture"]
        self.assertEqual(len(releases), 1)
        self.assertEqual(releases[0][2], {"captureId": "cap_1"})
        self.assertTrue(captures[0].cancelled)
        self.assertEqual(self.radio.capture_id, "")

    # --- lifecycle ---------------------------------------------------------

    def test_disconnect_releases_owned_resources(self):
        self.start_playing()
        playback = self.playback()
        self.host.emit("disconnect", {"status": 0, "detail": ""})
        self.assertTrue(playback.cancelled)
        self.assertEqual(self.presentation().sent[-1], {"close": {}})
        self.assertEqual(self.radio.phase, "stopped")
        self.assertEqual(self.radio.session_id, "")
        self.assertEqual(self.radio.watchers, [])

    def test_state_changes_reuse_one_presentation(self):
        self.start_playing()
        opened = len(self.host.open_calls(PRESENTATION, "Open"))
        self.radio.control("pause")
        self.assertEqual(len(self.host.open_calls(PRESENTATION, "Open")), opened)
        self.assertGreaterEqual(len(self.frames()), 3)


if __name__ == "__main__":
    unittest.main()
