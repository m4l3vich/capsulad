"use strict";
// Development QuickJS probe over the public `capsulad` host module. The echo
// contract is descriptor-less: the handler uses protobuf field 1 (a string).

var CONFIG = "capsulad.framework.v1.ApplicationConfigurationService";
var DEVICE = "capsulad.framework.v1.DeviceService";
var AUDIO = "capsulad.framework.v1.AudioService";
var PRESENTATION = "capsulad.framework.v1.PresentationService";
var RING = "light.ring.main";

function log(message) { capsulad.log("devtools-quickjs: " + message); }
function errorText(err) { return String((err && err.message) || err); }

// --- raw protobuf field 1 -------------------------------------------------
function encodeText(text) {
  var bytes = [0x0a], length = text.length;
  while (length >= 0x80) { bytes.push((length & 0x7f) | 0x80); length = Math.floor(length / 0x80); }
  bytes.push(length);
  for (var i = 0; i < text.length; i++) {
    var code = text.charCodeAt(i);
    if (code > 0x7f) throw new Error("echo text must be ASCII");
    bytes.push(code);
  }
  return new Uint8Array(bytes);
}

function decodeText(bytes) {
  if (!bytes || bytes.length < 2 || bytes[0] !== 0x0a) return null;
  var pos = 1, shift = 0, length = 0;
  while (pos < bytes.length) {
    var b = bytes[pos++];
    length += (b & 0x7f) * Math.pow(2, shift);
    if ((b & 0x80) === 0) break;
    shift += 7;
  }
  if (pos + length > bytes.length) return null;
  var out = "";
  for (var i = 0; i < length; i++) out += String.fromCharCode(bytes[pos + i]);
  return out;
}

// --- probes ---------------------------------------------------------------
function probeState(label) {
  try {
    var info = capsulad.call(DEVICE, "GetInfo", {}, 5000);
    var audio = capsulad.call(AUDIO, "GetAudioState", {}, 5000);
    var presets = capsulad.call(PRESENTATION, "ListPresets", { resourceId: RING }, 5000);
    log("state " + label + " app=" + capsulad.env.appId + " run=" + capsulad.env.runId +
        " sdk=" + capsulad.env.sdk + " model=" + info.model + " daemon=" + info.daemonVersion +
        " platform=" + (info.targetPlatform || "unspecified") + " volume=" + audio.masterVolume +
        " muted=" + !!audio.microphoneMuted + " ring_presets=" + ((presets.presets || []).length));
  } catch (err) { log("state " + label + " failed: " + errorText(err)); }
}

function bumpRuns(label) {
  try {
    var snap = capsulad.call(CONFIG, "Get", { appId: capsulad.env.appId }, 5000);
    var runs = 0, found = false, values = snap.values || [];
    for (var i = 0; i < values.length; i++) {
      if (values[i].key === "runs" && values[i].hasValue) {
        runs = parseInt(values[i].valueJson, 10) || 0;
        found = true;
      }
    }
    var next = runs + 1;
    var updated = capsulad.call(CONFIG, "Update", {
      appId: capsulad.env.appId, expectedRevision: snap.revision,
      changes: [{ key: "runs", valueJson: String(next) }]
    }, 5000);
    log("settings " + label + " revision " + snap.revision + "->" + updated.revision +
        " runs=" + next + " had=" + found);
  } catch (err) { log("settings " + label + " failed: " + errorText(err)); }
}

// heartbeat logs a bounded periodic line before the event loop starts. The
// QuickJS host exposes no timer and its binding errors are not catchable from
// script, so a timed stream read would terminate the app on the first timeout.
// MicroPython, whose SDK raises a catchable OSError, uses a timed read instead.
function heartbeat(beats) {
  var start = Date.now(), next = start + 1000, count = 0;
  while (count < beats) {
    var now = Date.now();
    if (now >= next) {
      count++;
      next += 1000;
      log("heartbeat " + count + "/" + beats + " uptime_ms=" + (now - start));
    }
  }
}

// --- handlers -------------------------------------------------------------
capsulad.on("invocation", function (event) {
  var step = "probe", args = (event && event.arguments) || [];
  for (var i = 0; i < args.length; i++) {
    if (args[i].name === "step" && args[i].value) step = args[i].value;
  }
  log("invocation " + ((event && event.invocationId) || "?") + " step=" + step);
  probeState("invocation");
  bumpRuns("invocation");
  capsulad.notifyInvocation(event && event.invocationId, true, "quickjs devtools handled " + step);
});

capsulad.on("settings_changed", function (event) {
  log("settings_changed revision=" + ((event && event.revision) || 0));
});

capsulad.on("action", function (event) {
  log("action " + ((event && event.actionId) || "?"));
  probeState("action");
  capsulad.notifyAction(event && event.actionInvocationId, true, "probe logged framework state");
});

capsulad.on("call", function (call) {
  var info = call.info || {};
  if (info.rpc_name !== "Echo") { call.end(12, "only Echo is implemented"); return; }
  try {
    var request = call.recvBytes(5000);
    if (request === null) { call.end(0, "empty request"); return; }
    var text = decodeText(request);
    if (text === null) { call.end(3, "expected one string field"); return; }
    log("echo request text=" + text);
    call.sendBytes(encodeText("quickjs:" + text));
    call.end(0, "ok");
  } catch (err) { call.end(13, errorText(err)); }
});

// --- startup --------------------------------------------------------------
capsulad.start();
log("ready app=" + capsulad.env.appId + " run=" + capsulad.env.runId + " sdk=" + capsulad.env.sdk);
probeState("startup");
bumpRuns("startup");
try {
  var triggered = capsulad.call(CONFIG, "TriggerAction", {
    appId: capsulad.env.appId, actionId: "probe"
  }, 5000);
  log("action triggered id=" + triggered.actionInvocationId);
} catch (err) {
  log("action trigger failed: " + errorText(err));
}
heartbeat(3);
capsulad.run();