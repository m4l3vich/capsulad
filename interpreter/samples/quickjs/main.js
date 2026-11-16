"use strict";

// Runnable QuickJS Capsulad app. The host embeds the real QuickJS engine and
// exposes the same public libcapsulad ABI as native apps through `capsulad`.

capsulad.on("invocation", function (event) {
  capsulad.log("sample: invocation " + (event && event.invocationId));
  capsulad.notifyInvocation(event.invocationId, true, "sample accepted");
});

capsulad.on("settings_changed", function (event) {
  capsulad.log("sample: settings revision " + (event && event.revision));
});

capsulad.on("action", function (event) {
  capsulad.log("sample: action " + (event && event.actionId));
  capsulad.notifyAction(event.actionInvocationId, true, "greeted");
});

capsulad.on("call", function (call) {
  var info = call.info || {};
  if (info.rpc_name !== "Echo") {
    call.end(12, "the sample only implements Echo");
    return;
  }
  var request = call.recv(5000) || {};
  call.send({ text: "quickjs:" + (request.text || "") });
  call.end(0, "ok");
});

capsulad.start();
capsulad.log("sample: READY app=" + capsulad.env.appId);

var device = capsulad.call("capsulad.framework.v1.DeviceService", "GetInfo", {});
capsulad.log("sample: framework API model=" + device.model);

capsulad.run();
