"use strict";

var TARGET = "org.example.micropython-smoke";
var DEVICE = "capsula.framework.v1.DeviceService";

capsula.on("disconnect", function (event) {
  capsula.log("SMOKE-DISCONNECT quickjs status=" + ((event && event.status) || 0) + " detail=" + ((event && event.detail) || ""));
});

capsula.on("invocation", function (event) {
  capsula.log("SMOKE-INVOCATION quickjs " + (event && event.invocationId));
  capsula.notifyInvocation(event.invocationId, true, "smoke probe");
});

capsula.start();
capsula.log("SMOKE-READY quickjs app=" + capsula.env.appId);

var device = capsula.call("capsula.framework.v1.DeviceService", "GetInfo", {});
capsula.log("SMOKE-PUBLIC-API quickjs model=" + device.model);

var reply = capsula.routed(TARGET, DEVICE, "GetInfo", {}, 5000);
capsula.log("SMOKE-ROUTED-OK quickjs " + JSON.stringify(reply));

var stream = capsula.routedStream(TARGET, DEVICE, "WatchResources", {}, 5000);
var first = stream.recv(5000);
stream.cancel();
capsula.log("SMOKE-CANCEL-CLIENT quickjs " + JSON.stringify(first));

capsula.run();
