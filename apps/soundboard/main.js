"use strict";

// Soundboard for Capsula Mini (QuickJS).
//
// The centre key plays the active WAV from the shared asset store. Built-in
// left/right swipes move through the configured sounds and briefly show the
// active name on the segment display. Each sound carries a structured colour:
// a fixed hex value or "random", chosen at activation. The LED ring lights that
// colour while the sound plays, then releases to the configured baseline.
//
// Configuration is one JSON text setting (`sounds`), edited by the package's
// custom settings UI. Each entry is {name, asset, color}: name is one to four
// seven-segment characters, asset is a /assets/*.wav path and color is
// "#rrggbb" or "random". The app validates all three again on receipt because
// the framework enforces only the stored JSON type.
//
// QuickJS has no timers and does not pump AppBus events while the app spins in
// its own input loop. A bounded Call.tryRecv slice therefore drives every
// deadline and drains ownership events. Playback is part of that loop too: a
// press opens the speaker stream and starts the colour window, then each loop
// pass converts and queues one small PCM slice. Encoding the whole WAV in the
// press path would hold this single thread for seconds on the device, so the
// ring would be dark before the sound started. Settings changes are read at
// startup; restart the app after editing the sound list.
//
// The app holds a partial touch capture: centre and swipes belong to it while
// the volume and microphone keys keep their framework behaviour. Neither
// presentation is kept selected between sounds; the segment and ring captures
// are acquired only for the brief display/colour window, so the clock and ring
// baseline return immediately.

var CONFIG_SERVICE = "capsulad.framework.v1.ApplicationConfigurationService";
var DEVICE_SERVICE = "capsulad.framework.v1.DeviceService";
var PRESENTATION_SERVICE = "capsulad.framework.v1.PresentationService";
var ROUTING_SERVICE = "capsulad.framework.v1.RoutingService";
var INPUT_SERVICE = "capsulad.framework.v1.InputService";
var AUDIO_SERVICE = "capsulad.framework.v1.AudioService";

var RING_RESOURCE = "light.ring.main";
var SEGMENT_RESOURCE = "display.segment.front";
var PANEL_RESOURCE = "touch.panel.main";
var SPEAKER_ENDPOINT = "audio.output.speaker";

var MAX_SOUNDS = 16;
var MAX_WAV_BYTES = 8 * 1024 * 1024;
// Playback is streamed from the input loop, not encoded in one press-path
// burst: on the device a single 256 KiB base64 conversion blocks this
// single-threaded host for about a second, which freezes the ring deadline and
// input handling. Chunks stay protocol-sized while each pass converts only a
// small slice, and the first chunk is smaller so audio starts promptly.
var PLAY_CHUNK_BYTES = 64 * 1024;
var PLAY_FIRST_CHUNK_BYTES = 12 * 1024;
var PLAY_SLICE_BYTES = 12 * 1024;
var PLAY_PUMP_BUDGET_MS = 8;
var INPUT_POLL_MS = 20;
var NAME_SHOW_MS = 1400;
var RING_MIN_MS = 700;
var RING_MAX_MS = 10000;
var DASH_MASK = 0x40;

// Curated uppercase seven-segment alphabet. Names are uppercased before
// validation, so a user may type either case.
var GLYPHS = {
  "0": 0x3f, "1": 0x06, "2": 0x5b, "3": 0x4f, "4": 0x66,
  "5": 0x6d, "6": 0x7d, "7": 0x07, "8": 0x7f, "9": 0x6f,
  "A": 0x77, "B": 0x7c, "C": 0x39, "D": 0x5e, "E": 0x79,
  "F": 0x71, "G": 0x3d, "H": 0x76, "I": 0x06, "J": 0x1e,
  "L": 0x38, "N": 0x54, "O": 0x5c, "P": 0x73, "Q": 0x67,
  "R": 0x50, "S": 0x6d, "T": 0x78, "U": 0x3e, "Y": 0x6e,
  "-": 0x40
};

var sounds = [];
var active = 0;

var panelCapture = null;
var input = null;
var segmentAvailable = false;
var ringAvailable = false;
var ringPixelCount = 0;

var segmentPresentation = null;
var segmentCapture = null;
var segmentUntil = 0;
var segmentBroken = false;

var ringPresentation = null;
var ringCapture = null;
var ringUntil = 0;
var ringBroken = false;

var player = null;
var playerWav = null;
var playerOffset = 0;
var playerChunkEnd = 0;
var playerPiece = 0;
var playerChunkText = "";
var playerChunkOpen = false;
var playerHalfClosed = false;
var exiting = false;
var exitReason = "";

function log(message) {
  capsulad.log("soundboard: " + message);
}

// --- settings -------------------------------------------------------------

function displayableName(value) {
  if (typeof value !== "string") return "";
  var name = value.trim().toUpperCase();
  if (name.length < 1 || name.length > 4) return "";
  for (var i = 0; i < name.length; i++) {
    if (GLYPHS[name.charAt(i)] === undefined) return "";
  }
  return name;
}

function assetPath(value) {
  if (typeof value !== "string") return "";
  if (value.indexOf("/assets/") !== 0) return "";
  if (value.indexOf("\\") >= 0 || value.indexOf("\u0000") >= 0) return "";
  if (!/\.wav$/i.test(value)) return "";
  var parts = value.substring("/assets/".length).split("/");
  if (parts.length === 0) return "";
  for (var i = 0; i < parts.length; i++) {
    if (parts[i] === "" || parts[i] === "." || parts[i] === "..") return "";
  }
  return value;
}

function normalizeColor(value) {
  if (value === undefined || value === null || value === "" || value === "random") {
    return "random";
  }
  if (typeof value !== "string") return "random";
  var text = value.trim();
  if (/^#[0-9a-fA-F]{3}$/.test(text)) {
    return ("#" + text.charAt(1) + text.charAt(1) + text.charAt(2) +
      text.charAt(2) + text.charAt(3) + text.charAt(3)).toLowerCase();
  }
  if (/^#[0-9a-fA-F]{6}$/.test(text)) return text.toLowerCase();
  return "random";
}

function parseSounds(raw) {
  if (typeof raw !== "string" || raw === "") return [];
  var parsed;
  try {
    parsed = JSON.parse(raw);
  } catch (err) {
    log("settings: sounds is not valid JSON: " + String(err && err.message || err));
    return [];
  }
  if (!Array.isArray(parsed)) {
    log("settings: sounds must be a JSON array");
    return [];
  }
  var out = [];
  for (var i = 0; i < parsed.length && out.length < MAX_SOUNDS; i++) {
    var entry = parsed[i];
    if (!entry || typeof entry !== "object") {
      log("settings: ignoring sounds[" + i + "]: not an object");
      continue;
    }
    var name = displayableName(entry.name);
    var asset = assetPath(entry.asset);
    if (name === "") {
      log("settings: ignoring sounds[" + i + "]: name must be 1-4 displayable characters");
      continue;
    }
    if (asset === "") {
      log("settings: ignoring sounds[" + i + "]: asset must be /assets/*.wav");
      continue;
    }
    out.push({ name: name, asset: asset, color: normalizeColor(entry.color) });
  }
  return out;
}

function settingValue(snapshot, key) {
  var values = (snapshot && snapshot.values) || [];
  for (var i = 0; i < values.length; i++) {
    if (values[i].key === key) return JSON.parse(values[i].valueJson);
  }
  return "";
}

function loadSettings() {
  var snapshot = capsulad.call(CONFIG_SERVICE, "Get",
    { appId: capsulad.env.appId }, 5000);
  sounds = parseSounds(settingValue(snapshot, "sounds"));
  if (sounds.length === 0) {
    log("settings: no playable sounds; add them in the app settings");
  } else {
    log("settings: loaded sounds=" + sounds.length + " active=" + sounds[0].name);
  }
  active = 0;
}

// --- output: segment ------------------------------------------------------

function nameMasks(name) {
  var masks = [0, 0, 0, 0];
  for (var i = 0; i < name.length && i < 4; i++) {
    masks[i] = GLYPHS[name.charAt(i)];
  }
  return masks;
}

function dashMasks() {
  return [DASH_MASK, DASH_MASK, DASH_MASK, DASH_MASK];
}

function writeSegment(masks) {
  if (segmentPresentation === null) return;
  var cells = [];
  for (var i = 0; i < 4; i++) {
    cells.push({ mask: masks[i], decimalPoint: false });
  }
  segmentPresentation.call.send({ frame: { segment: { cells: cells, colon: false } } });
}

function openSegment() {
  if (segmentBroken || !segmentAvailable) return false;
  if (segmentPresentation !== null) return true;
  var pres = openPresentation(SEGMENT_RESOURCE);
  if (pres === null) {
    segmentBroken = true;
    log("segment display unavailable; continuing without a name display");
    return false;
  }
  segmentPresentation = pres;
  writeSegment(dashMasks());
  segmentCapture = acquireCapture(SEGMENT_RESOURCE, pres.id, "");
  if (segmentCapture === null) {
    log("segment capture refused; continuing without a name display");
    closePresentation(pres);
    segmentPresentation = null;
    segmentBroken = true;
    return false;
  }
  return true;
}

function showName(name, durationMs) {
  var now = Date.now();
  if (!openSegment()) return;
  segmentUntil = now + durationMs;
  writeSegment(name === "" ? dashMasks() : nameMasks(name));
}

function expireSegment(now) {
  if (segmentPresentation === null || segmentUntil === 0 || now < segmentUntil) return;
  segmentUntil = 0;
  writeSegment([0, 0, 0, 0]);
  shutdownSegment();
}

function shutdownSegment() {
  if (segmentCapture !== null) {
    releaseCapture(segmentCapture, "display");
    segmentCapture = null;
  }
  if (segmentPresentation !== null) {
    closePresentation(segmentPresentation);
    segmentPresentation = null;
  }
}

// --- output: ring ---------------------------------------------------------

function hsv(r, g, b) { return { red: r, green: g, blue: b }; }

function HSV(h, s, v) {
  h -= Math.floor(h);
  var i = Math.floor(h * 6);
  var f = h * 6 - i;
  var p = v * (1 - s);
  var q = v * (1 - f * s);
  var t = v * (1 - (1 - f) * s);
  var rr, gg, bb;
  switch (i % 6) {
    case 0: rr = v; gg = t; bb = p; break;
    case 1: rr = q; gg = v; bb = p; break;
    case 2: rr = p; gg = v; bb = t; break;
    case 3: rr = p; gg = q; bb = v; break;
    case 4: rr = t; gg = p; bb = v; break;
    default: rr = v; gg = p; bb = q; break;
  }
  return hsv(Math.round(rr * 255), Math.round(gg * 255), Math.round(bb * 255));
}

function colorFor(sound) {
  if (sound.color === "random") return HSV(Math.random(), 0.85, 1);
  var hex = sound.color.replace("#", "");
  return hsv(parseInt(hex.substring(0, 2), 16),
    parseInt(hex.substring(2, 4), 16),
    parseInt(hex.substring(4, 6), 16));
}

function ringPixels(color) {
  var pixels = [];
  for (var i = 0; i < ringPixelCount; i++) {
    pixels.push({ red: color.red, green: color.green, blue: color.blue });
  }
  return pixels;
}

function openRing() {
  if (ringBroken || !ringAvailable) return false;
  if (ringPresentation !== null) return true;
  var pres = openPresentation(RING_RESOURCE);
  if (pres === null) {
    ringBroken = true;
    log("ring unavailable; continuing without a colour");
    return false;
  }
  ringPresentation = pres;
  pres.call.send({ frame: { ring: { pixels: ringPixels({ red: 0, green: 0, blue: 0 }) } } });
  ringCapture = acquireCapture(RING_RESOURCE, pres.id, "");
  if (ringCapture === null) {
    log("ring capture refused; continuing without a colour");
    closePresentation(pres);
    ringPresentation = null;
    ringBroken = true;
    return false;
  }
  return true;
}

function showColor(color, durationMs) {
  if (!openRing()) return;
  ringUntil = Date.now() + durationMs;
  ringPresentation.call.send({ frame: { ring: { pixels: ringPixels(color) } } });
}

function expireRing(now) {
  if (ringPresentation === null || ringUntil === 0 || now < ringUntil) return;
  ringUntil = 0;
  ringPresentation.call.send({
    frame: { ring: { pixels: ringPixels({ red: 0, green: 0, blue: 0 }) } }
  });
  shutdownRing();
}

function shutdownRing() {
  if (ringCapture !== null) {
    releaseCapture(ringCapture, "ring");
    ringCapture = null;
  }
  if (ringPresentation !== null) {
    closePresentation(ringPresentation);
    ringPresentation = null;
  }
}

// --- playback -------------------------------------------------------------

var BASE64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

function base64FromBytes(bytes) {
  var out = "";
  var i;
  for (i = 0; i + 2 < bytes.length; i += 3) {
    var n = (bytes[i] << 16) | (bytes[i + 1] << 8) | bytes[i + 2];
    out += BASE64.charAt((n >> 18) & 63) + BASE64.charAt((n >> 12) & 63) +
      BASE64.charAt((n >> 6) & 63) + BASE64.charAt(n & 63);
  }
  var left = bytes.length - i;
  if (left === 1) {
    var a = bytes[i] << 16;
    out += BASE64.charAt((a >> 18) & 63) + BASE64.charAt((a >> 12) & 63) + "==";
  } else if (left === 2) {
    var b = (bytes[i] << 16) | (bytes[i + 1] << 8);
    out += BASE64.charAt((b >> 18) & 63) + BASE64.charAt((b >> 12) & 63) +
      BASE64.charAt((b >> 6) & 63) + "=";
  }
  return out;
}

function readU16(bytes, offset) {
  return bytes[offset] | (bytes[offset + 1] << 8);
}

function readU32(bytes, offset) {
  return (bytes[offset] | (bytes[offset + 1] << 8) |
    (bytes[offset + 2] << 16) | (bytes[offset + 3] << 24)) >>> 0;
}

function fourcc(bytes, offset) {
  return String.fromCharCode(bytes[offset], bytes[offset + 1],
    bytes[offset + 2], bytes[offset + 3]);
}

// wavInfo validates a standard RIFF/WAVE file and returns the data chunk plus
// the playback format. PCM 16-bit and IEEE float 32-bit are accepted; the
// daemon's playback mixer resamples to its 48 kHz stereo output.
function wavInfo(bytes, path) {
  if (!bytes || typeof bytes.length !== "number" || bytes.length < 12) {
    throw new Error(path + ": not a RIFF/WAVE file");
  }
  if (fourcc(bytes, 0) !== "RIFF" || fourcc(bytes, 8) !== "WAVE") {
    throw new Error(path + ": not a RIFF/WAVE file");
  }
  var riffEnd = 8 + readU32(bytes, 4);
  if (riffEnd < 12 || riffEnd > bytes.length) {
    throw new Error(path + ": RIFF length exceeds the file");
  }
  var format = null;
  var dataStart = -1;
  var dataSize = 0;
  var offset = 12;
  while (offset < riffEnd) {
    if (riffEnd - offset < 8) throw new Error(path + ": truncated chunk header");
    var id = fourcc(bytes, offset);
    var size = readU32(bytes, offset + 4);
    offset += 8;
    if (size > riffEnd - offset) throw new Error(path + ": " + id + " exceeds RIFF");
    var padded = offset + size + (size & 1);
    if (padded > riffEnd) throw new Error(path + ": " + id + " missing pad byte");
    if (id === "fmt ") {
      if (size < 16) throw new Error(path + ": short fmt chunk");
      format = {
        encoding: readU16(bytes, offset),
        channels: readU16(bytes, offset + 2),
        rate: readU32(bytes, offset + 4),
        byteRate: readU32(bytes, offset + 8),
        blockAlign: readU16(bytes, offset + 12),
        bits: readU16(bytes, offset + 14)
      };
    } else if (id === "data") {
      dataStart = offset;
      dataSize = size;
    }
    offset = padded;
  }
  if (format === null) throw new Error(path + ": no fmt chunk");
  if (dataStart < 0) throw new Error(path + ": no data chunk");
  if (format.channels < 1 || format.channels > 8 ||
      format.rate < 8000 || format.rate > 192000) {
    throw new Error(path + ": unsupported channel count or sample rate");
  }
  var sampleFormat;
  if (format.encoding === 1 && format.bits === 16) {
    sampleFormat = "PCM_S16LE";
  } else if (format.encoding === 3 && format.bits === 32) {
    sampleFormat = "PCM_F32LE";
  } else {
    throw new Error(path + ": expected 16-bit PCM or 32-bit float WAV");
  }
  var blockAlign = format.channels * format.bits / 8;
  if (format.blockAlign !== blockAlign ||
      format.byteRate !== format.rate * blockAlign) {
    throw new Error(path + ": inconsistent fmt block/byte rate");
  }
  if (dataSize === 0 || dataSize % blockAlign !== 0) {
    throw new Error(path + ": malformed PCM data");
  }
  return {
    dataStart: dataStart,
    dataSize: dataSize,
    blockAlign: blockAlign,
    durationMs: (dataSize / format.byteRate) * 1000,
    format: {
      sampleFormat: sampleFormat,
      sampleRateHz: format.rate,
      channels: format.channels
    }
  };
}

function loadWav(path) {
  var bytes = capsulad.readAssetFile(path);
  if (!bytes || typeof bytes.length !== "number" ||
      typeof bytes.subarray !== "function") {
    throw new Error(path + ": readAssetFile did not return bytes");
  }
  if (bytes.length > MAX_WAV_BYTES) {
    throw new Error(path + ": file is larger than " + MAX_WAV_BYTES + " bytes");
  }
  return { bytes: bytes, info: wavInfo(bytes, path) };
}

function clearPlayer() {
  player = null;
  playerWav = null;
  playerOffset = 0;
  playerChunkEnd = 0;
  playerPiece = 0;
  playerChunkText = "";
  playerChunkOpen = false;
  playerHalfClosed = false;
}

function abortPlayer() {
  var call = player;
  clearPlayer();
  if (call !== null) {
    try { call.cancel(); } catch (_) {}
  }
}

function playSound(sound) {
  var wav;
  try {
    wav = loadWav(sound.asset);
  } catch (err) {
    log("cannot load " + sound.asset + ": " + String(err && err.message || err));
    showName("----", NAME_SHOW_MS);
    return;
  }
  showName(sound.name, NAME_SHOW_MS);

  abortPlayer();
  var call = capsulad.open(AUDIO_SERVICE, "Playback", 3, 0);
  call.send({ open: { endpointId: SPEAKER_ENDPOINT, format: wav.info.format } });
  var event = call.recv(10000);
  if (event === null || !event.started) {
    call.cancel();
    log("speaker playback did not open");
    return;
  }

  // The colour window starts when the speaker stream is open, so the ring
  // cannot expire before the first chunk is encoded. Chunks then stream from
  // the input loop, which keeps the deadline and a second press responsive.
  var ringMS = Math.max(RING_MIN_MS, Math.min(RING_MAX_MS, wav.info.durationMs));
  showColor(colorFor(sound), ringMS);

  player = call;
  playerWav = wav;
  playerOffset = 0;
  playerChunkOpen = false;
  playerHalfClosed = false;
  log("playing " + sound.name + " asset=" + sound.asset +
    " bytes=" + wav.info.dataSize + " ms=" + Math.round(wav.info.durationMs));
}

function endPlayerStream() {
  if (player === null || playerHalfClosed) return;
  playerHalfClosed = true;
  playerChunkOpen = false;
  playerChunkText = "";
  // The bytes are no longer needed once the final chunk is queued; the call
  // stays alive until pumpPlayer sees the drained stream end.
  playerWav = null;
  player.halfClose();
}

// One bounded stretch of encoding and sending. Each pass converts at most a
// small slice before returning to the input loop, so the ring deadline and
// touches are serviced between slices even when the whole WAV is large. The
// first chunk is smaller to start the speaker promptly.
function pumpAudio() {
  if (player === null || playerHalfClosed || playerWav === null) return;
  var info = playerWav.info;
  var deadline = Date.now() + PLAY_PUMP_BUDGET_MS;
  do {
    if (!playerChunkOpen) {
      if (playerOffset >= info.dataSize) {
        endPlayerStream();
        return;
      }
      var width = playerOffset === 0 ? PLAY_FIRST_CHUNK_BYTES : PLAY_CHUNK_BYTES;
      var end = Math.min(playerOffset + width, info.dataSize);
      // Keep every chunk a whole number of frames for the opened format.
      end -= end % info.blockAlign;
      if (end <= playerOffset) {
        endPlayerStream();
        return;
      }
      playerChunkEnd = info.dataStart + end;
      playerPiece = info.dataStart + playerOffset;
      playerChunkText = "";
      playerChunkOpen = true;
    }
    var pieceEnd = Math.min(playerPiece + PLAY_SLICE_BYTES, playerChunkEnd);
    playerChunkText += base64FromBytes(playerWav.bytes.subarray(playerPiece, pieceEnd));
    playerPiece = pieceEnd;
    if (playerPiece >= playerChunkEnd) {
      var text = playerChunkText;
      playerChunkText = "";
      playerChunkOpen = false;
      playerOffset = playerChunkEnd - info.dataStart;
      player.send({ chunk: { pcm: text } });
      if (playerOffset >= info.dataSize) {
        endPlayerStream();
        return;
      }
    }
  } while (Date.now() < deadline);
}

function pumpPlayer() {
  if (player === null) return;
  for (;;) {
    var event = player.tryRecv(0);
    if (event === undefined) return;
    if (event === null) {
      var call = player;
      clearPlayer();
      call.cancel();
      return;
    }
    if (event.notice) {
      if (event.notice.code !== "STREAM_CLOSED") {
        log("speaker notice code=" + event.notice.code + " detail=" + event.notice.detail);
      }
    }
  }
}

// --- framework streams ----------------------------------------------------

function openPresentation(resourceId) {
  var call = capsulad.open(PRESENTATION_SERVICE, "Open", 3, 0);
  call.send({ open: { resourceId: resourceId } });
  var event = call.recv(10000);
  if (event === null || !event.ready || !event.ready.presentationId) {
    call.cancel();
    return null;
  }
  return { call: call, id: event.ready.presentationId, ended: false };
}

function acquireCapture(resourceId, presentationId, touchMode) {
  var call = capsulad.open(ROUTING_SERVICE, "Capture", 1, 0);
  var request = { resourceId: resourceId };
  if (presentationId !== "") request.presentationId = presentationId;
  if (touchMode !== "") request.touchMode = touchMode;
  call.send(request);
  var event = call.recv(10000);
  if (event === null || !event.granted || !event.granted.capture ||
      !event.granted.capture.captureId) {
    call.cancel();
    return null;
  }
  return { call: call, id: event.granted.capture.captureId };
}

function releaseCapture(capture, label) {
  if (capture === null) return;
  var reply = capsulad.call(ROUTING_SERVICE, "ReleaseCapture",
    { captureId: capture.id }, 5000);
  log("released " + label + " capture=" + capture.id + " released=" + !!reply.released);
  capture.call.cancel();
}

function closePresentation(presentation) {
  if (presentation === null) return;
  var ended = presentation.ended === true;
  if (!ended && typeof presentation.call.callInfo === "function") {
    var info = presentation.call.callInfo();
    ended = !!(info && info.ended);
  }
  if (!ended) {
    presentation.call.send({ close: {} });
    if (typeof presentation.call.tryRecv === "function") {
      for (var i = 0; i < 8; i++) {
        var event = presentation.call.tryRecv(i === 0 ? 2000 : 0);
        if (event === null || event === undefined) break;
      }
    }
  }
  presentation.call.cancel();
}

function drainCapture(capture, label) {
  if (capture === null) return false;
  for (;;) {
    var event = capture.call.tryRecv(0);
    if (event === undefined) return false;
    if (event === null) {
      log(label + " capture stream ended");
      return true;
    }
    if (event.changed && event.changed.selected === false) {
      log(label + " capture preempted: " + event.changed.reason);
      return true;
    }
  }
}

function drainPresentation(presentation, label) {
  if (presentation === null) return false;
  for (;;) {
    var event = presentation.call.tryRecv(0);
    if (event === undefined) return false;
    if (event === null) {
      presentation.ended = true;
      log(label + " presentation stream ended");
      return true;
    }
  }
}

function checkSupport() {
  var endpoints = capsulad.call(AUDIO_SERVICE, "ListEndpoints", {}, 5000).endpoints || [];
  var speaker = false;
  for (var i = 0; i < endpoints.length; i++) {
    if (endpoints[i].endpointId === SPEAKER_ENDPOINT) speaker = true;
  }
  if (!speaker) return "no " + SPEAKER_ENDPOINT;

  var resources = capsulad.call(DEVICE_SERVICE, "ListResources", {}, 5000).resources || [];
  for (var j = 0; j < resources.length; j++) {
    if (resources[j].resourceId === SEGMENT_RESOURCE && resources[j].available === true &&
        resources[j].segment && resources[j].segment.digitCount >= 4) {
      segmentAvailable = true;
    }
    if (resources[j].resourceId === RING_RESOURCE && resources[j].available === true &&
        resources[j].ring && resources[j].ring.pixelCount > 0) {
      ringAvailable = true;
      ringPixelCount = resources[j].ring.pixelCount;
    }
  }
  if (!segmentAvailable) log("no four-digit segment display; names are logged only");
  if (!ringAvailable) log("no LED ring; colours are logged only");
  return null;
}

// --- controls and lifecycle ----------------------------------------------

function currentSound() {
  if (sounds.length === 0) return null;
  if (active < 0 || active >= sounds.length) active = 0;
  return sounds[active];
}

function switchSound(delta) {
  if (sounds.length === 0) {
    showName("----", NAME_SHOW_MS);
    return;
  }
  active = (active + delta + sounds.length) % sounds.length;
  var sound = currentSound();
  showName(sound.name, NAME_SHOW_MS);
  log("selected " + sound.name + " asset=" + sound.asset + " (" + (active + 1) + "/" + sounds.length + ")");
}

function handleInput(event) {
  if (event.key && event.key.pressed && event.key.key === "center") {
    var sound = currentSound();
    if (sound === null) {
      showName("----", NAME_SHOW_MS);
      log("centre pressed with no configured sounds");
      return;
    }
    playSound(sound);
    return;
  }
  if (event.gesture && event.gesture.phase === "end") {
    if (event.gesture.gestureId === "swipe_left") switchSound(-1);
    else if (event.gesture.gestureId === "swipe_right") switchSound(1);
  }
}

function pumpOwnership() {
  if (drainPresentation(segmentPresentation, "display")) {
    if (segmentCapture !== null) {
      segmentCapture.call.cancel();
      segmentCapture = null;
    }
    segmentPresentation.call.cancel();
    segmentPresentation = null;
    segmentBroken = true;
  }
  if (drainPresentation(ringPresentation, "ring")) {
    if (ringCapture !== null) {
      ringCapture.call.cancel();
      ringCapture = null;
    }
    ringPresentation.call.cancel();
    ringPresentation = null;
    ringBroken = true;
  }
  if (drainCapture(segmentCapture, "display")) {
    segmentCapture.call.cancel();
    segmentCapture = null;
    shutdownSegment();
    segmentBroken = true;
  }
  if (drainCapture(ringCapture, "ring")) {
    ringCapture.call.cancel();
    ringCapture = null;
    shutdownRing();
    ringBroken = true;
  }
  if (drainCapture(panelCapture, "panel")) {
    exitReason = "touch capture lost";
    exiting = true;
    return true;
  }
  return false;
}

function waitForInput() {
  for (;;) {
    if (pumpOwnership()) return;
    var now = Date.now();
    expireSegment(now);
    expireRing(now);
    pumpPlayer();
    pumpAudio();
    var event = input.tryRecv(INPUT_POLL_MS);
    if (event === undefined) continue;
    if (event === null) {
      exitReason = "touch input subscription ended";
      return;
    }
    handleInput(event);
  }
}

function shutdown() {
  log("exit reason=" + (exitReason !== "" ? exitReason : "requested"));
  abortPlayer();
  if (input !== null) {
    input.cancel();
    input = null;
  }
  if (panelCapture !== null) {
    releaseCapture(panelCapture, "panel");
    panelCapture = null;
  }
  shutdownSegment();
  shutdownRing();
  capsulad.stop();
}

function main() {
  capsulad.start();
  log("ready app=" + capsulad.env.appId + " run=" + capsulad.env.runId);

  if (typeof capsulad.readAssetFile !== "function") {
    log("unsupported: this QuickJS host has no readAssetFile; rebuild the interpreter host");
    capsulad.stop();
    return;
  }

  try {
    loadSettings();
    var unsupported = checkSupport();
    if (unsupported !== null) {
      log("unsupported: " + unsupported);
      capsulad.stop();
      return;
    }
    panelCapture = acquireCapture(PANEL_RESOURCE, "", "TOUCH_PARTIAL_GESTURES");
    if (panelCapture === null) {
      log("unavailable: the partial touch capture was refused");
      capsulad.stop();
      return;
    }
    input = capsulad.open(INPUT_SERVICE, "WatchInput", 1, 0);
    input.send({ resourceId: PANEL_RESOURCE, captureId: panelCapture.id });
  } catch (err) {
    log("unavailable: " + String(err && err.message || err));
    shutdown();
    return;
  }

  log("controls: centre plays the active sound; swipe left/right switches; " +
    "stop the app to leave");
  var sound = currentSound();
  if (sound !== null) showName(sound.name, NAME_SHOW_MS);
  waitForInput();
  shutdown();
}

main();
