<script setup>
import { computed, ref } from 'vue'
import ModalDialog from '../components/ModalDialog.vue'
import ApplicationView from './ApplicationView.vue'
import { link } from '../router.js'
import { useTopic } from '../lib/live.js'
import { notify } from '../lib/toast.js'
import { formatBytes, pct } from '../lib/format.js'
import {
  installOutcome,
  installPackage,
  MAX_PACKAGE_BYTES,
  packageFileError,
  removePackage
} from '../lib/apps.js'

// Apps manages installed packages. The device validates the archive, swaps the
// package atomically and refuses to touch an app that is running; this page
// only offers the actions and reports what the device decided. Run control
// (launch and stop) stays on the Overview page.

const { data, error, loading, refresh } = useTopic('overview')

const apps = computed(() => data.value?.apps || [])
const runs = computed(() => data.value?.runs || [])

const runningApps = computed(() => {
  const set = new Set()
  for (const run of runs.value) set.add(run.appId)
  return set
})

const installed = computed(() =>
  [...apps.value].sort((a, b) =>
    String(a.displayName || a.appId).localeCompare(String(b.displayName || b.appId))
  )
)

const RUNTIME_LABEL = {
  RUNTIME_NATIVE: 'Native',
  RUNTIME_QUICKJS: 'QuickJS',
  RUNTIME_MICROPYTHON: 'MicroPython',
  RUNTIME_BUILTIN: 'Built-in'
}

function runtimeLabel(app) {
  return RUNTIME_LABEL[app?.entrypoint?.runtime] || ''
}

function isRunning(app) {
  return runningApps.value.has(app.appId)
}

function removalBlockedReason(app) {
  if (app.systemApp) return 'Built-in system apps cannot be removed'
  if (isRunning(app)) return 'Stop the app on Overview before removing its package'
  return ''
}

// --- detail -----------------------------------------------------------------

// App detail is the same live surface as Overview, so the user guide,
// settings and run status stay in one place. The page only opens it; the
// detail view owns fetching and dismissing the guide.
const selectedApp = ref('')

// --- install ----------------------------------------------------------------

const picked = ref(null)
const dragging = ref(false)
const busy = ref(false)
const progress = ref(0)
const failure = ref('')
const fileInput = ref(null)

const pickedIssue = computed(() => (picked.value ? packageFileError(picked.value) : ''))
const canInstall = computed(() => Boolean(picked.value) && !pickedIssue.value && !busy.value)
const pickedLabel = computed(() => {
  if (!picked.value) return 'No archive chosen'
  return `${picked.value.name} · ${formatBytes(picked.value.size)}`
})
const phase = computed(() => {
  if (!busy.value) return ''
  if (progress.value >= 1) return 'Installing on the speaker…'
  return `Sending the archive… ${pct(progress.value)}%`
})

function chooseFile(file) {
  if (!file) return
  failure.value = ''
  progress.value = 0
  picked.value = file
}

function onPick(event) {
  chooseFile(event.target.files?.[0])
  // Allow re-picking the same archive after a failed install.
  if (event.target) event.target.value = ''
}

function onDrop(event) {
  dragging.value = false
  chooseFile(event.dataTransfer?.files?.[0])
}

function startDrag(event) {
  event.preventDefault()
  dragging.value = true
}

function resetDraft() {
  picked.value = null
  progress.value = 0
  if (fileInput.value) fileInput.value.value = ''
}

async function submitInstall() {
  if (!canInstall.value) return
  busy.value = true
  progress.value = 0
  failure.value = ''
  try {
    const result = await installPackage(picked.value, {
      onProgress(value) {
        progress.value = value
      }
    })
    progress.value = 1
    notify.success(installOutcome(result))
    resetDraft()
    await refresh()
  } catch (err) {
    // The device's refusal is the actionable text (a running app, a rejected
    // archive, a native SDK requirement); keep it visible beside the form
    // instead of letting a toast disappear.
    failure.value = err?.message || 'The device rejected the package.'
  } finally {
    busy.value = false
  }
}

// --- remove -----------------------------------------------------------------

const removeTarget = ref(null)
const removeBusy = ref(false)

function askRemove(app) {
  if (removalBlockedReason(app)) return
  removeTarget.value = app
}

async function confirmRemove() {
  if (!removeTarget.value) return
  const app = removeTarget.value
  removeBusy.value = true
  try {
    await removePackage(app.appId)
    removeTarget.value = null
    notify.success(`Removed ${app.displayName || app.appId}. Its settings are kept for a reinstall.`)
    await refresh()
  } catch (err) {
    notify.error(err?.message || 'The device rejected the removal.')
  } finally {
    removeBusy.value = false
  }
}
</script>

<template>
  <div class="space-y-4">
    <header class="flex flex-wrap items-end justify-between gap-3">
      <div>
        <p class="text-xs font-semibold uppercase tracking-[0.16em] text-base-content/60">Packages</p>
        <h1 class="text-2xl font-semibold tracking-tight">Apps</h1>
        <p class="mt-1 max-w-3xl text-sm text-base-content/70">
          Applications installed on this speaker. An install is validated, swapped atomically and
          active immediately; start and stop an app from <a v-bind="link('/')" class="link">Overview</a>.
          Built-in system apps ship with the daemon and cannot be removed.
        </p>
      </div>
      <button type="button" class="btn btn-sm btn-ghost" :disabled="loading" @click="refresh">
        <span v-if="loading" class="loading loading-spinner loading-xs" aria-hidden="true"></span>
        Refresh
      </button>
    </header>

    <section class="card border border-base-300 bg-base-100">
      <div class="card-body gap-4 p-5">
        <div>
          <h2 class="card-title text-base">Install or update a package</h2>
          <p class="mt-1 text-sm text-base-content/70">
            Send a <code class="font-mono text-xs">.tar.gz</code> built by
            <code class="font-mono text-xs">capsulad-cli app package</code>, up to
            {{ formatBytes(MAX_PACKAGE_BYTES) }}. Installing over an existing app replaces its package
            and keeps that app's settings; the app must be stopped first.
          </p>
        </div>

        <div
          class="rounded-xl border-2 border-dashed p-4 transition-colors"
          :class="dragging ? 'border-primary bg-base-200' : 'border-base-300'"
          @dragenter="startDrag"
          @dragover="startDrag"
          @dragleave.prevent="dragging = false"
          @drop.prevent="onDrop"
        >
          <input
            ref="fileInput"
            type="file"
            class="file-input file-input-bordered w-full"
            accept=".tar.gz,application/gzip"
            aria-label="Choose an application package archive"
            :disabled="busy"
            @change="onPick"
          />
          <p class="mt-2 text-xs text-base-content/60">…or drop a package archive onto this area.</p>
          <p class="mt-1 break-all text-xs" :class="pickedIssue ? 'text-error' : 'text-base-content/70'">
            {{ pickedIssue || pickedLabel }}
          </p>
        </div>

        <div class="flex flex-wrap items-center gap-3">
          <button
            type="button"
            class="btn btn-primary btn-sm"
            :disabled="!canInstall"
            :aria-busy="busy ? 'true' : 'false'"
            @click="submitInstall"
          >
            <span v-if="busy" class="loading loading-spinner loading-xs" aria-hidden="true"></span>
            {{ busy ? 'Installing…' : 'Install' }}
          </button>
          <span v-if="busy" class="text-xs text-base-content/70" role="status">{{ phase }}</span>
          <button v-else-if="picked" type="button" class="btn btn-ghost btn-sm" @click="resetDraft">Clear</button>
        </div>

        <progress
          v-if="busy"
          class="progress progress-primary w-full"
          :value="Math.round(progress * 100)"
          max="100"
          aria-label="Package upload progress"
        ></progress>

        <div v-if="failure" role="alert" class="alert alert-error py-2 text-sm">
          <span class="break-words">{{ failure }}</span>
          <button type="button" class="btn btn-sm btn-outline" :disabled="busy" @click="failure = ''">Dismiss</button>
        </div>
      </div>
    </section>

    <section class="card border border-base-300 bg-base-100">
      <div class="card-body gap-3 p-5">
        <div class="flex flex-wrap items-baseline justify-between gap-2">
          <h2 class="card-title text-base">Installed packages</h2>
          <p class="text-xs text-base-content/60">
            {{ installed.length }} {{ installed.length === 1 ? 'application' : 'applications' }}
          </p>
        </div>

        <div v-if="error" role="alert" class="alert alert-error py-2 text-sm">
          <span>
            <strong class="font-semibold">Application list unavailable.</strong>
            {{ error }}
          </span>
          <button type="button" class="btn btn-sm btn-outline" :disabled="loading" @click="refresh">Try again</button>
        </div>

        <div v-if="loading && !data" class="space-y-2" aria-hidden="true">
          <div class="skeleton h-8 w-full"></div>
          <div class="skeleton h-8 w-full"></div>
          <div class="skeleton h-8 w-2/3"></div>
        </div>

        <div v-else-if="installed.length === 0" class="rounded-xl border border-dashed border-base-300 p-6 text-center">
          <p class="text-sm font-medium">No applications are installed</p>
          <p class="mt-1 text-sm text-base-content/70">
            Install a package above, or deploy an application bundle to this speaker.
          </p>
        </div>

        <div v-else class="overflow-x-auto">
          <table class="table table-sm">
            <thead>
              <tr>
                <th scope="col">Application</th>
                <th scope="col">Version</th>
                <th scope="col">Runtime</th>
                <th scope="col">State</th>
                <th scope="col"><span class="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="app in installed" :key="app.appId">
                <td class="max-w-[24rem]">
                  <span class="block font-medium">{{ app.displayName || app.appId }}</span>
                  <span class="block break-all font-mono text-xs text-base-content/60">{{ app.appId }}</span>
                  <span class="mt-1 flex flex-wrap gap-1">
                    <span v-if="app.systemApp" class="badge badge-ghost badge-sm">System</span>
                    <span v-if="app.startOnBoot" class="badge badge-ghost badge-sm">Starts at boot</span>
                  </span>
                </td>
                <td class="whitespace-nowrap text-xs">{{ app.version || '—' }}</td>
                <td class="whitespace-nowrap text-xs">{{ runtimeLabel(app) || '—' }}</td>
                <td class="whitespace-nowrap">
                  <span v-if="isRunning(app)" class="badge badge-success badge-sm gap-1">
                    <span class="inline-block h-1.5 w-1.5 rounded-full bg-current" aria-hidden="true"></span>
                    Running
                  </span>
                  <span v-else class="badge badge-ghost badge-sm">Stopped</span>
                </td>
                <td class="text-right">
                  <div class="flex flex-wrap justify-end gap-1">
                    <button
                      type="button"
                      class="btn btn-xs btn-outline"
                      :aria-label="`Open details for ${app.displayName || app.appId}`"
                      @click="selectedApp = app.appId"
                    >Details</button>
                    <button
                      v-if="!app.systemApp"
                      type="button"
                      class="btn btn-error btn-xs btn-outline"
                      :disabled="Boolean(removalBlockedReason(app))"
                      :title="removalBlockedReason(app) || `Remove ${app.displayName || app.appId}`"
                      :aria-label="`Remove ${app.displayName || app.appId}`"
                      @click="askRemove(app)"
                    >Remove</button>
                    <span v-else class="text-xs text-base-content/50">Built in</span>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <p class="text-xs text-base-content/60">
          Removing a package keeps that application's settings; reinstalling the same application ID
          finds them again. Stopping a running app is a run action on Overview, not a package action.
        </p>
      </div>
    </section>

    <ModalDialog v-if="removeTarget" title="Remove application" @close="removeTarget = null">
      <p class="text-sm">
        Remove <strong>{{ removeTarget.displayName || removeTarget.appId }}</strong>
        (<code class="font-mono text-xs">{{ removeTarget.appId }}</code>) from this speaker?
      </p>
      <p class="mt-2 text-sm text-base-content/70">
        The package directory is deleted and the app disappears from the device at once. App settings
        are kept, so reinstalling the same application ID keeps its configuration. This cannot be undone
        without installing the package again.
      </p>
      <template #footer>
        <button type="button" class="btn btn-ghost" :disabled="removeBusy" @click="removeTarget = null">Cancel</button>
        <button
          type="button"
          class="btn btn-error"
          :disabled="removeBusy"
          :aria-busy="removeBusy ? 'true' : 'false'"
          @click="confirmRemove"
        >
          <span v-if="removeBusy" class="loading loading-spinner loading-xs" aria-hidden="true"></span>
          Remove
        </button>
      </template>
    </ModalDialog>

    <ApplicationView
      v-if="selectedApp"
      :key="selectedApp"
      :app-id="selectedApp"
      @close="selectedApp = ''"
    />
  </div>
</template>
