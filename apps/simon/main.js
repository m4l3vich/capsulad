"use strict";

// Simon Says for Capsula Mini (QuickJS).
//
// One run owns exactly four things while it is active and hands every one of
// them back on every terminal path: the LED ring presentation (the game
// colours), an optional segment presentation (the score), one full-panel touch
// capture (the four side keys and the unmarked centre), and one speaker
// playback stream (one packaged tone per colour). Closing the presentations
// restores the configured clock/ring baseline and releasing the captures
// restores the framework panel semantics.
//
// The tones are pre-rendered WAV files in the package's audio/ directory. They
// are loaded once through capsulad.readPackageFile at startup; the game never
// synthesizes samples. A host without that entry point, or a package with a
// missing or malformed tone file, stops with an explicit log line before any
// presentation, capture or playback stream is opened.
//
// Geometry and capability come from the device, never from a board name:
// DeviceService.ListResources advertises the ring pixel count and RGB support,
// and pixel 0 is the top with indices advancing clockwise. Each side of the
// panel owns the ring pixels nearest its cardinal direction, so north is red,
// east yellow, south green and west blue on a 12-pixel Mini and on any other
// advertised count.
//
// Four properties of the QuickJS host shape this file:
//
//   * There are no timers (no setTimeout/setInterval), so a flash duration is
//     measured with Date.now(). While a delay runs the game polls the panel
//     with Call.tryRecv, whose bounded wait reports "nothing yet" as
//     undefined; the same bounded wait drains capture, presentation and
//     playback events so a preemption or a lost speaker is noticed while the
//     game is otherwise idle.
//   * SDK failures are thrown as InternalError, which quickjs-ng marks
//     uncatchable, so a try/catch cannot recover and a blocking recv that
//     times out kills the app. Idle waits therefore use bounded tryRecv slices
//     and the two handshake reads (presentation ready, capture granted,
//     playback started) are answered by the daemon immediately.
//   * capsulad.run() only pumps AppBus events, and the game's main loop is the
//     input stream, so the game runs before run() is reached. capsulad.stop()
//     makes the host's run() return as soon as the script finishes.
//   * open()'s fourth argument is the whole-RPC deadline. Long-lived streams
//     pass 0; a positive value tears them down when it expires.
//
// Interaction: the four side keys are the four colours (north=1 red,
// east=2 yellow, south=3 green, west=4 blue). The ring replays the growing
// sequence in colour with one tone per colour; the player repeats it with the
// same side keys. A wrong side key ends the round with a white error flash and
// a low tone, shows the final score, and any side key starts a retry. Holding
// the unmarked centre for 800 ms leaves the game; a shorter centre tap pauses a
// replay (or just shows the hold hint) and then restores it. Side keys are
// ignored while the sequence plays, while a round-complete or error display
// runs, and while the centre is held.

var DEVICE_SERVICE = "capsulad.framework.v1.DeviceService";
var PRESENTATION_SERVICE = "capsulad.framework.v1.PresentationService";
var ROUTING_SERVICE = "capsulad.framework.v1.RoutingService";
var INPUT_SERVICE = "capsulad.framework.v1.InputService";
var AUDIO_SERVICE = "capsulad.framework.v1.AudioService";

var RING_RESOURCE = "light.ring.main";
var SEGMENT_RESOURCE = "display.segment.front";
var PANEL_RESOURCE = "touch.panel.main";
var SPEAKER_ENDPOINT = "audio.output.speaker";

// Seven-segment masks: bit 0 = segment a, through bit 6 = segment g. The
// digit table matches the framework's device-independent cell mapping.
var DIGIT_MASK = [0x3f, 0x06, 0x5b, 0x4f, 0x66, 0x6d, 0x7d, 0x07, 0x7f, 0x6f];
var DASH_MASK = 0x40;
var CELL_COUNT = 4;

// Colours are brand-independent game symbols, one per panel side clockwise
// from north. Each symbol names its two pre-rendered package tones (the replay
// cue and the shorter press confirmation); the error tone is a deliberately
// low, falling buzz outside the four colour frequencies.
var SYMBOLS = [
  { name: "red", key: "mic_toggle",
    longWav: "audio/red-long.wav", shortWav: "audio/red-short.wav",
    red: 255, green: 0, blue: 0 },
  { name: "yellow", key: "volume_up",
    longWav: "audio/yellow-long.wav", shortWav: "audio/yellow-short.wav",
    red: 255, green: 180, blue: 0 },
  { name: "green", key: "play_pause",
    longWav: "audio/green-long.wav", shortWav: "audio/green-short.wav",
    red: 0, green: 200, blue: 0 },
  { name: "blue", key: "volume_down",
    longWav: "audio/blue-long.wav", shortWav: "audio/blue-short.wav",
    red: 0, green: 80, blue: 255 }
];
var ERROR_TONE_WAV = "audio/error.wav";

var KEY_SYMBOL = {};
for (var s = 0; s < SYMBOLS.length; s++) KEY_SYMBOL[SYMBOLS[s].key] = s + 1;

var FLASH_ON_MS = 320;
var FLASH_OFF_MS = 140;
var START_PAUSE_MS = 350;
var ROUND_PAUSE_MS = 260;
var ERROR_FLASH_ON_MS = 180;
var ERROR_FLASH_OFF_MS = 120;
var ERROR_FLASHES = 3;
var HOLD_EXIT_MS = 800;
// Longest native input wait while an animation is in progress. The same slice
// drains ownership events; the host makes it a bounded wait, not a busy spin.
var INPUT_POLL_MS = 20;

// The speaker stream's input format. Playback resamples to the device mixer, so
// 24 kHz mono S16LE is a documented playback format rather than an assumption
// about the output device. The packaged WAV files carry the same format.
var AUDIO_RATE = 24000;
var AUDIO_CHANNELS = 1;

var STATE_READY = 0;     // before a game and after the start pause
var STATE_PLAYBACK = 1;  // replaying the full sequence; side keys are ignored
var STATE_INPUT = 2;     // repeating the shown sequence
var STATE_BREAK = 3;     // round-complete display; side keys are ignored
var STATE_ERROR = 4;     // wrong-answer display; side keys are ignored
var STATE_GAMEOVER = 5;  // final score; a side key retries, centre exits

var ringPresentation = null;
var segmentPresentation = null;
var ringCapture = null;
var segmentCapture = null;
var panelCapture = null;
var speaker = null;
var input = null;

var ringPixelCount = 0;
var sidePixels = [[], [], [], []];
var segmentSupported = false;
var colonSupported = false;
var decimalSupported = false;

var longTones = [];
var shortTones = [];
var errorTone = "";

var state = STATE_READY;
var score = 0;
var sequence = [];
var position = 0;
var playingSymbol = 0;
var centerDown = false;
var centerDownNs = 0;
var exiting = false;
var exitReason = "";

function log(message) {
  capsulad.log("simon: " + message);
}

// --- output: ring ---------------------------------------------------------

function ringPixels(fill) {
  var pixels = [];
  for (var i = 0; i < ringPixelCount; i++) {
    pixels.push({
      red: fill(i).red,
      green: fill(i).green,
      blue: fill(i).blue
    });
  }
  return pixels;
}

function paintRingPixels(pixels) {
  if (ringPresentation === null) return;
  ringPresentation.call.send({ frame: { ring: { pixels: pixels } } });
}

function ringSymbol(symbol) {
  var color = SYMBOLS[symbol - 1];
  var owned = sidePixels[symbol - 1];
  paintRingPixels(ringPixels(function (index) {
    for (var i = 0; i < owned.length; i++) {
      if (owned[i] === index) {
        return { red: color.red, green: color.green, blue: color.blue };
      }
    }
    return { red: 0, green: 0, blue: 0 };
  }));
}

function ringOff() {
  paintRingPixels(ringPixels(function () {
    return { red: 0, green: 0, blue: 0 };
  }));
}

// A round-complete celebration: every side in its own colour. It is distinct
// from one symbol because all four sides light together.
function ringAllSides() {
  paintRingPixels(ringPixels(function (index) {
    var color = SYMBOLS[sideOfPixel(index) - 1];
    return { red: color.red, green: color.green, blue: color.blue };
  }));
}

// White is not a game colour, so it cannot be mistaken for a symbol.
function ringWhite(level) {
  paintRingPixels(ringPixels(function () {
    return { red: level, green: level, blue: level };
  }));
}

function sideOfPixel(index) {
  // A 90-degree arc centred on each cardinal direction (pixel 0 top, indices
  // clockwise), with the boundary between arcs resolved clockwise. The result
  // is 1 north, 2 east, 3 south, 4 west. Every side is non-empty for an
  // advertised count of at least 4.
  return (Math.round(index * 4 / ringPixelCount) % 4) + 1;
}

function buildSidePixels() {
  sidePixels = [[], [], [], []];
  for (var i = 0; i < ringPixelCount; i++) {
    sidePixels[sideOfPixel(i) - 1].push(i);
  }
}

// --- output: segment score -------------------------------------------------

function showSegment(masks, colon, dots) {
  if (!segmentSupported || segmentPresentation === null) return;
  var cells = [];
  for (var i = 0; i < CELL_COUNT; i++) {
    cells.push({
      mask: masks[i],
      decimalPoint: decimalSupported && dots !== null && dots !== undefined ? !!dots[i] : false
    });
  }
  segmentPresentation.call.send({
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
  showSegment(scoreMasks(), colon, dots);
}

function showDashes(colon) {
  showSegment([DASH_MASK, DASH_MASK, DASH_MASK, DASH_MASK], colon, null);
}

function showHoldHint() {
  showDashes(true);
}

function restoreDisplay() {
  if (state === STATE_PLAYBACK) showScore(false);
  else if (state === STATE_BREAK) showScore(false, [true, true, true, true]);
  else if (state === STATE_ERROR) showDashes(false);
  else if (state === STATE_GAMEOVER) showScore(false);
  else showScore(true);
}

function restoreRing() {
  if (state === STATE_PLAYBACK) {
    if (playingSymbol > 0) ringSymbol(playingSymbol);
    else ringOff();
    return;
  }
  if (state === STATE_BREAK) {
    ringAllSides();
    return;
  }
  if (state === STATE_ERROR) {
    ringWhite(180);
    return;
  }
  ringOff();
}

function restoreOutput() {
  restoreDisplay();
  restoreRing();
}

// --- output: speaker tones ------------------------------------------------

// The packaged cues are standard RIFF/WAVE files: 16-bit PCM, mono, 24 kHz.
// The long cues last FLASH_ON_MS (320 ms), the short cues 180 ms, and the
// error cue 600 ms. Loading validates the container and hands the data chunk
// to the decoder exactly as shipped. It does not resample and does not
// generate samples.
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

function wavPcm(bytes, path) {
  if (!bytes || typeof bytes.length !== "number" || bytes.length < 12) {
    throw new Error(path + ": not a RIFF/WAVE file");
  }
  if (fourcc(bytes, 0) !== "RIFF" || fourcc(bytes, 8) !== "WAVE") {
    throw new Error(path + ": not a RIFF/WAVE file");
  }
  // RIFF's size field counts everything after those eight bytes. Requiring it
  // to describe the whole file rejects a corrupt length instead of silently
  // walking past the declared container.
  var riffEnd = 8 + readU32(bytes, 4);
  if (riffEnd < 12 || riffEnd !== bytes.length) {
    throw new Error(path + ": RIFF length does not match the file");
  }
  var format = null;
  var dataStart = -1;
  var dataSize = 0;
  var offset = 12;
  while (offset < riffEnd) {
    if (riffEnd - offset < 8) {
      throw new Error(path + ": truncated chunk header");
    }
    var chunkId = fourcc(bytes, offset);
    var chunkSize = readU32(bytes, offset + 4);
    offset += 8;
    if (chunkSize > riffEnd - offset) {
      throw new Error(path + ": " + chunkId + " chunk exceeds the RIFF boundary");
    }
    var paddedEnd = offset + chunkSize + (chunkSize & 1);
    if (paddedEnd > riffEnd) {
      throw new Error(path + ": " + chunkId + " chunk is missing its pad byte");
    }
    if (chunkId === "fmt ") {
      if (chunkSize < 16) throw new Error(path + ": short fmt chunk");
      format = {
        encoding: readU16(bytes, offset),
        channels: readU16(bytes, offset + 2),
        rate: readU32(bytes, offset + 4),
        byteRate: readU32(bytes, offset + 8),
        blockAlign: readU16(bytes, offset + 12),
        bits: readU16(bytes, offset + 14)
      };
    } else if (chunkId === "data") {
      dataStart = offset;
      dataSize = chunkSize;
    }
    offset = paddedEnd;
  }
  if (offset !== riffEnd) {
    throw new Error(path + ": RIFF length does not match the file");
  }
  if (format === null) throw new Error(path + ": no fmt chunk");
  if (dataStart < 0) throw new Error(path + ": no data chunk");
  if (format.encoding !== 1 || format.channels !== AUDIO_CHANNELS ||
      format.rate !== AUDIO_RATE || format.bits !== 16 ||
      format.blockAlign !== AUDIO_CHANNELS * 2 ||
      format.byteRate !== AUDIO_RATE * AUDIO_CHANNELS * 2) {
    throw new Error(path + ": expected " + AUDIO_RATE + " Hz mono 16-bit PCM");
  }
  if (dataSize === 0 || dataSize % (AUDIO_CHANNELS * 2) !== 0) {
    throw new Error(path + ": malformed PCM data");
  }
  return { start: dataStart, size: dataSize };
}

function loadTone(path) {
  var bytes = capsulad.readPackageFile(path);
  if (!bytes || typeof bytes.length !== "number" ||
      typeof bytes.subarray !== "function") {
    throw new Error(path + ": readPackageFile did not return bytes");
  }
  var pcm = wavPcm(bytes, path);
  return base64FromBytes(bytes.subarray(pcm.start, pcm.start + pcm.size));
}

function loadTones() {
  longTones = [];
  shortTones = [];
  for (var i = 0; i < SYMBOLS.length; i++) {
    longTones.push(loadTone(SYMBOLS[i].longWav));
    shortTones.push(loadTone(SYMBOLS[i].shortWav));
  }
  errorTone = loadTone(ERROR_TONE_WAV);
  log("tones loaded=" + (SYMBOLS.length * 2 + 1) + " packaged wav " +
      AUDIO_RATE + "Hz mono 16-bit");
}

function flushTones() {
  if (speaker === null) return;
  // FLUSH discards only the audio written before it; the cue written next on
  // this same stream is kept, so the backlog is dropped without losing it.
  speaker.send({ control: "PLAYBACK_FLUSH" });
}

function playLongTone(symbol) {
  if (speaker === null) return;
  speaker.send({ chunk: { pcm: longTones[symbol - 1] } });
}

function playPressTone(symbol) {
  if (speaker === null) return;
  // A fast player must hear the new press immediately instead of a backlog of
  // earlier confirmations.
  flushTones();
  speaker.send({ chunk: { pcm: shortTones[symbol - 1] } });
}

function playErrorTone() {
  if (speaker === null) return;
  flushTones();
  speaker.send({ chunk: { pcm: errorTone } });
}

// --- ownership ------------------------------------------------------------

// Drain one capture stream. A capture becomes unselected when another owner
// wins routing, which is a preemption: the game releases everything instead of
// continuing invisibly or with a dead panel.
function drainCapture(capture, label) {
  if (capture === null) return false;
  for (;;) {
    var event = capture.call.tryRecv(0);
    if (event === undefined) return false;
    if (event === null) {
      exitReason = label + " capture ended";
      exiting = true;
      return true;
    }
    if (event.changed && event.changed.selected === false) {
      exitReason = label + " capture preempted";
      exiting = true;
      return true;
    }
  }
}

// Presentation events are informational (visibility reasons); draining them
// keeps the stream bounded. Losing the stream means the output the game owns is
// gone, so the run ends.
function drainPresentation(presentation, label) {
  if (presentation === null) return false;
  for (;;) {
    var event = presentation.call.tryRecv(0);
    if (event === undefined) return false;
    if (event === null) {
      presentation.ended = true;
      exitReason = label + " presentation ended";
      exiting = true;
      return true;
    }
  }
}

function drainPlayback() {
  if (speaker === null) return false;
  for (;;) {
    var event = speaker.tryRecv(0);
    if (event === undefined) return false;
    if (event === null) {
      exitReason = "speaker stream ended";
      exiting = true;
      return true;
    }
    if (event.notice) {
      exitReason = "speaker stream " + (event.notice.code || "notice");
      exiting = true;
      return true;
    }
  }
}

function pumpOwnership() {
  if (exiting) return true;
  if (drainPresentation(ringPresentation, "ring")) return true;
  if (drainPresentation(segmentPresentation, "display")) return true;
  if (drainCapture(panelCapture, "panel")) return true;
  if (drainCapture(ringCapture, "ring")) return true;
  if (drainCapture(segmentCapture, "display")) return true;
  if (drainPlayback()) return true;
  return false;
}

// --- timing and input -----------------------------------------------------

function failStreamEnded() {
  exitReason = "panel subscription ended";
  exiting = true;
}

// Bounded read of one input event. The host waits at most the slice, so the
// caller's ownership pump runs continuously without a blocking recv.
function readInput(waitMs, ignoreSide) {
  var event = input.tryRecv(waitMs);
  if (event === undefined) return false;
  if (event === null) {
    failStreamEnded();
    return true;
  }
  handleInput(event, ignoreSide);
  return exiting;
}

function spin(ms) {
  var deadline = Date.now() + ms;
  var pauseStart = 0;
  for (;;) {
    if (pumpOwnership()) return true;
    var now = Date.now();
    if (centerDown) {
      // A held centre freezes the animation and the replay timer. The hold is
      // still measured from the press timestamp, so the exit does not need the
      // animation to finish first.
      if (pauseStart === 0) pauseStart = now;
      if (readInput(INPUT_POLL_MS, true)) return true;
      continue;
    }
    if (pauseStart !== 0) {
      deadline += now - pauseStart;
      pauseStart = 0;
    }
    var remaining = deadline - Date.now();
    if (remaining <= 0) return false;
    if (readInput(remaining < INPUT_POLL_MS ? remaining : INPUT_POLL_MS, true)) return true;
  }
}

// Idle wait for the player's turn. All side input is meaningful here; the
// centre is always meaningful.
function waitForInput() {
  for (;;) {
    if (pumpOwnership()) return;
    if (readInput(INPUT_POLL_MS, false)) return;
  }
}

// --- game -----------------------------------------------------------------

function startGame() {
  score = 0;
  sequence = [];
  position = 0;
  playingSymbol = 0;
  state = STATE_READY;
  log("start");
  showScore(true);
  ringOff();
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
  state = STATE_PLAYBACK;
  playingSymbol = 0;
  flushTones();
  showScore(false);
  ringOff();
  log("play round=" + sequence.length + " symbols=" + sequence.length);
  for (var i = 0; i < sequence.length; i++) {
    playingSymbol = sequence[i];
    ringSymbol(playingSymbol);
    playLongTone(playingSymbol);
    if (spin(FLASH_ON_MS)) return;
    playingSymbol = 0;
    ringOff();
    if (spin(FLASH_OFF_MS)) return;
  }
  if (exiting) return;
  state = STATE_INPUT;
  ringOff();
  showScore(true);
  log("await round=" + sequence.length + " position=0");
}

function completeRound() {
  state = STATE_BREAK; // the short break accepts no side input
  showScore(false, [true, true, true, true]);
  ringAllSides();
  if (spin(ROUND_PAUSE_MS)) return;
  ringOff();
  showScore(false);
  nextRound();
}

function gameOver(pressedSymbol) {
  log("game over score=" + score + " round=" + sequence.length +
      " expected=" + sequence[position] + " pressed=" + pressedSymbol);
  state = STATE_ERROR;
  showDashes(false);
  playErrorTone();
  for (var i = 0; i < ERROR_FLASHES; i++) {
    ringWhite(180);
    if (spin(ERROR_FLASH_ON_MS)) return;
    ringOff();
    if (spin(ERROR_FLASH_OFF_MS)) return;
  }
  state = STATE_GAMEOVER;
  ringOff();
  showScore(false);
  log("final score=" + score + "; a side key retries, holding the centre exits");
}

function submitSymbol(symbol) {
  if (sequence[position] === symbol) {
    position++;
    score++;
    ringSymbol(symbol);   // keep the accepted side lit until the next action
    showScore(true);
    playPressTone(symbol);
    log("accept symbol=" + symbol + " position=" + position + " score=" + score);
    if (position === sequence.length) {
      log("round complete score=" + score);
      completeRound();
    }
    return;
  }
  gameOver(symbol);
}

// --- input ----------------------------------------------------------------

function handleCenter(pressed, eventNs) {
  if (pressed) {
    if (centerDown) return; // a repeated press is not a second hold
    centerDown = true;
    centerDownNs = eventNs > 0 ? eventNs : 0;
    showHoldHint();
    ringWhite(35);
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
  restoreOutput();
}

function handleKey(key, pressed, eventNs, ignoreSide) {
  if (key === "center") {
    handleCenter(pressed, eventNs);
    return;
  }
  if (!pressed || ignoreSide) return;
  var symbol = KEY_SYMBOL[key];
  if (symbol === undefined) return;
  if (state === STATE_INPUT) {
    submitSymbol(symbol);
    return;
  }
  if (state === STATE_GAMEOVER) {
    // The final score waits for an explicit retry. The error display itself is
    // a separate state, so a press during the flashes cannot nest a game.
    startGame();
  }
}

function handleInput(event, ignoreSide) {
  if (!event || !event.key) return; // gestures and raw reports are not game input
  var eventNs = Number(event.monotonicTimeNs);
  if (!(eventNs > 0)) eventNs = 0;
  handleKey(String(event.key.key), !!event.key.pressed, eventNs, ignoreSide === true);
}

// --- setup and teardown ---------------------------------------------------

function checkSupport() {
  var reply = capsulad.call(DEVICE_SERVICE, "ListResources", {}, 5000);
  var resources = (reply && reply.resources) || [];
  var ring = null;
  var display = null;
  var panel = null;
  for (var i = 0; i < resources.length; i++) {
    if (resources[i].resourceId === RING_RESOURCE) ring = resources[i];
    if (resources[i].resourceId === SEGMENT_RESOURCE) display = resources[i];
    if (resources[i].resourceId === PANEL_RESOURCE) panel = resources[i];
  }
  if (ring === null) return "no " + RING_RESOURCE;
  if (ring.available !== true) return RING_RESOURCE + " is unavailable";
  if (ring.ring === undefined || ring.ring === null) {
    return RING_RESOURCE + " reports no ring capabilities";
  }
  var pixels = Number(ring.ring.pixelCount);
  if (!(pixels >= 4)) {
    return RING_RESOURCE + " has " + (isNaN(pixels) ? 0 : pixels) +
      " pixels; Simon needs at least 4";
  }
  if (ring.ring.supportsRgb !== true) {
    return RING_RESOURCE + " does not support RGB colour";
  }
  ringPixelCount = pixels;
  buildSidePixels();
  log("ring pixels=" + ringPixelCount + " rgb=true");

  if (panel === null) return "no " + PANEL_RESOURCE;
  if (panel.available === false) return PANEL_RESOURCE + " is unavailable";
  if (!panel.touch || panel.touch.fullPanelAvailable !== true) {
    return PANEL_RESOURCE + " does not advertise a full-panel capture";
  }

  // The score display is optional: the colours, tones and side keys carry the
  // game, and the README documents the no-display behaviour. A display with
  // fewer than four cells cannot show the full score, so it is not used.
  if (display !== null && display.available === true &&
      display.segment !== undefined && display.segment !== null) {
    var digits = Number(display.segment.digitCount);
    if (digits >= CELL_COUNT) {
      segmentSupported = true;
      colonSupported = display.segment.supportsColon === true;
      decimalSupported = display.segment.supportsDecimalPoints === true;
      log("score display digits=" + digits + " colon=" + colonSupported +
        " dots=" + decimalSupported);
    } else {
      log("score display has " + (isNaN(digits) ? 0 : digits) +
        " digits; running without a score display");
    }
  } else {
    log("no score display; the sequence and tones are the progress indicator");
  }

  var audio = capsulad.call(AUDIO_SERVICE, "ListEndpoints", {}, 5000);
  var endpoints = (audio && audio.endpoints) || [];
  var speakerFound = false;
  for (var j = 0; j < endpoints.length; j++) {
    if (endpoints[j].endpointId === SPEAKER_ENDPOINT) speakerFound = true;
  }
  if (!speakerFound) return "no " + SPEAKER_ENDPOINT;
  return null;
}

// The fourth open() argument is the whole-RPC deadline, not a handshake
// timeout: the native SDK puts it on the gRPC request head. A live stream must
// pass 0 (no deadline); a positive value would tear it down mid-game.
function openPresentation(resourceId) {
  var call = capsulad.open(PRESENTATION_SERVICE, "Open", 3, 0);
  call.send({ open: { resourceId: resourceId } });
  var event = call.recv(10000); // the daemon answers before any content
  if (event === null || !event.ready || !event.ready.presentationId) {
    call.cancel();
    return null;
  }
  log("presentation=" + event.ready.presentationId + " resource=" + resourceId);
  return { call: call, id: event.ready.presentationId };
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
  return { call: call, id: capture.captureId };
}

function openPlayback() {
  var call = capsulad.open(AUDIO_SERVICE, "Playback", 3, 0);
  call.send({
    open: {
      endpointId: SPEAKER_ENDPOINT,
      format: {
        sampleFormat: "PCM_S16LE",
        sampleRateHz: AUDIO_RATE,
        channels: AUDIO_CHANNELS
      }
    }
  });
  var event = call.recv(10000); // PlaybackStarted precedes any progress
  if (event === null || !event.started) {
    call.cancel();
    return null;
  }
  log("speaker stream=" + event.started.streamId);
  return call;
}

function subscribeInput() {
  input = capsulad.open(INPUT_SERVICE, "WatchInput", 1, 0);
  input.send({ resourceId: PANEL_RESOURCE, captureId: panelCapture.id });
}

function releaseCapture(capture, label) {
  if (capture === null) return;
  var reply = capsulad.call(ROUTING_SERVICE, "ReleaseCapture",
    { captureId: capture.id }, 5000);
  log("release " + label + " capture=" + capture.id + " released=" + !!reply.released);
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
    // ClosePresentation makes the daemon end the stream; the null is the
    // confirmation that the presentation and its captures are gone. A bounded
    // tryRecv cannot turn a slow daemon into an uncatchable SDK timeout.
    if (typeof presentation.call.tryRecv === "function") {
      for (var i = 0; i < 8; i++) {
        var event = presentation.call.tryRecv(i === 0 ? 2000 : 0);
        if (event === null || event === undefined) break;
      }
    }
  }
  presentation.call.cancel();
}

// Every exit path runs this once. Tone first so no cue outlives dismissal,
// then input, then the captures and presentations so the framework's selected
// baseline and panel semantics return before the run disappears.
function shutdown() {
  log("exit reason=" + (exitReason !== "" ? exitReason : "requested") + " score=" + score);
  if (speaker !== null) {
    speaker.cancel();
    speaker = null;
  }
  if (input !== null) {
    input.cancel();
    input = null;
  }
  releaseCapture(panelCapture, "panel");
  panelCapture = null;
  releaseCapture(segmentCapture, "display");
  segmentCapture = null;
  releaseCapture(ringCapture, "ring");
  ringCapture = null;
  closePresentation(segmentPresentation);
  segmentPresentation = null;
  closePresentation(ringPresentation);
  ringPresentation = null;
  capsulad.stop();
}

function main() {
  capsulad.start();
  log("ready app=" + capsulad.env.appId + " run=" + capsulad.env.runId);

  if (typeof capsulad.readPackageFile !== "function") {
    log("unsupported: this QuickJS host has no readPackageFile; rebuild the interpreter host");
    capsulad.stop();
    return;
  }
  try {
    loadTones();
  } catch (err) {
    log("unsupported: " + (err && err.message ? err.message : String(err)));
    capsulad.stop();
    return;
  }

  var unsupported = checkSupport();
  if (unsupported !== null) {
    log("unsupported: " + unsupported);
    capsulad.stop();
    return;
  }

  ringPresentation = openPresentation(RING_RESOURCE);
  if (ringPresentation === null) {
    log("unavailable: the ring presentation did not open");
    shutdown();
    return;
  }
  if (typeof ringPresentation.call.tryRecv !== "function") {
    log("unavailable: this QuickJS host has no Call.tryRecv; rebuild the interpreter host");
    shutdown();
    return;
  }
  ringOff(); // first valid frame makes the capture eligible
  ringCapture = acquireCapture(RING_RESOURCE, ringPresentation.id, "");
  if (ringCapture === null) {
    log("unavailable: the ring capture was refused");
    shutdown();
    return;
  }

  if (segmentSupported) {
    segmentPresentation = openPresentation(SEGMENT_RESOURCE);
    if (segmentPresentation === null) {
      log("score display unavailable; continuing without it");
      segmentSupported = false;
    } else {
      showScore(true);
      segmentCapture = acquireCapture(SEGMENT_RESOURCE, segmentPresentation.id, "");
      if (segmentCapture === null) {
        log("score display capture refused; continuing without it");
        closePresentation(segmentPresentation);
        segmentPresentation = null;
        segmentSupported = false;
      }
    }
  }

  panelCapture = acquireCapture(PANEL_RESOURCE, "", "TOUCH_FULL_RAW");
  if (panelCapture === null) {
    log("unavailable: the full-panel capture was refused");
    shutdown();
    return;
  }

  speaker = openPlayback();
  if (speaker === null) {
    log("unavailable: the speaker playback stream did not open");
    shutdown();
    return;
  }

  subscribeInput();
  log("controls: repeat each colour with its side key; hold the centre to exit");

  startGame();
  if (exiting) {
    shutdown();
    return;
  }
  waitForInput();
  shutdown();
}

main();