import capsulad


def handle_call(call):
    info = call.info or {}
    service = info.get("grpc_service", "")
    rpc = info.get("rpc_name", "")
    capsulad.log("SMOKE-CALL micropython %s/%s" % (service, rpc))
    try:
        request = call.recv(5000) or {}
        if rpc == "GetInfo":
            call.send({"model": "micropython-smoke", "deviceId": "smoke"})
            call.end(0, "ok")
            capsulad.log("SMOKE-ROUTED-OK micropython")
            return
        if rpc == "WatchResources":
            call.send({})
            call.send({})
            call.send({})
            call.end(0, "stream complete")
            capsulad.log("SMOKE-STREAM-OK micropython")
            return
        call.end(12, "unknown method")
    except Exception as err:
        capsulad.log("SMOKE-CANCEL-SERVER micropython %s" % err)
        try:
            call.cancel()
        except Exception:
            pass


def handle_disconnect(event):
    event = event or {}
    capsulad.log("SMOKE-DISCONNECT micropython status=%s detail=%s" % (event.get("status", 0), event.get("detail", "")))


def handle_invocation(event):
    capsulad.log("SMOKE-INVOCATION micropython %s" % (event or {}).get("invocationId"))
    capsulad.notify_invocation((event or {}).get("invocationId"), True, "smoke target")


capsulad.on("call", handle_call)
capsulad.on("disconnect", handle_disconnect)
capsulad.on("invocation", handle_invocation)
capsulad.start()
capsulad.log("SMOKE-READY micropython app=" + capsulad.env["appId"])
capsulad.run()
