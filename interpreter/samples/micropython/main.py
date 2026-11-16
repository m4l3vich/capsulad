"""Runnable MicroPython Capsulad app.

The host embeds the real MicroPython runtime; `capsulad` is the injected module
over the public libcapsulad ABI.
"""

import capsulad


def on_invocation(event):
    capsulad.log("sample: invocation %s" % (event or {}).get("invocationId"))
    capsulad.notify_invocation((event or {}).get("invocationId"), True, "sample accepted")


def on_settings(event):
    capsulad.log("sample: settings revision %s" % (event or {}).get("revision"))


def on_action(event):
    event = event or {}
    capsulad.log("sample: action %s" % event.get("actionId"))
    capsulad.notify_action(event.get("actionInvocationId"), True, "greeted")


def on_call(call):
    info = call.info or {}
    if info.get("rpc_name") != "Echo":
        call.end(12, "the sample only implements Echo")
        return
    request = call.recv(5000) or {}
    call.send({"text": "micropython:" + str(request.get("text", ""))})
    call.end(0, "ok")


capsulad.on("invocation", on_invocation)
capsulad.on("settings_changed", on_settings)
capsulad.on("action", on_action)
capsulad.on("call", on_call)

capsulad.start()
capsulad.log("sample: READY app=" + capsulad.env["appId"])

device = capsulad.call("capsulad.framework.v1.DeviceService", "GetInfo", {})
capsulad.log("sample: framework API model=" + device.get("model", ""))

capsulad.run()
