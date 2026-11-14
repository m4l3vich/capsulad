<script setup>
import { computed, onUnmounted, ref } from 'vue'
import WakewordTrainingDialog from './WakewordTrainingDialog.vue'
import { socket, topicNames } from '../lib/ws.js'
import { useSettings, useTopic } from '../lib/live.js'
import { dateTime, durationMs } from '../lib/format.js'

const { data: wakewordData, error: wakewordError, refresh } = useTopic(topicNames.wakeword)
const { data: settingsData, error: settingsError, pending: settingsPending, update } = useSettings()

const trainingOpen = ref(false)
const actionError = ref('')
const deleteBusy = ref('')
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

function labelFor(sample) {
  if (sample.label === 'word') return 'Wakeword recording'
  return 'Room or phrase recording'
}

function languageName(code) {
  if (!code) return ''
  const value = String(code)
  if (value.toLowerCase() === 'ru') return 'Russian'
  if (value.toLowerCase() === 'en') return 'English'
  if (value.toLowerCase() === 'any') return 'Any language'
  return value
}

async function apply(payload) {
  actionError.value = ''
  try {
    const ok = await update('assistant', payload)
    if (ok === false) actionError.value = 'Capsula did not accept that change.'
  } catch (err) {
    actionError.value = err?.message || 'The change could not be sent to Capsula.'
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
  actionError.value = ''
  try {
    await socket.request({ deleteSample: { sampleId: sample.sampleId } }, { timeout: 10000 })
    confirmDeleteId.value = ''
    await refresh()
  } catch (err) {
    actionError.value = `Could not delete the recording: ${err.message}`
  } finally {
    deleteBusy.value = ''
  }
}

async function onRefresh() {
  actionError.value = ''
  try {
    await refresh()
  } catch (err) {
    actionError.value = err?.message || 'The device did not return its wakeword state.'
  }
}

function closeTraining() {
  trainingOpen.value = false
  refresh().catch(() => {})
}

onUnmounted(() => {
  clearTimeout(confirmTimer)
})
</script>

<template>
  <section aria-labelledby="wakeword-settings-title" class="mt-8">
    <div class="flex flex-wrap items-end justify-between gap-2">
      <div>
        <h2 id="wakeword-settings-title">Wakeword</h2>
        <p class="mt-1 text-sm opacity-70">
          Capsula listens and learns on the device. Your browser never records audio.
        </p>
      </div>
      <button type="button" class="btn btn-ghost btn-sm" :disabled="applying" @click="onRefresh">Refresh</button>
    </div>

    <div v-if="actionError" class="alert alert-error mt-3" role="alert">
      <span>{{ actionError }}</span>
    </div>
    <div v-if="settingsError" class="alert alert-warning mt-3" role="alert">
      <div class="min-w-0">
        <p>Settings did not update. Check the connection and try again.</p>
        <details>
          <summary class="cursor-pointer text-xs opacity-70">Technical details</summary>
          <p class="mt-1 break-words text-xs opacity-70">{{ settingsError }}</p>
        </details>
      </div>
    </div>
    <div v-if="wakewordError" class="alert alert-error mt-3" role="alert">
      <div class="min-w-0 flex-1">
        <p>The wakeword section could not reach Capsula.</p>
        <details>
          <summary class="cursor-pointer text-xs opacity-70">Technical details</summary>
          <p class="mt-1 break-words text-xs opacity-70">{{ wakewordError }}</p>
        </details>
      </div>
      <button type="button" class="btn btn-ghost btn-sm" @click="onRefresh">Try again</button>
    </div>

    <div class="mt-3 grid gap-4 xl:grid-cols-2">
      <div class="card border border-base-300 bg-base-100">
        <div class="card-body gap-3 p-4 sm:p-5">
          <div class="flex flex-wrap items-center justify-between gap-2">
            <h3 class="card-title text-base">Detection</h3>
            <span class="badge badge-sm" :class="enabled && selectedModel?.available ? 'badge-success' : enabled ? 'badge-warning' : 'badge-ghost'">
              {{ statusText }}
            </span>
          </div>

          <label class="flex cursor-pointer items-start justify-between gap-4">
            <span class="min-w-0">
              <span class="block font-medium">Wakeword detection</span>
              <span class="mt-0.5 block text-sm opacity-70">Say your wakeword to start Capsula without pressing anything.</span>
            </span>
            <input
              type="checkbox"
              class="toggle toggle-primary mt-1"
              :checked="enabled"
              :disabled="!canToggle || applying"
              @change="toggleEnabled($event.target.checked)"
            />
          </label>

          <p v-if="applying" class="flex items-center gap-2 text-sm opacity-70" role="status">
            <span class="loading loading-spinner loading-xs" aria-hidden="true"></span>
            Applying on Capsula…
          </p>

          <div v-if="!wakewordData && !wakewordError" class="flex items-center gap-2 text-sm opacity-70" role="status">
            <span class="loading loading-spinner loading-xs" aria-hidden="true"></span>
            Waiting for Capsula…
          </div>
          <div v-else-if="wakewordError" class="text-sm opacity-70" role="status">
            Capsula’s wakeword state is unavailable until the connection recovers.
          </div>
          <div v-else-if="!installedAvailable" class="alert alert-info" role="status">
            <div class="min-w-0">
              <p class="text-sm font-medium">Capsula does not have a wakeword yet.</p>
              <p class="mt-1 text-sm">Use “Create a wakeword” in the next card to get started. Until then, detection stays unavailable.</p>
            </div>
          </div>
          <div v-else-if="!selectedModel?.available" class="alert alert-warning" role="status">
            <span>Detection will start once you choose a ready wakeword.</span>
          </div>
          <div v-else-if="enabled && settingsReady && !assistantSelected" class="alert alert-warning" role="status">
            <span>No assistant is chosen yet. Capsula will hear the wakeword, but it will not answer until you choose an assistant above.</span>
          </div>
          <p v-if="enabled && micMuted" class="text-sm opacity-70">
            The microphone is muted, so detection is suspended until it is unmuted.
          </p>
          <p v-if="settingsError && !settingsReady" class="text-sm opacity-60">
            The switch waits for the authoritative settings snapshot.
          </p>
        </div>
      </div>

      <div class="card border border-base-300 bg-base-100">
        <div class="card-body gap-3 p-4 sm:p-5">
          <div class="flex flex-wrap items-center justify-between gap-2">
            <h3 class="card-title text-base">Your wakeword</h3>
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
              class="flex items-start gap-3 rounded-box border p-3"
              :class="model.available ? 'border-base-300' : 'border-base-300 opacity-70'"
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
                <div class="flex flex-wrap items-center gap-2">
                  <span class="font-medium">{{ model.displayName || 'Personal wakeword' }}</span>
                  <span class="badge badge-sm" :class="model.available ? 'badge-success' : 'badge-warning'">
                    {{ model.available ? 'Ready' : 'Not ready' }}
                  </span>
                  <span v-if="model.language" class="badge badge-ghost badge-sm">{{ languageName(model.language) }}</span>
                  <span v-if="selectedModelId === model.modelId" class="badge badge-primary badge-sm">In use</span>
                </div>
                <details class="mt-1">
                  <summary class="cursor-pointer text-xs opacity-60">Technical details</summary>
                  <p class="mt-1 break-all font-mono text-xs opacity-60">
                    {{ model.modelId }}<span v-if="model.engineId"> · {{ model.engineId }}</span>
                  </p>
                </details>
              </div>
            </li>
          </ul>
          <div v-else-if="wakewordError" class="rounded-box border border-dashed border-base-300 p-4 text-sm opacity-70">
            The wakeword list is unavailable while the connection is down.
          </div>
          <div v-else class="rounded-box border border-dashed border-base-300 p-4 text-sm opacity-70">
            No wakeword has been set up yet. Use “Create a wakeword” to get started.
          </div>

          <p class="text-sm opacity-70">
            Capsula keeps one wakeword. A new recording session replaces it only after the new recordings pass the checks. Your choice and the detection switch are not changed automatically.
          </p>

          <details v-if="samples.length" class="rounded-box border border-base-300">
            <summary class="cursor-pointer px-4 py-3 text-sm font-medium">
              Recordings on Capsula
              <span class="badge badge-ghost badge-sm ml-1">{{ samples.length }}</span>
            </summary>
            <ul class="divide-y divide-base-300 border-t border-base-300">
              <li v-for="sample in savedTakes" :key="sample.sampleId" class="flex flex-wrap items-center gap-x-3 gap-y-1 px-4 py-2 text-sm">
                <span class="min-w-0 flex-1">
                  <span class="block">{{ labelFor(sample) }}</span>
                  <span class="block text-xs opacity-60">{{ dateTime(sample.recordedUnixMs) }} · {{ durationMs(sample.durationMs) }}</span>
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
            <p class="border-t border-base-300 px-4 py-2 text-xs opacity-60">
              Deleting a recording does not change the wakeword Capsula is using. Only recordings chosen in a new setup are used for training.
            </p>
          </details>
        </div>
      </div>

      <div class="card border border-base-300 bg-base-100 xl:col-span-2">
        <div class="card-body gap-3 p-4 sm:p-5">
          <div class="flex flex-wrap items-center justify-between gap-2">
            <h3 class="card-title text-base">Recent detections</h3>
            <span class="badge badge-ghost badge-sm">{{ detections.length }}</span>
          </div>
          <p class="text-sm opacity-70">
            Times Capsula heard your wakeword hands-free, newest first. Button presses are not listed.
          </p>
          <div v-if="!wakewordData && !wakewordError" class="skeleton h-16 w-full"></div>
          <ul v-else-if="detections.length" class="divide-y divide-base-300 rounded-box border border-base-300">
            <li v-for="(detection, index) in detections" :key="`${detection.detectedUnixMs}-${detection.modelId}-${index}`" class="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1 px-4 py-2 text-sm">
              <span class="font-medium">{{ detection.modelDisplayName || 'Your wakeword' }}</span>
              <span class="text-xs opacity-60">{{ dateTime(detection.detectedUnixMs) }}</span>
            </li>
          </ul>
          <div v-else-if="wakewordError" class="rounded-box border border-dashed border-base-300 p-4 text-sm opacity-70">
            Detection history is unavailable while the connection is down.
          </div>
          <div v-else class="rounded-box border border-dashed border-base-300 p-4 text-sm opacity-70">
            <template v-if="!enabled">Detection is off, so Capsula is not listening for the wakeword.</template>
            <template v-else-if="!selectedModel?.available">No wakeword is ready to use yet, so there is nothing to detect.</template>
            <template v-else>Capsula has not heard the wakeword hands-free yet. Detections appear here when it does.</template>
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
  </section>
</template>
