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

// Bounded, non-blocking receive. An idle stream must report "nothing yet" as
// undefined instead of terminating the app, and a bounded wait must not turn
// into an error; this is what lets a script animate and stay responsive.
var idle = capsulad.open(DEVICE, "WatchResources", 1, 0);
idle.send({});
var seen = 0;
for (var i = 0; i < 32; i++) {
  var item = idle.tryRecv(1000);
  if (item === undefined || item === null) break;
  seen++;
}
var started = Date.now();
var empty = idle.tryRecv(0);
var emptyMs = Date.now() - started;
capsulad.log("SMOKE-TRYRECV quickjs events=" + seen +
             " empty=" + (empty === undefined) + " ms=" + emptyMs);
idle.cancel();

var rawStream = capsulad.open(DEVICE, "WatchResources", 1, 0);
rawStream.send({});
var rawFirst = rawStream.tryRecvBytes(2000);
var rawEmpty = false;
for (var j = 0; j < 32; j++) {
  var probe = rawStream.tryRecvBytes(0);
  if (probe === undefined) { rawEmpty = true; break; }
  if (probe === null) break;
}
// A non-positive timeout must poll once, not block or fail.
var negStarted = Date.now();
var neg = rawStream.tryRecvBytes(-1);
var negMs = Date.now() - negStarted;
rawStream.cancel();
capsulad.log("SMOKE-TRYRECV-BYTES quickjs first=" + (rawFirst instanceof Uint8Array) +
             " empty=" + rawEmpty);
capsulad.log("SMOKE-TRYRECV-NEG quickjs empty=" + (neg === undefined) + " ms=" + negMs);

var tryRecvOk = empty === undefined && emptyMs < 100 && rawFirst instanceof Uint8Array && rawEmpty &&
  neg === undefined && negMs < 100;
capsulad.log(tryRecvOk ? "SMOKE-TRYRECV-OK quickjs" : "SMOKE-TRYRECV-FAIL quickjs");

capsulad.run();
