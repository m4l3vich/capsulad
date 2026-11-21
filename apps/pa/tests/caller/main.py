"""Test-only caller for the PA speaker app.

Drives ``org.capsulad.pa`` over the app bus from a real MicroPython app and
logs one ``pa-caller:`` line per case plus a final
``pa-caller: DONE <phase> ok=N fail=M`` line that ``tests/e2e.sh`` asserts.
The WAVs are built in memory, so the test needs no filesystem, exactly like
the app under test.

Phases (selected by the ``phase`` launch argument, so the runner can change a
setting between them):

* ``main`` — accepted files (whole and split), every rejection class,
  interruption, a stalled upload and two callers in sequence.
* ``settings`` — the same contract with ``max_seconds`` reduced to 1 s by the
  administrator: the exact bound, one frame over it, and well over it.

It is a test fixture, not a product app: see ../e2e.sh.
"""

import capsulad

PA_APP = "org.capsulad.pa"
SERVICE = "org.capsulad.pa.v1.SpeakerService"
FRAME_LIMIT = 128 * 1024
# Longer than the app's 15 s per-frame wait, so a stalled upload ends in
# DEADLINE_EXCEEDED rather than in the caller's own timeout.
STALL_TIMEOUT = 25000

passed = 0
failed = 0


def log(message):
    capsulad.log("pa-caller: " + str(message))


def u16(value):
    return bytes([value & 0xFF, (value >> 8) & 0xFF])


def u32(value):
    return bytes([value & 0xFF, (value >> 8) & 0xFF, (value >> 16) & 0xFF, (value >> 24) & 0xFF])


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


def frame(data):
    # org.capsulad.pa.v1.PlayRequest{bytes data = 1}
    return b"\x0a" + varint(len(data)) + data


def wav(tag, channels, rate, bits, pcm, data_size=None):
    block = channels * bits // 8
    fmt = u16(tag) + u16(channels) + u32(rate) + u32(rate * block) + u16(block) + u16(bits)
    size = len(pcm) if data_size is None else data_size
    body = b"WAVE" + b"fmt " + u32(len(fmt)) + fmt + b"data" + u32(size) + pcm
    return b"RIFF" + u32(len(body)) + body


def square(frames, amplitude, channels):
    return u16(amplitude) * channels * frames


def decode_result(data):
    """Decode org.capsulad.pa.v1.PlayResult into a dict."""
    fields = {}
    pos = 0
    size = len(data)
    while pos < size:
        key = data[pos]
        pos += 1
        number = key >> 3
        wire = key & 7
        if wire == 0:
            value = 0
            shift = 0
            while pos < size:
                byte = data[pos]
                pos += 1
                value |= (byte & 0x7F) << shift
                if not byte & 0x80:
                    break
                shift += 7
            fields[number] = value
        elif wire == 2:
            length = 0
            shift = 0
            while pos < size:
                byte = data[pos]
                pos += 1
                length |= (byte & 0x7F) << shift
                if not byte & 0x80:
                    break
                shift += 7
            fields[number] = bytes(data[pos:pos + length])
            pos += length
        else:
            break
    return fields


def call_status(call, err):
    try:
        info = call.status()
    except Exception:
        info = None
    if info is None:
        return (0, str(err))
    return (info.get("code", 0) or 0, info.get("detail", "") or str(err))


def reply_result(call, reply):
    """(status, detail, frames) for one PlayResult (or a terminal status)."""
    if reply is None:
        code, detail = call_status(call, "no reply")
        return (code, detail, 0)
    fields = decode_result(reply)
    if not fields.get(1):
        return (13, "reply says played=false", 0)
    detail = ""
    if fields.get(5):
        detail = fields[5].decode()
    return (0, detail, fields.get(2, 0))


def event_credit(event):
    """PlayEvent.credit.frames, or 0 for a non-credit event."""
    fields = decode_result(event)
    credit = fields.get(1)
    if credit is None:
        return 0
    return decode_result(credit).get(1, 0) or 0


def event_result(event):
    """PlayEvent.result payload, or None."""
    return decode_result(event).get(2)


def await_result(call, timeout):
    """Drain credits and return (status, detail, frames) for the result."""
    while True:
        try:
            event = call.recv_bytes(timeout)
        except OSError as err:
            code, detail = call_status(call, err)
            return (code, detail, 0)
        if event is None:
            return reply_result(call, None)
        result = event_result(event)
        if result is not None:
            return reply_result(call, result)
        # A credit event; keep reading until the result or terminal status.


def wait_credit(call, timeout):
    """Read events until the app grants a frame credit. Returns a result tuple
    if the app ended first, else None."""
    while True:
        try:
            event = call.recv_bytes(timeout)
        except OSError as err:
            code, detail = call_status(call, err)
            return (code, detail, 0)
        if event is None:
            return reply_result(call, None)
        result = event_result(event)
        if result is not None:
            return reply_result(call, result)
        if event_credit(event) > 0:
            return None


def send_frames(call, frames, timeout):
    """Send PlayRequest frames within the app's credit window. Returns a result
    tuple only when the app ended before all frames were sent."""
    credit = 0
    index = 0
    while index < len(frames):
        while credit > 0 and index < len(frames):
            call.send_bytes(frames[index])
            index += 1
            credit -= 1
        if index >= len(frames):
            break
        ended = wait_credit(call, timeout)
        if ended is not None:
            return ended
        credit += 1
    return None


def play(data, split=0, cancel=False, timeout=90000):
    """Play one WAV. Returns (status, detail, frames)."""
    call = capsulad.open_routed(PA_APP, SERVICE, "Play", timeout)
    try:
        if split > 0:
            frames = []
            offset = 0
            while offset < len(data):
                frames.append(frame(data[offset:offset + split]))
                offset += split
        else:
            frames = [frame(data)]
        ended = send_frames(call, frames, timeout)
        if ended is not None:
            return ended
        if cancel:
            # Give the app time to start playing: the reply only arrives after
            # the drain, so this bounded read times out while the clip plays.
            try:
                call.recv_bytes(200)
            except OSError:
                pass
            call.cancel()
            return (0, "cancelled", 0)
        call.half_close()
        return await_result(call, timeout)
    finally:
        try:
            call.cancel()
        except Exception:
            pass


def play_raw(frames, timeout=30000):
    """Send raw PlayRequest frames; used for frames that carry no WAV bytes."""
    call = capsulad.open_routed(PA_APP, SERVICE, "Play", timeout)
    try:
        try:
            ended = send_frames(call, frames, timeout)
        except OSError as err:
            code, detail = call_status(call, err)
            return (code, detail, 0)
        if ended is not None:
            return ended
        call.half_close()
        return await_result(call, timeout)
    finally:
        try:
            call.cancel()
        except Exception:
            pass


def play_stalled(data, timeout=STALL_TIMEOUT):
    """Send a WAV header and then stall: the app's own frame timeout must end
    the call with DEADLINE_EXCEEDED instead of hanging or playing the tail."""
    call = capsulad.open_routed(PA_APP, SERVICE, "Play", timeout)
    try:
        try:
            ended = send_frames(call, [frame(data)], timeout)
        except OSError as err:
            code, detail = call_status(call, err)
            return (code, detail, 0)
        if ended is not None:
            return ended
        # No half-close: the request stream stays open with nothing pending.
        return await_result(call, timeout)
    finally:
        try:
            call.cancel()
        except Exception:
            pass


def play_queued(first, second, timeout=60000):
    """Two calls opened at once: the app serves them one after another and both
    must succeed, with the first reply arriving before the second."""
    calls = []
    try:
        for data in (first, second):
            call = capsulad.open_routed(PA_APP, SERVICE, "Play", timeout)
            calls.append(call)
            # The second call's initial credit waits until the app finishes the
            # first clip; opening it here still proves the queue.
            try:
                ended = send_frames(call, [frame(data)], timeout)
            except OSError as err:
                code, detail = call_status(call, err)
                calls[-1] = (call, (code, detail, 0))
                continue
            if ended is not None:
                calls[-1] = (call, ended)
                continue
            call.half_close()
        results = []
        for entry in calls:
            if isinstance(entry, tuple):
                results.append(entry[1])
                continue
            call = entry
            results.append(await_result(call, timeout))
        return results
    finally:
        for entry in calls:
            call = entry[0] if isinstance(entry, tuple) else entry
            try:
                call.cancel()
            except Exception:
                pass


def check(name, expect_ok, expect_status, result):
    global passed, failed
    status, detail, frames = result
    if expect_ok:
        ok = status == 0 and frames > 0
    else:
        ok = status == expect_status and frames == 0
    if ok:
        passed += 1
    else:
        failed += 1
    log("%s %s status=%d frames=%d detail=%s" % (
        "ok" if ok else "FAIL", name, status, frames, detail))


def run_main():
    mono = wav(1, 1, 16000, 16, square(3200, 0x2000, 1))
    stereo = wav(1, 2, 48000, 16, square(960, 0x1000, 2))
    invalid = b"this is not a wav file"
    eight_bit = wav(1, 1, 16000, 8, b"\x80" * 1600)
    high_rate = wav(1, 1, 96000, 16, square(960, 0x1000, 1))
    # Declares 40 s of audio: rejected from the header, before any audio.
    too_long = wav(1, 1, 16000, 16, b"", data_size=16000 * 2 * 40)
    # Declares 2000 bytes but delivers 1000: truncated.
    truncated = wav(1, 1, 16000, 16, square(500, 0x1000, 1), data_size=2000)
    big_frame = wav(1, 1, 16000, 16, b"\x00" * (FRAME_LIMIT + 1))
    # Declares 1 s but delivers 200 bytes: an upload that then stalls.
    stalled = wav(1, 1, 16000, 16, square(100, 0x1000, 1), data_size=16000 * 2)

    check("mono", True, 0, play(mono))
    check("mono-split", True, 0, play(mono, split=700))
    check("stereo", True, 0, play(stereo))
    check("not-a-wav", False, 3, play(invalid))
    check("eight-bit", False, 3, play(eight_bit))
    check("high-rate", False, 3, play(high_rate))
    check("too-long", False, 8, play(too_long))
    check("truncated", False, 3, play(truncated))
    check("oversized-frame", False, 8, play(big_frame))
    # A frame without WAV bytes must not be a no-op that resets the read timer.
    check("empty-frame", False, 3, play_raw([b"\x0a\x00"]))
    check("data-less-frame", False, 3, play_raw([b"\x08\x01"]))

    # A 3 s clip is still playing when the caller cancels: the app must
    # discard it, log the cancellation and be ready for the next call.
    cancelled = wav(1, 1, 16000, 16, b"\x00\x00" * 48000)
    play(cancelled, cancel=True)
    log("cancel sent")
    check("after-cancel", True, 0, play(mono))

    first, second = play_queued(
        wav(1, 1, 16000, 16, square(9600, 0x1000, 1)),
        wav(1, 1, 16000, 16, square(1600, 0x1000, 1)))
    check("queued-first", True, 0, first)
    check("queued-second", True, 0, second)

    check("stalled", False, 4, play_stalled(stalled))


def run_settings():
    # max_seconds is 1 here (set by the runner through the admin CLI).
    exact = wav(1, 1, 16000, 16, square(16000, 0x1000, 1))  # 1.0 s
    over = wav(1, 1, 16000, 16, square(16001, 0x1000, 1))  # one frame more
    far_over = wav(1, 1, 16000, 16, square(48000, 0x1000, 1))

    check("boundary-exact", True, 0, play(exact))
    check("boundary-plus-one-frame", False, 8, play(over))
    check("over-limit", False, 8, play(far_over))


PHASES = {"main": run_main, "settings": run_settings}


def on_invocation(event):
    event = event or {}
    phase = "main"
    for argument in event.get("arguments") or []:
        if argument.get("name") == "phase" and argument.get("value"):
            phase = argument["value"]
    log("invocation phase=%s" % phase)
    capsulad.notify_invocation(event.get("invocationId"), True, "pa-caller " + phase)
    try:
        runner = PHASES.get(phase)
        if runner is None:
            log("FAIL unknown phase %s" % phase)
            raise RuntimeError("unknown phase " + str(phase))
        runner()
    finally:
        log("DONE %s ok=%d fail=%d" % (phase, passed, failed))
        capsulad.stop()


def on_disconnect(event):
    event = event or {}
    log("bus ended status=%s detail=%s" % (event.get("status", 0), event.get("detail", "")))


def startup():
    capsulad.on("invocation", on_invocation)
    capsulad.on("disconnect", on_disconnect)
    capsulad.start()
    log("ready")
    capsulad.run()


startup()
