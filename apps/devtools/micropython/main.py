"""Development MicroPython probe over the public ``capsula`` host module.

The echo contract is descriptor-less: the handler speaks protobuf field 1 (a
string). No filesystem, private socket or daemon internals are used.
"""

import capsula

CONFIG = "capsula.framework.v1.ApplicationConfigurationService"
DEVICE = "capsula.framework.v1.DeviceService"
AUDIO = "capsula.framework.v1.AudioService"
PRESENTATION = "capsula.framework.v1.PresentationService"
RING = "light.ring.main"


def log(message):
    capsula.log("devtools-micropython: " + str(message))


def encode_text(text):
    data = text.encode()
    out = bytearray([0x0A])
    length = len(data)
    while length >= 0x80:
        out.append((length & 0x7F) | 0x80)
        length >>= 7
    out.append(length)
    out.extend(data)
    return bytes(out)


def decode_text(data):
    if not data or data[0] != 0x0A:
        return None
    pos = 1
    shift = 0
    length = 0
    while pos < len(data):
        byte = data[pos]
        pos += 1
        length |= (byte & 0x7F) << shift
        if not byte & 0x80:
            break
        shift += 7
    if pos + length > len(data):
        return None
    return data[pos:pos + length].decode()


def probe_state(label):
    try:
        info = capsula.call(DEVICE, "GetInfo", {}, 5000)
        audio = capsula.call(AUDIO, "GetAudioState", {}, 5000)
        presets = capsula.call(PRESENTATION, "ListPresets", {"resourceId": RING}, 5000)
        log("state %s app=%s run=%s sdk=%s model=%s daemon=%s platform=%s volume=%s muted=%s ring_presets=%d" % (
            label, capsula.env["appId"], capsula.env["runId"], capsula.env["sdk"],
            info.get("model", ""), info.get("daemonVersion", ""),
            info.get("targetPlatform") or "unspecified",
            audio.get("masterVolume", 0), audio.get("microphoneMuted", False),
            len(presets.get("presets") or [])))
    except Exception as err:
        log("state %s failed: %s" % (label, err))


def bump_runs(label):
    try:
        snap = capsula.call(CONFIG, "Get", {"appId": capsula.env["appId"]}, 5000)
        runs = 0
        for value in snap.get("values") or []:
            if value.get("key") == "runs" and value.get("hasValue"):
                runs = int(value.get("valueJson") or 0)
        nxt = runs + 1
        updated = capsula.call(CONFIG, "Update", {
            "appId": capsula.env["appId"],
            "expectedRevision": snap.get("revision", 0),
            "changes": [{"key": "runs", "valueJson": str(nxt)}],
        }, 5000)
        log("settings %s revision %s->%s runs=%d" % (
            label, snap.get("revision", 0), updated.get("revision", 0), nxt))
    except Exception as err:
        log("settings %s failed: %s" % (label, err))


def heartbeat(beats):
    """Read WatchResources until `beats` one-second quiet intervals elapse."""
    try:
        stream = capsula.stream(DEVICE, "WatchResources", {}, 30000)
    except Exception as err:
        log("heartbeat unavailable: %s" % err)
        return
    events = 0
    quiet = 0
    attempt = 0
    while attempt < beats * 8 and quiet < beats:
        attempt += 1
        try:
            event = stream.recv(1000)
            if event is None:
                log("heartbeat stream ended events=%d" % events)
                break
            events += 1
            resource = event.get("resource") or {}
            log("heartbeat event %d resource=%s removed=%s" % (
                events, resource.get("resourceId", "?"), event.get("removed", False)))
        except Exception as err:
            text = str(err).lower()
            if "timed out" not in text and "timeout" not in text and "deadline" not in text:
                log("heartbeat stopped: %s" % err)
                break
            quiet += 1
            log("heartbeat %d/%d quiet second resources_events=%d" % (quiet, beats, events))
    try:
        stream.cancel()
    except Exception:
        pass


def on_invocation(event):
    event = event or {}
    step = "probe"
    for argument in event.get("arguments") or []:
        if argument.get("name") == "step" and argument.get("value"):
            step = argument.get("value")
    log("invocation %s step=%s" % (event.get("invocationId", "?"), step))
    probe_state("invocation")
    bump_runs("invocation")
    capsula.notify_invocation(event.get("invocationId"), True, "micropython devtools handled " + step)


def on_settings(event):
    log("settings_changed revision=%s" % (event or {}).get("revision", 0))


def on_action(event):
    event = event or {}
    log("action %s" % event.get("actionId", "?"))
    probe_state("action")
    capsula.notify_action(event.get("actionInvocationId"), True, "probe logged framework state")


def on_call(call):
    info = call.info or {}
    if info.get("rpc_name") != "Echo":
        call.end(12, "only Echo is implemented")
        return
    try:
        request = call.recv_bytes(5000)
        if request is None:
            call.end(0, "empty request")
            return
        text = decode_text(request)
        if text is None:
            call.end(3, "expected one string field")
            return
        log("echo request text=%s" % text)
        call.send_bytes(encode_text("micropython:" + text))
        call.end(0, "ok")
    except Exception as err:
        call.end(13, str(err))


def startup():
    capsula.start()
    log("ready app=%s run=%s sdk=%s" % (
        capsula.env["appId"], capsula.env["runId"], capsula.env["sdk"]))
    probe_state("startup")
    bump_runs("startup")
    try:
        triggered = capsula.call(CONFIG, "TriggerAction", {
            "appId": capsula.env["appId"], "actionId": "probe",
        }, 5000)
        log("action triggered id=%s" % triggered.get("actionInvocationId", "?"))
    except Exception as err:
        log("action trigger failed: %s" % err)
    heartbeat(3)
    capsula.run()


capsula.on("invocation", on_invocation)
capsula.on("settings_changed", on_settings)
capsula.on("action", on_action)
capsula.on("call", on_call)
startup()