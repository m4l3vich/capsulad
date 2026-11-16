"use strict";

var TARGET = "org.example.micropython-smoke";
var DEVICE = "capsulad.framework.v1.DeviceService";

capsulad.on("disconnect", function (event) {
  capsulad.log("SMOKE-DISCONNECT quickjs status=" + ((event && event.status) || 0) + " detail=" + ((event && event.detail) || ""));
});

capsulad.on("invocation", function (event) {
  capsulad.log("SMOKE-INVOCATION quickjs " + (event && event.invocationId));
  capsulad.notifyInvocation(event.invocationId, true, "smoke probe");
});

capsulad.start();
capsulad.log("SMOKE-READY quickjs app=" + capsulad.env.appId);

var device = capsulad.call("capsulad.framework.v1.DeviceService", "GetInfo", {});
capsulad.log("SMOKE-PUBLIC-API quickjs model=" + device.model);

var reply = capsulad.routed(TARGET, DEVICE, "GetInfo", {}, 5000);
capsulad.log("SMOKE-ROUTED-OK quickjs " + JSON.stringify(reply));

var stream = capsulad.routedStream(TARGET, DEVICE, "WatchResources", {}, 5000);
var first = stream.recv(5000);
stream.cancel();
capsulad.log("SMOKE-CANCEL-CLIENT quickjs " + JSON.stringify(first));

capsulad.run();
