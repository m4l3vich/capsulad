"use strict";

// Runnable QuickJS Capsula app. The host embeds the real QuickJS engine and
// exposes the same public libcapsula ABI as native apps through `capsula`.

capsula.on("invocation", function (event) {
  capsula.log("sample: invocation " + (event && event.invocationId));
  capsula.notifyInvocation(event.invocationId, true, "sample accepted");
});

capsula.on("settings_changed", function (event) {
  capsula.log("sample: settings revision " + (event && event.revision));
});

capsula.on("action", function (event) {
  capsula.log("sample: action " + (event && event.actionId));
  capsula.notifyAction(event.actionInvocationId, true, "greeted");
});

capsula.on("call", function (call) {
  var info = call.info || {};
  if (info.rpc_name !== "Echo") {
    call.end(12, "the sample only implements Echo");
    return;
  }
  var request = call.recv(5000) || {};
  call.send({ text: "quickjs:" + (request.text || "") });
  call.end(0, "ok");
});

capsula.start();
capsula.log("sample: READY app=" + capsula.env.appId);

var device = capsula.call("capsula.framework.v1.DeviceService", "GetInfo", {});
capsula.log("sample: framework API model=" + device.model);

capsula.run();
