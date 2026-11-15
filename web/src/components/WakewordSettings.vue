<script setup>
import { computed, onUnmounted, ref, watch } from 'vue'
import WakewordTrainingDialog from './WakewordTrainingDialog.vue'
import Icon from './Icon.vue'
import { socket, topicNames } from '../lib/ws.js'
import { useSettings, useTopic } from '../lib/live.js'
import { dateTime, durationMs } from '../lib/format.js'
import { notify } from '../lib/toast.js'

const { data: wakewordData, error: wakewordError, refresh } = useTopic(topicNames.wakeword)
const { data: settingsData, error: settingsError, pending: settingsPending, update } = useSettings()

const trainingOpen = ref(false)
const deleteBusy = ref('')
const detectionBusy = ref('')
const confirmDeleteId = ref('')
let confirmTimer = null

const models = computed(() => wakewordData.value?.models || [])
const samples = computed(() => wakewordData.value?.samples || [])
const detections = computed(() => [...(wakewordData.value?.detections || [])]
  .sort((a, b) => Number(b.detectedUnixMs || 0) - Number(a.detectedUnixMs || 0)))

const values = computed(() => settingsData.value?.values || {})
const selectedModelId = computed(() => values.value.wakewordModelId || '')
const selectedModel = computed(() => models.value.find((model) => model.modelId === selectedModelId.value) || null)
const enabled = computed(() => Boolean(values.value.wakewordEnabled))
const assistantSelected = computed(() => Boolean(values.value.defaultAssistantAppId))
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
const canToggle = computed(() => enabled.value || Boolean(selectedModel.value?.available))
const installedAvailable = computed(() => models.value.some((model) => model.available))
const savedTakes = computed(() => [...samples.value].sort((a, b) => Number(b.recordedUnixMs || 0) - Number(a.recordedUnixMs || 0)))

const statusText = computed(() => {
  if (!settingsReady.value) return 'Checking'
  if (enabled.value && selectedModel.value?.available) return 'Detection on'
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
  } catch (err) {
    notify.error(err?.message || 'The change could not be sent to Capsula.')
  }
}

function toggleEnabled(next) {
  if (!canToggle.value) return
  apply({
    wakewordModel: selectedModelId.value,
    wakewordEnabled: Boolean(next),
    updateFields: ['wakeword_enabled']
  })
}

function selectModel(model) {
  if (!model.available || model.modelId === selectedModelId.value) return
  apply({
    wakewordModel: model.modelId,
    wakewordEnabled: enabled.value,
    updateFields: ['wakeword_model']
  })
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

function closeTraining() {
  trainingOpen.value = false
  refresh().catch(() => { })
}

onUnmounted(() => {
  clearTimeout(confirmTimer)
})
</script>

<template>
  <section aria-labelledby="wakeword-settings-title" class="card rounded-box bg-base-100">
    <div class="card-body gap-3 p-4">
    <div class="flex flex-wrap items-center gap-2">
      <Icon size="20px">record_voice_over</Icon>
      <h2 id="wakeword-settings-title" class="flex-grow text-lg font-semibold">Wakeword</h2>
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
              <span class="mt-0.5 block text-sm text-base-content/60">Say your wakeword to start Capsula without pressing anything.</span>
            </span>
            <input
              type="checkbox"
              class="toggle toggle-primary mt-1"
              :checked="enabled"
              :disabled="!canToggle || applying"
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
              Use “Create a wakeword” in the next block to get started. Until then, detection stays unavailable.
            </p>
          </div>
          <p v-else-if="!selectedModel?.available" class="text-sm text-warning" role="status">
            Detection starts once you choose a ready wakeword.
          </p>
          <div v-else-if="enabled && settingsReady && !assistantSelected" role="status">
            <p class="text-sm font-medium text-warning">No assistant is chosen yet.</p>
            <p class="mt-1 text-xs text-base-content/60">
              Capsula hears the wakeword but will not answer until you choose an assistant above.
            </p>
          </div>
          <p v-if="enabled && micMuted" class="text-sm text-base-content/60">
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
            <h3 class="text-base font-semibold">Your wakeword</h3>
            <button
              type="button"
              class="btn btn-primary btn-sm"
              @click="trainingOpen = true"
            >{{ installedAvailable ? 'Record a new wakeword' : 'Create a wakeword' }}</button>
          </div>

          <div v-if="!wakewordData && !wakewordError" class="space-y-2" aria-hidden="true">
            <div class="skeleton h-12 w-full"></div>
            <div class="skeleton h-12 w-full"></div>
          </div>

          <ul v-else-if="models.length" class="space-y-2">
            <li
              v-for="model in models"
              :key="model.modelId"
              class="flex items-start gap-3 rounded-box bg-base-100 p-3"
              :class="model.available ? '' : 'text-base-content/60'"
            >
              <input
                type="radio"
                class="radio radio-primary radio-sm mt-1"
                name="wakeword-model"
                :value="model.modelId"
                :checked="selectedModelId === model.modelId"
                :disabled="!model.available || applying"
                :aria-label="`Use ${model.displayName || model.modelId}`"
                @change="selectModel(model)"
              />
              <div class="min-w-0 flex-1">
                <span class="font-medium">{{ model.displayName || 'Personal wakeword' }}</span>
              </div>
            </li>
          </ul>
          <div v-else-if="wakewordError" class="rounded-box bg-base-100 p-4 text-sm text-base-content/60">
            The wakeword list is unavailable while the connection is down.
          </div>
          <div v-else class="rounded-box bg-base-100 p-4 text-sm text-base-content/60">
            No wakeword has been set up yet. Use “Create a wakeword” to get started.
          </div>

          <details v-if="samples.length" class="rounded-box bg-base-100">
            <summary class="cursor-pointer px-4 py-3 text-sm font-medium">
              Recordings on Capsula
              <span class="badge badge-ghost badge-sm ml-1">{{ samples.length }}</span>
            </summary>
            <ul class="divide-y divide-base-300 border-t border-base-300">
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
            <p class="border-t border-base-300 px-4 py-2 text-xs text-base-content/60">
              Deleting a recording does not change the wakeword Capsula is using. Only recordings chosen in a new setup are used for training.
            </p>
          </details>
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
    </div>

    <WakewordTrainingDialog
      v-if="trainingOpen"
      :samples="samples"
      :samples-loaded="samplesLoaded"
      :mic-live="micLive"
      :mic-reason="micReason"
      @close="closeTraining"
      @refresh="refresh"
    />
    </div>
  </section>
</template>
