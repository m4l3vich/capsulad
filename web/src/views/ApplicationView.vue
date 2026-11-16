<script setup>
import { computed, nextTick, onUnmounted, reactive, ref, watch } from 'vue'
import { socket } from '../lib/ws.js'
import { liveState, useTopic } from '../lib/live.js'
import { SettingsFrameHost } from '../lib/appSettingsBridge.js'
import { logLine, safeJson, shortId, status } from '../lib/format.js'
import { notify } from '../lib/toast.js'
import ModalDialog from '../components/ModalDialog.vue'

const props = defineProps({
  appId: { type: String, required: true }
})

const emit = defineEmits(['close'])

const { data, error: loadError, refresh } = useTopic('application', props.appId, { logs: true })

const busy = ref('')
const settingBusy = ref('')
const discardPrompt = ref(false)
const launchDirty = ref(false)

const configDraft = reactive({})
const draftDirty = reactive({})
const maskedInput = reactive({})
const clearMasked = reactive({})
let configRevision = 0

const launchArgs = reactive({})
let launchInitialized = ''

const settingsFrame = ref(null)
const settingsUI = ref(null)
const frameError = ref('')
let frameHost = null
let frameTimer = null
let uiGeneration = 0

const title = computed(() => data.value?.app?.displayName || props.appId)
const valueMap = computed(() => {
  const out = {}
  for (const value of data.value?.values || []) out[value.key] = value
  return out
})
const descriptions = computed(() => {
  const out = {}
  for (const desc of data.value?.descriptions || []) out[desc.grpcService] = desc
  return out
})
const schemaSettings = computed(() => data.value?.schema?.settings || [])
const schemaActions = computed(() => data.value?.schema?.actions || [])
const provenance = computed(() => data.value?.provenance || null)
const interfaces = computed(() => data.value?.interfaces || [])
const logs = computed(() => data.value?.logs || [])
const runs = computed(() => data.value?.runs || [])
const hasSettingsDocument = computed(() => !!data.value?.schema?.settingsUi?.available)
const connected = computed(() => liveState.value === 'open')

function syncSettingsState() {
  const view = data.value
  if (!view) return
  configRevision = Math.max(configRevision, Number(view.configRevision) || 0)
  syncDrafts(view)
  maybeMountSettingsUI()
  updateFrameHost()
}

// Log pushes replace the view wrapper every few hundred milliseconds; watch
// only the references that can change settings, not the wrapper.
watch(() => data.value?.app, (app) => {
  if (app) syncLaunchArgs(data.value)
}, { immediate: true })
watch(() => data.value?.schema, syncSettingsState, { immediate: true })
watch(() => data.value?.values, syncSettingsState, { immediate: true })
watch(() => data.value?.configRevision, syncSettingsState, { immediate: true })

// A refused or failed snapshot stays an inline error on the page; it is also
// announced once per distinct failure so the reason is not only visible after
// scrolling. The transport is untouched: a per-request error never closes the
// socket or starts a reconnect epoch.
let lastLoadError = ''
watch(loadError, (value) => {
  if (!value) {
    lastLoadError = ''
    return
  }
  if (value === lastLoadError) return
  lastLoadError = value
  notify.warning(`The last refresh failed: ${value}`)
})

onUnmounted(() => {
  teardownFrame()
})

// --- generic application settings -----------------------------------------

function valueForInput(def, state) {
  if (def.type?.includes('BOOLEAN')) {
    if (!state?.hasValue) return safeJson(def.defaultValueJson) === 'true'
    return safeJson(state.valueJson) === 'true'
  }
  if (!state?.hasValue) return def.defaultValueJson ? safeJson(def.defaultValueJson) : ''
  return safeJson(state.valueJson)
}

function isDraftDirty(def) {
  const type = def.type || ''
  if (type.includes('TEXT_MASKED')) return !!maskedInput[def.key] || !!clearMasked[def.key]
  if (type.includes('BOOLEAN') || type.includes('SELECT')) return false
  return !!draftDirty[def.key]
}

function syncDrafts(view) {
  for (const def of view.schema?.settings || []) {
    const type = def.type || ''
    if (type.includes('TEXT_MASKED')) continue
    // Never overwrite a value the user is editing. The dirty flag is set only
    // by user input, so the first snapshot initializes every field.
    if (!type.includes('BOOLEAN') && !type.includes('SELECT') && draftDirty[def.key]) continue
    configDraft[def.key] = valueForInput(def, (view.values || []).find((v) => v.key === def.key))
  }
}

function hasDrafts() {
  return schemaSettings.value.some((def) => isDraftDirty(def))
}

function hasUnsavedInputs() {
  return hasDrafts() || launchDirty.value
}

// The modal owner asks before dropping drafts or while a settings save is
// still in flight. Nothing is discarded silently.
function requestClose() {
  if (settingBusy.value) {
    notify.info('Changes are still being saved. Wait for the current save to finish, then close.')
    return false
  }
  if (discardPrompt.value) return false
  if (!hasUnsavedInputs()) return true
  discardPrompt.value = true
  return false
}

function discardAndClose() {
  if (settingBusy.value) {
    notify.info('Changes are still being saved. Wait for the current save to finish, then close.')
    discardPrompt.value = false
    return
  }
  for (const key of Object.keys(draftDirty)) delete draftDirty[key]
  for (const key of Object.keys(maskedInput)) delete maskedInput[key]
  for (const key of Object.keys(clearMasked)) delete clearMasked[key]
  launchDirty.value = false
  discardPrompt.value = false
  emit('close')
}

// --- app settings patches --------------------------------------------------

let patchChain = Promise.resolve()

// The target app is captured when a patch is issued so a closed or reopened
// modal cannot apply one app's response to another.
function applyAppChanges(changes, expectedRevision, appId = props.appId) {
  const app = appId
  const run = async () => {
    if (!data.value || data.value.app?.appId !== app) {
      return { ok: false, error: 'application changed before the update was sent' }
    }
    if ((data.value.values || []).length === 0 && (data.value.schema?.settings || []).length) {
      await refresh()
      if (!data.value || data.value.app?.appId !== app || (data.value.values || []).length === 0) {
        return { ok: false, error: 'settings snapshot is unavailable' }
      }
    }
    const revision = Math.max(configRevision, Number(expectedRevision) || 0)
    try {
      const result = await socket.request({
        appSettings: { appId: app, expectedRevision: revision, changes }
      })
      const r = result?.appSettings
      if (!data.value || data.value.app?.appId !== app) {
        return { ok: true, revision: Number(r?.revision) || 0, values: r?.values, message: 'Saved.' }
      }
      configRevision = Math.max(configRevision, Number(r?.revision) || 0)
      data.value = {
        ...data.value,
        configRevision,
        values: r?.values?.length ? r.values : data.value.values
      }
      syncDrafts(data.value)
      updateFrameHost()
      return { ok: true, revision: configRevision, values: data.value.values, message: r?.message || 'Saved.' }
    } catch (err) {
      const message = err?.message || 'The update failed.'
      if (!data.value || data.value.app?.appId !== app) {
        return { ok: false, error: message }
      }
      notify.error(message)
      await refresh()
      return { ok: false, error: message, revision: configRevision, values: data.value?.values }
    }
  }
  const promise = patchChain.then(run, run)
  patchChain = promise.catch(() => {})
  return promise
}

async function applyImmediate(key, value) {
  const token = `set:${key}`
  settingBusy.value = token
  const result = await applyAppChanges([{ key, valueJson: JSON.stringify(value) }], configRevision)
  if (settingBusy.value === token) settingBusy.value = ''
  if (result.ok) {
    notify.success('Setting applied')
    if (!hasUnsavedInputs()) discardPrompt.value = false
  }
}

function buildDraftChanges() {
  const changes = []
  for (const def of schemaSettings.value) {
    const type = def.type || ''
    if (type.includes('BOOLEAN') || type.includes('SELECT')) continue
    if (type.includes('TEXT_MASKED')) {
      if (clearMasked[def.key]) {
        changes.push({ key: def.key, clear: true })
        continue
      }
      if (maskedInput[def.key]) changes.push({ key: def.key, valueJson: JSON.stringify(maskedInput[def.key]) })
      continue
    }
    if (!isDraftDirty(def)) continue
    const raw = configDraft[def.key]
    if (type.includes('INTEGER') || type.includes('NUMBER')) {
      const text = String(raw ?? '').trim()
      if (text === '') {
        if (def.required) throw new Error(`${def.label} is required`)
        continue
      }
      const number = Number(text)
      if (!Number.isFinite(number)) throw new Error(`${def.label} must be a number`)
      if (type.includes('INTEGER') && !Number.isInteger(number)) throw new Error(`${def.label} must be a whole number`)
      if (def.hasMin && number < def.min) throw new Error(`${def.label} must be at least ${def.min}`)
      if (def.hasMax && number > def.max) throw new Error(`${def.label} must be at most ${def.max}`)
      changes.push({ key: def.key, valueJson: JSON.stringify(number) })
      continue
    }
    if (def.required && String(raw ?? '') === '') throw new Error(`${def.label} is required`)
    changes.push({ key: def.key, valueJson: JSON.stringify(String(raw ?? '')) })
  }
  return changes
}

async function saveConfig() {
  let changes
  try {
    changes = buildDraftChanges()
  } catch (err) {
    notify.error(err.message)
    return
  }
  if (!changes.length) {
    notify.info('No text or number changes to save')
    return
  }
  settingBusy.value = 'config'
  const result = await applyAppChanges(changes, configRevision)
  settingBusy.value = ''
  if (result.ok) {
    for (const key of Object.keys(configDraft)) {
      delete draftDirty[key]
      delete maskedInput[key]
      delete clearMasked[key]
    }
    syncDrafts(data.value)
    notify.success('Configuration saved')
    if (!hasUnsavedInputs()) discardPrompt.value = false
  }
}

// --- custom settings document ---------------------------------------------

function teardownFrame() {
  uiGeneration += 1
  if (frameTimer) {
    clearTimeout(frameTimer)
    frameTimer = null
  }
  if (frameHost) {
    frameHost.dispose()
    frameHost = null
  }
  settingsUI.value = null
  frameError.value = ''
}

async function maybeMountSettingsUI() {
  const ui = data.value?.schema?.settingsUi
  if (!ui?.available || !ui.assetUrl) return
  if (frameHost && settingsUI.value?.assetUrl === ui.assetUrl) return
  if (frameHost) teardownFrame()
  const generation = ++uiGeneration
  const app = props.appId
  settingsUI.value = ui
  await nextTick()
  if (generation !== uiGeneration || !data.value || data.value.app?.appId !== app) return
  const iframe = settingsFrame.value
  if (!iframe) return
  frameError.value = ''
  frameHost = new SettingsFrameHost({
    iframe,
    appId: app,
    schema: data.value.schema,
    revision: configRevision,
    values: data.value.values,
    applyChanges: (changes, expectedRevision) => applyAppChanges(changes, expectedRevision, app),
    onStatus: (next) => {
      if (next?.error) {
        frameError.value = next.error
        notify.error(next.error)
        if (frameTimer) {
          clearTimeout(frameTimer)
          frameTimer = null
        }
        if (frameHost) {
          frameHost.dispose()
          frameHost = null
        }
      } else if (next?.ready) {
        if (frameTimer) {
          clearTimeout(frameTimer)
          frameTimer = null
        }
      }
    }
  })
  frameHost.arm(ui.assetUrl)
  if (frameTimer) clearTimeout(frameTimer)
  frameTimer = setTimeout(() => {
    frameTimer = null
    if (frameHost && !frameHost.ready) {
      frameError.value = 'The settings document did not become ready.'
      notify.error(frameError.value)
      frameHost.dispose()
      frameHost = null
    }
  }, 10000)
}

function updateFrameHost() {
  if (!frameHost || !data.value) return
  frameHost.updateState({ revision: configRevision, values: data.value.values })
}

// --- lifecycle, launch and actions ----------------------------------------

function syncLaunchArgs(view) {
  const appId = view.app?.appId
  if (!appId || launchInitialized === appId) return
  launchInitialized = appId
  for (const arg of view.app.launchArguments || []) {
    launchArgs[arg.name] = arg.value || ''
  }
}

async function act(key, command, message) {
  if (busy.value) return null
  if (liveState.value !== 'open') {
    notify.error('The speaker is not connected.')
    return null
  }
  busy.value = key
  try {
    const result = await socket.request(command)
    if (message) notify.success(message)
    return result
  } catch (err) {
    notify.error(err?.message || 'The device rejected the request.')
    return null
  } finally {
    busy.value = ''
  }
}

async function launch() {
  const app = data.value?.app
  if (!app) return
  const args = []
  for (const arg of app.launchArguments || []) {
    const value = String(launchArgs[arg.name] || '').trim()
    if (!value) {
      if (arg.required) {
        notify.error(`${arg.name} is required to launch this application.`)
        return
      }
      continue
    }
    args.push({ name: arg.name, value })
  }
  const result = await act('launch', { launch: { appId: app.appId, arguments: args } }, 'Launch requested')
  if (result?.launch) {
    launchDirty.value = false
    if (!hasUnsavedInputs()) discardPrompt.value = false
    await refresh()
  }
}

async function stop(runItem) {
  const result = await act(
    `stop:${runItem.runId}`,
    { stop: { appId: props.appId, runId: runItem.runId } }
  )
  if (result?.stop?.message) notify.info(result.stop.message)
  await refresh()
}

async function trigger(action) {
  if (action.confirmBeforeRun && !window.confirm(`${action.label}?`)) return
  const result = await act(
    `action:${action.actionId}`,
    { action: { appId: props.appId, actionId: action.actionId } }
  )
  if (result?.action) notify.info('Action sent')
}

function uptime(ns) {
  const seconds = Number(ns) / 1e9
  if (!Number.isFinite(seconds) || seconds <= 0) return '—'
  return `${seconds.toFixed(1)} s after boot`
}
</script>

<template>
  <ModalDialog :title="title" :before-close="requestClose" @close="emit('close')">
    <div v-if="!data && !loadError" class="flex flex-col gap-3" aria-busy="true">
      <div class="skeleton h-4 w-2/3"></div>
      <div class="skeleton h-4 w-1/3"></div>
      <div class="skeleton h-24 w-full"></div>
    </div>

    <div v-else-if="loadError && !data" class="alert alert-error" role="alert">
      <span class="text-sm">Application unavailable: {{ loadError }}</span>
      <button type="button" class="btn btn-sm btn-ghost" @click="refresh">Try again</button>
    </div>

    <div v-else-if="data" class="flex flex-col gap-4">
      <div v-if="!connected" class="alert alert-warning py-2" role="status">
        <span class="text-sm">Disconnected: changes are paused until the connection returns.</span>
      </div>
      <div class="flex flex-wrap items-center gap-1.5">
        <span class="badge badge-ghost badge-sm">v{{ data.app.version || '—' }}</span>
        <span v-if="data.app.systemApp" class="badge badge-outline badge-sm">built-in</span>
        <span v-if="data.selectedDefault" class="badge badge-primary badge-sm">default app</span>
        <span v-if="data.app.background" class="badge badge-ghost badge-sm">runs in background</span>
      </div>

      <div v-if="discardPrompt" class="alert alert-warning py-2" role="alert">
        <span class="text-sm">You have unsaved changes.</span>
        <div class="flex gap-1">
          <button type="button" class="btn btn-sm" @click="discardPrompt = false">Keep editing</button>
          <button type="button" class="btn btn-sm btn-warning" @click="discardAndClose">Discard changes</button>
        </div>
      </div>

      <!-- Launch: required arguments are explicit, not guessed. -->
      <section class="rounded-lg border border-base-300 p-3">
        <h3 class="text-sm font-semibold uppercase tracking-wide text-base-content/60">Launch</h3>
        <form class="mt-2 flex flex-col gap-2" @submit.prevent="launch">
          <div v-if="(data.app.launchArguments || []).length" class="grid gap-2 sm:grid-cols-2">
            <label v-for="arg in data.app.launchArguments" :key="arg.name" class="form-control">
              <span class="label-text pb-1 text-sm">
                {{ arg.name }}
                <span v-if="arg.required" class="ml-1 text-error" title="Required">*</span>
              </span>
              <input
                v-model="launchArgs[arg.name]"
                class="input input-bordered input-sm w-full"
                :type="arg.valueType?.includes('INT') ? 'number' : 'text'"
                :placeholder="arg.valueType || 'value'"
                :required="arg.required"
                @input="launchDirty = true"
              />
              <span v-if="arg.description" class="mt-1 text-xs text-base-content/60">{{ arg.description }}</span>
            </label>
          </div>
          <p v-else class="text-xs text-base-content/60">This application declares no launch arguments.</p>
          <div class="flex flex-wrap items-center gap-2">
            <button type="submit" class="btn btn-primary btn-sm" :disabled="!!busy || !connected">
              <span v-if="busy === 'launch'" class="loading loading-spinner loading-xs"></span>
              Launch
            </button>
            <span v-if="data.app.startOnBoot" class="badge badge-ghost badge-sm">starts at boot</span>
          </div>
        </form>
      </section>

      <!-- Settings: the app's own document when declared, else its schema. -->
      <section v-if="hasSettingsDocument || schemaSettings.length" class="rounded-lg border border-base-300 p-3">
        <div class="flex flex-wrap items-center justify-between gap-2">
          <h3 class="text-sm font-semibold uppercase tracking-wide text-base-content/60">Settings</h3>
          <span v-if="hasSettingsDocument" class="badge badge-ghost badge-sm">provided by this app</span>
        </div>

        <div v-if="hasSettingsDocument" class="mt-2 flex flex-col gap-2">
          <iframe
            v-show="!frameError"
            ref="settingsFrame"
            class="h-[360px] min-h-[240px] w-full rounded-lg border border-base-300 bg-base-100"
            sandbox="allow-scripts"
            referrerpolicy="no-referrer"
            allow="camera 'none'; microphone 'none'; geolocation 'none'"
            :title="`${data.app.displayName} settings`"
          ></iframe>
          <details class="text-xs text-base-content/60">
            <summary class="cursor-pointer">Technical details</summary>
            <p class="mt-1 leading-relaxed">
              This document runs in an isolated frame with no network, host session or
              cross-application access. The console accepts only declared setting values that
              carry this frame’s token and checks them against the latest revision.
            </p>
          </details>
        </div>

        <form v-else class="mt-2 flex flex-col gap-3" @submit.prevent="saveConfig">
          <div v-for="def in schemaSettings" :key="def.key" class="form-control">
            <span class="label-text pb-1 text-sm font-medium">{{ def.label }}</span>
            <span v-if="def.description" class="pb-1 text-xs text-base-content/60">{{ def.description }}</span>
            <span v-if="def.restartRequired" class="pb-1 text-xs text-warning">Restart the application to apply.</span>

            <template v-if="def.type.includes('TEXT_MASKED')">
              <input
                v-model="maskedInput[def.key]"
                type="password"
                autocomplete="new-password"
                class="input input-bordered input-sm w-full"
                :placeholder="valueMap[def.key]?.hasValue ? 'configured — leave empty to keep' : 'not set'"
              />
              <label class="mt-1 flex items-center gap-2 text-xs">
                <input v-model="clearMasked[def.key]" type="checkbox" class="checkbox checkbox-sm" />
                <span>Clear the stored value</span>
              </label>
            </template>

            <label v-else-if="def.type.includes('BOOLEAN')" class="flex items-center gap-2">
              <input
                v-model="configDraft[def.key]"
                type="checkbox"
                class="toggle toggle-primary toggle-sm"
                :disabled="settingBusy === `set:${def.key}` || !connected"
                @change="applyImmediate(def.key, !!configDraft[def.key])"
              />
              <span class="text-sm">{{ configDraft[def.key] ? 'enabled' : 'disabled' }}</span>
            </label>

            <select
              v-else-if="def.type.includes('SELECT')"
              v-model="configDraft[def.key]"
              class="select select-bordered select-sm w-full"
              :disabled="settingBusy === `set:${def.key}` || !connected"
              @change="applyImmediate(def.key, String(configDraft[def.key]))"
            >
              <option v-for="choice in def.choices" :key="choice.value" :value="choice.value">{{ choice.label }}</option>
            </select>

            <input
              v-else-if="def.type.includes('INTEGER') || def.type.includes('NUMBER')"
              v-model="configDraft[def.key]"
              type="number"
              class="input input-bordered input-sm w-full"
              :min="def.hasMin ? def.min : undefined"
              :max="def.hasMax ? def.max : undefined"
              :step="def.type.includes('INTEGER') ? 1 : 'any'"
              @input="draftDirty[def.key] = true"
            />

            <input
              v-else
              v-model="configDraft[def.key]"
              type="text"
              class="input input-bordered input-sm w-full"
              @input="draftDirty[def.key] = true"
            />
          </div>

          <div class="flex flex-wrap items-center gap-2">
            <button type="submit" class="btn btn-primary btn-sm" :disabled="settingBusy === 'config' || !hasDrafts() || !connected">
              <span v-if="settingBusy === 'config'" class="loading loading-spinner loading-xs"></span>
              Save text and number fields
            </button>
            <span class="text-xs text-base-content/60">Toggles and choices apply as you change them. Text and number fields save with this button.</span>
          </div>
        </form>
      </section>

      <!-- Declared actions stay separate from settings. -->
      <section v-if="schemaActions.length" class="rounded-lg border border-base-300 p-3">
        <h3 class="text-sm font-semibold uppercase tracking-wide text-base-content/60">Actions</h3>
        <div class="mt-2 flex flex-col gap-2">
          <div v-for="action in schemaActions" :key="action.actionId" class="flex flex-wrap items-center justify-between gap-2">
            <div class="min-w-0">
              <p class="text-sm font-medium">{{ action.label }}</p>
              <p v-if="action.description" class="text-xs text-base-content/60">{{ action.description }}</p>
            </div>
            <button
              type="button"
              class="btn btn-sm btn-outline"
              :disabled="!!busy || !connected"
              :aria-label="`${action.label || 'Run action'} for ${data.app.displayName}`"
              @click="trigger(action)"
            >{{ action.label || 'Run action' }}</button>
          </div>
        </div>
      </section>

      <!-- Progressive detail: run state first, then metadata, contracts,
           verification and logs. -->
      <details class="rounded-lg border border-base-300">
        <summary class="cursor-pointer px-3 py-2 text-sm font-medium">
          Run status <span class="badge badge-ghost badge-sm ml-1">{{ runs.length }}</span>
        </summary>
        <div class="border-t border-base-300 p-3">
          <p v-if="data.app.canStop && data.selectedDefault" class="text-xs text-base-content/60">
            This app is the default for its role. Stopping it restarts it; choose another app or none in
            Settings to turn it off for good.
          </p>
          <div v-if="runs.length" class="mt-2 overflow-x-auto">
            <table class="table table-sm">
              <thead>
                <tr>
                  <th scope="col">Run</th>
                  <th scope="col">State</th>
                  <th scope="col" class="hidden sm:table-cell">PID</th>
                  <th scope="col" class="hidden md:table-cell">Detail</th>
                  <th scope="col" class="text-right"></th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="runItem in runs" :key="runItem.runId">
                  <td class="data-value" :title="runItem.runId">{{ shortId(runItem.runId) }}</td>
                  <td><span class="badge badge-sm" :class="status(runItem.state) === 'ready' ? 'badge-success' : 'badge-ghost'">{{ status(runItem.state) || 'run' }}</span></td>
                  <td class="hidden sm:table-cell tabular-nums">{{ runItem.pid || '—' }}</td>
                  <td class="hidden md:table-cell text-xs text-base-content/60">{{ runItem.statusDetail || uptime(runItem.startedMonotonicNs) }}</td>
                  <td>
                    <div class="flex justify-end">
                      <button
                        v-if="data.app.canStop"
                        type="button"
                        class="btn btn-xs"
                        :class="data.selectedDefault ? 'btn-outline' : 'btn-error btn-outline'"
                        :disabled="busy === `stop:${runItem.runId}` || !connected"
                        @click="stop(runItem)"
                      >{{ data.selectedDefault ? 'Restart' : 'Stop' }}</button>
                      <span v-else class="text-xs text-base-content/60">managed by the speaker</span>
                    </div>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
          <p v-else class="mt-2 text-sm text-base-content/60">Not running.</p>
        </div>
      </details>

      <details class="rounded-lg border border-base-300">
        <summary class="cursor-pointer px-3 py-2 text-sm font-medium">Application details</summary>
        <div class="border-t border-base-300 p-3">
          <dl class="grid gap-2 text-sm sm:grid-cols-2">
            <div>
              <dt class="text-xs uppercase tracking-wide text-base-content/60">Application ID</dt>
              <dd class="data-value">{{ data.app.appId }}</dd>
            </div>
            <div>
              <dt class="text-xs uppercase tracking-wide text-base-content/60">Entrypoint</dt>
              <dd class="data-value">{{ data.app.systemApp ? 'built into capsulad' : (data.app.entrypoint?.relativePath || '—') }}</dd>
            </div>
            <div>
              <dt class="text-xs uppercase tracking-wide text-base-content/60">Runtime</dt>
              <dd class="data-value">
                {{ data.app.systemApp ? 'in-process' : [data.app.entrypoint?.runtime, data.app.entrypoint?.cpuArchitecture, data.app.entrypoint?.binaryAbi].filter(Boolean).join(' ') || '—' }}
              </dd>
            </div>
            <div>
              <dt class="text-xs uppercase tracking-wide text-base-content/60">Framework SDK</dt>
              <dd class="data-value">{{ data.app.entrypoint?.frameworkSdkVersion || '—' }}</dd>
            </div>
            <div>
              <dt class="text-xs uppercase tracking-wide text-base-content/60">Provides</dt>
              <dd class="data-value">{{ (data.app.provides || []).join(', ') || '—' }}</dd>
            </div>
            <div>
              <dt class="text-xs uppercase tracking-wide text-base-content/60">Lifecycle</dt>
              <dd class="text-sm">
                {{ data.app.background ? 'background' : 'on demand' }} ·
                {{ data.app.singleInstance ? 'single instance' : 'multi instance' }} ·
                {{ data.app.startOnBoot ? 'starts at boot' : 'manual start' }} ·
                {{ data.app.restartOnFailure ? 'restart on failure' : 'no manifest restart' }}
              </dd>
            </div>
            <div>
              <dt class="text-xs uppercase tracking-wide text-base-content/60">Packaging</dt>
              <dd class="text-sm">
                {{ data.app.systemApp ? 'built into capsulad; no package directory' : 'installed package' }}
              </dd>
            </div>
          </dl>
        </div>
      </details>

      <details v-if="interfaces.length" class="rounded-lg border border-base-300">
        <summary class="cursor-pointer px-3 py-2 text-sm font-medium">
          App interfaces <span class="badge badge-ghost badge-sm ml-1">{{ interfaces.length }}</span>
        </summary>
        <div class="flex flex-col gap-3 border-t border-base-300 p-3">
          <div v-for="iface in interfaces" :key="iface.grpcService">
            <div class="flex flex-wrap items-center gap-2">
              <span class="data-value font-medium">{{ iface.grpcService }}</span>
              <span class="badge badge-ghost badge-sm">v{{ iface.interfaceVersion || '—' }}</span>
              <span class="badge badge-sm" :class="iface.available ? 'badge-success' : 'badge-ghost'">
                {{ iface.available ? 'available' : 'unavailable' }}
              </span>
            </div>
            <p v-if="iface.summary" class="mt-1 text-sm text-base-content/70">{{ iface.summary }}</p>
            <template v-if="descriptions[iface.grpcService]">
              <p v-if="descriptions[iface.grpcService].documentationMarkdown" class="mt-2 whitespace-pre-wrap text-xs text-base-content/60">
                {{ descriptions[iface.grpcService].documentationMarkdown }}
              </p>
              <div v-if="(descriptions[iface.grpcService].methods || []).length" class="mt-2 overflow-x-auto">
                <table class="table table-xs">
                  <thead>
                    <tr>
                      <th scope="col">Method</th>
                      <th scope="col">Summary</th>
                    </tr>
                  </thead>
                  <tbody>
                    <tr v-for="method in descriptions[iface.grpcService].methods" :key="method.rpcName">
                      <td class="data-value">{{ method.rpcName }}</td>
                      <td class="text-xs">{{ method.summary }}</td>
                    </tr>
                  </tbody>
                </table>
                <details
                  v-for="method in descriptions[iface.grpcService].methods"
                  :key="`${method.rpcName}-detail`"
                  class="mt-1 text-xs"
                >
                  <summary class="cursor-pointer text-base-content/60">{{ method.rpcName }} examples</summary>
                  <pre class="mt-1 overflow-x-auto rounded bg-base-200 p-2">{{ method.requestExampleJson || '—' }}</pre>
                  <pre class="mt-1 overflow-x-auto rounded bg-base-200 p-2">{{ method.responseExampleJson || '—' }}</pre>
                </details>
              </div>
            </template>
          </div>
        </div>
      </details>

      <details
        v-if="provenance && (provenance.status || provenance.detail || provenance.subjectSha256Hex || provenance.verifiedRepository)"
        class="rounded-lg border border-base-300"
      >
        <summary class="cursor-pointer px-3 py-2 text-sm font-medium">Build verification</summary>
        <div class="border-t border-base-300 p-3">
          <div class="flex flex-wrap items-center gap-2">
            <span class="badge badge-sm" :class="status(provenance.status) === 'verified' ? 'badge-success' : 'badge-warning'">
              {{ status(provenance.status) || 'unreported' }}
            </span>
            <span v-if="provenance.detail" class="text-xs text-base-content/70">{{ provenance.detail }}</span>
          </div>
          <dl class="mt-2 grid gap-1 text-xs">
            <div class="flex flex-wrap gap-2"><dt class="text-base-content/60">Subject SHA-256</dt><dd class="data-value">{{ provenance.subjectSha256Hex || '—' }}</dd></div>
            <div class="flex flex-wrap gap-2"><dt class="text-base-content/60">Repository</dt><dd class="data-value">{{ provenance.verifiedRepository || '—' }}</dd></div>
            <div class="flex flex-wrap gap-2"><dt class="text-base-content/60">Workflow</dt><dd class="data-value">{{ provenance.verifiedWorkflow || '—' }}</dd></div>
            <div class="flex flex-wrap gap-2"><dt class="text-base-content/60">Commit</dt><dd class="data-value">{{ provenance.verifiedCommitSha || '—' }}</dd></div>
          </dl>
          <p class="mt-2 text-xs text-base-content/60">
            Verified provenance attests build origin and integrity under the configured trust policy;
            it is not a promise that the software is safe.
          </p>
        </div>
      </details>

      <details class="rounded-lg border border-base-300">
        <summary class="cursor-pointer px-3 py-2 text-sm font-medium">
          Logs <span class="badge badge-ghost badge-sm ml-1">{{ logs.length }}</span>
        </summary>
        <div class="border-t border-base-300 p-3">
          <div v-if="logs.length" class="max-h-72 overflow-y-auto rounded bg-base-200 p-2 font-mono text-[11px] leading-relaxed">
            <p v-for="(entry, index) in logs" :key="index" class="whitespace-pre-wrap break-words">
              {{ logLine(entry) }}
            </p>
          </div>
          <p v-else class="text-sm text-base-content/60">No log records yet.</p>
        </div>
      </details>
    </div>
  </ModalDialog>
</template>
