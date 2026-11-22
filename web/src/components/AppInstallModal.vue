<script setup>
import { computed, onMounted, onUnmounted, ref, useTemplateRef } from 'vue'
import ModalDialog from '../components/ModalDialog.vue'
import Icon from '../components/Icon.vue'
import { formatBytes } from '../lib/format.js'
import { notify } from '../lib/toast.js'
import {
  appStorage,
  installPackage,
  installPackageFromURL,
  MAX_PACKAGE_BYTES,
  packageFileError,
  packageURLError
} from '../lib/apps.js'

// The install dialog owns the pre-flight facts (applications filesystem
// capacity), the two package sources (a local archive or an http(s) URL) and
// the cancellation contract. The daemon still validates every archive; this
// component only refuses obviously unusable input before a transfer starts.

const emit = defineEmits(['close', 'installed'])

const storage = ref(null)
const storageError = ref('')
const storageLoading = ref(true)
const storageController = new AbortController()

const url = ref('')
const dragging = ref(false)
const error = ref('')
const fileBusy = ref(false)
const urlBusy = ref(false)
const progress = ref(0)
const fileInput = useTemplateRef('fileInput')

let activeController = null

const busy = computed(() => fileBusy.value || urlBusy.value)

const storagePercent = computed(() => {
  if (!storage.value?.available) return null
  const total = Number(storage.value.totalBytes) || 0
  const used = Number(storage.value.usedBytes) || 0
  if (total <= 0) return null
  return Math.max(0, Math.min(100, Math.round((used / total) * 100)))
})

// <75% is healthy, 75–90% is worth watching, and at 90% or above the next
// package may not fit. The number is the same fact; only the colour changes.
const storageTone = computed(() => {
  const percent = storagePercent.value
  if (percent === null) return ''
  if (percent < 75) return 'text-success'
  if (percent < 90) return 'text-warning'
  return 'text-error'
})

const freeText = computed(() =>
  storage.value?.available ? formatBytes(storage.value.freeBytes) : 'Unavailable')

const usedText = computed(() => {
  if (!storage.value?.available || storagePercent.value === null) return 'Unavailable'
  return `${formatBytes(storage.value.usedBytes)} (${storagePercent.value}%)`
})

async function loadStorage() {
  storageLoading.value = true
  storageError.value = ''
  try {
    storage.value = await appStorage({ signal: storageController.signal })
  } catch (err) {
    if (err?.name === 'AbortError') return
    storage.value = { available: false, totalBytes: 0, freeBytes: 0, usedBytes: 0 }
    storageError.value = 'The applications filesystem could not be read.'
  } finally {
    storageLoading.value = false
  }
}

onMounted(loadStorage)
onUnmounted(() => {
  storageController.abort()
  activeController?.abort()
})

function cancelled(err) {
  return err?.name === 'AbortError' || err?.message === 'install cancelled'
}

function beforeClose() {
  // Closing while an install is in flight would hide progress without stopping
  // the transfer; the Cancel button aborts first.
  return !busy.value
}

function cancelInstall() {
  if (!busy.value) return
  activeController?.abort()
}

function reportFailure(err) {
  if (cancelled(err)) {
    notify.info('Install cancelled.')
    return
  }
  error.value = err?.message || 'The install failed.'
  notify.error(error.value)
}

async function installFile(file) {
  if (busy.value) return
  const problem = packageFileError(file)
  if (problem) {
    error.value = problem
    notify.error(problem)
    return
  }
  error.value = ''
  progress.value = 0
  fileBusy.value = true
  const controller = new AbortController()
  activeController = controller
  try {
    const result = await installPackage(file, {
      signal: controller.signal,
      onProgress(value) {
        progress.value = value
      }
    })
    progress.value = 1
    emit('installed', result)
    emit('close')
  } catch (err) {
    reportFailure(err)
  } finally {
    if (activeController === controller) activeController = null
    fileBusy.value = false
  }
}

async function installFromURL() {
  if (busy.value) return
  const problem = packageURLError(url.value)
  if (problem) {
    error.value = problem
    notify.error(problem)
    return
  }
  error.value = ''
  urlBusy.value = true
  const controller = new AbortController()
  activeController = controller
  try {
    const result = await installPackageFromURL(url.value.trim(), { signal: controller.signal })
    emit('installed', result)
    emit('close')
  } catch (err) {
    reportFailure(err)
  } finally {
    if (activeController === controller) activeController = null
    urlBusy.value = false
  }
}

function pickFile() {
  if (busy.value) return
  fileInput.value?.click()
}

function onPick(event) {
  const file = event.target?.files?.[0]
  // Allow re-picking the same file after a failed install.
  if (event.target) event.target.value = ''
  installFile(file)
}

function startDrag() {
  if (!busy.value) dragging.value = true
}

function onDrop(event) {
  dragging.value = false
  if (busy.value) return
  installFile(event.dataTransfer?.files?.[0])
}
</script>

<template>
  <ModalDialog
    max-width="max-w-xl"
    title="Install an app"
    :before-close="beforeClose"
    @close="emit('close')"
  >
    <div class="flex flex-wrap items-center justify-between gap-4 rounded border border-base-300 p-3">
      <dl class="flex flex-col justify-between gap-2">
        <div>
          <dt class="text-xs uppercase tracking-wide text-base-content/60">Disk space free</dt>
          <dd class="data-value">{{ freeText }}</dd>
        </div>
        <div>
          <dt class="text-xs uppercase tracking-wide text-base-content/60">Disk space used</dt>
          <dd class="data-value">{{ usedText }}</dd>
        </div>
      </dl>

      <div
        v-if="storagePercent !== null"
        class="radial-progress"
        :class="storageTone"
        :style="{ '--value': storagePercent }"
        :aria-valuenow="storagePercent"
        role="progressbar"
        :aria-label="`${storagePercent}% of the applications filesystem used`"
      >
        {{ storagePercent }}%<br/>
        <span class="text-xs uppercase tracking-wide text-base-content/60">used</span>
      </div>
      <div v-else class="text-sm text-base-content/60" role="status">Unavailable</div>
    </div>

    <p v-if="storageLoading" class="mt-1 text-xs text-base-content/60">Checking the applications filesystem…</p>
    <p v-else-if="storageError" class="mt-1 text-xs text-warning">{{ storageError }}</p>

    <p class="mt-3 text-sm">
      Choose a .tar.gz app package file or enter a URL to install the app.
    </p>

    <input
      ref="fileInput"
      type="file"
      class="hidden"
      accept=".tar.gz,application/gzip"
      :disabled="busy"
      @change="onPick"
    />

    <button
      type="button"
      class="mt-2 flex w-full flex-col items-center gap-1 rounded-box border-2 border-dashed p-6 text-center transition-colors disabled:opacity-50"
      :class="dragging ? 'border-primary bg-base-200' : 'border-base-300 hover:border-primary'"
      :disabled="busy"
      aria-label="Choose a .tar.gz package file, or drop one here"
      @click="pickFile"
      @dragenter.prevent="startDrag"
      @dragover.prevent="startDrag"
      @dragleave.prevent="dragging = false"
      @drop.prevent="onDrop"
    >
      <Icon size="32px" class="text-primary">cloud_upload</Icon>
      <span class="text-sm font-medium">
        {{ dragging ? 'Drop the package to install it' : 'Drop a .tar.gz package here or choose a file' }}
      </span>
      <span class="text-xs text-base-content/60">
        The speaker accepts at most {{ formatBytes(MAX_PACKAGE_BYTES) }} per package.
      </span>
    </button>

    <div v-if="fileBusy" class="mt-2 flex items-center gap-3" role="status">
      <progress
        class="progress progress-primary flex-1"
        :value="Math.round(progress * 100)"
        max="100"
        aria-label="Upload progress"
      />
      <span class="text-sm font-mono">{{ Math.round(progress * 100) }}%</span>
    </div>

    <form class="mt-4 flex flex-col gap-2" @submit.prevent="installFromURL">
      <label for="app-package-url" class="text-sm font-medium">Install from a URL</label>
      <div class="flex flex-wrap gap-2">
        <input
          id="app-package-url"
          v-model="url"
          type="url"
          inputmode="url"
          autocomplete="off"
          spellcheck="false"
          placeholder="https://example.com/app-1.0.0.tar.gz"
          class="input input-bordered input-sm min-w-0 flex-1"
          :disabled="busy"
        />
        <button type="submit" class="btn btn-primary btn-sm" :disabled="busy">
          <Icon size="16px">download</Icon>
          Install
        </button>
      </div>
      <p class="text-xs text-base-content/60">
        The speaker downloads the archive over http or https and validates it like an upload.
      </p>
    </form>

    <div v-if="urlBusy" class="mt-2 flex items-center gap-2 text-sm" role="status">
      <span class="loading loading-spinner loading-sm"></span>
      <span>Downloading and installing…</span>
    </div>

    <div v-if="error" class="alert alert-error mt-3 py-2 text-sm" role="alert">
      <span>{{ error }}</span>
    </div>

    <div v-if="busy" class="mt-3 flex justify-end">
      <button type="button" class="btn btn-ghost btn-sm" @click="cancelInstall">
        Cancel install
      </button>
    </div>
  </ModalDialog>
</template>
