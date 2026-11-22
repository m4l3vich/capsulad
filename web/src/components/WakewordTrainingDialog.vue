<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, shallowRef, watch } from 'vue'
import { socket } from '../lib/ws.js'
import { dateTime, durationMs } from '../lib/format.js'
import Icon from './Icon.vue'

const props = defineProps({
  samples: { type: Array, default: () => [] },
  samplesLoaded: { type: Boolean, default: false },
  micLive: { type: Boolean, default: false },
  micReason: { type: String, default: '' }
})

const emit = defineEmits(['close', 'refresh'])

const STORAGE_KEY = 'capsula.wakeword.setup.v5'
const CUE_MS = 3000

const STEPS = [
  { key: 'setup', label: 'Setup', title: 'What\'s the word?' },
  { key: 'room', label: 'Room', title: 'Reading the room' },
  { key: 'speech', label: 'Phrases', title: 'Talk to me' },
  { key: 'word', label: 'Wakeword', title: 'Say the word' },
  { key: 'review', label: 'Review', title: 'Review and training' }
]

const GROUPS = {
  room: {
    title: 'Room-noise recordings',
    short: 'room noise',
    durationMs: 8000,
    target: 3,
    minimum: 3,
    recordLabel: 'room'
  },
  speech: {
    title: 'Everyday-phrase recordings',
    short: 'a phrase',
    durationMs: 4000,
    target: 4,
    minimum: 3,
    recordLabel: 'speech'
  },
  word: {
    title: 'Wakeword recordings',
    short: 'the wakeword',
    durationMs: 2000,
    target: 6,
    minimum: 6,
    recordLabel: 'word'
  }
}

const PHRASES = {
  ru: [
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
  ],
  en: [
    'what is the weather in Arkhangelsk?',
    'how much is twenty dollars in rubles?',
    'turn the music up a little',
    'remind me to call my mother',
    'how long is left in the film?',
    'set an alarm for seven in the morning',
    'tell me a joke about a cat',
    'what time is it in Tokyo?',
    'find the nearest pharmacy',
    'what can I cook with chicken tonight?',
    'which film should I watch tonight?',
    'how far is it to Moscow?',
    'translate “good morning” into Russian',
    'order a taxi to the station',
    'what will the weather be tomorrow?',
    'turn on the light in the living room',
    'how many calories are in an apple?',
    'who wrote “War and Peace”?',
    'how are you?',
    'call the service centre',
    'what is the euro exchange rate today?',
    'where is the nearest cash machine?',
    'tell me something interesting',
    'turn off the television in half an hour'
  ]
}

const WORD_VARIATIONS = {
  ru: [
    (word) => `Say «${word}» in your normal voice.`,
    (word) => `Say «${word}» louder than usual, without shouting.`,
    (word) => `Say «${word}» in a clear whisper.`,
    (word) => `Say «${word}» from about two metres away.`,
    (word) => `Say «${word}» as if calling someone by name.`,
    (word) => `Say «${word}» softly, with your head turned slightly to one side.`
  ],
  en: [
    (word) => `Say “${word}” in your normal voice.`,
    (word) => `Say “${word}” louder than usual, without shouting.`,
    (word) => `Say “${word}” in a clear whisper.`,
    (word) => `Say “${word}” from about two metres away.`,
    (word) => `Say “${word}” as if calling someone by name.`,
    (word) => `Say “${word}” softly, with your head turned slightly to one side.`
  ]
}

const STAGE_LABELS = {
  load: 'Getting ready to listen',
  validate: 'Checking the selected recordings',
  prepare: 'Preparing recordings',
  augment: 'Making varied copies of the recordings',
  embed: 'Listening for speech patterns',
  negatives: 'Studying room noise and other speech',
  train: 'Teaching Capsula your wakeword',
  calibrate: 'Calibrating detection sensitivity',
  finaltest: 'Final check on held-back recordings',
  install: 'Saving your wakeword',
  installed: 'Wakeword ready'
}

function emptySetup() {
  return {
    version: 5,
    word: '',
    language: 'ru',
    placement: false,
    step: 'setup',
    completed: false,
    finalTestConsumed: false,
    ids: { room: [], speech: [], word: [] }
  }
}

function loadSetup() {
  const base = emptySetup()
  try {
    const saved = JSON.parse(sessionStorage.getItem(STORAGE_KEY) || 'null')
    if (!saved || typeof saved !== 'object') return base
    const word = typeof saved.word === 'string' ? saved.word.slice(0, 100) : ''
    const language = saved.language === 'en' ? 'en' : 'ru'
    const step = STEPS.some((s) => s.key === saved.step) ? saved.step : 'setup'
    const ids = { room: [], speech: [], word: [] }
    for (const group of Object.keys(ids)) {
      const list = saved.ids?.[group]
      if (Array.isArray(list)) {
        ids[group] = [...new Set(list.filter((id) => typeof id === 'string' && id))]
      }
    }
    return {
      ...base,
      word,
      language,
      step,
      placement: Boolean(saved.placement),
      completed: Boolean(saved.completed),
      finalTestConsumed: Boolean(saved.finalTestConsumed),
      ids
    }
  } catch (_) {
    return base
  }
}

const dialogEl = ref(null)
const headingEl = ref(null)
const setup = reactive(loadSetup())
const stepIndex = ref(Math.max(0, STEPS.findIndex((s) => s.key === setup.step)))
const wordDraft = ref(setup.word)
const languageDraft = ref(setup.language)

const recording = shallowRef(null)
const recordElapsed = ref(0)
const takeStatus = ref('')
const takeError = ref('')

const training = reactive({
  running: false,
  cancelling: false,
  id: '',
  stage: '',
  fraction: 0,
  logs: [],
  error: '',
  installed: null
})

const closing = ref(false)
const speechPhrase = ref('')

let timer = null
let offTraining = null
let offState = null
let disposed = false
let orphanTrainingId = ''
let cancelRequested = false
let cancelAcknowledged = false
let cancelBackstop = null
let closeBackstop = null

const stepKey = computed(() => STEPS[stepIndex.value]?.key || 'setup')
const spec = computed(() => GROUPS[stepKey.value] || null)
const stepDescription = computed(() => {
  switch (stepKey.value) {
    case 'setup': return 'Pick a word that will be used to summon an assistant'
    case 'room': return 'Let Capsula hear the normal background of the room.'
    case 'speech': return 'Capsula needs to hear ordinary speech that is not your wakeword.'
    case 'word': return 'Record the word six times, in the variations shown.'
    case 'review': return 'If everything\'s good, we\'ll start the training.'
    default: return ''
  }
})
const stepNumber = computed(() => stepIndex.value + 1)
const busy = computed(() => Boolean(recording.value) || training.running || training.cancelling || closing.value)

function saveSetup() {
  try {
    sessionStorage.setItem(STORAGE_KEY, JSON.stringify({
      version: 5,
      word: setup.word,
      language: setup.language,
      step: STEPS[stepIndex.value]?.key || 'setup',
      completed: setup.completed,
      finalTestConsumed: setup.finalTestConsumed,
      ids: setup.ids
    }))
  } catch (_) { }
}

function randomIndex(count) {
  if (count <= 0) return 0
  try {
    const buffer = new Uint32Array(1)
    crypto.getRandomValues(buffer)
    return buffer[0] % count
  } catch (_) {
    return Math.floor(Math.random() * count)
  }
}

function nextPhrase(previous = '') {
  const list = PHRASES[setup.language] || PHRASES.ru
  if (!list.length) return ''
  if (list.length === 1) return list[0]
  let phrase = list[randomIndex(list.length)]
  if (phrase === previous) phrase = list[(list.indexOf(phrase) + 1) % list.length]
  return phrase
}

const counts = computed(() => ({
  room: setup.ids.room.length,
  speech: setup.ids.speech.length,
  word: setup.ids.word.length
}))

const readyToTrain = computed(() =>
  props.samplesLoaded &&
  !setup.completed &&
  !setup.finalTestConsumed &&
  !training.running &&
  !training.cancelling &&
  !recording.value &&
  counts.value.word >= GROUPS.word.minimum &&
  counts.value.room >= GROUPS.room.minimum &&
  counts.value.speech >= GROUPS.speech.minimum
)

const missingForTraining = computed(() => {
  const missing = []
  if (counts.value.word < GROUPS.word.minimum) missing.push(`${GROUPS.word.minimum - counts.value.word} more wakeword recording${GROUPS.word.minimum - counts.value.word === 1 ? '' : 's'}`)
  if (counts.value.room < GROUPS.room.minimum) missing.push(`${GROUPS.room.minimum - counts.value.room} more room recording${GROUPS.room.minimum - counts.value.room === 1 ? '' : 's'}`)
  if (counts.value.speech < GROUPS.speech.minimum) missing.push(`${GROUPS.speech.minimum - counts.value.speech} more phrase recording${GROUPS.speech.minimum - counts.value.speech === 1 ? '' : 's'}`)
  return missing
})

function sampleFor(id) {
  return props.samples.find((sample) => sample.sampleId === id) || null
}

function describeSample(id) {
  const sample = sampleFor(id)
  if (!sample) return 'Saved on the device'
  return `${dateTime(sample.recordedUnixMs)} · ${durationMs(sample.durationMs)}`
}

function languageName(code) {
  if (!code) return ''
  const value = String(code)
  if (value.toLowerCase() === 'ru') return 'Russian'
  if (value.toLowerCase() === 'en') return 'English'
  if (value.toLowerCase() === 'any') return 'Any language'
  return value
}

function groupIds(group) {
  return setup.ids[group] || []
}

const prompt = computed(() => {
  if (stepKey.value === 'room') return 'Don\'t say anything yet.'
  if (stepKey.value === 'speech') return speechPhrase.value ? `Say «${speechPhrase.value}»` : 'Say an everyday phrase.'
  if (stepKey.value === 'word') {
    const word = (setup.word || wordDraft.value).trim() || 'your wakeword'
    const variations = WORD_VARIATIONS[setup.language] || WORD_VARIATIONS.ru
    return variations[counts.value.word % variations.length](word)
  }
  return ''
})

const promptHint = computed(() => {
  if (stepKey.value === 'room') return 'Leave the room sounding normal: fan, traffic, fridge. Do not speak.'
  if (stepKey.value === 'speech') return 'Read the phrase in a normal voice. Do not say your wakeword in this recording.'
  if (stepKey.value === 'word') return 'Wait for the final, longer tone, then say the complete word once, about half a second later.'
  return ''
})

const recordingPhase = computed(() => (recordElapsed.value < CUE_MS ? 'cue' : 'capture'))
const cueLeft = computed(() => Math.max(0, Math.ceil((CUE_MS - recordElapsed.value) / 1000)))
const captureLeft = computed(() => {
  const duration = recording.value?.durationMs || 0
  return Math.max(0, Math.ceil((duration - (recordElapsed.value - CUE_MS)) / 1000))
})
const recordProgress = computed(() => {
  const total = CUE_MS + (recording.value?.durationMs || 0)
  if (!total) return 0

  const pureRecordingTime = recordElapsed.value - CUE_MS
  if (pureRecordingTime < 0) return 0
  return Math.min(100, (pureRecordingTime / recording.value?.durationMs) * 100)
})

const percent = computed(() => Math.min(100, Math.max(0, Math.round((Number(training.fraction) || 0) * 100))))
const stageLabel = computed(() => {
  if (training.cancelling) return 'Stopping on Capsula…'
  const stage = String(training.stage || '')
  return STAGE_LABELS[stage] || (stage ? stage.replaceAll('_', ' ') : 'Preparing on Capsula…')
})

const canAdvance = computed(() => {
  if (busy.value) return false
  if (stepKey.value === 'setup') return Boolean(wordDraft.value.trim())
  if (stepKey.value === 'room') return counts.value.room >= GROUPS.room.minimum
  if (stepKey.value === 'speech') return counts.value.speech >= GROUPS.speech.minimum
  if (stepKey.value === 'word') return counts.value.word >= GROUPS.word.minimum
  return false
})

function stepClass(index) {
  if (index === stepIndex.value) return 'badge-primary'
  if (index < stepIndex.value) return 'badge-success'
  return 'badge-ghost bg-base-300'
}

function focusHeading() {
  nextTick(() => headingEl.value?.focus())
}

function nextStep() {
  if (!canAdvance.value) return
  if (stepKey.value === 'setup') {
    const nextWord = wordDraft.value.trim()
    const nextLanguage = languageDraft.value
    if (nextWord !== setup.word && setup.ids.word.length) {
      setup.ids.word = []
      takeStatus.value = 'The wakeword changed, so earlier wakeword recordings are no longer selected. Record new ones for the new word.'
    }
    setup.word = nextWord
    const languageChanged = nextLanguage !== setup.language
    setup.language = nextLanguage
    if (languageChanged || !speechPhrase.value) speechPhrase.value = nextPhrase()
  }
  if (stepIndex.value < STEPS.length - 1) stepIndex.value += 1
  saveSetup()
  focusHeading()
}

function previousStep() {
  if (busy.value || stepIndex.value === 0) return
  stepIndex.value -= 1
  saveSetup()
  focusHeading()
}

function saveSelection() {
  saveSetup()
}

function startRecordingPhase(group, replaceId = '') {
  takeError.value = ''
  takeStatus.value = ''
  const groupSpec = GROUPS[group]
  if (!groupSpec) return null
  const phrase = group === 'speech' ? (speechPhrase.value || nextPhrase()) : ''
  if (group === 'speech') speechPhrase.value = phrase
  let takePrompt = ''
  if (group === 'room') {
    takePrompt = 'Stay quiet for the whole recording.'
  } else if (group === 'speech') {
    takePrompt = `Say «${phrase}»`
  } else {
    const word = (setup.word || wordDraft.value).trim() || 'your wakeword'
    const variations = WORD_VARIATIONS[setup.language] || WORD_VARIATIONS.ru
    takePrompt = variations[counts.value.word % variations.length](word)
  }
  const active = {
    group,
    replaceId,
    label: groupSpec.recordLabel,
    durationMs: groupSpec.durationMs,
    phrase,
    prompt: takePrompt,
    controller: new AbortController(),
    cancelled: false,
    disconnected: false,
    startedAt: performance.now()
  }
  recording.value = active
  recordElapsed.value = 0
  timer = setInterval(() => {
    if (!recording.value) return
    recordElapsed.value = performance.now() - recording.value.startedAt
  }, 10)
  return active
}

async function recordTake(group, replaceId = '') {
  if (busy.value) return
  if (!props.micLive) {
    takeError.value = props.micReason || 'The microphone is not ready.'
    return
  }
  const active = startRecordingPhase(group, replaceId)
  if (!active) return
  takeStatus.value = 'Capsula is showing the recording cue. Wait for the final tone before speaking.'
  try {
    const result = await socket.request(
      { recordSample: { label: active.label, durationMs: active.durationMs } },
      { timeout: active.durationMs + 25000, signal: active.controller.signal }
    )
    const sample = result?.sample?.sample
    if (!sample?.sampleId) throw new Error('the device did not return a saved recording')
    if (active.cancelled || disposed) {
      await discardSample(sample.sampleId)
      return
    }
    if (replaceId) {
      setup.ids[group] = setup.ids[group].filter((id) => id !== replaceId)
    }
    setup.ids[group] = [...setup.ids[group], sample.sampleId]
    saveSelection()
    if (group === 'speech') speechPhrase.value = nextPhrase(active.phrase)
    takeStatus.value = replaceId
      ? 'Replacement saved; the previous recording is no longer selected.'
      : `Take ${setup.ids[group].length} saved on Capsula.`
    if (counts.value[group] >= GROUPS[group].target) {
      takeStatus.value = `${GROUPS[group].title} complete. Continue when ready.`
    }
    emit('refresh')
    if (replaceId) {
      try {
        await socket.request({ deleteSample: { sampleId: replaceId } }, { timeout: 10000 })
      } catch (err) {
        takeStatus.value = `The replacement is selected, but the old recording could not be deleted: ${err.message}`
      }
      emit('refresh')
    }
  } catch (err) {
    if (active.cancelled || err?.name === 'AbortError') {
      takeStatus.value = 'Recording cancelled.'
    } else {
      takeStatus.value = ''
      takeError.value = active.disconnected
        ? 'The connection to Capsula closed before the recording finished.'
        : `The recording failed: ${err.message}`
    }
  } finally {
    if (timer) {
      clearInterval(timer)
      timer = null
    }
    recordElapsed.value = 0
    if (recording.value === active) recording.value = null
    saveSelection()
    if (closing.value && !training.running && !training.cancelling) closeDialog()
  }
}

async function discardSample(sampleId) {
  if (!sampleId) return
  try {
    await socket.request({ deleteSample: { sampleId } }, { timeout: 10000 })
    emit('refresh')
  } catch (_) {
    // A disconnected or already-removed take is not recoverable here; the
    // unselected file remains visible in the saved-takes list.
  }
}

function cancelTake() {
  const active = recording.value
  if (!active) return
  active.cancelled = true
  takeStatus.value = 'Cancelling the recording…'
  try {
    active.controller.abort()
  } catch (_) { }
  if (closing.value) {
    clearCloseBackstop()
    closeBackstop = setTimeout(() => closeDialog(), 12000)
  }
}

function resetTrainingState() {
  training.running = false
  training.cancelling = false
  training.id = ''
  training.stage = ''
  training.fraction = 0
  training.logs = []
  training.error = ''
  training.installed = null
  cancelRequested = false
  cancelAcknowledged = false
  clearCancelBackstop()
  offTraining?.()
  offTraining = null
}

async function startTraining() {
  if (!readyToTrain.value) return
  resetTrainingState()
  training.running = true
  training.stage = ''
  training.fraction = 0
  takeError.value = ''
  takeStatus.value = ''
  try {
    const result = await socket.request({
      train: {
        displayName: setup.word,
        language: setup.language,
        seed: randomSeed(),
        wordSampleIds: [...setup.ids.word],
        roomSampleIds: [...setup.ids.room],
        speechSampleIds: [...setup.ids.speech],
        falseSampleIds: []
      }
    }, { timeout: 20000 })
    const id = result?.training?.trainingId
    if (!id) throw new Error('the device did not return a training id')
    training.id = id
    if (disposed || socket.state !== 'open') {
      socket.request({ cancelTraining: { trainingId: id } }, { timeout: 8000 }).catch(() => { })
      training.running = false
      training.cancelling = false
      return
    }
    offTraining?.()
    offTraining = socket.onTraining(id, handleProgress)
    if (cancelRequested) {
      training.cancelling = false
      cancelTraining()
    }
  } catch (err) {
    training.running = false
    training.cancelling = false
    training.error = err.message
    cancelRequested = false
    emit('refresh')
    if (closing.value) closeDialog()
  }
}

function randomSeed() {
  try {
    const buffer = new Uint32Array(1)
    crypto.getRandomValues(buffer)
    return buffer[0] || 1
  } catch (_) {
    return (Date.now() & 0xffffffff) || 1
  }
}

function handleProgress(progress) {
  if (disposed) return
  if (!progress) return
  if (progress.trainingId && training.id && progress.trainingId !== training.id) return
  const stage = String(progress.stage || '')
  if (stage === 'error') {
    const stopped = cancelRequested || training.cancelling || /cancel/i.test(String(progress.log || ''))
    training.running = false
    training.cancelling = false
    training.error = stopped
      ? 'Training stopped. If you stopped it near the final step, refresh and check the current wakeword.'
      : (progress.log || 'Training stopped before a new wakeword was confirmed.')
    training.installed = null
    cancelRequested = false
    clearCancelBackstop()
    offTraining?.()
    offTraining = null
    emit('refresh')
    if (closing.value) closeDialog()
    return
  }
  if (progress.log) training.logs.push(progress.log)
  if (stage === 'finaltest' && !setup.finalTestConsumed) {
    setup.finalTestConsumed = true
    saveSetup()
  }
  if (stage === 'installed') {
    training.running = false
    training.cancelling = false
    training.stage = stage
    training.fraction = 1
    training.installed = progress.classifier || {}
    setup.completed = true
    saveSetup()
    emit('refresh')
    clearCancelBackstop()
    offTraining?.()
    offTraining = null
    if (closing.value) closeDialog()
    return
  }
  if (stage) training.stage = stage
  training.fraction = Number(progress.fraction) || 0
}

async function cancelTraining() {
  if (training.cancelling) return
  if (!training.running && !training.id) {
    if (closing.value) closeDialog()
    return
  }
  training.cancelling = true
  clearCancelBackstop()
  if (!training.id) {
    cancelRequested = true
    training.logs.push('Cancellation requested; waiting for Capsula to confirm.')
    return
  }
  cancelRequested = true
  try {
    await socket.request({ cancelTraining: { trainingId: training.id } }, { timeout: 10000 })
    // The device may have committed just before the cancellation reached it;
    // refresh the authoritative model list instead of assuming cancellation won.
    cancelAcknowledged = true
    emit('refresh')
    cancelBackstop = setTimeout(() => {
      if (!disposed && training.cancelling) {
        training.running = false
        training.cancelling = false
        training.error = 'Capsula did not confirm whether training stopped or finished. Close this dialog and check the current wakeword.'
        offTraining?.()
        offTraining = null
        emit('refresh')
        if (closing.value) closeDialog()
      }
    }, 15000)
  } catch (err) {
    training.running = false
    training.cancelling = false
    training.error = `Training could not be stopped from this page: ${err.message}. It may still be running or may have finished; check the current wakeword after reconnecting.`
    cancelRequested = false
    offTraining?.()
    offTraining = null
    emit('refresh')
    if (closing.value) closeDialog()
  }
}

function clearCancelBackstop() {
  if (cancelBackstop) {
    clearTimeout(cancelBackstop)
    cancelBackstop = null
  }
}

function clearCloseBackstop() {
  if (closeBackstop) {
    clearTimeout(closeBackstop)
    closeBackstop = null
  }
}

function onSocketState(state) {
  if (state === 'open') {
    if (orphanTrainingId) {
      const id = orphanTrainingId
      orphanTrainingId = ''
      socket.request({ cancelTraining: { trainingId: id } }, { timeout: 8000 }).catch(() => { })
    }
    emit('refresh')
    return
  }
  if (state !== 'closed' && state !== 'reconnecting' && state !== 'expired') return
  if (recording.value) {
    recording.value.disconnected = true
    cancelTake()
  }
  if (training.running) {
    orphanTrainingId = training.id
    training.running = false
    training.cancelling = false
    training.error = 'The connection to Capsula closed. Training may have stopped or finished; check the current wakeword after reconnecting.'
    clearCancelBackstop()
    offTraining?.()
    offTraining = null
    emit('refresh')
  }
  if (closing.value && !recording.value) closeDialog()
}

function requestClose() {
  if (closing.value) return
  const trainingNeedsWait = !cancelAcknowledged && (training.running || training.cancelling)
  if (recording.value || trainingNeedsWait) {
    closing.value = true
    cancelTake()
    if (!cancelAcknowledged && training.running) cancelTraining()
    clearCloseBackstop()
    closeBackstop = setTimeout(() => closeDialog(), 16000)
    return
  }
  closeDialog()
}

function closeDialog() {
  if (disposed) return
  clearCloseBackstop()
  try {
    dialogEl.value?.close()
  } catch (_) { }
  emit('close')
}

function onCancel(event) {
  event?.preventDefault?.()
  requestClose()
}

function pruneSelection() {
  if (!props.samplesLoaded) return
  const present = new Set(props.samples.map((sample) => sample.sampleId))
  let changed = false
  for (const group of ['room', 'speech', 'word']) {
    const next = setup.ids[group].filter((id) => present.has(id))
    if (next.length !== setup.ids[group].length) {
      setup.ids[group] = next
      changed = true
    }
  }
  if (changed) saveSelection()
}

watch(() => props.samples, pruneSelection, { deep: true })
watch(() => props.samplesLoaded, pruneSelection, { immediate: true })

onMounted(() => {
  speechPhrase.value = nextPhrase()
  offState = socket.on('state', onSocketState)
  try {
    dialogEl.value?.showModal()
  } catch (_) { }
  focusHeading()
})

onBeforeUnmount(() => {
  disposed = true
  if (timer) {
    clearInterval(timer)
    timer = null
  }
  clearCancelBackstop()
  clearCloseBackstop()
  if (recording.value) {
    recording.value.cancelled = true
    try {
      recording.value.controller.abort()
    } catch (_) { }
  }
  if ((training.running || training.cancelling) && !cancelAcknowledged) {
    cancelRequested = true
    if (training.id) {
      socket.request({ cancelTraining: { trainingId: training.id } }, { timeout: 5000 }).catch(() => { })
    }
  }
  offTraining?.()
  offTraining = null
  offState?.()
  offState = null
  saveSetup()
})
</script>

<template>
  <dialog
    ref="dialogEl"
    class="modal"
    aria-labelledby="wakeword-dialog-title"
    @cancel="onCancel"
  >
    <div class="modal-box flex max-h-[calc(100dvh-2rem)] w-full max-w-xl flex-col overflow-hidden p-0 sm:max-h-[54rem] sm:w-[min(96vw,54rem)]">
      <header class="border-b border-base-300 px-5 py-4">
        <div class="flex shrink-0 items-start gap-3">
          <div class="min-w-0 flex-1">
            <h2 id="wakeword-dialog-title" ref="headingEl" tabindex="-1" class="text-lg font-semibold sm:text-xl">
              {{ STEPS[stepIndex].title }}
            </h2>
            <p class="mt-1 hidden text-sm opacity-70 [@media(min-height:420px)]:block">{{ stepDescription }}</p>
          </div>
          <button
            type="button"
            class="btn btn-ghost btn-circle"
            aria-label="Close wakeword setup"
            :disabled="closing"
            @click="requestClose"
          >✕</button>
        </div>
        <ol class="flex flex-wrap items-center gap-1 mt-1" aria-label="Setup progress">
          <li v-for="(step, index) in STEPS" :key="step.key" class="flex-grow">
            <span class="badge badge-xs w-full h-[6px]" :class="stepClass(index)"/>
          </li>
        </ol>
      </header>

      <div class="min-h-0 flex-1 overflow-y-auto overscroll-contain px-5 py-4 sm:py-5">
        <div v-if="closing" class="alert alert-warning mb-4" role="status">
          <span>{{ recording ? 'Cancelling the recording…' : 'Stopping training…' }} Capsula will confirm whether it stopped or finished.</span>
        </div>
        <div v-if="takeError" class="alert alert-error mb-4" role="alert">
          <span>{{ takeError }}</span>
        </div>

        <!-- Setup -->
        <div v-if="stepKey === 'setup'" class="grid gap-4">
          <div class="rounded-box bg-base-200 p-4 text-sm">
            <p class="font-medium text-lg">Before we begin</p>
            <p class="mt-2 opacity-80">
              Place the device in its usual place. Before recording it will produce three short tones.
            </p>
            <p class="mt-2 opacity-80">
              For the wakeword it's recommended to use a word with a fricative sound towards its end, because it's easier for algorithm to detect in a sound stream. Examples you probably know: "Крыса", "Маруся", "Алиса", "Яндекс", "Салют", "Сири", "Алекса".
            </p>
            <p class="mt-2 opacity-80">
              If you don't like any of the takes or want to re-record a take — no big deal, you can just delete a take and record again. No pressure.
            </p>

            <p class="font-medium mt-3">What will we record</p>
            <ul class="mt-2 list-disc space-y-1 pl-5 opacity-80">
              <li>Three room recordings of eight seconds each: background only.</li>
              <li>Four everyday phrases of four seconds each: ordinary speech without your wakeword.</li>
              <li>Six short wakeword recordings of two seconds each, in different ways of speaking.</li>
            </ul>
          </div>

          <div class="grid gap-4 sm:grid-cols-2">
            <label class="form-control w-full">
              <span class="label-text font-medium">Which word should wake Capsula?</span>
              <input
                v-model="wordDraft"
                type="text"
                class="input input-bordered mt-1 w-full"
                maxlength="100"
                autocomplete="off"
                placeholder="For example, Маруся"
              />
              <span class="label-text-alt mt-1 opacity-70">Use a distinctive word you can say naturally. Keep saying the same word throughout setup.</span>
            </label>
            <label class="form-control w-full">
              <span class="label-text font-medium">Language</span>
              <select v-model="languageDraft" class="select select-bordered mt-1 w-full">
                <option value="ru">Russian</option>
                <option value="en">English</option>
              </select>
              <span class="label-text-alt mt-1 opacity-70">Pick the language you will speak during the recordings.</span>
            </label>
          </div>
        </div>

        <!-- Recording steps -->
        <div v-else-if="stepKey !== 'review'" class="grid gap-4">
          <div>
            <p class="text-sm font-semibold text-primary">
              {{ spec.title }} · recording {{ Math.min(counts[stepKey] + 1, spec.target) }} of {{ spec.target }}
            </p>
            <h3 class="mt-1 text-xl font-semibold">{{ prompt }}</h3>
            <p class="mt-2 text-sm opacity-70">{{ promptHint }}</p>
            <p class="mt-2 text-sm opacity-70">You can record more takes than required for better detection accuracy.</p>

            <div class="mt-12 mb-8 flex flex-wrap justify-center gap-2 relative">
              <div
                v-if="recordProgress"
                class="radial-progress absolute -top-[1rem] pointer-events-none"
                style="--size:6rem; --thickness: 0.4rem;"
                :style="{ '--value': recordProgress }"
                :aria-valuenow="recordProgress"
                role="progressbar"
              />
              <button
                type="button"
                class="btn btn-error btn-circle btn-lg"
                v-if="recording"
                @click="cancelTake"
              >
                <Icon size="32px">close</Icon>
              </button>
              <button
                v-else
                type="button"
                class="btn btn-primary btn-circle btn-lg"
                :disabled="busy || !micLive"
                @click="recordTake(stepKey)"
              >
                <Icon size="32px">mic</Icon>
              </button>
            </div>
          </div>

          <div v-if="counts[stepKey]" class="rounded-box border border-base-300">
            <div class="flex items-center justify-between gap-2 px-4 py-2">
              <p class="text-sm font-medium">Saved recordings</p>
              <span class="badge badge-sm font-bold">{{ counts[stepKey] }} / {{ spec.target }}</span>
            </div>
            <ul class="divide-y divide-base-300 border-t border-base-300">
              <li v-for="(id, index) in groupIds(stepKey)" :key="id" class="flex items-center gap-3 px-4 py-2 text-sm">
                <span class="badge badge-ghost badge-sm">{{ index + 1 }}</span>
                <span class="opacity-75 flex-grow">{{ describeSample(id) }}</span>
                <button class="btn btn-outline btn-error btn-xs">
                  Delete
                </button>
              </li>
            </ul>
          </div>
        </div>

        <!-- Review and train -->
        <div v-else class="grid gap-4">
          <div v-if="setup.completed" class="alert alert-info" role="status">
            <span>This setup already made a wakeword. Start a new setup with fresh recordings before training again.</span>
          </div>
          <div v-else-if="setup.finalTestConsumed" class="alert alert-info" role="status">
            <span>The final check has already been used for these recordings. Start a new setup with fresh recordings; the same recordings cannot be used to check another model.</span>
          </div>
          <div v-else-if="!samplesLoaded" class="alert alert-info" role="status">
            <span>Waiting for Capsula’s saved recordings before training can start.</span>
          </div>
          <div v-else-if="missingForTraining.length" class="alert alert-warning" role="status">
            <span>Still needed: {{ missingForTraining.join(', ') }}.</span>
          </div>
          <p v-if="takeStatus" class="text-sm" role="status" aria-live="polite">{{ takeStatus }}</p>

          <p class="text-sm">We've recorded enough audio to begin training the wakeword model. If you want to add more for better accuracy, you can go back and do it.</p>

          <div class="rounded-box border border-base-300">
            <div class="flex items-center justify-between gap-2 px-4 py-2" v-for="group in ['room', 'speech', 'word']" :key="group">
              <p class="text-sm font-medium">{{ GROUPS[group].title }}</p>
              <span class="badge badge-sm font-bold">
                {{ counts[group] }} recorded · {{ GROUPS[group].minimum }} minimum
              </span>
            </div>
          </div>

          <div v-if="recording" class="rounded-box border border-primary/30 bg-primary/5 p-4" aria-live="polite">
            <div class="flex items-center gap-2">
              <span class="inline-block h-2.5 w-2.5 animate-pulse rounded-full bg-error" aria-hidden="true"></span>
              <span v-if="recordingPhase === 'cue'" class="font-medium">Capsula cue — {{ cueLeft }} s</span>
              <span v-else class="font-medium">Recording — {{ captureLeft }} s left</span>
            </div>
            <p class="mt-2 text-sm opacity-75">{{ recording.prompt }}</p>
            <progress class="progress progress-primary mt-3 w-full" :value="recordProgress" max="100"></progress>
            <button type="button" class="btn btn-outline btn-error btn-sm mt-3" @click="cancelTake">Cancel recording</button>
          </div>

          <div v-if="training.running || training.cancelling" class="rounded-box border border-primary/30 bg-base-200 p-4" aria-live="polite">
            <div class="flex items-center gap-2">
              <span class="loading loading-spinner loading-sm" aria-hidden="true"></span>
              <h3 class="font-semibold">Training on Capsula</h3>
            </div>
            <p class="mt-2 text-sm">{{ stageLabel }}</p>
            <progress class="progress progress-primary mt-3 w-full" :value="percent" max="100"></progress>
            <div class="mt-2 flex flex-wrap items-center justify-between gap-2 text-sm">
              <span class="opacity-60">{{ percent }}%</span>
              <button
                type="button"
                class="btn btn-outline btn-error btn-sm"
                :disabled="training.cancelling"
                @click="cancelTraining"
              >{{ training.cancelling ? 'Stopping…' : 'Stop training' }}</button>
            </div>
            <details class="mt-3">
              <summary class="cursor-pointer text-xs font-medium opacity-60">Training logs</summary>
              <pre class="mt-2 max-h-40 overflow-auto whitespace-pre-wrap rounded-box bg-base-300/40 p-2 text-xs">{{ training.logs.length ? training.logs.join('\n') : 'Waiting for the device…' }}</pre>
            </details>
          </div>

          <div v-else-if="training.installed" class="alert alert-success" role="status">
            <div class="min-w-0">
              <p class="font-semibold">Your wakeword is ready on Capsula</p>
              <p class="mt-1 text-sm">
                {{ training.installed.displayName || setup.word }}
                <span v-if="training.installed.language">· {{ languageName(training.installed.language) }}</span>
              </p>
              <p class="mt-2 text-sm opacity-80">Nothing was switched on automatically. Close this dialog, then use the Wakeword section to choose this wakeword and turn detection on.</p>
              <details v-if="training.installed.modelId" class="mt-2">
                <summary class="cursor-pointer text-xs opacity-70">Technical details</summary>
                <p class="mt-1 break-all font-mono text-xs opacity-70">{{ training.installed.modelId }}</p>
              </details>
              <div class="mt-3 flex flex-wrap gap-2">
                <button type="button" class="btn btn-primary btn-sm" @click="requestClose">Done</button>
              </div>
            </div>
          </div>

          <div v-else-if="training.error" class="alert alert-error" role="alert">
            <p class="text-sm">{{ training.error }}</p>
          </div>
        </div>
      </div>

      <footer class="flex shrink-0 flex-wrap items-center justify-between gap-2 border-t border-base-300 px-5 py-3">
        <button
          type="button"
          class="btn btn-ghost btn-sm"
          :disabled="busy || stepIndex === 0"
          @click="previousStep"
        >Back</button>
        <div class="flex items-center gap-4">
          <span class="text-xs opacity-60">Step {{ stepNumber }} of {{ STEPS.length }}</span>
          <button
            v-if="stepKey !== 'review'"
            type="button"
            class="btn btn-primary btn-sm"
            :disabled="!canAdvance"
            @click="nextStep"
          >Continue</button>
          <button
            v-else-if="!training.installed"
            type="button"
            class="btn btn-primary btn-sm"
            :disabled="!readyToTrain || training.running || training.cancelling"
            @click="startTraining"
          >Begin training</button>
          <button
            v-else
            type="button"
            class="btn btn-ghost btn-sm"
            :disabled="closing"
            @click="requestClose"
          >Close</button>
        </div>
      </footer>
    </div>
  </dialog>
</template>
