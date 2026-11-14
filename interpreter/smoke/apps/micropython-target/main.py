import capsula


def handle_call(call):
    info = call.info or {}
    service = info.get("grpc_service", "")
    rpc = info.get("rpc_name", "")
    capsula.log("SMOKE-CALL micropython %s/%s" % (service, rpc))
    try:
        request = call.recv(5000) or {}
        if rpc == "GetInfo":
            call.send({"model": "micropython-smoke", "deviceId": "smoke"})
            call.end(0, "ok")
            capsula.log("SMOKE-ROUTED-OK micropython")
            return
        if rpc == "WatchResources":
            call.send({})
            call.send({})
            call.send({})
            call.end(0, "stream complete")
            capsula.log("SMOKE-STREAM-OK micropython")
            return
        call.end(12, "unknown method")
    except Exception as err:
        capsula.log("SMOKE-CANCEL-SERVER micropython %s" % err)
        try:
            call.cancel()
        except Exception:
            pass


def handle_disconnect(event):
    event = event or {}
    capsula.log("SMOKE-DISCONNECT micropython status=%s detail=%s" % (event.get("status", 0), event.get("detail", "")))


def handle_invocation(event):
    capsula.log("SMOKE-INVOCATION micropython %s" % (event or {}).get("invocationId"))
    capsula.notify_invocation((event or {}).get("invocationId"), True, "smoke target")


capsula.on("call", handle_call)
capsula.on("disconnect", handle_disconnect)
capsula.on("invocation", handle_invocation)
capsula.start()
capsula.log("SMOKE-READY micropython app=" + capsula.env["appId"])
capsula.run()
