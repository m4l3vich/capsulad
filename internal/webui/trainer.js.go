package webui

// trainerJS is the wakeword setup wizard. It owns guided recording and shows
// the device's training progress. All features, fitting and quality gates run
// on the device; the browser only sends the explicit sample selection and
// reads NDJSON progress. No audio or feature vectors reach the browser.
const trainerJS = `
// Six independent word takes keep fitting, threshold tuning and the final
// test separate, with two original recordings in each role.
const MIN = { room: 3, speech: 3, word: 6 };
const GUIDED = { room: 3, speech: 4, word: 6 };

const WORD_VARIATIONS = [
  'in your normal voice.',
  'louder than usual, without shouting.',
  'in a clear whisper.',
  'from a distance of about two metres.',
  'as if calling someone by name.',
  'softly, with your head turned slightly to one side.'
];

// Random everyday Russian phrases for the unrelated-phrase takes. The prompt
// changes for every take so the head does not learn one fixed utterance as the
// only negative class.
const PHRASES = [
  'какая погода в Архангельске?',
  'двадцать долларов в рублях',
  'включи музыку погромче',
  'напомни мне позвонить маме',
  'сколько времени осталось до конца фильма?',
  'поставь будильник на семь утра',
  'расскажи анекдот про кота',
  'который сейчас час в Токио?',
  'найди ближайшую аптеку',
  'что приготовить на ужин из курицы?',
  'какой фильм посмотреть вечером?',
  'сколько километров до Москвы?',
  'переведи слово «здравствуйте» на английский',
  'закажи такси до вокзала',
  'какая завтра будет погода?',
  'включи свет в гостиной',
  'сколько калорий в яблоке?',
  'кто написал «Войну и мир»?',
  'как дела?',
  'позвони в сервисный центр',
  'какой курс евро сегодня?',
  'где находится ближайший банкомат?',
  'расскажи что-нибудь интересное',
  'выключи телевизор через полчаса'
];

// CUE_MS mirrors the daemon's three-second tone countdown. It only offsets the
// progress bar, which otherwise starts while the tones play.
const CUE_MS = 3000;

const KEY = 'capsula.wakeword.setup.v3';
const el = (id) => document.getElementById(id);
const now = () => Date.now();

function loadState() {
  try {
    const s = JSON.parse(sessionStorage.getItem(KEY) || 'null');
    if (s && s.word && s.ids) {
      s.ids.word = s.ids.word || [];
      s.ids.room = s.ids.room || [];
      s.ids.speech = s.ids.speech || [];
      return s;
    }
  } catch (_) {}
  return null;
}
function saveState(s) {
  try { sessionStorage.setItem(KEY, JSON.stringify(s)); } catch (_) {}
}
function clearState() {
  try { sessionStorage.removeItem(KEY); } catch (_) {}
}

const step = { number: 1, word: '', ids: { room: [], speech: [], word: [] }, tested: false };
let running = false;
let trainAbort = null;
let lastPhrase = -1;
let phrase = '';
let lastTake = null;

function randomIndex(n) {
  const buf = new Uint32Array(1);
  try { crypto.getRandomValues(buf); } catch (_) {}
  return (buf[0] || Math.floor(Math.random() * n)) % n;
}

// nextPhrase never repeats the previous take's phrase back to back.
function nextPhrase() {
  let i = randomIndex(PHRASES.length);
  if (PHRASES.length > 1 && i === lastPhrase) i = (i + 1) % PHRASES.length;
  lastPhrase = i;
  phrase = PHRASES[i];
  return phrase;
}
const TITLES = [
  'Name your wakeword and position Capsula',
  'Record the room',
  'Record unrelated phrases',
  'Record the wakeword',
  'Train your wakeword'
];

function ready() {
  return step.ids.word.length >= MIN.word &&
    step.ids.room.length >= MIN.room &&
    step.ids.speech.length >= MIN.speech;
}

function render() {
  el('stepprogress').textContent = 'Step ' + step.number + ' of 5';
  el('steptitle').textContent = TITLES[step.number - 1];
  el('setupform').hidden = step.number !== 1;
  el('recordform').hidden = !(step.number === 2 || step.number === 3 || step.number === 4);
  el('trainstep').hidden = step.number !== 5;
  el('backstep').hidden = step.number === 1 || step.number === 2;
  el('backstep').disabled = running;
  el('restartsetup').hidden = step.number !== 5;
  el('restartsetup').disabled = running;
  el('restarthelp').hidden = step.number !== 5;
  if (step.number >= 2 && step.number <= 4) {
    el('recordlabel').value = step.number === 2 ? 'room' : (step.number === 3 ? 'speech' : 'word');
    el('recordlength').value = step.number === 2 ? '8000' : (step.number === 3 ? '4000' : '2000');
    el('recordbtn').textContent = step.number === 2 ? 'Record room noise' : (step.number === 3 ? 'Record phrase' : 'Record wakeword');
    const done = step.number === 2 ? step.ids.room.length
      : (step.number === 3 ? step.ids.speech.length : step.ids.word.length);
    const want = step.number === 2 ? GUIDED.room : (step.number === 3 ? GUIDED.speech : GUIDED.word);
    el('takeprogress').textContent = done + ' of ' + want + ' takes recorded.';
    let prompt;
    let help = 'Recording starts after a three-second countdown. The ring pulses red while recording.';
    if (step.number === 2) {
      prompt = 'Stay quiet for eight seconds: room tone, fan, traffic.';
    } else if (step.number === 3) {
      nextPhrase();
      prompt = 'Say «' + phrase + '»';
      help = 'The ring pulses red, then a three-second tone countdown marks the start: two short tones and a longer one. Read the phrase in a normal voice.';
    } else {
      prompt = 'Say «' + step.word + '» ' + WORD_VARIATIONS[done % WORD_VARIATIONS.length];
      help = 'The ring pulses red, then a three-second tone countdown marks the start: two short tones and a longer one. Say the complete word once, about half a second after the final tone.';
    }
    el('recordprompt').textContent = prompt;
    el('recordhelp').textContent = help;
  }
  el('retrybtn').hidden = !(lastTake && lastTake.step === step.number);
  if (step.number === 5) {
    el('quietcount').textContent = step.ids.room.length + ' / ' + MIN.room;
    el('phrasecount').textContent = step.ids.speech.length + ' / ' + GUIDED.speech;
    el('wordcount').textContent = step.ids.word.length + ' / ' + GUIDED.word;
    const ok = ready();
    el('trainbtn').disabled = !ok || running || step.tested;
    el('trainhelp').textContent = running
      ? 'Training on Capsula. Keep this tab open; it can take several minutes.'
      : (step.tested
        ? 'The final test has been used. Start a new setup with fresh takes before retraining.'
        : (ok
          ? 'Training uses only the ' + (step.ids.word.length + step.ids.room.length + step.ids.speech.length) +
            ' takes from this setup. Audio stays on the device.'
          : 'Record at least ' + MIN.word + ' wakeword, ' + MIN.room + ' room-noise and ' +
            MIN.speech + ' unrelated-phrase takes. At least three of each background kind are required.'));
  }
  if (el('steptitle').focus) el('steptitle').focus({ preventScroll: true });
}

function advance() {
  if (step.number < 5) step.number++;
  saveState(step);
  render();
}

function countdown(seconds) {
  return new Promise((resolve, reject) => {
    let left = seconds;
    const cancel = el('cancelrecord');
    cancel.hidden = false;
    el('recordstatus').textContent = 'Recording in ' + left + '…';
    const timer = setInterval(() => {
      left--;
      if (left > 0) { el('recordstatus').textContent = 'Recording in ' + left + '…'; return; }
      clearInterval(timer);
      cancel.hidden = true;
      resolve();
    }, 1000);
    cancel.onclick = () => {
      clearInterval(timer);
      cancel.hidden = true;
      el('recordstatus').textContent = 'Recording cancelled.';
      reject(new Error('cancelled'));
    };
  });
}

async function recordTake(label, ms) {
  const bar = el('recordprogress');
  el('recorderror').hidden = true;
  el('recordbtn').disabled = true;
  el('retrybtn').disabled = true;
  el('retrybtn').hidden = true;
  try {
    await countdown(3);
    // The server parses only URL-encoded forms (ParseForm does not decode
    // multipart), so the CSRF field must travel as application/x-www-form-urlencoded.
    const form = new URLSearchParams();
    form.set('csrf', el('csrftoken').value);
    form.set('label', label);
    form.set('duration_ms', String(ms));
    // The device plays the countdown tones before it captures audio. Start the
    // request immediately so the tones begin now, but hold the progress bar
    // until they finish so it tracks the actual take.
    const cueMs = label === 'word' || label === 'speech' ? CUE_MS : 0;
    const request = fetch('/wakeword/record', {
      method: 'POST',
      headers: { 'Accept': 'application/json' },
      body: form
    });
    if (cueMs > 0) {
      el('recordstatus').textContent = 'Three-second tone countdown…';
      await new Promise((resolve) => setTimeout(resolve, cueMs));
    }
    bar.hidden = false; bar.value = 0;
    el('recordstatus').textContent = 'Recording…';
    const started = now();
    const tick = setInterval(() => { bar.value = Math.min(100, ((now() - started) / ms) * 100); }, 100);
    const resp = await request;
    clearInterval(tick);
    bar.value = 100;
    if (!resp.ok) throw new Error(await resp.text());
    const take = await resp.json();
    const id = take.sample && take.sample.sample_id;
    if (!id) throw new Error('the device did not return a sample id');
    const group = step.number === 2 ? 'room' : (step.number === 3 ? 'speech' : 'word');
    step.ids[group].push(id);
    lastTake = { step: step.number, group: group, id: id };
    el('recordstatus').textContent = 'Saved ' + (group === 'word' ? 'wakeword take ' : (group === 'room' ? 'room take ' : 'phrase take ')) + step.ids[group].length + '.';
    el('samplecount').textContent = String(Number(el('samplecount').textContent || '0') + 1);
    saveState(step);
    return true;
  } catch (err) {
    if (String(err.message) !== 'cancelled') {
      el('recorderror').textContent = 'Recording failed: ' + err.message;
      el('recorderror').hidden = false;
    }
    el('recordstatus').textContent = 'Ready when you are.';
    return false;
  } finally {
    el('recordbtn').disabled = false;
    el('retrybtn').disabled = false;
    el('retrybtn').hidden = !(lastTake && lastTake.step === step.number);
    setTimeout(() => { bar.hidden = true; }, 400);
  }
}

async function onRecord(event) {
  event.preventDefault();
  const label = step.number === 4 ? 'word' : (step.number === 3 ? 'speech' : 'room');
  const ms = Number(el('recordlength').value);
  if (!(await recordTake(label, ms))) return;
  if (step.number === 2 && step.ids.room.length >= GUIDED.room) { advance(); return; }
  if (step.number === 3 && step.ids.speech.length >= GUIDED.speech) { advance(); return; }
  if (step.number === 4 && step.ids.word.length >= GUIDED.word) { advance(); return; }
  render();
}

// retryLastTake records a replacement before deleting the previous take, so a
// cancelled or failed retry leaves the existing selection untouched.
async function retryLastTake() {
  if (!lastTake || lastTake.step !== step.number) return;
  const previous = lastTake;
  const label = step.number === 4 ? 'word' : (step.number === 3 ? 'speech' : 'room');
  const ms = Number(el('recordlength').value);
  if (!(await recordTake(label, ms))) return;
  const form = new URLSearchParams();
  form.set('csrf', el('csrftoken').value);
  form.set('sample_id', previous.id);
  try {
    const resp = await fetch('/wakeword/delete', {
      method: 'POST',
      headers: { 'Accept': 'application/json' },
      body: form
    });
    if (!resp.ok) throw new Error(await resp.text());
  } catch (err) {
    el('recorderror').textContent = 'The replacement take was saved, but the previous take could not be deleted: ' + err.message;
    el('recorderror').hidden = false;
    render();
    return;
  }
  step.ids[previous.group] = step.ids[previous.group].filter((id) => id !== previous.id);
  el('samplecount').textContent = String(Math.max(0, Number(el('samplecount').textContent || '0') - 1));
  saveState(step);
  render();
}

function randomSeed() {
  const buf = new Uint32Array(1);
  try { crypto.getRandomValues(buf); } catch (_) {}
  return buf[0] || (Date.now() & 0xffffffff) || 1;
}

function setProgress(fraction, text) {
  const bar = el('trainprogress');
  if (typeof fraction === 'number' && isFinite(fraction)) {
    bar.hidden = false;
    bar.value = Math.max(0, Math.min(100, fraction * 100));
  }
  if (typeof text === 'string') el('trainstatus').textContent = text;
}

function logLine(text) {
  const box = el('trainlog');
  box.textContent += text + "\n";
  box.scrollTop = box.scrollHeight;
}

function finish() {
  running = false;
  trainAbort = null;
  el('cancelbtn').hidden = true;
  render();
}

async function run() {
  if (running || !ready() || step.tested) return;
  running = true;
  el('trainbtn').disabled = true;
  el('trainlog').textContent = '';
  el('trainerror').hidden = true;
  el('cancelbtn').hidden = false;
  el('trainprogress').hidden = false;
  el('trainprogress').value = 0;
  setProgress(0, 'Preparing on-device training…');
  trainAbort = new AbortController();
  const controller = trainAbort;
  try {
    const form = new URLSearchParams();
    form.set('csrf', el('csrftoken').value);
    form.set('word', step.word);
    form.set('seed', String(randomSeed()));
    // Explicit selection only: never fall back to older saved recordings.
    form.set('word_ids', step.ids.word.join(','));
    form.set('room_ids', step.ids.room.join(','));
    form.set('speech_ids', step.ids.speech.join(','));
    form.set('false_ids', '');
    const resp = await fetch('/wakeword/train', {
      method: 'POST',
      headers: { 'Accept': 'application/x-ndjson' },
      body: form,
      signal: controller.signal
    });
    if (!resp.ok) {
      const text = await resp.text();
      let message = text.trim();
      try {
        const first = message.split('\n')[0];
        const parsed = JSON.parse(first);
        if (parsed && parsed.log) message = parsed.log;
      } catch (_) {}
      throw new Error(message || ('training request failed (' + resp.status + ')'));
    }
    if (!resp.body || !resp.body.getReader) throw new Error('this browser cannot read training progress');
    const reader = resp.body.getReader();
    const decoder = new TextDecoder();
    let buffer = '';
    let failed = null;
    let installed = false;
    for (;;) {
      const part = await reader.read();
      if (part.done) break;
      buffer += decoder.decode(part.value, { stream: true });
      let nl;
      while ((nl = buffer.indexOf('\n')) >= 0) {
        const line = buffer.slice(0, nl).trim();
        buffer = buffer.slice(nl + 1);
        if (!line) continue;
        let ev;
        try { ev = JSON.parse(line); } catch (_) { continue; }
        if (ev.stage === 'finaltest' || ev.stage === 'installed') {
          step.tested = true;
          saveState(step);
        }
        if (ev.stage === 'error') {
          failed = ev.log || 'training failed';
          continue;
        }
        if (ev.log) logLine(ev.log);
        if (typeof ev.fraction === 'number') {
          setProgress(ev.fraction, ev.stage === 'installed'
            ? 'Installed.'
            : 'On device: ' + (ev.log || ev.stage) + '…');
        }
        if (ev.stage === 'installed' && ev.classifier) {
          installed = true;
          const c = ev.classifier;
          logLine('Installed ' + c.model_id + ' (threshold ' + Number(c.threshold).toFixed(2) + ').');
          setProgress(1, 'Installed. Reload this page to list it, then choose it in Settings and enable detection.');
          el('trainerror').hidden = true;
        }
      }
    }
    if (failed) throw new Error(failed);
    if (!installed) throw new Error('training ended without installing a model');
  } catch (err) {
    const aborted = controller.signal.aborted || String(err.name) === 'AbortError';
    if (!aborted) {
      const message = String(err.message || err);
      el('trainerror').textContent = message;
      el('trainerror').hidden = false;
      logLine('error: ' + message);
      setProgress(undefined, 'Training failed or disconnected. Reload to check the installed model.');
    } else {
      setProgress(undefined, 'Training request cancelled. Reload to check whether installation had already completed.');
    }
  } finally {
    finish();
  }
}

document.addEventListener('DOMContentLoaded', () => {
  const saved = loadState();
  if (saved) {
    step.number = saved.number || 1;
    step.word = saved.word || '';
    step.ids = saved.ids;
    step.tested = !!saved.tested;
    const name = el('wordname');
    if (name) name.value = step.word;
    if (step.number > 1) {
      const box = el('placement');
      if (box) box.checked = true;
    }
  }
  // A take deleted in Manage recordings must not stay in the selection: the
  // setup would otherwise fail much later with a stale sample id.
  const present = new Set(Array.from(document.querySelectorAll('#samplelist tr[data-sample-id]'))
    .map((tr) => tr.getAttribute('data-sample-id')));
  const keep = (ids) => ids.filter((id) => present.has(id));
  step.ids.word = keep(step.ids.word);
  step.ids.room = keep(step.ids.room);
  step.ids.speech = keep(step.ids.speech);
  if (saved) saveState(step);
  el('setupform')?.addEventListener('submit', (e) => {
    e.preventDefault();
    const name = el('wordname');
    if (!name || !name.value.trim()) { name?.focus(); return; }
    step.word = name.value.trim();
    step.number = 2;
    saveState(step);
    render();
  });
  el('recordform')?.addEventListener('submit', onRecord);
  el('retrybtn')?.addEventListener('click', retryLastTake);
  el('trainbtn')?.addEventListener('click', run);
  el('cancelbtn')?.addEventListener('click', () => {
    if (trainAbort) trainAbort.abort();
  });
  el('backstep')?.addEventListener('click', () => {
    step.number = Math.max(2, step.number - 1);
    saveState(step);
    render();
  });
  el('restartsetup')?.addEventListener('click', () => {
    clearState();
    step.number = 1;
    step.word = '';
    step.ids = { room: [], speech: [], word: [] };
    step.tested = false;
    lastTake = null;
    el('trainlog').textContent = '';
    el('trainerror').hidden = true;
    setProgress(0, '');
    el('trainprogress').hidden = true;
    const name = el('wordname');
    if (name) name.value = '';
    const box = el('placement');
    if (box) box.checked = false;
    render();
  });
  render();
});
`
