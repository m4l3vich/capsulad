// Contract tests for the Soundboard QuickJS app.
//
// The shipped main.js runs as the real host loads it: one classic script with
// `capsulad` and the standard JavaScript built-ins. The fake implements the
// documented QuickJS host surface plus the framework streams this app uses.
// Nothing here is loaded by the package; every assertion is about the app's
// observable settings reads, presentation frames, capture releases, PCM chunks
// and logs.
//
// Run: node --test apps/soundboard/test/*.test.mjs

import nodeTest from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import vm from "node:vm";

const test = (name, fn) => nodeTest(name, { timeout: 20000 }, fn);

const here = dirname(fileURLToPath(import.meta.url));
const appDir = join(here, "..");
const source = readFileSync(join(appDir, "main.js"), "utf8");
const manifest = readFileSync(join(appDir, "manifest.toml"), "utf8");
const settingsHTML = readFileSync(join(appDir, "ui", "settings.html"), "utf8");

const CONFIG = "capsulad.framework.v1.ApplicationConfigurationService";
const DEVICE = "capsulad.framework.v1.DeviceService";
const PRESENTATION = "capsulad.framework.v1.PresentationService";
const ROUTING = "capsulad.framework.v1.RoutingService";
const INPUT = "capsulad.framework.v1.InputService";
const AUDIO = "capsulad.framework.v1.AudioService";

const RING = "light.ring.main";
const SEGMENT = "display.segment.front";
const PANEL = "touch.panel.main";
const SPEAKER = "audio.output.speaker";

const DIGIT = [0x3f, 0x06, 0x5b, 0x4f, 0x66, 0x6d, 0x7d, 0x07, 0x7f, 0x6f];
const GLYPH = {
  A: 0x77, B: 0x7c, C: 0x39, D: 0x5e, E: 0x79, F: 0x71, G: 0x3d,
  H: 0x76, I: 0x06, J: 0x1e, L: 0x38, N: 0x54, O: 0x5c, P: 0x73,
  Q: 0x67, R: 0x50, S: 0x6d, T: 0x78, U: 0x3e, Y: 0x6e, "-": 0x40,
};
const DASH = 0x40;

function glyphMasks(name) {
  const masks = [0, 0, 0, 0];
  for (let i = 0; i < name.length && i < 4; i += 1) {
    const ch = name[i];
    masks[i] = /[0-9]/.test(ch) ? DIGIT[Number(ch)] : GLYPH[ch];
  }
  return masks;
}

function wav({ rate = 16000, channels = 1, seconds = 0.08, frequency = 440, amplitude = 0.3 } = {}) {
  const frames = Math.round(rate * seconds);
  const block = channels * 2;
  const data = Buffer.alloc(frames * block);
  for (let i = 0; i < frames; i += 1) {
    const value = Math.round(Math.sin((2 * Math.PI * frequency * i) / rate) * amplitude * 32767);
    for (let ch = 0; ch < channels; ch += 1) {
      data.writeInt16LE(value, i * block + ch * 2);
    }
  }
  const fmt = Buffer.alloc(16);
  fmt.writeUInt16LE(1, 0);
  fmt.writeUInt16LE(channels, 2);
  fmt.writeUInt32LE(rate, 4);
  fmt.writeUInt32LE(rate * block, 8);
  fmt.writeUInt16LE(block, 12);
  fmt.writeUInt16LE(16, 14);
  const body = Buffer.concat([
    Buffer.from("WAVE"),
    Buffer.from("fmt "), u32(fmt.length), fmt,
    Buffer.from("data"), u32(data.length), data,
  ]);
  return Buffer.concat([Buffer.from("RIFF"), u32(body.length), body]);
}

function u32(value) {
  const out = Buffer.alloc(4);
  out.writeUInt32LE(value, 0);
  return out;
}

function dataChunk(bytes) {
  let offset = 12;
  while (offset + 8 <= bytes.length) {
    const id = bytes.toString("ascii", offset, offset + 4);
    const size = bytes.readUInt32LE(offset + 4);
    if (id === "data") return bytes.subarray(offset + 8, offset + 8 + size);
    offset += 8 + size + (size & 1);
  }
  throw new Error("no data chunk");
}

function settingsSnapshot(sounds) {
  return {
    revision: "1",
    values: [{ key: "sounds", valueJson: JSON.stringify(JSON.stringify(sounds)) }],
  };
}

function resources({ ring = true, segment = true } = {}) {
  const out = [];
  if (ring) {
    out.push({
      resourceId: RING,
      kind: "RESOURCE_LED_RING",
      available: true,
      ring: { pixelCount: 12, supportsRgb: true },
    });
  }
  if (segment) {
    out.push({
      resourceId: SEGMENT,
      kind: "RESOURCE_SEGMENT_DISPLAY",
      available: true,
      segment: { digitCount: 4, supportsColon: true, supportsDecimalPoints: true },
    });
  }
  out.push({
    resourceId: PANEL,
    kind: "RESOURCE_TOUCH",
    available: true,
    touch: { partialGesturesAvailable: true, fullPanelAvailable: true },
  });
  return out;
}

function endpoints() {
  return [{
    endpointId: SPEAKER,
    direction: "AUDIO_PLAYBACK",
    supportedFormats: [{ sampleFormat: "PCM_S16LE", sampleRateHz: 48000, channels: 2 }],
  }];
}

/**
 * Runs main.js against a fake host.
 *
 * options.events is [{after, event}] relative to the first input wait.
 * options.endAfter ends the input stream at that relative time.
 */
function runApp(options = {}) {
  const sounds = options.sounds || [];
  const assets = options.assets || {};
  const events = (options.events || []).slice();
  const endAfter = options.endAfter ?? 3000;
  const randomValues = (options.randomValues || [0.5]).slice();
  const nowBase = 10_000;
  let now = nowBase;
  const inputStart = now;
  const logs = [];
  const frames = { segment: [], ring: [] };
  const calls = [];
  const releases = [];
  const opened = [];
  let pending = [];
  let captureSeq = 0;
  let presentationSeq = 0;
  let playlist = null;
  let inputCall = null;
  let stopped = 0;
  let runCalls = 0;

  const snap = (value) => JSON.parse(JSON.stringify(value));

  function callRecord(service, method, kind) {
    const record = {
      service, method, kind, sent: [], cancelled: false, cancelledAt: 0,
      ended: false, closed: false, chunks: [], chunkAt: [], recvQueue: [],
      recvCalls: 0, resource: "", captureId: "",
    };
    opened.push(record);
    return record;
  }

  function queueSend(record, value) {
    record.sent.push(snap(value));
    if (record.service === PRESENTATION && value.open) {
      record.resource = value.open.resourceId;
      presentationSeq += 1;
      record.recvQueue.push({
        ready: {
          presentationId: "prs_" + presentationSeq,
          resourceId: record.resource,
          hasValidFrame: false,
        },
      });
    }
    if (record.service === PRESENTATION && value.frame) {
      if (value.frame.segment) {
        // Array.from runs in this realm: a VM array keeps its foreign
        // prototype and strict assert.deepEqual would reject it even when the
        // contents match.
        frames.segment.push({
          masks: Array.from(value.frame.segment.cells, (cell) => Number(cell.mask || 0)),
          colon: !!value.frame.segment.colon,
          at: now,
          order: frames.segment.length,
        });
      }
      if (value.frame.ring) {
        frames.ring.push({
          pixels: Array.from(value.frame.ring.pixels, (px) =>
            [Number(px.red), Number(px.green), Number(px.blue)]),
          at: now,
          order: frames.ring.length,
        });
      }
    }
    if (record.service === PRESENTATION && value.close) {
      record.closed = true;
      record.recvQueue.push(null);
    }
    if (record.service === ROUTING && value.resourceId) {
      captureSeq += 1;
      record.resource = value.resourceId;
      record.captureId = "cap_" + captureSeq;
      record.recvQueue.push({
        granted: {
          capture: {
            captureId: "cap_" + captureSeq,
            resourceId: value.resourceId,
            selected: true,
            ready: true,
          },
        },
      });
    }
    if (record.service === AUDIO && value.open) {
      playlist = record;
      record.recvQueue.push({ started: { streamId: "spk_1", actualFormat: value.open.format } });
    }
    if (record.service === AUDIO && value.chunk) {
      record.chunks.push(value.chunk.pcm);
      record.chunkAt.push(now);
    }
  }

  function makeCall(service, method, kind) {
    const record = callRecord(service, method, kind);
    const call = {
      info: null,
      send(value) { queueSend(record, value); return call; },
      sendBytes() { return call; },
      halfClose() {
        if (record.service === AUDIO) record.recvQueue.push(null);
        return call;
      },
      recv() {
        record.recvCalls += 1;
        if (record.recvQueue.length > 0) return record.recvQueue.shift();
        return null;
      },
      tryRecv() {
        if (record.cancelled) return null;
        if (record.recvQueue.length > 0) return record.recvQueue.shift();
        if (record.service === INPUT) {
          now += 20;
          return nextInput();
        }
        return undefined;
      },
      cancel() {
        if (!record.cancelled) {
          record.cancelled = true;
          record.cancelledAt = now;
          calls.push("cancel:" + record.service);
        }
        return call;
      },
      end() { record.ended = true; return call; },
      callInfo() { return { ended: record.ended || record.closed }; },
      status() { return { code: 0, detail: "" }; },
    };
    return call;
  }

  function nextInput() {
    if (pending.length > 0) return pending.shift();
    const elapsed = now - inputStart;
    if (events.length > 0 && events[0].after <= elapsed) {
      return events.shift().event;
    }
    if (elapsed >= endAfter) return null;
    return undefined;
  }

  const capsulad = {
    env: { appId: "org.capsulad.soundboard", runId: "run_test", sdk: "0.5", appDir: "/app" },
    WAIT_FOREVER: 0xffffffff,
    log(message) { logs.push(String(message)); },
    start() {},
    run() { runCalls += 1; },
    stop() { stopped += 1; },
    on() {},
    notifyInvocation() {},
    notifyAction() {},
    registerDescriptors() {},
    info() { return {}; },
    handleInfo() { return {}; },
    readPackageFile() { throw new Error("not used"); },
    readAssetFile(path) {
      if (!Object.prototype.hasOwnProperty.call(assets, path)) {
        throw new Error(path + ": no such asset");
      }
      if (options.readCostMs) now += options.readCostMs;
      const bytes = assets[path];
      const perByte = options.encodeMsPerByte || 0;
      if (!perByte) return bytes;
      // Model the device's single-threaded base64 cost by charging simulated
      // time for each byte handed to the encoder. The app calls subarray once
      // per slice; charging here makes a whole-file burst visibly slower than
      // the budgeted slice loop.
      return new Proxy(bytes, {
        get(target, prop) {
          if (prop === "subarray") {
            return (start, end) => {
              const from = start === undefined ? 0 : Number(start);
              const to = end === undefined ? target.length : Number(end);
              now += (to - from) * perByte;
              return target.subarray(from, to);
            };
          }
          const value = Reflect.get(target, prop, target);
          return typeof value === "function" ? value.bind(target) : value;
        },
      });
    },
    call(service, method, request) {
      if (service === CONFIG && method === "Get") return settingsSnapshot(sounds);
      if (service === DEVICE && method === "ListResources") {
        return { resources: options.resources || resources(options.resourceFlags) };
      }
      if (service === AUDIO && method === "ListEndpoints") return { endpoints: endpoints() };
      if (service === ROUTING && method === "ReleaseCapture") {
        releases.push(request.captureId);
        return { released: true };
      }
      throw new Error("unexpected unary " + service + "/" + method);
    },
    callBytes() { throw new Error("not used"); },
    open(service, method, kind) {
      const call = makeCall(service, method, kind);
      if (service === INPUT) inputCall = call;
      return call;
    },
    openRouted() { throw new Error("not used"); },
    stream() { throw new Error("not used"); },
    routed() { throw new Error("not used"); },
    routedStream() { throw new Error("not used"); },
  };

  const context = {
    capsulad,
    Date: { now: () => now },
    Math: Object.create(Math),
    console,
  };
  context.Math.random = () => {
    const value = randomValues.length > 1 ? randomValues.shift() : randomValues[0];
    return value;
  };

  vm.runInNewContext(source, context, { filename: "main.js" });

  return {
    logs,
    frames,
    calls,
    releases,
    opened,
    now,
    stopped,
    runCalls,
    inputCall,
    audioCalls: opened.filter((record) => record.service === AUDIO),
    captures: opened.filter((record) => record.service === ROUTING),
    presentations: opened.filter((record) => record.service === PRESENTATION),
  };
}

const beep = wav({ seconds: 0.08 });
const boop = wav({ rate: 22050, channels: 2, seconds: 0.06, frequency: 660 });

function base(overrides = {}) {
  return {
    sounds: [
      { name: "BEEP", asset: "/assets/beep.wav", color: "#00c2ff" },
      { name: "BOOP", asset: "/assets/boop.wav", color: "random" },
    ],
    assets: { "/assets/beep.wav": beep, "/assets/boop.wav": boop },
    ...overrides,
  };
}

test("manifest declares the QuickJS app and custom settings UI", () => {
  assert.match(manifest, /runtime = "quickjs"/);
  assert.match(manifest, /\[settings_ui\]/);
  assert.match(manifest, /html = "ui\/settings\.html"/);
  assert.match(settingsHTML, /type="color"/);
  assert.match(settingsHTML, /CapsuladUI\.patch/);
});

test("starts with the first sound name and a partial panel capture", () => {
  const run = runApp(base({ endAfter: 100 }));
  assert.equal(run.logs.some((line) => line.includes("loaded sounds=2 active=BEEP")), true);
  assert.equal(run.captures.some((record) =>
    record.resource === PANEL && record.sent[0].touchMode === "TOUCH_PARTIAL_GESTURES"), true);
  assert.equal(run.frames.segment.some((frame) =>
    JSON.stringify(frame.masks) === JSON.stringify(glyphMasks("BEEP"))), true);
});

test("centre plays the active WAV and lights its fixed colour", () => {
  const run = runApp(base({
    endAfter: 400,
    events: [{ after: 100, event: { key: { key: "center", pressed: true } } }],
  }));
  const audio = run.audioCalls.find((record) => record.chunks.length > 0);
  assert.ok(audio, "no playback chunks were sent");
  const pcm = Buffer.concat(audio.chunks.map((chunk) => Buffer.from(chunk, "base64")));
  assert.deepEqual(pcm, dataChunk(beep));
  assert.equal(audio.sent[0].open.endpointId, SPEAKER);
  assert.deepEqual(audio.sent[0].open.format, {
    sampleFormat: "PCM_S16LE", sampleRateHz: 16000, channels: 1,
  });
  const coloured = run.frames.ring.filter((frame) =>
    frame.pixels.some(([r, g, b]) => r || g || b));
  assert.ok(coloured.length > 0, "the ring never lit");
  assert.deepEqual(coloured[0].pixels[0], [0, 194, 255]);
  assert.equal(run.logs.some((line) => line.includes("playing BEEP")), true);
});

test("swipes switch sounds and show their names", () => {
  const run = runApp(base({
    endAfter: 500,
    events: [
      { after: 100, event: { gesture: { gestureId: "swipe_right", phase: "end" } } },
      { after: 300, event: { gesture: { gestureId: "swipe_left", phase: "end" } } },
    ],
  }));
  assert.equal(run.logs.some((line) => line.includes("selected BOOP")), true);
  assert.equal(run.logs.some((line) => line.includes("selected BEEP")), true);
  const shown = run.frames.segment.map((frame) => frame.masks.join(","));
  assert.equal(shown.includes(glyphMasks("BOOP").join(",")), true);
  assert.equal(shown.includes(glyphMasks("BEEP").join(",")), true);
});

test("a random colour is applied to every ring pixel", () => {
  const run = runApp(base({
    endAfter: 400,
    randomValues: [0.5],
    events: [
      { after: 100, event: { gesture: { gestureId: "swipe_right", phase: "end" } } },
      { after: 200, event: { key: { key: "center", pressed: true } } },
    ],
  }));
  const audio = run.audioCalls.find((record) => record.chunks.length > 0);
  assert.ok(audio, "no playback");
  const frames = run.frames.ring.filter((frame) =>
    frame.pixels.some(([r, g, b]) => r || g || b));
  const randomFrame = frames[frames.length - 1];
  assert.ok(randomFrame, "no coloured frame");
  const first = randomFrame.pixels[0];
  assert.ok(first[0] || first[1] || first[2]);
  assert.deepEqual(new Set(randomFrame.pixels.map((px) => px.join(","))).size, 1);
});

test("invalid settings entries are skipped and bad assets do not play", () => {
  const run = runApp(base({
    sounds: [
      { name: "TOOLONG", asset: "/assets/beep.wav", color: "#00c2ff" },
      { name: "GO", asset: "/app/beep.wav", color: "#00c2ff" },
      { name: "NO", asset: "/assets/missing.wav", color: "#00c2ff" },
    ],
    assets: { "/assets/beep.wav": beep },
    endAfter: 300,
    events: [{ after: 100, event: { key: { key: "center", pressed: true } } }],
  }));
  assert.equal(run.logs.some((line) => line.includes("name must be 1-4")), true);
  assert.equal(run.logs.some((line) => line.includes("asset must be /assets/*.wav")), true);
  assert.equal(run.logs.some((line) => line.includes("cannot load /assets/missing.wav")), true);
  assert.equal(run.audioCalls.some((record) => record.chunks.length > 0), false);
  assert.equal(run.frames.segment.some((frame) =>
    JSON.stringify(frame.masks) === JSON.stringify([DASH, DASH, DASH, DASH])), true);
});

test("name glyphs are uppercased and left-aligned", () => {
  const run = runApp(base({
    sounds: [{ name: "ab", asset: "/assets/beep.wav", color: "random" }],
    endAfter: 100,
  }));
  const frame = run.frames.segment.find((entry) =>
    entry.masks[0] === GLYPH.A && entry.masks[1] === GLYPH.B);
  assert.ok(frame, "AB frame missing");
  assert.deepEqual(frame.masks, [GLYPH.A, GLYPH.B, 0, 0]);
});

test("the segment and ring captures are released after the brief window", () => {
  const run = runApp(base({
    endAfter: 2600,
    events: [{ after: 100, event: { key: { key: "center", pressed: true } } }],
  }));
  const segmentCapture = run.captures.find((record) => record.resource === SEGMENT);
  const ringCapture = run.captures.find((record) => record.resource === RING);
  assert.ok(segmentCapture, "segment capture never acquired");
  assert.ok(ringCapture, "ring capture never acquired");
  assert.equal(run.releases.includes(segmentCapture.captureId), true);
  assert.equal(run.releases.length >= 2, true);
  assert.equal(run.calls.includes("cancel:" + PRESENTATION), true);
  assert.equal(run.calls.includes("cancel:" + INPUT), true);
  assert.equal(run.stopped, 1);
});

test("no configured sounds leaves the app usable and shows dashes", () => {
  const run = runApp({
    sounds: [],
    assets: {},
    endAfter: 300,
    events: [{ after: 100, event: { key: { key: "center", pressed: true } } }],
  });
  assert.equal(run.logs.some((line) => line.includes("no playable sounds")), true);
  assert.equal(run.logs.some((line) => line.includes("centre pressed with no configured sounds")), true);
  assert.equal(run.frames.segment.some((frame) =>
    JSON.stringify(frame.masks) === JSON.stringify([DASH, DASH, DASH, DASH])), true);
});

test("a malformed WAV is rejected before playback", () => {
  const run = runApp(base({
    sounds: [{ name: "BAD", asset: "/assets/bad.wav", color: "#ffffff" }],
    assets: { "/assets/bad.wav": Buffer.from("not a wav") },
    endAfter: 300,
    events: [{ after: 100, event: { key: { key: "center", pressed: true } } }],
  }));
  assert.equal(run.logs.some((line) => line.includes("cannot load /assets/bad.wav")), true);
  assert.equal(run.audioCalls.some((record) => record.chunks.length > 0), false);
});

// The device takes about 0.6 s to base64-encode and send one 157 KiB WAV when
// the whole file is encoded in the press path. A 2 s stereo file is therefore
// the compact regression: encoding in one burst leaves a long dead wait
// before the first PCM and the ring deadline cannot run until the burst ends.
// The budgeted loop must keep both close.
test("a slow WAV encode keeps the ring window and audio start concurrent", () => {
  const slow = wav({ rate: 48000, channels: 2, seconds: 2.0 });
  const run = runApp({
    sounds: [{ name: "SLUG", asset: "/assets/slow.wav", color: "#00c2ff" }],
    assets: { "/assets/slow.wav": slow },
    encodeMsPerByte: 0.006,
    endAfter: 6000,
    events: [{ after: 100, event: { key: { key: "center", pressed: true } } }],
  });
  const colour = run.frames.ring.find((frame) =>
    frame.pixels.some(([r, g, b]) => r || g || b));
  assert.ok(colour, "the ring never lit");
  const audio = run.audioCalls.find((record) => record.chunkAt.length > 0);
  assert.ok(audio, "no playback chunks were sent");
  const firstChunkDelay = audio.chunkAt[0] - colour.at;
  assert.ok(firstChunkDelay <= 150,
    "first PCM chunk " + firstChunkDelay + " ms after the colour frame");
  const ringMs = Math.max(700, Math.min(10000, 2000));
  const off = run.frames.ring.find((frame) => frame.at > colour.at &&
    frame.pixels.every(([r, g, b]) => !r && !g && !b));
  assert.ok(off, "the ring never expired");
  const ringWindow = off.at - colour.at;
  assert.ok(Math.abs(ringWindow - ringMs) <= 150,
    "ring window " + ringWindow + " ms, expected " + ringMs + " ms");
  const pcm = Buffer.concat(audio.chunks.map((chunk) => Buffer.from(chunk, "base64")));
  assert.deepEqual(pcm, dataChunk(slow));
});

test("a second centre press replaces the stream and the colour safely", () => {
  const first = wav({ rate: 48000, channels: 2, seconds: 3.0 });
  const second = wav({ rate: 48000, channels: 2, seconds: 0.4, frequency: 880 });
  const run = runApp({
    sounds: [
      { name: "ONE", asset: "/assets/one.wav", color: "#ff0000" },
      { name: "TIC", asset: "/assets/two.wav", color: "#00ff00" },
    ],
    assets: { "/assets/one.wav": first, "/assets/two.wav": second },
    encodeMsPerByte: 0.002,
    endAfter: 4000,
    events: [
      { after: 100, event: { key: { key: "center", pressed: true } } },
      { after: 700, event: { gesture: { gestureId: "swipe_right", phase: "end" } } },
      { after: 900, event: { key: { key: "center", pressed: true } } },
    ],
  });
  const streams = run.audioCalls.filter((record) => record.sent.some((value) => value.open));
  assert.equal(streams.length, 2, "expected two playback streams");
  assert.ok(streams[0].chunks.length > 0, "the first stream sent no audio before the switch");
  assert.equal(streams[0].cancelled, true, "the first stream was not cancelled");
  const lastFirstChunk = streams[0].chunkAt[streams[0].chunkAt.length - 1];
  assert.ok(lastFirstChunk <= streams[0].cancelledAt,
    "the first stream emitted audio after it was cancelled");
  const pcm = Buffer.concat(streams[1].chunks.map((chunk) => Buffer.from(chunk, "base64")));
  assert.deepEqual(pcm, dataChunk(second));
  assert.equal(run.frames.ring.some((frame) =>
    frame.pixels.some(([r, g, b]) => r === 0 && g === 255 && b === 0)), true,
  "the second sound's colour never reached the ring");
});
