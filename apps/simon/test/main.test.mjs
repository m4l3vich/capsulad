// Contract tests for the Simon Says QuickJS app.
//
// The app is exercised as the real host loads it: one classic script whose
// globals are `capsulad`, `Date`, `Math` and the standard JavaScript built-ins.
// The fake implements the documented QuickJS host surface plus the five
// framework streams the app uses (presentation, capture, touch input, playback
// and unary discovery). It is not a second product path: nothing here is
// loaded by the packaged app, and every assertion is about the app's observable
// requests, ring/segment frames, PCM chunks, logs and terminal cleanup.
//
// Run: node --test apps/simon/test/*.test.mjs

import nodeTest from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import vm from "node:vm";

// Every test must reach a terminal path. A 15 s bound keeps a harness mistake
// (for example an unreachable `when`) from hanging the suite.
const test = (name, fn) => nodeTest(name, { timeout: 15000 }, fn);

const here = dirname(fileURLToPath(import.meta.url));
const appDir = join(here, "..");
const source = readFileSync(join(appDir, "main.js"), "utf8");
const manifest = readFileSync(join(appDir, "manifest.toml"), "utf8");

const DEVICE_SERVICE = "capsulad.framework.v1.DeviceService";
const PRESENTATION_SERVICE = "capsulad.framework.v1.PresentationService";
const ROUTING_SERVICE = "capsulad.framework.v1.RoutingService";
const INPUT_SERVICE = "capsulad.framework.v1.InputService";
const AUDIO_SERVICE = "capsulad.framework.v1.AudioService";
const RING_RESOURCE = "light.ring.main";
const SEGMENT_RESOURCE = "display.segment.front";
const PANEL_RESOURCE = "touch.panel.main";
const SPEAKER_ENDPOINT = "audio.output.speaker";

const DIGIT = [0x3f, 0x06, 0x5b, 0x4f, 0x66, 0x6d, 0x7d, 0x07, 0x7f, 0x6f];
const DASH = 0x40;
const RED = [255, 0, 0];
const YELLOW = [255, 180, 0];
const GREEN = [0, 200, 0];
const BLUE = [0, 80, 255];
const FREQUENCIES = [392, 523, 659, 784];

function defaultResources(overrides = {}) {
  return [
    {
      resourceId: RING_RESOURCE,
      kind: "RESOURCE_RING",
      available: true,
      ring: { pixelCount: 12, supportsRgb: true },
      ...(overrides.ring || {}),
    },
    {
      resourceId: SEGMENT_RESOURCE,
      kind: "RESOURCE_SEGMENT",
      available: true,
      segment: { digitCount: 4, supportsColon: true, supportsDecimalPoints: true },
      ...(overrides.segment || {}),
    },
    {
      resourceId: PANEL_RESOURCE,
      kind: "RESOURCE_TOUCH",
      available: true,
      touch: { fullPanelAvailable: true, partialGesturesAvailable: true },
      ...(overrides.panel || {}),
    },
  ];
}

function defaultEndpoints() {
  return [
    {
      endpointId: SPEAKER_ENDPOINT,
      direction: "AUDIO_PLAYBACK",
      supportedFormats: [
        { sampleFormat: "PCM_S16LE", sampleRateHz: 48000, channels: 2 },
      ],
    },
    {
      endpointId: "audio.input.microphone",
      direction: "AUDIO_CAPTURE",
      supportedFormats: [
        { sampleFormat: "PCM_S16LE", sampleRateHz: 16000, channels: 1 },
      ],
    },
  ];
}

const snapshot = (value) => JSON.parse(JSON.stringify(value));
const litIndices = (pixels) =>
  pixels.flatMap((pixel, index) =>
    pixel.red || pixel.green || pixel.blue ? [index] : [],
  );
const isColored = (pixels) =>
  pixels.some((pixel) => pixel.red !== pixel.green || pixel.green !== pixel.blue);
const rgbAt = (pixels, index) => [
  pixels[index].red,
  pixels[index].green,
  pixels[index].blue,
];

/**
 * Runs main.js against a fake host.
 *
 * options.scenario is an ordered list of steps:
 *   { when: (view) => bool, events: (ctl) => event | event[] | null | undefined }
 * A step is pulled only when the app asks for panel input and its `when` is
 * true. `null` ends the input stream, `undefined` performs an action (for
 * example injecting a routing change) without delivering an event.
 *
 * When the scenario is exhausted, the stream ends at the next player-input
 * opportunity so every test has a terminal path.
 */
function runApp(options = {}) {
  const resources = options.resources || defaultResources();
  const endpoints = options.endpoints !== undefined ? options.endpoints : defaultEndpoints();
  const scenario = options.scenario ? options.scenario.slice() : [];
  const random = options.random ?? (() => 0.01);
  const refusedCaptures = new Set(options.refusedCaptures || []);
  const refusedPresentations = new Set(options.refusedPresentations || []);

  const logs = [];
  const ringFrames = [];
  const segmentFrames = [];
  const opened = [];
  const captures = [];
  const unary = [];
  const order = [];
  const inputQueue = [];
  let stopped = 0;
  let runCalls = 0;
  let started = false;
  let virtualMs = 5000;
  let captureSeq = 0;
  let presentationSeq = 0;
  let phase = "idle";
  let ctl = null;

  const nowMs = () => {
    virtualMs += 1;
    return virtualMs;
  };
  const nowNs = () => virtualMs * 1000000;

  const stateView = () => ({ logs, phase, ringFrames, segmentFrames, ctl });

  function captureRecord(resourceId) {
    return opened.find(
      (record) =>
        record.service === ROUTING_SERVICE &&
        record.resource === resourceId &&
        !record.cancelled,
    );
  }

  function callRecord(service, method, kind, timeout) {
    const record = {
      service,
      method,
      kind,
      timeout,
      sent: [],
      events: [],
      chunks: [],
      controls: [],
      cancelled: false,
      recvCalls: 0,
      resource: "",
      captureId: "",
    };
    opened.push(record);
    return record;
  }

  function cancelLabel(record) {
    if (record.service === PRESENTATION_SERVICE) {
      return "presentation-cancel:" + (record.resource || "unknown");
    }
    if (record.service === ROUTING_SERVICE) {
      return "capture-cancel:" + record.resource;
    }
    if (record.service === INPUT_SERVICE) return "input-cancel";
    if (record.service === AUDIO_SERVICE) return "speaker-cancel";
    return "cancel:" + record.service;
  }

  function handlePresentationSend(record, value) {
    if (value.open) {
      record.resource = value.open.resourceId;
      if (refusedPresentations.has(record.resource)) {
        record.events.push(null);
        return;
      }
      presentationSeq += 1;
      record.presentationId = "prs_" + presentationSeq;
      record.events.push({
        ready: {
          presentationId: record.presentationId,
          resourceId: record.resource,
          hasValidFrame: false,
        },
      });
      return;
    }
    if (value.frame) {
      if (value.frame.ring) {
        const pixels = snapshot(value.frame.ring.pixels || []);
        ringFrames.push({ pixels, atMs: virtualMs, logsAt: logs.length });
        if (options.onRingFrame) options.onRingFrame(pixels, ctl);
        return;
      }
      if (value.frame.segment) {
        const segment = value.frame.segment;
        const frame = {
          masks: Array.from(segment.cells || [], (cell) => cell.mask || 0),
          dots: Array.from(segment.cells || [], (cell) => !!cell.decimalPoint),
          colon: !!segment.colon,
          atMs: virtualMs,
          logsAt: logs.length,
        };
        segmentFrames.push(frame);
        if (options.onSegmentFrame) options.onSegmentFrame(frame, ctl);
        return;
      }
      throw new Error("unexpected presentation frame " + JSON.stringify(value));
    }
    if (value.close) {
      order.push("close:" + record.resource);
      record.events.push(null);
      return;
    }
    throw new Error("unexpected presentation request " + JSON.stringify(value));
  }

  function handleCaptureSend(record, request) {
    record.resource = request.resourceId;
    if (refusedCaptures.has(request.resourceId)) {
      record.events.push(null);
      return;
    }
    captureSeq += 1;
    const capture = {
      captureId: "cap_" + captureSeq,
      resourceId: request.resourceId,
      appId: "org.capsulad.simon",
      runId: "run_test",
      presentationId: request.presentationId || "",
      acquiredMonotonicNs: String(nowNs()),
      selected: true,
      ready: true,
      touchMode: request.touchMode || "TOUCH_CAPTURE_MODE_UNSPECIFIED",
    };
    record.captureId = capture.captureId;
    captures.push(capture);
    record.events.push({ granted: { capture } });
    record.events.push({ changed: { selected: true, reason: "granted" } });
  }

  function handleInputSend(record, request) {
    const panel = captures.find((capture) => capture.resourceId === PANEL_RESOURCE);
    if (!panel || request.captureId !== panel.captureId) {
      throw new Error("WatchInput must reference the selected panel capture");
    }
  }

  function handleAudioSend(record, value) {
    if (value.open) {
      if (options.refuseAudio) {
        record.events.push({ notice: { code: "UNAVAILABLE", detail: "test" } });
        return;
      }
      record.events.push({
        started: {
          streamId: "pbs_test",
          actualFormat: { sampleFormat: "PCM_S16LE", sampleRateHz: 48000, channels: 2 },
        },
      });
      return;
    }
    if (value.chunk) {
      record.chunks.push(value.chunk.pcm);
      if (options.onAudioChunk) options.onAudioChunk(value.chunk, ctl);
      return;
    }
    if (value.control) {
      record.controls.push(value.control);
      return;
    }
    throw new Error("unexpected playback request " + JSON.stringify(value));
  }

  function dispatch(record, value) {
    if (record.service === PRESENTATION_SERVICE) return handlePresentationSend(record, value);
    if (record.service === ROUTING_SERVICE) return handleCaptureSend(record, value);
    if (record.service === INPUT_SERVICE) return handleInputSend(record, value);
    if (record.service === AUDIO_SERVICE) return handleAudioSend(record, value);
    throw new Error("unexpected send on " + record.service + "/" + record.method);
  }

  function inputTryRecv() {
    if (inputQueue.length > 0) return inputQueue.shift();
    for (;;) {
      if (scenario.length === 0) {
        // No more scripted player actions: end the session at the next turn.
        if (phase === "input" || phase === "retry") return null;
        return undefined;
      }
      const step = scenario[0];
      if (typeof step.when === "function" && !step.when(stateView())) return undefined;
      scenario.shift();
      const produced =
        typeof step.events === "function" ? step.events(ctl) : step.events;
      if (produced === null) return null;
      // An action-only step (for example injecting a routing change) delivers
      // no input. Returning undefined lets the app's ownership pump run before
      // the next read instead of treating the end of the script as a stream end.
      if (produced === undefined) return undefined;
      const list = Array.isArray(produced) ? produced : [produced];
      for (const event of list) inputQueue.push(event);
      if (inputQueue.length > 0) return inputQueue.shift();
    }
  }

  function makeCall(record) {
    const call = {
      info: null,
      send(value) {
        record.sent.push(snapshot(value));
        dispatch(record, value);
        return call;
      },
      recv() {
        record.recvCalls += 1;
        if (record.events.length > 0) return record.events.shift();
        throw new Error(
          "test harness: blocking recv on " + record.service + "/" + record.method,
        );
      },
      cancel() {
        if (record.cancelled) return;
        record.cancelled = true;
        order.push(cancelLabel(record));
      },
      end() {},
      halfClose() {},
      status() {
        return { code: 0, detail: "" };
      },
      callInfo() {
        return {};
      },
    };
    if (options.hostTryRecv !== false) {
      call.tryRecv = function () {
        if (record.service === INPUT_SERVICE) return inputTryRecv();
        if (record.events.length > 0) return record.events.shift();
        return undefined;
      };
    }
    return call;
  }

  const capsulad = {
    WAIT_FOREVER: 0xffffffff,
    env: {
      socket: "/tmp/test.sock",
      runId: "run_test",
      appId: "org.capsulad.simon",
      sdk: "0.5",
      library: "",
      appDir,
    },
    start() {
      started = true;
    },
    run() {
      runCalls += 1;
    },
    stop() {
      stopped += 1;
      order.push("stop");
    },
    log(message) {
      const line = String(message);
      logs.push(line);
      if (line.includes("play round=")) phase = "play";
      else if (line.includes("await round=")) phase = "input";
      else if (line.includes("round complete")) phase = "break";
      else if (line.includes("game over")) phase = "error";
      else if (line.includes("final score")) phase = "retry";
      else if (line.includes("start")) phase = "ready";
    },
    on() {
      throw new Error("the game never registers bus handlers");
    },
    call(service, method, request) {
      unary.push({ service, method, request: snapshot(request) });
      if (service === DEVICE_SERVICE && method === "ListResources") {
        return { revision: 1, resources: resources.slice() };
      }
      if (service === AUDIO_SERVICE && method === "ListEndpoints") {
        return { endpoints: endpoints.slice() };
      }
      if (service === ROUTING_SERVICE && method === "ReleaseCapture") {
        const capture = captures.find((item) => item.captureId === request.captureId);
        if (capture) {
          capture.released = true;
          order.push("release:" + capture.resourceId);
          return { released: true };
        }
        order.push("release:unknown");
        return { released: false };
      }
      throw new Error("unexpected unary call " + service + "/" + method);
    },
    open(service, method, kind, timeout) {
      return makeCall(callRecord(service, method, kind, timeout));
    },
  };

  ctl = {
    nowNs,
    currentPhase() {
      return phase;
    },
    queueInput(events) {
      const list = Array.isArray(events) ? events : [events];
      for (const event of list) inputQueue.push(event);
    },
    preempt(resourceId) {
      const record = captureRecord(resourceId);
      if (!record) throw new Error("no capture record for " + resourceId);
      record.events.push({ changed: { selected: false, reason: "preempted" } });
    },
    endCapture(resourceId) {
      const record = captureRecord(resourceId);
      if (!record) throw new Error("no capture record for " + resourceId);
      record.events.push(null);
    },
    audioNotice(code) {
      const record = opened.find(
        (item) => item.service === AUDIO_SERVICE && !item.cancelled,
      );
      if (!record) throw new Error("no audio record");
      record.events.push({ notice: { code, detail: "test" } });
    },
    ringCapture() {
      return captureRecord(RING_RESOURCE);
    },
    panelCapture() {
      return captureRecord(PANEL_RESOURCE);
    },
  };

  const math = {};
  for (const key of Object.getOwnPropertyNames(Math)) math[key] = Math[key];
  math.random = random;

  const context = vm.createContext({
    capsulad,
    Date: { now: nowMs },
    Math: math,
  });
  new vm.Script(source, { filename: "main.js" }).runInContext(context);

  const audio = opened.find((record) => record.service === AUDIO_SERVICE);
  const ringPresentation = opened.find(
    (record) => record.service === PRESENTATION_SERVICE && record.resource === RING_RESOURCE,
  );

  return {
    logs,
    ringFrames,
    segmentFrames,
    opened,
    captures,
    unary,
    order,
    stopped,
    runCalls,
    started,
    nowNs,
    audio,
    ringPresentationId: ringPresentation && ringPresentation.presentationId,
  };
}

// --- scenario helpers ------------------------------------------------------

function press(key, ns) {
  return {
    resourceId: PANEL_RESOURCE,
    monotonicTimeNs: String(ns),
    key: { key, pressed: true },
  };
}

function release(key, ns) {
  return {
    resourceId: PANEL_RESOURCE,
    monotonicTimeNs: String(ns),
    key: { key, pressed: false },
  };
}

const whenLog = (text) => (view) => view.logs.some((line) => line.includes(text));

function pressKeys(keys, when) {
  return {
    when,
    events: (ctl) => {
      const events = [];
      for (const key of keys) {
        events.push(press(key, ctl.nowNs()));
        events.push(release(key, ctl.nowNs()));
      }
      return events;
    },
  };
}

function holdCenter(when, heldMs = 900) {
  return {
    when,
    events: (ctl) => {
      const down = ctl.nowNs();
      return [press("center", down), release("center", down + heldMs * 1000000)];
    },
  };
}

const atRound = (round) => whenLog("await round=" + round + " position=0");
const atFinalScore = whenLog("final score=");

// --- frame helpers ---------------------------------------------------------

function firstColoredFrame(harness) {
  const frame = harness.ringFrames.find((item) => isColored(item.pixels));
  assert.ok(frame, "expected a coloured ring frame");
  return frame;
}

function assertQuadrant(frame, expectedIndices, color) {
  const pixels = frame.pixels;
  const expected = new Set(expectedIndices);
  for (let index = 0; index < pixels.length; index += 1) {
    if (expected.has(index)) {
      assert.deepEqual(rgbAt(pixels, index), color, "pixel " + index + " colour");
    } else {
      assert.deepEqual(rgbAt(pixels, index), [0, 0, 0], "pixel " + index + " dark");
    }
  }
  assert.deepEqual(
    litIndices(pixels).sort((a, b) => a - b),
    expectedIndices.slice().sort((a, b) => a - b),
    "lit pixel set",
  );
}

function scoreFrame(value, colon, dots = [false, false, false, false]) {
  const text = String(value).padStart(4, "0").slice(-4);
  return {
    masks: [...text].map((ch) => DIGIT[Number(ch)]),
    dots,
    colon,
  };
}

function hasScoreFrame(harness, value, colon, dots) {
  const want = scoreFrame(value, colon, dots);
  return harness.segmentFrames.some(
    (frame) =>
      frame.masks.join(",") === want.masks.join(",") &&
      frame.colon === want.colon &&
      frame.dots.join(",") === want.dots.join(","),
  );
}

function decodePcm(base64) {
  return Buffer.from(base64, "base64");
}

function estimateFrequency(pcm) {
  const rate = 24000;
  const samples = pcm.length / 2;
  let last = 0;
  let crossings = 0;
  for (let offset = 0; offset + 1 < pcm.length; offset += 2) {
    const value = pcm.readInt16LE(offset);
    if (value > 800) {
      if (last < 0) crossings += 1;
      last = 1;
    } else if (value < -800) {
      if (last > 0) crossings += 1;
      last = -1;
    }
  }
  return crossings / (2 * (samples / rate));
}

// --- manifest and startup --------------------------------------------------

test("manifest is a classic QuickJS script describing the colour game", () => {
  assert.match(manifest, /^id = "org\.capsulad\.simon"$/m);
  assert.match(manifest, /^\[entrypoint\]$/m);
  assert.match(manifest, /^runtime = "quickjs"$/m);
  assert.match(manifest, /^path = "main\.js"$/m);
  assert.doesNotMatch(manifest, /^architecture =/m);
  assert.doesNotMatch(manifest, /^abi =/m);
  assert.doesNotMatch(manifest, /^\[\[settings\]\]$/m);
  assert.doesNotMatch(manifest, /^\[\[actions\]\]$/m);
  assert.match(manifest, /colour-and-tone/i);
});

test("startup owns ring, score, panel and speaker through documented streams", () => {
  const h = runApp({ scenario: [pressKeys(["mic_toggle"], atRound(1))] });
  const services = h.opened.map((record) => record.service + "/" + record.method);
  assert.deepEqual(services, [
    PRESENTATION_SERVICE + "/Open",
    ROUTING_SERVICE + "/Capture",
    PRESENTATION_SERVICE + "/Open",
    ROUTING_SERVICE + "/Capture",
    ROUTING_SERVICE + "/Capture",
    AUDIO_SERVICE + "/Playback",
    INPUT_SERVICE + "/WatchInput",
  ]);

  for (const record of h.opened) {
    assert.equal(
      record.timeout,
      0,
      record.service + "/" + record.method + " must be deadline-free",
    );
  }
  assert.equal(h.opened[0].kind, 3); // bidi presentation
  assert.equal(h.opened[1].kind, 1); // server-stream captures
  assert.equal(h.opened[2].kind, 3);
  assert.equal(h.opened[3].kind, 1);
  assert.equal(h.opened[4].kind, 1);
  assert.equal(h.opened[5].kind, 3); // bidi playback
  assert.equal(h.opened[6].kind, 1); // server-stream input

  assert.deepEqual(h.opened[0].sent[0], { open: { resourceId: RING_RESOURCE } });
  // The ring frame is content, so the capture can be eligible immediately.
  assert.deepEqual(h.opened[0].sent[1], {
    frame: { ring: { pixels: Array.from({ length: 12 }, () => ({ red: 0, green: 0, blue: 0 })) } },
  });
  assert.equal(h.opened[1].sent[0].resourceId, RING_RESOURCE);
  assert.equal(h.opened[1].sent[0].presentationId, h.ringPresentationId);
  assert.equal(h.opened[1].sent[0].touchMode, undefined);

  assert.deepEqual(h.opened[2].sent[0], { open: { resourceId: SEGMENT_RESOURCE } });
  assert.equal(h.opened[3].sent[0].resourceId, SEGMENT_RESOURCE);
  assert.equal(h.opened[3].sent[0].presentationId, h.opened[2].presentationId);

  assert.equal(h.opened[4].sent[0].resourceId, PANEL_RESOURCE);
  assert.equal(h.opened[4].sent[0].touchMode, "TOUCH_FULL_RAW");
  assert.equal(h.opened[4].sent[0].presentationId, undefined);
  assert.deepEqual(h.opened[6].sent[0], {
    resourceId: PANEL_RESOURCE,
    captureId: h.captures.find((c) => c.resourceId === PANEL_RESOURCE).captureId,
  });

  assert.deepEqual(h.opened[5].sent[0], {
    open: {
      endpointId: SPEAKER_ENDPOINT,
      format: { sampleFormat: "PCM_S16LE", sampleRateHz: 24000, channels: 1 },
    },
  });

  // The idle score is the first display content and the first valid frame.
  assert.ok(hasScoreFrame(h, 0, true), "idle score 0000 with the turn colon");
  assert.ok(h.logs.some((line) => line.includes("controls: repeat each colour")));
  assert.equal(h.started, true);
  assert.equal(h.runCalls, 0, "the game loop replaces the AppBus pump");
});

// --- ring geometry ---------------------------------------------------------

test("a 12-pixel ring centres north red on pixel 0 with clockwise neighbours", () => {
  const h = runApp({
    random: () => 0.01,
    scenario: [pressKeys(["mic_toggle"], atRound(1))],
  });
  const frame = firstColoredFrame(h);
  assert.equal(frame.pixels.length, 12);
  assertQuadrant(frame, [0, 1, 11], RED);
});

test("a 16-pixel ring still centres each side on its cardinal direction", () => {
  const h = runApp({
    random: () => 0.26, // east / yellow
    resources: defaultResources({ ring: { ring: { pixelCount: 16, supportsRgb: true } } }),
    scenario: [pressKeys(["volume_up"], atRound(1))],
  });
  const frame = firstColoredFrame(h);
  assert.equal(frame.pixels.length, 16);
  assertQuadrant(frame, [2, 3, 4, 5], YELLOW);
});

test("a 10-pixel ring keeps every side non-empty and angularly centred", () => {
  const h = runApp({
    random: () => 0.01, // north / red
    resources: defaultResources({ ring: { ring: { pixelCount: 10, supportsRgb: true } } }),
    scenario: [pressKeys(["mic_toggle"], atRound(1))],
  });
  const frame = firstColoredFrame(h);
  assert.equal(frame.pixels.length, 10);
  assertQuadrant(frame, [0, 1, 9], RED);
});

// --- colour and tone pairing ----------------------------------------------

const PAIRS = [
  [0.01, "mic_toggle", RED, [0, 1, 11], FREQUENCIES[0]],
  [0.26, "volume_up", YELLOW, [2, 3, 4], FREQUENCIES[1]],
  [0.51, "play_pause", GREEN, [5, 6, 7], FREQUENCIES[2]],
  [0.76, "volume_down", BLUE, [8, 9, 10], FREQUENCIES[3]],
];

for (const [randomValue, key, color, pixels, frequency] of PAIRS) {
  test(`the ${key} side maps to ${color.join(",")} and ${frequency} Hz`, () => {
    const h = runApp({
      random: () => randomValue,
      scenario: [pressKeys([key], atRound(1))],
    });
    const frame = h.ringFrames.find(
      (item) => item.pixels.some((pixel) => pixel.red !== pixel.green || pixel.green !== pixel.blue),
    );
    assert.ok(frame, "expected the sequence flash");
    assertQuadrant(frame, pixels, color);

    assert.ok(h.audio, "the playback stream was opened");
    assert.ok(h.audio.chunks.length > 0, "the sequence tone was written");
    const measured = estimateFrequency(decodePcm(h.audio.chunks[0]));
    assert.ok(
      Math.abs(measured - frequency) < 20,
      `expected about ${frequency} Hz, measured ${measured.toFixed(1)} Hz`,
    );
    assert.ok(
      h.logs.some((line) => line.includes("round complete score=1")),
      `${key} must be the matching side for this colour`,
    );
  });
}

test("the four colour tones and the error tone are distinct", () => {
  const tones = new Set(FREQUENCIES);
  assert.equal(tones.size, 4);
  const h = runApp({
    random: () => 0.01,
    scenario: [
      pressKeys(["volume_down"], atRound(1)), // wrong: error cue
    ],
  });
  assert.ok(h.audio.chunks.length >= 2, "expected a colour tone and an error tone");
  const error = h.audio.chunks[h.audio.chunks.length - 1];
  const errorFrequency = estimateFrequency(decodePcm(error));
  for (const frequency of FREQUENCIES) {
    assert.ok(
      Math.abs(errorFrequency - frequency) > 40,
      `error tone at ${errorFrequency.toFixed(1)} Hz must not be a colour tone`,
    );
  }
});

// --- gameplay --------------------------------------------------------------

test("a correct repeat grows the sequence and the score", () => {
  const values = [0.01, 0.26]; // round one: 1; round two adds 2
  let index = 0;
  const h = runApp({
    random: () => values[Math.min(index++, values.length - 1)],
    scenario: [
      pressKeys(["mic_toggle"], atRound(1)),
      pressKeys(["mic_toggle", "volume_up"], atRound(2)),
    ],
  });
  assert.ok(h.logs.some((line) => line.includes("round=2 sequence=12 score=1")));
  assert.ok(h.logs.some((line) => line.includes("accept symbol=1 position=1 score=2")));
  assert.ok(h.logs.some((line) => line.includes("accept symbol=2 position=2 score=3")));
  assert.ok(h.logs.some((line) => line.includes("round complete score=3")));
  assert.ok(hasScoreFrame(h, 3, true), "the score frame reached 0003");
  assert.ok(h.segmentFrames.some((frame) => frame.dots.every((dot) => dot)), "round dots");

  // The round-complete celebration lights all four arcs, proving the pixel
  // partition covers the ring once and each side keeps its cardinal colour.
  const celebration = h.ringFrames.find((frame) =>
    frame.pixels.every((pixel) => pixel.red || pixel.green || pixel.blue),
  );
  assert.ok(celebration, "expected the round-complete ring frame");
  assert.deepEqual(rgbAt(celebration.pixels, 0), RED);
  assert.deepEqual(rgbAt(celebration.pixels, 3), YELLOW);
  assert.deepEqual(rgbAt(celebration.pixels, 6), GREEN);
  assert.deepEqual(rgbAt(celebration.pixels, 9), BLUE);

  const roundTwo = h.logs.findIndex((line) => line.includes("play round=2 symbols=2"));
  assert.ok(roundTwo >= 0);
  const replay = h.ringFrames.filter(
    (frame) => frame.logsAt > roundTwo && isColored(frame.pixels),
  );
  assert.deepEqual(rgbAt(replay[0].pixels, 0), RED, "round two replays red first");
  assert.deepEqual(rgbAt(replay[1].pixels, 2), YELLOW, "then yellow");
});

test("a side key pressed during playback is ignored", () => {
  let injected = false;
  const h = runApp({
    random: () => 0.01,
    scenario: [pressKeys(["mic_toggle"], atRound(1))],
    onRingFrame: (pixels, ctl) => {
      if (injected || !isColored(pixels)) return;
      injected = true;
      ctl.queueInput([press("volume_down", ctl.nowNs()), release("volume_down", ctl.nowNs())]);
    },
  });
  assert.ok(!h.logs.some((line) => line.includes("game over")));
  assert.ok(h.logs.some((line) => line.includes("await round=1")));
  assert.ok(h.logs.some((line) => line.includes("round complete score=1")));
  assert.ok(h.logs.some((line) => line.includes("accept symbol=1")));
});

test("a wrong side key shows error feedback, the final score and retries", () => {
  let injected = false;
  const h = runApp({
    random: () => 0.01,
    scenario: [
      pressKeys(["volume_down"], atRound(1)), // symbol 4, expected 1
      pressKeys(["mic_toggle"], atFinalScore),
    ],
    onRingFrame: (pixels, ctl) => {
      const white = pixels.every(
        (pixel) => pixel.red === 180 && pixel.green === 180 && pixel.blue === 180,
      );
      if (injected || !white) return;
      injected = true;
      // A press during the error display must not start a nested game.
      ctl.queueInput([press("mic_toggle", ctl.nowNs()), release("mic_toggle", ctl.nowNs())]);
    },
  });

  assert.ok(
    h.logs.some((line) =>
      line.includes("game over score=0 round=1 expected=1 pressed=4"),
    ),
  );
  assert.ok(h.logs.some((line) => line.includes("final score=0")));
  assert.ok(
    h.segmentFrames.some((frame) => frame.masks.every((mask) => mask === DASH)),
    "the error display uses four dashes",
  );
  assert.ok(h.ringFrames.some((frame) => frame.pixels.every((p) => p.red === 180)), "white error");
  assert.ok(hasScoreFrame(h, 0, false), "the final score keeps the turn colon off");
  assert.equal(
    h.logs.filter((line) => line.includes("simon: start")).length,
    2,
    "the error-display press is ignored; the final-score press starts one retry",
  );
});

test("holding the centre exits and releases tone, input, captures and output", () => {
  const h = runApp({ scenario: [holdCenter(whenLog("start"), 900)] });
  assert.ok(h.logs.some((line) => line.includes("exit reason=centre held 900ms")));
  assert.equal(h.stopped, 1);
  assert.deepEqual(h.order, [
    "speaker-cancel",
    "input-cancel",
    "release:touch.panel.main",
    "capture-cancel:touch.panel.main",
    "release:display.segment.front",
    "capture-cancel:display.segment.front",
    "release:light.ring.main",
    "capture-cancel:light.ring.main",
    "close:display.segment.front",
    "presentation-cancel:display.segment.front",
    "close:light.ring.main",
    "presentation-cancel:light.ring.main",
    "stop",
  ]);
  assert.ok(
    h.segmentFrames.some((frame) => frame.masks.every((mask) => mask === DASH)),
    "the centre hold is shown on the display",
  );
});

test("a centre tap during playback pauses the replay and resumes it", () => {
  let injected = false;
  const h = runApp({
    random: () => 0.01,
    scenario: [pressKeys(["mic_toggle"], atRound(1))],
    onRingFrame: (pixels, ctl) => {
      if (injected || !isColored(pixels)) return;
      injected = true;
      const down = ctl.nowNs();
      ctl.queueInput([press("center", down), release("center", down + 300000000)]);
    },
  });
  assert.ok(!h.logs.some((line) => line.includes("centre held")));
  assert.ok(h.logs.some((line) => line.includes("await round=1")));
  assert.ok(h.logs.some((line) => line.includes("round complete score=1")));
  assert.ok(
    h.ringFrames.some((frame) => frame.pixels.every((p) => p.red === 35)),
    "the hold hint is visible on the ring",
  );
});

// --- ownership failures ----------------------------------------------------

test("ring capture preemption ends the run and returns the baseline", () => {
  const h = runApp({
    scenario: [
      {
        when: atRound(1),
        events: (ctl) => {
          ctl.preempt(RING_RESOURCE);
          return undefined;
        },
      },
    ],
  });
  assert.ok(h.logs.some((line) => line.includes("exit reason=ring capture preempted")));
  assert.ok(h.order.includes("release:light.ring.main"));
  assert.ok(h.order.includes("close:light.ring.main"));
  assert.equal(h.order[h.order.length - 1], "stop");
});

test("a panel capture stream ending ends the run", () => {
  const h = runApp({
    scenario: [
      {
        when: atRound(1),
        events: (ctl) => {
          ctl.endCapture(PANEL_RESOURCE);
          return undefined;
        },
      },
    ],
  });
  assert.ok(h.logs.some((line) => line.includes("exit reason=panel capture ended")));
  assert.ok(h.order.includes("release:touch.panel.main"));
});

test("a speaker failure ends the run instead of playing silently", () => {
  const h = runApp({
    scenario: [
      {
        when: atRound(1),
        events: (ctl) => {
          ctl.audioNotice("APP_EXITED");
          return undefined;
        },
      },
    ],
  });
  assert.ok(h.logs.some((line) => line.includes("exit reason=speaker stream APP_EXITED")));
  assert.ok(h.order.includes("speaker-cancel"));
});

// --- display and degradation ----------------------------------------------

test("without a four-digit display the ring and tones remain a playable game", () => {
  const h = runApp({
    resources: defaultResources().filter((r) => r.resourceId !== SEGMENT_RESOURCE),
    random: () => 0.01,
    scenario: [pressKeys(["mic_toggle"], atRound(1))],
  });
  assert.ok(h.logs.some((line) => line.includes("no score display")));
  assert.equal(
    h.opened.filter((r) => r.service === PRESENTATION_SERVICE).length,
    1,
    "only the ring presentation opens",
  );
  assert.equal(h.segmentFrames.length, 0);
  assert.ok(h.logs.some((line) => line.includes("round complete score=1")));
  assert.ok(h.audio.chunks.length > 0, "the colour tones still play");
  assert.equal(firstColoredFrame(h).pixels.length, 12);
});

test("a refused score capture degrades to the no-display game", () => {
  const h = runApp({
    refusedCaptures: [SEGMENT_RESOURCE],
    random: () => 0.01,
    scenario: [pressKeys(["mic_toggle"], atRound(1))],
  });
  assert.ok(
    h.logs.some((line) => line.includes("score display capture refused; continuing without it")),
  );
  assert.ok(h.order.includes("close:display.segment.front"));
  assert.ok(h.logs.some((line) => line.includes("round complete score=1")));
  assert.ok(!h.logs.some((line) => line.includes("unsupported")));
});

test("a display with fewer than four digits is not used", () => {
  const h = runApp({
    resources: defaultResources({
      segment: { segment: { digitCount: 2, supportsColon: true, supportsDecimalPoints: true } },
    }),
    random: () => 0.01,
    scenario: [pressKeys(["mic_toggle"], atRound(1))],
  });
  assert.ok(
    h.logs.some((line) => line.includes("score display has 2 digits; running without a score display")),
  );
  assert.equal(
    h.opened.filter((r) => r.service === PRESENTATION_SERVICE).length,
    1,
    "the too-small display is never opened",
  );
  assert.equal(h.segmentFrames.length, 0);
  assert.ok(h.logs.some((line) => line.includes("round complete score=1")));
});

test("omits colon and decimal points when the display lacks them", () => {
  const h = runApp({
    resources: defaultResources({
      segment: {
        segment: { digitCount: 4, supportsColon: false, supportsDecimalPoints: false },
      },
    }),
    random: () => 0.01,
    scenario: [pressKeys(["mic_toggle"], atRound(1))],
  });
  assert.ok(h.segmentFrames.length > 0);
  assert.ok(h.segmentFrames.every((frame) => frame.colon === false), "no colon");
  assert.ok(
    h.segmentFrames.every((frame) => frame.dots.every((dot) => dot === false)),
    "no decimal points",
  );
  assert.ok(h.logs.some((line) => line.includes("round complete score=1")));
});

// --- unsupported devices and setup failures --------------------------------

test("refuses a device without an LED ring", () => {
  const h = runApp({
    resources: defaultResources().filter((r) => r.resourceId !== RING_RESOURCE),
  });
  assert.equal(h.opened.length, 0);
  assert.ok(h.logs.some((line) => line.includes("unsupported: no light.ring.main")));
  assert.equal(h.stopped, 1);
});

test("refuses a ring with fewer than four pixels", () => {
  const h = runApp({
    resources: defaultResources({ ring: { ring: { pixelCount: 2, supportsRgb: true } } }),
  });
  assert.equal(h.opened.length, 0);
  assert.ok(
    h.logs.some((line) =>
      line.includes("unsupported: light.ring.main has 2 pixels; Simon needs at least 4"),
    ),
  );
});

test("refuses a monochrome ring", () => {
  const h = runApp({
    resources: defaultResources({ ring: { ring: { pixelCount: 12, supportsRgb: false } } }),
  });
  assert.equal(h.opened.length, 0);
  assert.ok(
    h.logs.some((line) =>
      line.includes("unsupported: light.ring.main does not support RGB colour"),
    ),
  );
});

test("refuses a panel without a full-panel capture", () => {
  const h = runApp({
    resources: defaultResources({ panel: { touch: { fullPanelAvailable: false } } }),
  });
  assert.equal(h.opened.length, 0);
  assert.ok(
    h.logs.some((line) =>
      line.includes("unsupported: touch.panel.main does not advertise a full-panel capture"),
    ),
  );
});

test("refuses a device without a speaker endpoint", () => {
  const h = runApp({
    endpoints: defaultEndpoints().filter((e) => e.endpointId !== SPEAKER_ENDPOINT),
  });
  assert.equal(h.opened.length, 0);
  assert.ok(h.logs.some((line) => line.includes("unsupported: no audio.output.speaker")));
});

test("a refused ring capture releases the presentation", () => {
  const h = runApp({ refusedCaptures: [RING_RESOURCE] });
  assert.ok(h.logs.some((line) => line.includes("unavailable: the ring capture was refused")));
  assert.deepEqual(h.order, [
    "capture-cancel:light.ring.main",
    "close:light.ring.main",
    "presentation-cancel:light.ring.main",
    "stop",
  ]);
});

test("a refused ring presentation stops before opening any capture", () => {
  const h = runApp({ refusedPresentations: [RING_RESOURCE] });
  assert.ok(h.logs.some((line) => line.includes("unavailable: the ring presentation did not open")));
  assert.deepEqual(h.order, [
    "presentation-cancel:light.ring.main",
    "stop",
  ]);
});

test("a failed speaker open releases the panel, display and ring", () => {
  const h = runApp({ refuseAudio: true });
  assert.ok(
    h.logs.some((line) => line.includes("unavailable: the speaker playback stream did not open")),
  );
  assert.deepEqual(h.order, [
    "speaker-cancel",
    "release:touch.panel.main",
    "capture-cancel:touch.panel.main",
    "release:display.segment.front",
    "capture-cancel:display.segment.front",
    "release:light.ring.main",
    "capture-cancel:light.ring.main",
    "close:display.segment.front",
    "presentation-cancel:display.segment.front",
    "close:light.ring.main",
    "presentation-cancel:light.ring.main",
    "stop",
  ]);
});

test("an older host without Call.tryRecv fails clearly and closes the presentation", () => {
  const h = runApp({ hostTryRecv: false });
  assert.ok(
    h.logs.some((line) =>
      line.includes("unavailable: this QuickJS host has no Call.tryRecv"),
    ),
  );
  assert.equal(h.stopped, 1);
  assert.deepEqual(h.order, [
    "close:light.ring.main",
    "presentation-cancel:light.ring.main",
    "stop",
  ]);
});