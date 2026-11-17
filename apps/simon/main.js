"use strict";

// Simon Says for Capsula Mini (QuickJS).
//
// The game owns the device while a run is active: one captured segment
// presentation for feedback and one full-panel touch capture for input. Both
// are handed back on every terminal path (self-exit, stream loss, app stop or
// crash), so the configured clock/ring baseline and the framework panel
// semantics return when the app is gone.
//
// Four properties of the QuickJS host shape this file:
//
//   * There are no timers (no setTimeout/setInterval), so a flash duration is
//     measured with Date.now(). While a delay runs the game polls the panel
//     with Call.tryRecv, whose bounded wait reports "nothing yet" as
//     undefined; that keeps the centre hold responsive during a full replay.
//   * SDK failures are thrown as InternalError, which quickjs-ng marks
//     uncatchable, so a try/catch cannot recover and a blocking recv that
//     times out kills the app. Idle waits therefore use recv(WAIT_FOREVER)
//     and animation waits use tryRecv; the two handshake reads (presentation
//     ready, capture granted) are answered by the daemon immediately.
//   * capsulad.run() only pumps AppBus events, and the game's main loop is the
//     input stream, so the game runs before run() is reached. capsulad.stop()
//     makes the host's run() return as soon as the script finishes.
//   * open()'s fourth argument is the whole-RPC deadline. Long-lived streams
//     pass 0; a positive value tears them down when it expires.
//
// Interaction: the four side keys are the four game symbols (north=1,
// east=2, south=3, west=4). The segment display shows the flashing symbol at
// the left cell during playback, the score (zero-padded, colon on = "your
// turn") while waiting for input, and four dashes while the unmarked centre
// is held. Releasing the centre after HOLD_EXIT_MS leaves the game. A full
// capture is exactly that: while the game runs, the panel's volume, mute,
// media and assistant actions are game input, so the centre hold is the
// documented way back to the framework's panel behavior. The replay polls the
// panel through Call.tryRecv: a centre press freezes the replay immediately
// and a completed hold exits, so the full growing sequence can be replayed
// without delaying dismissal.

var DEVICE_SERVICE = "capsulad.framework.v1.DeviceService";
var PRESENTATION_SERVICE = "capsulad.framework.v1.PresentationService";
var ROUTING_SERVICE = "capsulad.framework.v1.RoutingService";
var INPUT_SERVICE = "capsulad.framework.v1.InputService";

var SEGMENT_RESOURCE = "display.segment.front";
var PANEL_RESOURCE = "touch.panel.main";

// Seven-segment masks: bit 0 = segment a, through bit 6 = segment g. The
// digit table matches the framework's device-independent cell mapping.
var DIGIT_MASK = [0x3f, 0x06, 0x5b, 0x4f, 0x66, 0x6d, 0x7d, 0x07, 0x7f, 0x6f];
var BLANK_MASK = 0x00;
var DASH_MASK = 0x40;
var CELL_COUNT = 4;

// Physical key -> game symbol, clockwise from north.
var KEY_SYMBOL = { mic_toggle: 1, volume_up: 2, play_pause: 3, volume_down: 4 };

var FLASH_ON_MS = 320;
var FLASH_OFF_MS = 140;
var START_PAUSE_MS = 350;
var ROUND_PAUSE_MS = 250;
var ERROR_MS = 900;
var HOLD_EXIT_MS = 800;
// Longest native input wait while an animation is in progress; the host's
// tryRecv makes this a bounded wait, not a poll-and-hope.
var INPUT_POLL_MS = 20;

var STATE_IDLE = 0;   // at rest: before a game and between rounds
var STATE_PLAYING = 1; // replaying the full sequence; side keys are ignored
var STATE_INPUT = 2;   // repeating the shown sequence
var STATE_OVER = 3;    // final score, waiting for a new game or an exit

var presentation = null;
var presentationId = "";
var segmentCapture = null;
var panelCapture = null;
var input = null;

var state = STATE_IDLE;
var score = 0;
var sequence = [];
var position = 0;
var centerDown = false;
var centerDownNs = 0;
var shownSymbol = 0;
var colonSupported = false;
var decimalSupported = false;
var exiting = false;
var exitReason = "";

function log(message) {
  capsulad.log("simon: " + message);
}

// --- output ---------------------------------------------------------------

function showSegment(masks, colon, dots) {
  if (presentation === null) return;
  var cells = [];
  for (var i = 0; i < CELL_COUNT; i++) {
    cells.push({
      mask: masks[i],
      decimalPoint: decimalSupported && dots !== null && dots !== undefined ? !!dots[i] : false
    });
  }
  presentation.send({
    frame: { segment: { cells: cells, colon: !!colon && colonSupported } }
  });
}

function scoreMasks() {
  var text = String(score);
  while (text.length < CELL_COUNT) text = "0" + text;
  if (text.length > CELL_COUNT) text = text.substring(text.length - CELL_COUNT);
  var masks = [];
  for (var i = 0; i < CELL_COUNT; i++) {
    masks.push(DIGIT_MASK[text.charCodeAt(i) - 48]);
  }
  return masks;
}

function showScore(colon, dots) {
  shownSymbol = 0;
  showSegment(scoreMasks(), colon, dots);
}

function showSequenceSymbol(symbol) {
  shownSymbol = symbol;
  showSegment([DIGIT_MASK[symbol], BLANK_MASK, BLANK_MASK, BLANK_MASK], false, null);
}

function showBlank() {
  shownSymbol = 0;
  showSegment([BLANK_MASK, BLANK_MASK, BLANK_MASK, BLANK_MASK], false, null);
}

function showHoldHint() {
  showSegment([DASH_MASK, DASH_MASK, DASH_MASK, DASH_MASK], true, null);
}

function restoreDisplay() {
  if (state === STATE_PLAYING) {
    if (shownSymbol > 0) showSequenceSymbol(shownSymbol);
    else showBlank();
    return;
  }
  if (state === STATE_OVER) showScore(false, null);
  else showScore(true, null);
}

// --- timing and input -----------------------------------------------------

// The QuickJS host has no timer, so every delay is measured with Date.now().
// While a delay runs the game also reads the panel through Call.tryRecv, which
// blocks for at most its slice: undefined means "nothing yet", null is the end
// of the subscription, and anything else is an InputEvent. A centre press is
// therefore seen within INPUT_POLL_MS even during a replay, and the spin
// freezes under the hold so the player gets immediate feedback.
function waitInput(ms) {
  var event = input.tryRecv(ms);
  if (event === undefined) return false;
  if (event === null) {
    exitReason = "panel subscription ended";
    exiting = true;
    return true;
  }
  handleInput(event, true);
  return exiting;
}

function spin(ms) {
  var deadline = Date.now() + ms;
  var pauseStart = 0;
  for (;;) {
    var now = Date.now();
    if (centerDown) {
      if (pauseStart === 0) pauseStart = now;
      if (waitInput(INPUT_POLL_MS)) return true;
      continue;
    }
    if (pauseStart !== 0) {
      deadline += now - pauseStart;
      pauseStart = 0;
    }
    var remaining = deadline - now;
    if (remaining <= 0) return false;
    if (waitInput(remaining < INPUT_POLL_MS ? remaining : INPUT_POLL_MS)) return true;
  }
}

// --- game -----------------------------------------------------------------

function startGame() {
  score = 0;
  sequence = [];
  position = 0;
  centerDown = false;
  centerDownNs = 0;
  state = STATE_IDLE;
  log("start");
  showScore(true, null);
  if (spin(START_PAUSE_MS)) return;
  nextRound();
}

function nextRound() {
  sequence.push(1 + Math.floor(Math.random() * 4));
  position = 0;
  log("round=" + sequence.length + " sequence=" + sequence.join("") + " score=" + score);
  playSequence();
}

function playSequence() {
  state = STATE_PLAYING;
  shownSymbol = 0;
  log("play round=" + sequence.length + " symbols=" + sequence.length);
  for (var i = 0; i < sequence.length; i++) {
    showSequenceSymbol(sequence[i]);
    if (spin(FLASH_ON_MS)) return;
    showBlank();
    if (spin(FLASH_OFF_MS)) return;
  }
  if (exiting) return;
  state = STATE_INPUT;
  showScore(true, null);
  log("await round=" + sequence.length + " position=0");
}

function completeRound() {
  state = STATE_IDLE; // the short break accepts no input
  showScore(true, [true, true, true, true]);
  if (spin(ROUND_PAUSE_MS)) return;
  nextRound();
}

function gameOver(pressedSymbol) {
  log("game over score=" + score + " round=" + sequence.length +
      " expected=" + sequence[position] + " pressed=" + pressedSymbol);
  state = STATE_OVER;
  showSegment([DASH_MASK, DASH_MASK, DASH_MASK, DASH_MASK], false, null);
  if (spin(ERROR_MS)) return;
  showScore(false, null);
}

function submitSymbol(symbol) {
  if (sequence[position] === symbol) {
    position++;
    score++;
    showScore(true, null);
    if (position === sequence.length) {
      log("round complete score=" + score);
      completeRound();
      return;
    }
    log("accept symbol=" + symbol + " position=" + position + " score=" + score);
    return;
  }
  gameOver(symbol);
}

// --- input ----------------------------------------------------------------

function handleCenter(pressed, eventNs) {
  if (pressed) {
    centerDown = true;
    centerDownNs = eventNs > 0 ? eventNs : 0;
    showHoldHint();
    return;
  }
  // Only a press and a release that both carry a valid monotonic timestamp
  // form a hold; a missing one is a tap, never a huge delta.
  var heldNs = centerDown && centerDownNs > 0 && eventNs >= centerDownNs
    ? eventNs - centerDownNs
    : 0;
  centerDown = false;
  centerDownNs = 0;
  if (heldNs >= HOLD_EXIT_MS * 1000000) {
    exitReason = "centre held " + Math.round(heldNs / 1000000) + "ms";
    exiting = true;
    return;
  }
  restoreDisplay();
}

function handleKey(key, pressed, eventNs, inDelay) {
  if (key === "center") {
    handleCenter(pressed, eventNs);
    return;
  }
  if (!pressed || inDelay) return; // a delay only reacts to the centre
  if (state === STATE_INPUT) {
    var symbol = KEY_SYMBOL[key];
    if (symbol !== undefined) submitSymbol(symbol);
    return;
  }
  if (state === STATE_IDLE || state === STATE_OVER) {
    // A side key at rest starts a new game. During a replay it is ignored:
    // the sequence owns the panel until the input phase.
    startGame();
  }
}

function handleInput(event, inDelay) {
  if (!event || !event.key) return; // gestures and raw reports are not game input
  var eventNs = Number(event.monotonicTimeNs);
  if (!(eventNs > 0)) eventNs = 0;
  handleKey(String(event.key.key), !!event.key.pressed, eventNs, inDelay === true);
}

function runInputLoop() {
  for (;;) {
    var event = input.recv(capsulad.WAIT_FOREVER);
    if (event === null) {
      exitReason = "panel subscription ended";
      return;
    }
    handleInput(event, false);
    if (exiting) return;
  }
}

// --- setup and teardown ---------------------------------------------------

function checkSupport() {
  var reply = capsulad.call(DEVICE_SERVICE, "ListResources", {}, 5000);
  var resources = (reply && reply.resources) || [];
  var display = null;
  var panel = null;
  for (var i = 0; i < resources.length; i++) {
    if (resources[i].resourceId === SEGMENT_RESOURCE) display = resources[i];
    if (resources[i].resourceId === PANEL_RESOURCE) panel = resources[i];
  }
  if (display === null) return "no " + SEGMENT_RESOURCE;
  if (display.available !== true) return SEGMENT_RESOURCE + " is unavailable";
  var segment = display.segment;
  if (segment === undefined || segment === null) {
    return SEGMENT_RESOURCE + " reports no segment capabilities";
  }
  var digits = Number(segment.digitCount);
  if (!(digits >= CELL_COUNT)) {
    return SEGMENT_RESOURCE + " has " + (isNaN(digits) ? 0 : digits) +
      " digits; Simon needs " + CELL_COUNT;
  }
  colonSupported = segment.supportsColon === true;
  decimalSupported = segment.supportsDecimalPoints === true;
  if (panel === null) return "no " + PANEL_RESOURCE;
  if (!panel.touch || panel.touch.fullPanelAvailable !== true) {
    return PANEL_RESOURCE + " does not advertise a full-panel capture";
  }
  return null;
}

// The fourth open() argument is the whole-RPC deadline, not a handshake
// timeout: the native SDK puts it on the gRPC request head. A live game stream
// must pass 0 (no deadline); a positive value would tear the presentation and
// the captures down mid-game.
function openPresentation() {
  presentation = capsulad.open(PRESENTATION_SERVICE, "Open", 3, 0);
  presentation.send({ open: { resourceId: SEGMENT_RESOURCE } });
  var event = presentation.recv(10000); // the daemon answers before any content
  if (event === null || !event.ready || !event.ready.presentationId) {
    presentation.cancel();
    presentation = null;
    return;
  }
  presentationId = event.ready.presentationId;
  log("presentation=" + presentationId);
}

function acquireCapture(resourceId, ownedPresentationId, touchMode) {
  var call = capsulad.open(ROUTING_SERVICE, "Capture", 1, 0);
  var request = { resourceId: resourceId };
  if (ownedPresentationId !== "") request.presentationId = ownedPresentationId;
  if (touchMode !== "") request.touchMode = touchMode;
  call.send(request);
  var event = call.recv(10000); // granted is queued before any selection event
  if (event === null || !event.granted || !event.granted.capture ||
      !event.granted.capture.captureId) {
    call.cancel();
    return null;
  }
  var capture = event.granted.capture;
  log("capture resource=" + resourceId + " id=" + capture.captureId +
      " selected=" + !!capture.selected);
  return { call: call, id: capture.captureId, info: capture };
}

function subscribeInput() {
  input = capsulad.open(INPUT_SERVICE, "WatchInput", 1, 0);
  input.send({ resourceId: PANEL_RESOURCE, captureId: panelCapture.id });
}

function shutdown() {
  log("exit reason=" + (exitReason !== "" ? exitReason : "requested") + " score=" + score);
  if (input !== null) {
    input.cancel();
    input = null;
  }
  if (panelCapture !== null) {
    panelCapture.call.cancel();
    panelCapture = null;
  }
  if (segmentCapture !== null) {
    segmentCapture.call.cancel();
    segmentCapture = null;
  }
  if (presentation !== null) {
    presentation.send({ close: {} });
    // ClosePresentation makes the daemon end the stream; the null is the
    // confirmation that the presentation and its captures are gone.
    presentation.recv(capsulad.WAIT_FOREVER);
    presentation.cancel();
    presentation = null;
  }
  capsulad.stop();
}

function main() {
  capsulad.start();
  log("ready app=" + capsulad.env.appId + " run=" + capsulad.env.runId);

  var unsupported = checkSupport();
  if (unsupported !== null) {
    log("unsupported: " + unsupported);
    capsulad.stop();
    return;
  }

  openPresentation();
  if (presentation === null) {
    log("unavailable: the segment presentation did not open");
    capsulad.stop();
    return;
  }

  showScore(true, null); // first valid frame makes the presentation eligible
  segmentCapture = acquireCapture(SEGMENT_RESOURCE, presentationId, "");
  if (segmentCapture === null) {
    log("unavailable: the segment capture was refused");
    shutdown();
    return;
  }
  panelCapture = acquireCapture(PANEL_RESOURCE, "", "TOUCH_FULL_RAW");
  if (panelCapture === null) {
    log("unavailable: the full-panel capture was refused");
    shutdown();
    return;
  }

  subscribeInput();
  if (typeof input.tryRecv !== "function") {
    log("unavailable: this QuickJS host has no Call.tryRecv; rebuild the interpreter host");
    shutdown();
    return;
  }
  log("controls: side keys play, hold the centre to exit");

  startGame();
  runInputLoop();
  shutdown();
}

main();
