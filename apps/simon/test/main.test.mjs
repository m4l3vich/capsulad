// Contract tests for the Simon Says QuickJS app.
//
// The app is exercised as the real host loads it: one classic script whose only
// globals are `capsulad`, `Date` and `Math`. The fake implements exactly the
// documented QuickJS host surface (blocking recv, ordered JSON frames, unary
// calls, WAIT_FOREVER). It is not a second product path: nothing here is loaded
// by the packaged app, and every assertion is about the app's observable
// requests, frames and terminal cleanup.
//
// Run: node --test apps/simon/test/*.test.mjs

import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import vm from "node:vm";

const here = dirname(fileURLToPath(import.meta.url));
const appDir = join(here, "..");
const source = readFileSync(join(appDir, "main.js"), "utf8");
const manifest = readFileSync(join(appDir, "manifest.toml"), "utf8");

const DEVICE_SERVICE = "capsulad.framework.v1.DeviceService";
const PRESENTATION_SERVICE = "capsulad.framework.v1.PresentationService";
const ROUTING_SERVICE = "capsulad.framework.v1.RoutingService";
const INPUT_SERVICE = "capsulad.framework.v1.InputService";
const SEGMENT_RESOURCE = "display.segment.front";
const PANEL_RESOURCE = "touch.panel.main";

const DIGIT = [0x3f, 0x06, 0x5b, 0x4f, 0x66, 0x6d, 0x7d, 0x07, 0x7f, 0x6f];
const DASH = 0x40;

function panelResources(overrides = {}) {
  return [
    {
      resourceId: SEGMENT_RESOURCE,
      available: true,
      segment: { digitCount: 4, supportsColon: true, supportsDecimalPoints: true },
      ...(overrides.segment || {}),
    },
    {
      resourceId: PANEL_RESOURCE,
      available: true,
      touch: { fullPanelAvailable: true, partialGesturesAvailable: true },
      ...(overrides.panel || {}),
    },
  ];
}

const frameOf = ({ masks, dots, colon }) => ({ masks, dots, colon });

function scoreFrame(value, colon, dots = [false, false, false, false]) {
  const text = String(value);
  const padded = text.padStart(4, "0").slice(-4);
  return {
    masks: [...padded].map((ch) => DIGIT[Number(ch)]),
    dots,
    colon,
  };
}

/**
 * Runs main.js against a fake host.
 *
 * options.scenario: array of step callbacks. Each step is invoked when the app
 * blocks for panel input with no queued event, and returns an InputEvent, an
 * array of them, or null to end the stream. This models the real host: the app
 * is only ever between recv calls, so playback cannot be interrupted.
 */
const snapshot = (value) => JSON.parse(JSON.stringify(value));

function runApp(options = {}) {
  const resources = options.resources || panelResources();
  const scenario = options.scenario ? options.scenario.slice() : [];
  const random = options.random ?? (() => 0);

  const logs = [];
  const frames = [];
  const opened = [];
  const captures = [];
  const unary = [];
  const order = [];
  let stopped = 0;
  let runCalls = 0;
  let started = false;
  let virtualMs = 5000;
  let scenarioIndex = 0;
  const inputQueue = [];
  let captureSeq = 0;

  const nowMs = () => {
    virtualMs += 5;
    return virtualMs;
  };
  const nowNs = () => virtualMs * 1000000;

  function callRecord(service, method, kind, timeout) {
    const record = {
      service,
      method,
      kind,
      timeout,
      sent: [],
      events: [],
      cancelled: false,
      recvCalls: 0,
    };
    opened.push(record);
    return record;
  }

  function grantCapture(record, request) {
    captureSeq += 1;
    const id = "cap_" + captureSeq;
    const capture = {
      captureId: id,
      resourceId: request.resourceId,
      appId: "org.capsulad.simon",
      runId: "run_test",
      presentationId: request.presentationId || "",
      acquiredMonotonicNs: String(nowNs()),
      selected: true,
      ready: true,
      touchMode: request.touchMode || "TOUCH_CAPTURE_MODE_UNSPECIFIED",
    };
    captures.push(capture);
    record.events.push({ granted: { capture } });
    record.events.push({ changed: { selected: true, reason: "granted" } });
  }

  function handleSend(record, value) {
    if (record.service === PRESENTATION_SERVICE) {
      if (value.open) {
        record.events.push({
          ready: {
            presentationId: "prs_test",
            resourceId: value.open.resourceId,
            hasValidFrame: false,
          },
        });
        return;
      }
      if (value.frame) {
        const segment = value.frame.segment;
        if (!segment) throw new Error("unexpected non-segment frame " + JSON.stringify(value));
        frames.push({
          masks: Array.from(segment.cells, (cell) => cell.mask || 0),
          dots: Array.from(segment.cells, (cell) => !!cell.decimalPoint),
          colon: !!segment.colon,
          atMs: virtualMs,
          logsAt: logs.length,
        });
        // A frame can inject panel input, modelling a press made while the
        // replay is on screen. The events are queued for the app's tryRecv.
        if (options.onFrame) {
          const produced = options.onFrame(frames[frames.length - 1], frames, nowNs);
          if (produced !== undefined && produced !== null) {
            const list = Array.isArray(produced) ? produced : [produced];
            for (const event of list) inputQueue.push(event);
          }
        }
        return;
      }
      if (value.close) {
        order.push("presentation-close");
        record.events.push(null);
        return;
      }
      throw new Error("unexpected presentation request " + JSON.stringify(value));
    }
    if (record.service === ROUTING_SERVICE) {
      grantCapture(record, value);
      return;
    }
    if (record.service === INPUT_SERVICE) {
      if (value.captureId !== captures[1]?.captureId) {
        throw new Error("WatchInput must reference the selected panel capture");
      }
      return;
    }
    throw new Error("unexpected send on " + record.service + "/" + record.method);
  }

  function makeCall(record) {
    return {
      info: null,
      send(value) {
        record.sent.push(snapshot(value));
        handleSend(record, value);
      },
      // Bounded non-blocking read: queued input only; never pulls a scenario
      // step, so a scripted "after the replay" press is not consumed early.
      ...(options.hostTryRecv === false && record.service === INPUT_SERVICE
        ? {}
        : {
            tryRecv() {
              if (record.service !== INPUT_SERVICE) return undefined;
              if (inputQueue.length > 0) return inputQueue.shift();
              return undefined;
            },
          }),
      recv() {
        record.recvCalls += 1;
        if (record.events.length > 0) return record.events.shift();
        if (record.service !== INPUT_SERVICE) {
          throw new Error("test harness: blocking recv on " + record.service + "/" + record.method);
        }
        if (inputQueue.length > 0) return inputQueue.shift();
        while (scenarioIndex < scenario.length) {
          const step = scenario[scenarioIndex];
          scenarioIndex += 1;
          const produced = step({ nowNs, frames, logs, captures });
          if (produced === null || produced === undefined) return null;
          const list = Array.isArray(produced) ? produced : [produced];
          for (const event of list) inputQueue.push(event);
          if (inputQueue.length > 0) return inputQueue.shift();
        }
        return null;
      },
      cancel() {
        if (record.cancelled) return;
        record.cancelled = true;
        if (record.service === INPUT_SERVICE) order.push("input-cancel");
        else if (record.service === ROUTING_SERVICE) {
          order.push("capture-cancel:" + record.sent[0].resourceId);
        } else order.push("presentation-cancel");
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
  }

  const capsulad = {
    WAIT_FOREVER: 0xffffffff,
    env: {
      socket: "/tmp/test.sock",
      runId: "run_test",
      appId: "org.capsulad.simon",
      sdk: "0.5",
      library: "",
      appDir: appDir,
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
      logs.push(String(message));
    },
    on() {
      throw new Error("the game never registers bus handlers");
    },
    call(service, method, request) {
      unary.push({ service, method, request });
      if (service === DEVICE_SERVICE && method === "ListResources") {
        return { revision: 1, resources: resources.slice() };
      }
      if (service === ROUTING_SERVICE && method === "ReleaseCapture") {
        return { released: true };
      }
      throw new Error("unexpected unary call " + service + "/" + method);
    },
    open(service, method, kind, timeout) {
      const record = callRecord(service, method, kind, timeout);
      return makeCall(record);
    },
  };

  const context = vm.createContext({
    capsulad,
    Date: { now: nowMs },
    Math: { floor: Math.floor, round: Math.round, min: Math.min, max: Math.max, random },
  });
  new vm.Script(source, { filename: "main.js" }).runInContext(context);

  return {
    logs,
    frames,
    opened,
    captures,
    unary,
    order,
    stopped,
    runCalls,
    started,
    nowNs,
  };
}

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

function keyPress(key) {
  return ({ nowNs }) => [press(key, nowNs()), release(key, nowNs())];
}

function assertScoreFrame(h, value, colon, dots) {
  const want = scoreFrame(value, colon, dots);
  const found = h.frames.filter(
    (f) =>
      f.masks.join(",") === want.masks.join(",") &&
      f.colon === want.colon &&
      f.dots.join(",") === want.dots.join(","),
  );
  assert.ok(
    found.length > 0,
    `expected score frame ${value} colon=${colon}; got ${JSON.stringify(h.frames)}`,
  );
}

// --- manifest and startup --------------------------------------------------

test("manifest declares a classic QuickJS script with no settings or actions", () => {
  assert.match(manifest, /^id = "org\.capsulad\.simon"$/m);
  assert.match(manifest, /^\[entrypoint\]$/m);
  assert.match(manifest, /^runtime = "quickjs"$/m);
  assert.match(manifest, /^path = "main\.js"$/m);
  assert.doesNotMatch(manifest, /^architecture =/m);
  assert.doesNotMatch(manifest, /^abi =/m);
  assert.doesNotMatch(manifest, /^\[\[settings\]\]$/m);
  assert.doesNotMatch(manifest, /^\[\[actions\]\]$/m);
});

test("startup captures the segment presentation and the whole panel", () => {
  const h = runApp();
  const services = h.opened.map((record) => record.service + "/" + record.method);
  assert.deepEqual(services, [
    PRESENTATION_SERVICE + "/Open",
    ROUTING_SERVICE + "/Capture",
    ROUTING_SERVICE + "/Capture",
    INPUT_SERVICE + "/WatchInput",
  ]);

  assert.deepEqual(h.opened[0].sent[0], { open: { resourceId: SEGMENT_RESOURCE } });
  // Live streams must not carry a whole-RPC deadline.
  for (const record of h.opened) {
    assert.equal(record.timeout, 0, record.service + "/" + record.method + " must be deadline-free");
  }
  assert.equal(h.opened[0].kind, 3); // bidi presentation
  assert.equal(h.opened[1].kind, 1); // server-stream captures
  assert.equal(h.opened[2].kind, 1);
  assert.equal(h.opened[3].kind, 1); // server-stream input subscription

  const segmentRequest = h.opened[1].sent[0];
  assert.equal(segmentRequest.resourceId, SEGMENT_RESOURCE);
  assert.equal(segmentRequest.presentationId, "prs_test");
  assert.equal(segmentRequest.touchMode, undefined);

  const panelRequest = h.opened[2].sent[0];
  assert.equal(panelRequest.resourceId, PANEL_RESOURCE);
  assert.equal(panelRequest.touchMode, "TOUCH_FULL_RAW");
  assert.equal(panelRequest.presentationId, undefined);

  assert.deepEqual(h.opened[3].sent[0], {
    resourceId: PANEL_RESOURCE,
    captureId: h.captures[1].captureId,
  });

  // The first content is the idle score, so the capture is eligible at once.
  assert.deepEqual(frameOf(h.frames[0]), scoreFrame(0, true));
  assert.ok(h.logs.some((line) => line.includes("presentation=prs_test")));
  assert.ok(h.logs.some((line) => line.includes("controls: side keys play, hold the centre to exit")));
  assert.equal(h.started, true);
  assert.equal(h.runCalls, 0, "the game loop replaces the AppBus pump");
});

test("the idle score flashes a captured segment before the first round", () => {
  const h = runApp();
  assert.deepEqual(frameOf(h.frames[0]), scoreFrame(0, true));
  assert.deepEqual(frameOf(h.frames[1]), scoreFrame(0, true));
  assert.deepEqual(frameOf(h.frames[2]), { masks: [DIGIT[1], 0, 0, 0], dots: [false, false, false, false], colon: false });
  assert.ok(h.logs.some((line) => line.includes("round=1 sequence=1")));
});

// --- gameplay --------------------------------------------------------------

for (const [key, symbol] of [
  ["mic_toggle", 1],
  ["volume_up", 2],
  ["play_pause", 3],
  ["volume_down", 4],
]) {
  test(`the ${key} side key is symbol ${symbol}`, () => {
    const h = runApp({
      random: () => (symbol - 1) / 4 + 0.01,
      scenario: [keyPress(key), () => null],
    });
    // Round one flashes the symbol, then the press completes it.
    const flash = h.frames.find((f) => f.masks[0] === DIGIT[symbol] && !f.colon);
    assert.ok(flash, `expected a flash of digit ${symbol}: ${JSON.stringify(h.frames)}`);
    assertScoreFrame(h, 1, true, [true, true, true, true]);
    assert.ok(h.logs.some((line) => line.includes("round complete score=1")));
  });
}

test("the full growing sequence is replayed every round", () => {
  // Ten correct presses reach round five, whose sequence has five symbols.
  const presses = [];
  for (let i = 0; i < 10; i += 1) presses.push(keyPress("mic_toggle"));
  const h = runApp({ scenario: [...presses, () => null] });

  const roundFive = h.logs.findIndex((line) => line.includes("round=5 sequence=11111 score=10"));
  assert.ok(roundFive >= 0, "expected round five: " + JSON.stringify(h.logs));
  assert.ok(h.logs.some((line) => line.includes("play round=5 symbols=5")));

  const replayFrames = h.frames.filter((f) => f.logsAt > roundFive);
  const flashes = replayFrames.filter((f) => !f.colon && f.masks[0] !== 0);
  assert.equal(flashes.length, 5, "round five replays all five symbols");
  const spanMs = flashes[flashes.length - 1].atMs - flashes[0].atMs;
  assert.ok(spanMs >= 1700, "the 320/140 ms replay cadence is intact: " + spanMs + "ms");
});

test("a correct symbol advances the score and starts the next round", () => {
  const h = runApp({ scenario: [keyPress("mic_toggle"), () => null] });
  assert.ok(h.logs.some((line) => line.includes("round=2 sequence=11")));
  assertScoreFrame(h, 1, true, [false, false, false, false]);
  // Round two replays both symbols.
  const flashes = h.frames.filter((f) => f.masks[0] === DIGIT[1] && !f.colon);
  assert.ok(flashes.length >= 3, "one flash for round one and two for round two");
});

test("a wrong side key shows dashes, the final score and restarts on a side key", () => {
  let injected = false;
  const h = runApp({
    scenario: [
      ({ nowNs }) => press("volume_up", nowNs()),
      ({ nowNs }) => [press("mic_toggle", nowNs()), release("mic_toggle", nowNs())],
      () => null,
    ],
    onFrame: (frame, frames, nowNs) => {
      // A side key during the error display must not nest a new game.
      if (injected || frame.colon || !frame.masks.every((mask) => mask === DASH)) return undefined;
      injected = true;
      return [press("mic_toggle", nowNs()), release("mic_toggle", nowNs())];
    },
  });
  const dash = h.frames.find((f) => f.masks.every((mask) => mask === DASH) && !f.colon);
  assert.ok(dash, "expected the error frame");
  assert.ok(h.logs.some((line) => line.includes("game over score=0 round=1 expected=1 pressed=2")));
  // Final score stays visible with the turn colon off.
  assertScoreFrame(h, 0, false);
  const gameOver = h.logs.findIndex((line) => line.includes("game over"));
  assert.equal(
    h.logs.slice(gameOver).filter((line) => line.includes("start")).length,
    1,
    "the error-display press is ignored; the awaited side key starts one game",
  );
});

test("gestures, raw reports and unknown keys are not game input", () => {
  const h = runApp({
    scenario: [
      ({ nowNs }) => [
        {
          resourceId: PANEL_RESOURCE,
          monotonicTimeNs: String(nowNs()),
          gesture: { gestureId: "swipe_left", phase: "end" },
        },
        {
          resourceId: PANEL_RESOURCE,
          monotonicTimeNs: String(nowNs()),
          raw: { contacts: [], coordinatesValid: false },
        },
        press("primary", nowNs()),
      ],
      keyPress("mic_toggle"),
      () => null,
    ],
  });
  assert.ok(!h.logs.some((line) => line.includes("game over")));
  assert.ok(h.logs.some((line) => line.includes("round complete score=1")));
});

test("a side key pressed during the replay is ignored", () => {
  let injected = false;
  const h = runApp({
    scenario: [keyPress("mic_toggle"), () => null],
    onFrame: (frame, frames, nowNs) => {
      if (injected || frame.colon || frame.masks[0] === 0) return undefined;
      injected = true;
      return [press("volume_down", nowNs()), release("volume_down", nowNs())];
    },
  });
  assert.ok(!h.logs.some((line) => line.includes("accept")));
  assert.ok(!h.logs.some((line) => line.includes("game over")));
  assert.ok(h.logs.some((line) => line.includes("await round=1")), "the replay finished");
  assert.ok(h.logs.some((line) => line.includes("round complete score=1")), "the awaited press scored");
});

test("holding the centre during a replay freezes it and exits promptly", () => {
  let injected = false;
  const h = runApp({
    scenario: [keyPress("mic_toggle"), () => null], // completes round one
    onFrame: (frame, frames, nowNs) => {
      const flashes = frames.filter((f) => !f.colon && f.masks[0] !== 0);
      if (injected || flashes.length !== 2) return undefined; // round two's first flash
      injected = true;
      const down = nowNs();
      return [press("center", down), release("center", down + 900000000)];
    },
  });
  assert.ok(h.logs.some((line) => line.includes("exit reason=centre held 900ms")));
  assert.ok(h.frames.some((f) => f.masks.every((mask) => mask === DASH)), "the hold hint was shown");
  const roundTwo = h.logs.findIndex((line) => line.includes("round=2 sequence=11 score=1"));
  const after = h.frames.filter((f) => f.logsAt > roundTwo);
  assert.equal(
    after.filter((f) => !f.colon && f.masks[0] !== 0).length,
    1,
    "the replay stopped at the first round-two symbol",
  );
  assert.deepEqual(h.order, [
    "input-cancel",
    "capture-cancel:" + PANEL_RESOURCE,
    "capture-cancel:" + SEGMENT_RESOURCE,
    "presentation-close",
    "presentation-cancel",
    "stop",
  ]);
});

test("a centre tap during a replay resumes it", () => {
  let injected = false;
  const h = runApp({
    scenario: [keyPress("mic_toggle"), () => null],
    onFrame: (frame, frames, nowNs) => {
      const flashes = frames.filter((f) => !f.colon && f.masks[0] !== 0);
      if (injected || flashes.length !== 2) return undefined;
      injected = true;
      const down = nowNs();
      return [press("center", down), release("center", down + 300000000)];
    },
  });
  assert.ok(!h.logs.some((line) => line.includes("centre held")));
  assert.ok(h.frames.some((f) => f.masks.every((mask) => mask === DASH)), "the hold hint was shown");
  assert.ok(h.logs.some((line) => line.includes("await round=2")), "the replay resumed and finished");
});

// --- dismissal and lifecycle ----------------------------------------------

test("holding the centre exits and releases the panel, the display and the run", () => {
  const h = runApp({
    scenario: [
      ({ nowNs }) => {
        const down = nowNs();
        return [press("center", down), release("center", down + 900000000)];
      },
      () => null,
    ],
  });
  assert.ok(h.logs.some((line) => line.includes("exit reason=centre held 900ms")));
  assert.deepEqual(h.order, [
    "input-cancel",
    "capture-cancel:" + PANEL_RESOURCE,
    "capture-cancel:" + SEGMENT_RESOURCE,
    "presentation-close",
    "presentation-cancel",
    "stop",
  ]);
  assert.equal(h.stopped, 1);
  assert.equal(h.opened[0].cancelled, true, "the presentation stream is closed");
});

test("a centre tap restores the game display and does not exit", () => {
  const h = runApp({
    scenario: [
      ({ nowNs }) => {
        const down = nowNs();
        return [press("center", down), release("center", down + 300000000)];
      },
      ({ frames }) => {
        // The held-centre hint must have been replaced by the score again.
        assert.deepEqual(frameOf(frames[frames.length - 1]), scoreFrame(0, true));
        return null;
      },
    ],
  });
  assert.ok(h.frames.some((f) => f.masks.every((mask) => mask === DASH)));
  assert.ok(!h.logs.some((line) => line.includes("centre held")));
  assert.ok(h.logs.some((line) => line.includes("exit reason=panel subscription ended")));
});

test("the app ends cleanly when the panel subscription ends", () => {
  const h = runApp();
  assert.ok(h.logs.some((line) => line.includes("exit reason=panel subscription ended")));
  assert.deepEqual(h.order, [
    "input-cancel",
    "capture-cancel:" + PANEL_RESOURCE,
    "capture-cancel:" + SEGMENT_RESOURCE,
    "presentation-close",
    "presentation-cancel",
    "stop",
  ]);
});

test("an older host without Call.tryRecv fails with a clear message", () => {
  const h = runApp({ hostTryRecv: false });
  assert.ok(
    h.logs.some((line) => line.includes("unavailable: this QuickJS host has no Call.tryRecv")),
  );
  assert.equal(h.stopped, 1);
  assert.deepEqual(h.order, [
    "input-cancel",
    "capture-cancel:" + PANEL_RESOURCE,
    "capture-cancel:" + SEGMENT_RESOURCE,
    "presentation-close",
    "presentation-cancel",
    "stop",
  ]);
});

test("refuses a segment display with fewer than four digits", () => {
  const h = runApp({
    resources: panelResources({ segment: { segment: { digitCount: 2, supportsColon: true } } }),
  });
  assert.equal(h.opened.length, 0);
  assert.ok(
    h.logs.some((line) =>
      line.includes("unsupported: display.segment.front has 2 digits; Simon needs 4"),
    ),
  );
  assert.equal(h.stopped, 1);
});

test("omits colon and decimal points when the display lacks them", () => {
  const h = runApp({
    resources: panelResources({
      segment: { segment: { digitCount: 4, supportsColon: false, supportsDecimalPoints: false } },
    }),
    scenario: [keyPress("mic_toggle"), () => null],
  });
  assert.ok(h.frames.length > 0);
  assert.ok(h.frames.every((f) => f.colon === false), "no colon on a display without one");
  assert.ok(
    h.frames.every((f) => f.dots.every((dot) => dot === false)),
    "no decimal points on a display without them",
  );
  assert.ok(h.logs.some((line) => line.includes("round complete score=1")));
});

test("an invalid centre press timestamp is a tap, not a hold", () => {
  const h = runApp({
    scenario: [
      ({ nowNs }) => [
        { resourceId: PANEL_RESOURCE, monotonicTimeNs: "0", key: { key: "center", pressed: true } },
        release("center", nowNs()),
      ],
      () => null,
    ],
  });
  assert.ok(!h.logs.some((line) => line.includes("centre held")));
  assert.ok(h.frames.some((f) => f.masks.every((mask) => mask === DASH)), "the hold hint was shown");
  assert.deepEqual(frameOf(h.frames[h.frames.length - 1]), scoreFrame(0, true));
});

test("an unsupported panel is reported and no stream is opened", () => {
  const h = runApp({
    resources: panelResources({ panel: { touch: { fullPanelAvailable: false } } }),
  });
  assert.equal(h.opened.length, 0);
  assert.ok(
    h.logs.some((line) =>
      line.includes("unsupported: touch.panel.main does not advertise a full-panel capture"),
    ),
  );
  assert.equal(h.stopped, 1);
});
