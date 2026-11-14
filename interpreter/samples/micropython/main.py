"""Runnable MicroPython Capsula app.

The host embeds the real MicroPython runtime; `capsula` is the injected module
over the public libcapsula ABI.
"""

import capsula


def on_invocation(event):
    capsula.log("sample: invocation %s" % (event or {}).get("invocationId"))
    capsula.notify_invocation((event or {}).get("invocationId"), True, "sample accepted")


def on_settings(event):
    capsula.log("sample: settings revision %s" % (event or {}).get("revision"))


def on_action(event):
    event = event or {}
    capsula.log("sample: action %s" % event.get("actionId"))
    capsula.notify_action(event.get("actionInvocationId"), True, "greeted")


def on_call(call):
    info = call.info or {}
    if info.get("rpc_name") != "Echo":
        call.end(12, "the sample only implements Echo")
        return
    request = call.recv(5000) or {}
    call.send({"text": "micropython:" + str(request.get("text", ""))})
    call.end(0, "ok")


capsula.on("invocation", on_invocation)
capsula.on("settings_changed", on_settings)
capsula.on("action", on_action)
capsula.on("call", on_call)

capsula.start()
capsula.log("sample: READY app=" + capsula.env["appId"])

device = capsula.call("capsula.framework.v1.DeviceService", "GetInfo", {})
capsula.log("sample: framework API model=" + device.get("model", ""))

capsula.run()
