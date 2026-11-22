<script setup>
import { computed, onUnmounted, ref, watch } from 'vue'
import ModalDialog from './ModalDialog.vue'
import WakewordTrainingDialog from './WakewordTrainingDialog.vue'
import Icon from './Icon.vue'
import { socket, topicNames } from '../lib/ws.js'
import { useSettings, useTopic } from '../lib/live.js'
import { dateTime, durationMs } from '../lib/format.js'
import { notify } from '../lib/toast.js'

const { data: wakewordData, error: wakewordError, refresh } = useTopic(topicNames.wakeword)
const { data: settingsData, error: settingsError, pending: settingsPending, update } = useSettings()

const trainingOpen = ref(false)
const trainingTarget = ref(null)
const modelDeleteBusy = ref('')
const confirmModelDeleteId = ref('')
const deleteBusy = ref('')
const deleteAllOpen = ref(false)
const deleteAllBusy = ref(false)
const deleteAllProgress = ref(0)
const deleteAllTotal = ref(0)
const detectionBusy = ref('')
const confirmDeleteId = ref('')
let confirmTimer = null
let confirmModelTimer = null

const models = computed(() => wakewordData.value?.models || [])
const samples = computed(() => wakewordData.value?.samples || [])
const detections = computed(() => [...(wakewordData.value?.detections || [])]
  .sort((a, b) => Number(b.detectedUnixMs || 0) - Number(a.detectedUnixMs || 0)))

const values = computed(() => settingsData.value?.values || {})
const enabled = computed(() => Boolean(values.value.wakewordEnabled))
const settingsReady = computed(() => Boolean(settingsData.value))
const micMuted = computed(() => Boolean(values.value.microphoneMuted))
const masterVolume = computed(() => Number(values.value.masterVolume || 0))
const micLive = computed(() => settingsReady.value && !micMuted.value && masterVolume.value > 0)
const micReason = computed(() => {
  if (!settingsReady.value) return 'Waiting for the microphone state from Capsula.'
  if (micMuted.value) return 'The microphone is muted. Unmute it in Settings before recording.'
  if (masterVolume.value <= 0) return 'The master volume is zero, so Capsula cannot play its start cue.'
  return ''
})

const applying = computed(() => Boolean(settingsPending.value))
const samplesLoaded = computed(() => wakewordData.value !== null)
const installedAvailable = computed(() => models.value.some((model) => model.available))
const savedTakes = computed(() => [...samples.value].sort((a, b) => Number(b.recordedUnixMs || 0) - Number(a.recordedUnixMs || 0)))

// Installed applications the browser may bind a word to. The settings snapshot
// already carries the installed list, so binding needs no second request.
const appOptions = computed(() => {
  const apps = settingsData.value?.apps || []
  return apps
    .filter((app) => app.appId)
    .map((app) => ({ id: app.appId, label: app.displayName || app.appId }))
    .sort((a, b) => a.label.localeCompare(b.label))
})

function appLabel(appId) {
  if (!appId) return 'Selected assistant'
  const found = appOptions.value.find((app) => app.id === appId)
  return found ? found.label : `${appId} (not installed)`
}

/**
 * The per-word draft the browser edits. The device decorates every model with
 * its effective enablement and binding, but that is the combined result; the
 * draft needs each word's own switch so toggling the global one cannot rewrite
 * the per-word intent. A stored list wins; without one the device is in the
 * legacy state, where every installed word is live under the global switch.
 */
function bindingsFrom(values) {
  const stored = values?.wakewords
  // A repeated protobuf field decodes as [] both when it was absent and when it
  // was empty, so the browser cannot tell the legacy state from an explicit
  // "no word is live" list. The empty case is equivalent here: it only exists
  // when no word is installed (deleting the last word), and then the legacy
  // draft below is empty too. Every installed word is switched off through its
  // own entry, which is present.
  if (Array.isArray(stored) && stored.length) {
    return stored.map((entry) => ({
      modelId: entry.modelId,
      appId: entry.appId || '',
      enabled: Boolean(entry.enabled)
    }))
  }
  return null
}

function legacyBindings() {
  return models.value
    .filter((model) => model.available)
    .map((model) => ({ modelId: model.modelId, appId: '', enabled: true }))
}

const draft = ref([])

function syncDraft() {
  draft.value = bindingsFrom(values.value) || legacyBindings()
}

// Resync only when the authoritative settings values change identity, so an
// optimistic edit is not wiped by our own revision bump, and when the set of
// installed words changes, so the draft always covers every installed word.
watch(() => settingsData.value?.values, syncDraft, { immediate: true })
watch(() => models.value.map((model) => model.modelId).join(','), syncDraft)

function bindingFor(modelId) {
  return draft.value.find((entry) => entry.modelId === modelId) || null
}

// One whole-list patch: the device replaces the whole per-word list, so adding,
// removing, enabling and rebinding are the same operation.
function commitBindings(next, previous) {
  draft.value = next
  return apply({ wakewords: next.map((entry) => ({
    modelId: entry.modelId,
    appId: entry.appId,
    enabled: Boolean(entry.enabled)
  })), updateFields: ['wakewords'] }).then((ok) => {
    if (ok === false) draft.value = previous
    return ok
  })
}

function toggleModel(model, next) {
  if (!model.available || applying.value) return
  const previous = draft.value.map((entry) => ({ ...entry }))
  let list = previous.map((entry) => entry.modelId === model.modelId
    ? { ...entry, enabled: Boolean(next) }
    : entry)
  if (!list.some((entry) => entry.modelId === model.modelId)) {
    list = [...list, { modelId: model.modelId, appId: '', enabled: Boolean(next) }]
  }
  commitBindings(list, previous)
}

function setBinding(model, appId) {
  if (applying.value) return
  const previous = draft.value.map((entry) => ({ ...entry }))
  let list = previous.map((entry) => entry.modelId === model.modelId
    ? { ...entry, appId }
    : entry)
  if (!list.some((entry) => entry.modelId === model.modelId)) {
    // Binding an app is not enabling: adding the missing entry keeps the word's
    // current state instead of switching it on behind the operator's back.
    list = [...list, { modelId: model.modelId, appId, enabled: Boolean(model.enabled) }]
  }
  commitBindings(list, previous)
}

function wordLive(model) {
  return Boolean(model.available && enabled.value && (bindingFor(model.modelId)?.enabled ?? false))
}

const SPLIT_LABELS = {
  tune: 'Tuning check',
  final_test: 'Final check',
  auxiliary_tune: 'External speech check · tuning',
  auxiliary_final_test: 'External speech check · final'
}

// The certificate recorded when a model was trained. Values come straight from
// the device; nothing here recomputes a metric.
function metricsFor(model) {
  const splits = Array.isArray(model?.metrics) ? model.metrics : []
  return splits.map((split, index) => {
    const words = Number(split.wordRecordings) || 0
    return {
      key: `${split.split || 'split'}-${index}`,
      label: SPLIT_LABELS[split.split] || String(split.split || 'Check'),
      words,
      detected: Number(split.detected) || 0,
      recall: words > 0 && typeof split.recall === 'number' ? `${Math.round(split.recall * 100)}%` : '—',
      falseActivations: Number(split.falseActivations) || 0
    }
  })
}

function thresholdFor(model) {
  const value = Number(model?.threshold)
  return Number.isFinite(value) && value > 0 ? value.toFixed(2) : ''
}

function statusText(model) {
  if (!model.available) return 'Needs attention'
  if (!enabled.value) return 'Detection off'
  if (!wordLive(model)) return 'Paused'
  return 'Listening'
}

const overallStatus = computed(() => {
  if (!settingsReady.value) return 'Checking'
  if (enabled.value && installedAvailable.value) return 'Detection on'
  if (enabled.value) return 'Model needed'
  return 'Detection off'
})

// Connection failures are state that repeats on every snapshot; announce a new
// message once and leave the section's own availability notices in place.
let lastSettingsError = ''

watch(settingsError, (message) => {
  const text = String(message || '')
  if (!text) {
    lastSettingsError = ''
    return
  }
  if (text === lastSettingsError) return
  lastSettingsError = text
  notify.warning(`Settings did not update: ${text}`)
}, { immediate: true })

let lastWakewordError = ''

watch(wakewordError, (message) => {
  const text = String(message || '')
  if (!text) {
    lastWakewordError = ''
    return
  }
  if (text === lastWakewordError) return
  lastWakewordError = text
  notify.warning(`The wakeword section could not reach Capsula: ${text}`)
}, { immediate: true })

function labelFor(sample) {
  if (sample.label === 'word') return 'Wakeword recording'
  return 'Room or phrase recording'
}

async function reportFalseDetection(detection) {
  if (!detection.detectionId || detectionBusy.value) return
  detectionBusy.value = detection.detectionId
  try {
    await socket.request({ falseDetection: { detectionId: detection.detectionId } }, { timeout: 10000 })
    await refresh()
  } catch (err) {
    notify.error(`Could not remove that detection: ${err.message}`)
  } finally {
    detectionBusy.value = ''
  }
}

async function apply(payload) {
  try {
    const ok = await update('assistant', payload)
    if (ok === false) notify.error('Capsula did not accept that change.')
    return ok
  } catch (err) {
    notify.error(err?.message || 'The change could not be sent to Capsula.')
    return false
  }
}

function toggleEnabled(next) {
  apply({
    wakewordEnabled: Boolean(next),
    updateFields: ['wakeword_enabled']
  })
}

function openTraining(model = null) {
  trainingTarget.value = model ? { modelId: model.modelId, displayName: model.displayName } : null
  trainingOpen.value = true
}

function askDeleteModel(model) {
  if (modelDeleteBusy.value) return
  if (confirmModelDeleteId.value === model.modelId) {
    removeModel(model)
    return
  }
  confirmModelDeleteId.value = model.modelId
  clearTimeout(confirmModelTimer)
  confirmModelTimer = setTimeout(() => { confirmModelDeleteId.value = '' }, 5000)
}

async function removeModel(model) {
  clearTimeout(confirmModelTimer)
  modelDeleteBusy.value = model.modelId
  try {
    await socket.request({ deleteWakeword: { modelId: model.modelId } }, { timeout: 25000 })
    confirmModelDeleteId.value = ''
    await refresh()
    notify.success('Wakeword deleted. Its recordings were kept.')
  } catch (err) {
    notify.error(`Could not delete the wakeword: ${err.message}`)
  } finally {
    modelDeleteBusy.value = ''
  }
}

function askDelete(sample) {
  if (deleteBusy.value) return
  if (confirmDeleteId.value === sample.sampleId) {
    removeTake(sample)
    return
  }
  confirmDeleteId.value = sample.sampleId
  clearTimeout(confirmTimer)
  confirmTimer = setTimeout(() => { confirmDeleteId.value = '' }, 5000)
}

async function removeTake(sample) {
  clearTimeout(confirmTimer)
  deleteBusy.value = sample.sampleId
  try {
    await socket.request({ deleteSample: { sampleId: sample.sampleId } }, { timeout: 10000 })
    confirmDeleteId.value = ''
    await refresh()
  } catch (err) {
    notify.error(`Could not delete the recording: ${err.message}`)
  } finally {
    deleteBusy.value = ''
  }
}

function askDeleteAll() {
  if (deleteAllBusy.value || !samples.value.length) return
  deleteAllTotal.value = samples.value.length
  deleteAllProgress.value = 0
  deleteAllOpen.value = true
}

function beforeDeleteAllClose() {
  return !deleteAllBusy.value
}

function closeDeleteAll() {
  if (deleteAllBusy.value) return
  deleteAllOpen.value = false
}

// Recordings are deleted one at a time because the device exposes a single-
// sample command. A failure does not stop the rest: the page reports how many
// remain so the user can retry, and only a complete run closes the dialog.
async function removeAllTakes() {
  if (deleteAllBusy.value) return
  const takes = [...samples.value]
  if (!takes.length) {
    deleteAllOpen.value = false
    return
  }
  deleteAllTotal.value = takes.length
  deleteAllProgress.value = 0
  deleteAllBusy.value = true
  let failed = 0
  try {
    for (const sample of takes) {
      try {
        await socket.request({ deleteSample: { sampleId: sample.sampleId } }, { timeout: 10000 })
      } catch (_) {
        failed += 1
      }
      deleteAllProgress.value += 1
    }
    try {
      await refresh()
    } catch (_) {
      // The per-sample results are authoritative; the topic resyncs on the
      // next event if this snapshot failed.
    }
    if (failed > 0) {
      notify.error(`${failed} of ${takes.length} recordings could not be deleted.`)
      return
    }
    deleteAllOpen.value = false
    notify.success(takes.length === 1 ? 'Recording deleted.' : `Deleted ${takes.length} recordings.`)
  } finally {
    deleteAllBusy.value = false
  }
}

function closeTraining() {
  trainingOpen.value = false
  trainingTarget.value = null
  refresh().catch(() => { })
}

onUnmounted(() => {
  clearTimeout(confirmTimer)
  clearTimeout(confirmModelTimer)
})
</script>

<template>
  <section aria-labelledby="wakeword-settings-title" class="card rounded-box bg-base-100">
    <div class="card-body gap-3 p-4">
    <div class="flex flex-wrap items-center gap-2">
      <Icon size="20px">record_voice_over</Icon>
      <h2 id="wakeword-settings-title" class="flex-grow text-lg font-semibold">Wakeword</h2>
      <span class="badge badge-ghost badge-sm">{{ overallStatus }}</span>
    </div>
    <p class="text-xs text-base-content/60">
      Capsula listens and learns on the device. Your browser never records audio.
    </p>

    <div class="grid gap-3">
      <div class="rounded-box bg-base-200/60">
        <div class="flex flex-col gap-3 p-3">
          <h3 class="text-base font-semibold">Enable detection</h3>

          <label class="flex cursor-pointer items-start justify-between gap-4">
            <span class="min-w-0">
              <span class="block font-medium">Wakeword detection</span>
              <span class="mt-0.5 block text-sm text-base-content/60">Say a wakeword to start Capsula without pressing anything.</span>
            </span>
            <input
              type="checkbox"
              class="toggle toggle-primary mt-1"
              :checked="enabled"
              :disabled="!installedAvailable || applying"
              @change="toggleEnabled($event.target.checked)"
            />
          </label>

          <div v-if="!wakewordData && !wakewordError" class="flex items-center gap-2 text-sm text-base-content/60" role="status">
            <span class="loading loading-spinner loading-xs" aria-hidden="true"></span>
            Waiting for Capsula…
          </div>
          <div v-else-if="wakewordError" class="text-sm text-base-content/60" role="status">
            Capsula’s wakeword state is unavailable until the connection recovers.
          </div>
          <div v-else-if="!installedAvailable" role="status">
            <p class="text-sm font-medium">Capsula does not have a wakeword yet.</p>
            <p class="mt-1 text-xs text-base-content/60">
              Use “Create a wakeword” below to get started. Until then, detection stays unavailable.
            </p>
          </div>
          <p v-else-if="enabled && micMuted" class="text-sm text-base-content/60">
            The microphone is muted, so detection is suspended until it is unmuted.
          </p>
          <p v-if="settingsError && !settingsReady" class="text-sm text-base-content/60">
            The switch waits for the authoritative settings snapshot.
          </p>
        </div>
      </div>

      <div class="rounded-box bg-base-200/60">
        <div class="flex flex-col gap-3 p-3">
          <div class="flex flex-wrap items-center justify-between gap-2">
            <h3 class="text-base font-semibold">Your wakewords</h3>
            <button
              type="button"
              class="btn btn-primary btn-sm"
              :disabled="applying"
              @click="openTraining()"
            >Create a wakeword</button>
          </div>
          <p class="text-xs text-base-content/60">
            Each wakeword can start its own app. A word bound to an app activates only that app; “Selected assistant” uses the assistant chosen above. A wakeword that starts an app does not reroute when that app is not running.
          </p>

          <div v-if="!wakewordData && !wakewordError" class="space-y-2" aria-hidden="true">
            <div class="skeleton h-12 w-full"></div>
            <div class="skeleton h-12 w-full"></div>
          </div>

          <ul v-else-if="models.length" class="space-y-2">
            <li
              v-for="model in models"
              :key="model.modelId"
              class="rounded-box bg-base-100 p-3"
            >
              <div class="flex flex-wrap items-start gap-x-3 gap-y-2">
                <div class="min-w-0 flex-1">
                  <div class="flex flex-wrap items-center gap-2">
                    <span class="font-medium">{{ model.displayName || model.modelId }}</span>
                    <span
                      class="badge badge-sm"
                      :class="wordLive(model) ? 'badge-success' : (model.available ? 'badge-ghost' : 'badge-warning')"
                    >{{ statusText(model) }}</span>
                  </div>
                  <p v-if="model.detail" class="mt-1 text-xs text-warning">{{ model.detail }}</p>
                </div>

                <label class="flex items-center gap-2 text-sm">
                  <span class="text-base-content/60">Enabled</span>
                  <input
                    type="checkbox"
                    class="toggle toggle-sm"
                    :checked="bindingFor(model.modelId)?.enabled ?? false"
                    :disabled="!model.available || applying"
                    :aria-label="`Enable ${model.displayName || model.modelId}`"
                    @change="toggleModel(model, $event.target.checked)"
                  />
                </label>
              </div>

              <div class="mt-2 flex flex-wrap items-center gap-2">
                <label class="flex min-w-0 flex-1 items-center gap-2 text-sm">
                  <span class="shrink-0 text-base-content/60">Starts</span>
                  <select
                    class="select select-bordered select-sm min-w-0 flex-1"
                    :value="bindingFor(model.modelId)?.appId || model.appId || ''"
                    :disabled="!model.available || applying"
                    :aria-label="`App started by ${model.displayName || model.modelId}`"
                    @change="setBinding(model, $event.target.value)"
                  >
                    <option value="">Selected assistant</option>
                    <option v-for="app in appOptions" :key="app.id" :value="app.id">{{ app.label }}</option>
                  </select>
                </label>
                <button
                  type="button"
                  class="btn btn-ghost btn-sm"
                  :disabled="!model.available || applying"
                  @click="openTraining(model)"
                >Retrain</button>
                <button
                  type="button"
                  class="btn btn-ghost btn-sm"
                  :class="confirmModelDeleteId === model.modelId ? 'text-error' : ''"
                  :disabled="Boolean(modelDeleteBusy)"
                  @click="askDeleteModel(model)"
                >
                  <span v-if="modelDeleteBusy === model.modelId" class="loading loading-spinner loading-xs" aria-hidden="true"></span>
                  {{ confirmModelDeleteId === model.modelId ? 'Confirm delete' : 'Delete' }}
                </button>
              </div>

              <details v-if="metricsFor(model).length" class="mt-2 rounded-box bg-base-200/60">
                <summary class="cursor-pointer px-3 py-2 text-sm font-medium">
                  Measured quality
                  <span v-if="thresholdFor(model)" class="badge badge-ghost badge-sm ml-1">threshold {{ thresholdFor(model) }}</span>
                </summary>
                <div class="overflow-x-auto px-2 pb-2">
                  <table class="table table-xs">
                    <thead>
                      <tr>
                        <th>Check</th>
                        <th>Words found</th>
                        <th>Recall</th>
                        <th>False activations</th>
                      </tr>
                    </thead>
                    <tbody>
                      <tr v-for="row in metricsFor(model)" :key="row.key">
                        <td class="whitespace-nowrap">{{ row.label }}</td>
                        <td>{{ row.words ? `${row.detected} / ${row.words}` : '—' }}</td>
                        <td>{{ row.recall }}</td>
                        <td>{{ row.falseActivations }}</td>
                      </tr>
                    </tbody>
                  </table>
                </div>
                <p class="px-3 py-2 text-xs text-base-content/60">
                  Measured once when this wakeword was trained, on recordings held back from fitting. The external speech check uses a separate licensed corpus and is not the final certificate.
                </p>
              </details>
            </li>
          </ul>
          <div v-else-if="wakewordError" class="rounded-box bg-base-100 p-4 text-sm text-base-content/60">
            The wakeword list is unavailable while the connection is down.
          </div>
          <div v-else class="rounded-box bg-base-100 p-4 text-sm text-base-content/60">
            No wakeword has been set up yet. Use “Create a wakeword” to get started.
          </div>
        </div>
      </div>

      <div class="rounded-box bg-base-200/60">
        <div class="flex flex-col gap-3 p-3">
          <div class="flex flex-wrap items-center justify-between gap-2">
            <h3 class="text-base font-semibold">Recent detections</h3>
            <span class="badge badge-ghost badge-sm">{{ detections.length }}</span>
          </div>
          <div v-if="!wakewordData && !wakewordError" class="skeleton h-16 w-full"></div>
          <ul v-else-if="detections.length" class="divide-y divide-base-300 rounded-box bg-base-100">
            <li
              v-for="(detection, index) in detections"
              :key="detection.detectionId || `${detection.detectedUnixMs}-${detection.modelId}-${index}`"
              class="flex flex-wrap items-center justify-between gap-x-3 gap-y-1 px-4 py-2 text-sm"
            >
              <span class="min-w-0">
                <span class="block font-medium">{{ detection.modelDisplayName || 'Your wakeword' }}</span>
                <span class="block text-xs text-base-content/60">{{ dateTime(detection.detectedUnixMs) }}</span>
              </span>
              <button
                type="button"
                class="btn btn-ghost btn-sm"
                :aria-label="`Mark as false detection: ${detection.modelDisplayName || 'Your wakeword'} at ${dateTime(detection.detectedUnixMs)}`"
                :disabled="!detection.detectionId || Boolean(detectionBusy)"
                @click="reportFalseDetection(detection)"
              >
                <span v-if="detectionBusy === detection.detectionId" class="loading loading-spinner loading-xs" aria-hidden="true"></span>
                False detection
              </button>
            </li>
          </ul>
          <div v-else-if="wakewordError" class="rounded-box border border-dashed border-base-300 p-4 text-sm text-base-content/60">
            Detection history is unavailable while the connection is down.
          </div>
          <div v-else class="rounded-box border border-dashed border-base-300 p-4 text-sm text-base-content/60">
            No wakeword detections yet.
          </div>
        </div>
      </div>

      <div class="rounded-box bg-base-200/60">
        <div class="flex flex-col gap-3 p-3">
          <div class="flex flex-wrap items-center gap-2">
            <h3 class="text-base font-semibold flex-grow">Recordings on Capsula</h3>
            <span class="badge badge-ghost badge-sm">{{ samples.length }}</span>
            <button
              type="button"
              class="btn btn-outline btn-error btn-xs"
              :disabled="!samples.length || deleteAllBusy || Boolean(deleteBusy)"
              @click="askDeleteAll"
            >
              <span v-if="deleteAllBusy" class="loading loading-spinner loading-xs" aria-hidden="true"></span>
              Delete all
            </button>
          </div>
          <div v-if="!samplesLoaded" class="skeleton h-12 w-full" aria-hidden="true"></div>
          <ul v-else-if="samples.length" class="divide-y divide-base-300 rounded-box bg-base-100">
            <li v-for="sample in savedTakes" :key="sample.sampleId" class="flex flex-wrap items-center gap-x-3 gap-y-1 px-4 py-2 text-sm">
              <span class="min-w-0 flex-1">
                <span class="block">{{ labelFor(sample) }}</span>
                <span class="block text-xs text-base-content/60">{{ dateTime(sample.recordedUnixMs) }} · {{ durationMs(sample.durationMs) }}</span>
              </span>
              <button
                type="button"
                class="btn btn-ghost btn-sm"
                :class="confirmDeleteId === sample.sampleId ? 'text-error' : ''"
                :disabled="Boolean(deleteBusy)"
                @click="askDelete(sample)"
              >
                <span v-if="deleteBusy === sample.sampleId" class="loading loading-spinner loading-xs" aria-hidden="true"></span>
                {{ confirmDeleteId === sample.sampleId ? 'Confirm delete' : 'Delete' }}
              </button>
            </li>
          </ul>
          <p v-else class="rounded-box bg-base-100 p-4 text-sm text-base-content/60">
            No recordings yet. A new wakeword setup records its own takes.
          </p>
          <p class="text-xs text-base-content/60">
            Recordings belong to the device, not to one wakeword: deleting a wakeword keeps them, and only recordings chosen in a new setup are used for training.
          </p>
        </div>
      </div>
    </div>

    <WakewordTrainingDialog
      v-if="trainingOpen"
      :samples="samples"
      :samples-loaded="samplesLoaded"
      :mic-live="micLive"
      :mic-reason="micReason"
      :target="trainingTarget"
      @close="closeTraining"
      @refresh="refresh"
    />

    <ModalDialog
      v-if="deleteAllOpen"
      max-width="max-w-md"
      title="Delete all recordings"
      :before-close="beforeDeleteAllClose"
      @close="closeDeleteAll"
    >
      <p class="text-sm">
        Delete {{ deleteAllTotal }} {{ deleteAllTotal === 1 ? 'recording' : 'recordings' }} from Capsula?
      </p>
      <p class="mt-2 text-sm text-base-content/60">
        This cannot be undone. Wakewords stay installed; only their saved recordings are removed.
      </p>
      <p v-if="deleteAllBusy" class="mt-2 text-sm" role="status">
        Deleting {{ deleteAllProgress }} of {{ deleteAllTotal }}…
      </p>
      <template #footer>
        <button type="button" class="btn btn-ghost btn-sm" :disabled="deleteAllBusy" @click="closeDeleteAll">
          Cancel
        </button>
        <button type="button" class="btn btn-error btn-sm" :disabled="deleteAllBusy" @click="removeAllTakes">
          <span v-if="deleteAllBusy" class="loading loading-spinner loading-sm"></span>
          Delete all
        </button>
      </template>
    </ModalDialog>
    </div>
  </section>
</template>