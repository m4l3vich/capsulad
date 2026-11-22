<script setup>
import { computed, nextTick, onUnmounted, reactive, ref, watch } from 'vue'
import { socket } from '../lib/ws.js'
import { liveState, useTopic } from '../lib/live.js'
import { SettingsFrameHost } from '../lib/appSettingsBridge.js'
import { theme as appearance } from '../lib/theme.js'
import { readGuide } from '../lib/apps.js'
import { guideBlocks } from '../lib/guide.js'
import { getAppBoot, setAppBoot } from '../lib/appBoot.js'
import { safeJson, shortId, status } from '../lib/format.js'
import { notify } from '../lib/toast.js'
import ModalDialog from '../components/ModalDialog.vue'
import Icon from '../components/Icon.vue'

const props = defineProps({
  appId: { type: String, required: true }
})

const emit = defineEmits(['close'])

const { data, error: loadError, refresh } = useTopic('application', props.appId, { logs: true })

const busy = ref('')
const settingBusy = ref('')
const discardPrompt = ref(false)
const launchDialog = ref(false)
const interfaceDialog = ref(null)
const defaultInfo = ref(false)
const boot = ref(null)
const bootBusy = ref(false)
const bootError = ref('')

// The user guide is inert text fetched over the authenticated same-origin API.
// It is deliberately independent of the run state: a stopped app can still
// explain its controls, launch and recovery before anyone starts it.
const guideOpen = ref(false)
const guide = ref(null)
const guideError = ref('')
const guideLoading = ref(false)
let guideController = null

const guideDocumentBlocks = computed(() => (guide.value ? guideBlocks(guide.value.markdown) : []))
const guideSourceLabel = computed(() => {
  if (!guide.value) return ''
  if (guide.value.source === 'builtin') return 'built-in guide'
  if (guide.value.source === 'package') return 'from the application package'
  return ''
})

function abortGuideLoad() {
  if (!guideController) return
  guideController.abort()
  guideController = null
  guideLoading.value = false
}

async function loadGuide() {
  const app = props.appId
  if (!app) return
  abortGuideLoad()
  const controller = new AbortController()
  guideController = controller
  guideLoading.value = true
  guideError.value = ''
  try {
    const doc = await readGuide(app, { signal: controller.signal })
    if (controller.signal.aborted || props.appId !== app) return
    guide.value = doc
  } catch (err) {
    if (controller.signal.aborted || props.appId !== app) return
    guideError.value = err?.message || 'The user guide could not be loaded.'
  } finally {
    if (guideController === controller) {
      guideController = null
      guideLoading.value = false
    }
  }
}

function openGuide() {
  guideOpen.value = true
  if (!guide.value || guide.value.appId !== props.appId) loadGuide()
}

function closeGuide() {
  abortGuideLoad()
  guideOpen.value = false
  guideError.value = ''
}

// Launch drafts stay local until submitted; closing the form preserves them.
// Running default providers restart on stop and retain their selected role.
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
const runs = computed(() => data.value?.runs || [])
const defaultRoles = computed(() => (data.value?.selectedDefaultRoles || []).map((role) => ({
  'light.ring.main': 'LED ring controller',
  'display.segment.front': 'display controller',
  assistant: 'voice assistant'
}[role] || role)))
const hasSettingsDocument = computed(() => !!data.value?.schema?.settingsUi?.available)
const connected = computed(() => liveState.value === 'open')

async function loadBoot() {
  try {
    boot.value = await getAppBoot(props.appId)
    bootError.value = ''
  } catch (err) {
    bootError.value = err?.message || 'Could not load the boot setting.'
  }
}

async function changeBoot(event) {
  bootBusy.value = true
  try {
    boot.value = await setAppBoot(props.appId, event.target.checked, boot.value.revision)
    bootError.value = ''
    notify.success('Boot setting saved')
  } catch (err) {
    notify.error(err?.message || 'Could not save the boot setting.')
    await loadBoot()
  } finally {
    bootBusy.value = false
  }
}

loadBoot()

function syncSettingsState() {
  const view = data.value
  if (!view) return
  // A reopened detail for another app starts with no guide from the previous
  // application, even if the fetched document arrives late.
  if (guide.value && guide.value.appId !== props.appId) {
    closeGuide()
    guide.value = null
  }
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

// A refresh failure keeps the previous view; surface it as a toast once per
// distinct failure instead of a banner.
let lastLoadError = ''
watch(loadError, (value) => {
  if (!value) {
    lastLoadError = ''
    return
  }
  if (!data.value || value === lastLoadError) return
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
  patchChain = promise.catch(() => { })
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
    theme: appearance.resolved,
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

// A Light/Dark/System change reaches an already-open settings document as one
// attribute update. It deliberately does not remount the frame or resend
// init/state, so in-progress drafts, pending patches, focus and scroll stay.
watch(
  () => appearance.resolved,
  (value) => {
    frameHost?.setTheme(value)
  }
)

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

async function launch(withArguments = true) {
  const app = data.value?.app
  if (!app) return
  const args = []
  for (const arg of withArguments ? (app.launchArguments || []) : []) {
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
    launchDialog.value = false
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

function documentationLines(text) {
  return (text || '').split('\n').map((line) => {
    const heading = line.match(/^(#{1,6})\s+(.+)$/)
    const list = line.match(/^\s*[-*]\s+(.+)$/)
    return { text: heading?.[2] || list?.[1] || line, heading: !!heading, list: !!list }
  })
}
</script>

<template>
  <ModalDialog :title="title" :before-close="requestClose" @close="emit('close')">
    <template #header="{ titleId }">
      <div class="flex flex-col">
        <h2 :id="titleId" class="text-lg font-semibold leading-tight">
          {{ title }}
          <span class="badge badge-ghost">v{{ data.app.version || '—' }}</span>
        </h2>

        <div class="flex flex-wrap items-center gap-1.5 mt-3">
          <span v-if="data.app.systemApp" class="badge badge-outline badge-sm">built-in</span>
          <span v-for="role in defaultRoles" :key="role" class="badge badge-primary badge-sm">default {{ role }}</span>
          <span v-if="data.app.background" class="badge badge-success badge-outline badge-sm">runs in background</span>
        </div>
      </div>
    </template>

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
      <div v-if="guideOpen" class="flex flex-col gap-3">
        <div class="flex flex-wrap items-center justify-between gap-2">
          <button type="button" class="btn btn-sm btn-ghost" @click="closeGuide">
            <span aria-hidden="true">←</span> Back to application
          </button>
          <span v-if="guideSourceLabel" class="badge badge-ghost badge-sm">{{ guideSourceLabel }}</span>
        </div>

        <div v-if="guideLoading" class="flex items-center gap-2 py-4 text-sm" role="status" aria-live="polite">
          <span class="loading loading-spinner loading-sm" aria-hidden="true"></span>
          Loading the user guide…
        </div>

        <div v-else-if="guideError" class="alert alert-warning py-2" role="alert">
          <span class="text-sm">{{ guideError }}</span>
          <button type="button" class="btn btn-sm btn-outline" @click="loadGuide">Try again</button>
        </div>

        <article v-else-if="guide" class="flex flex-col gap-3 break-words text-sm leading-relaxed">
          <template v-for="(block, index) in guideDocumentBlocks" :key="index">
            <component
              :is="`h${Math.min(block.level + 2, 6)}`"
              v-if="block.type === 'heading'"
              class="mt-2 font-semibold tracking-tight"
              :class="block.level <= 2 ? 'text-base' : 'text-sm'"
            >{{ block.text }}</component>

            <p v-else-if="block.type === 'paragraph'" class="text-base-content/80">{{ block.text }}</p>

            <ul v-else-if="block.type === 'list' && block.kind === 'ul'" class="list-disc space-y-1 pl-5 text-base-content/80">
              <li v-for="(item, itemIndex) in block.items" :key="itemIndex">{{ item }}</li>
            </ul>

            <ol v-else-if="block.type === 'list'" class="list-decimal space-y-1 pl-5 text-base-content/80">
              <li v-for="(item, itemIndex) in block.items" :key="itemIndex">{{ item }}</li>
            </ol>

            <pre
              v-else-if="block.type === 'code'"
              class="overflow-x-auto rounded bg-base-200 p-3 font-mono text-xs leading-relaxed"
            >{{ block.text }}</pre>

            <div v-else-if="block.type === 'table'" class="overflow-x-auto rounded border border-base-300">
              <table class="table table-xs">
                <thead>
                  <tr>
                    <th v-for="(header, headerIndex) in block.headers" :key="headerIndex" scope="col">{{ header }}</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="(row, rowIndex) in block.rows" :key="rowIndex">
                    <td v-for="(cell, cellIndex) in row" :key="cellIndex">{{ cell }}</td>
                  </tr>
                </tbody>
              </table>
            </div>
          </template>
        </article>

        <p v-else class="text-sm text-base-content/60">No guide content is available.</p>
      </div>

      <template v-else>
      <div v-if="!connected" class="alert alert-warning py-2" role="status">
        <span class="text-sm">Disconnected: changes are paused until the connection returns.</span>
      </div>

      <div class="flex flex-wrap items-center gap-2">
        <template v-if="runs.length">
          <span class="badge badge-success">Running</span>
          <template v-for="runItem in runs" :key="runItem.runId">
            <button v-if="data.app.canStop" type="button" class="btn btn-xs btn-outline"
              :class="data.selectedDefault ? '' : 'btn-error'" :disabled="!!busy || !connected"
              @click="stop(runItem)">{{ data.selectedDefault ? 'Restart' : 'Stop' }}<span v-if="runs.length > 1"> {{ shortId(runItem.runId) }}</span></button>
          </template>
          <span v-if="!data.app.canStop" class="text-sm text-base-content/60">Managed by the speaker</span>
          <button v-if="data.selectedDefault" type="button" class="btn btn-ghost btn-square btn-sm"
            aria-label="Why default apps cannot be stopped" @click="defaultInfo = true">
            <Icon size="20px" aria-hidden="true">info</Icon>
          </button>
        </template>
        <template v-else>
          <button type="button" class="btn btn-primary btn-sm" :disabled="!!busy || !connected"
            @click="(data.app.launchArguments || []).some(arg => arg.required) ? launchDialog = true : launch(false)">
            <span v-if="busy === 'launch'" class="loading loading-spinner loading-xs"></span>
            {{(data.app.launchArguments || []).some(arg => arg.required) ? 'Launch…' : 'Launch'}}
          </button>
          <button v-if="(data.app.launchArguments || []).length && !(data.app.launchArguments || []).some(arg => arg.required)"
            type="button" class="btn btn-primary btn-square btn-sm" aria-label="Launch with arguments"
            :disabled="!!busy || !connected" @click="launchDialog = true">
            <Icon size="20px" aria-hidden="true">more_vert</Icon>
          </button>
        </template>

        <button type="button" class="btn btn-ghost btn-sm ml-auto" @click="openGuide">
          <Icon size="20px" aria-hidden="true">menu_book</Icon> User guide
        </button>
      </div>
      <div>
        <label class="flex items-center gap-2 text-sm">
          <input type="checkbox" class="checkbox checkbox-primary checkbox-sm"
            :checked="data.selectedDefault || !!boot?.enabled"
            :disabled="data.selectedDefault || !boot?.editable || bootBusy || !connected"
            @change="changeBoot" />
          Start at boot
        </label>
        <p v-if="bootError" class="mt-1 text-xs text-error">{{ bootError }} <button type="button" class="link" @click="loadBoot">Retry</button></p>
        <p v-else-if="data.selectedDefault" class="mt-1 text-xs text-base-content/60">Selected default apps start automatically.</p>
        <p v-else-if="boot && !boot.editable" class="mt-1 text-xs text-base-content/60">Managed by the speaker.</p>
      </div>

      <details v-if="schemaActions.length" class="border-y border-base-300 py-2">
        <summary class="cursor-pointer text-sm font-semibold">Actions</summary>
        <div class="mt-3 flex flex-col gap-3">
          <div v-for="action in schemaActions" :key="action.actionId" class="flex flex-wrap items-center justify-between gap-2">
            <div><p class="text-sm font-medium">{{ action.label }}</p><p v-if="action.description" class="text-xs text-base-content/60">{{ action.description }}</p></div>
            <button type="button" class="btn btn-sm btn-outline" :disabled="!!busy || !connected" @click="trigger(action)">{{ action.label || 'Run action' }}</button>
          </div>
        </div>
      </details>

      <!-- Settings: the app's own document when declared, else its schema. -->
      <section v-if="hasSettingsDocument || schemaSettings.length">
        <h3 v-if="!hasSettingsDocument" class="text-sm font-semibold">Settings</h3>

        <div v-if="hasSettingsDocument" class="mt-2 flex flex-col gap-2">
          <iframe
            v-show="!frameError"
            ref="settingsFrame"
            class="h-[360px] min-h-[240px] w-full"
            sandbox="allow-scripts"
            referrerpolicy="no-referrer"
            allow="camera 'none'; microphone 'none'; geolocation 'none'"
            :title="`${data.app.displayName} settings`"
          ></iframe>
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

      <section class="border-t border-base-300 pt-4">
        <h3 class="text-sm font-semibold">Application details</h3>
        <div class="pt-3">
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
                {{ data.app.startOnBoot ? 'manifest starts at boot' : 'manifest manual start' }} ·
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
      </section>

      <section v-if="interfaces.length" class="border-t border-base-300 pt-4">
        <h4 class="text-sm font-semibold">Interfaces</h4>
        <div class="mt-2 flex flex-col gap-3">
          <div v-for="iface in interfaces" :key="iface.grpcService">
            <div class="flex flex-wrap items-center gap-2">
              <span class="data-value font-medium">{{ iface.grpcService }}</span>
              <span class="badge badge-ghost badge-sm">v{{ iface.interfaceVersion || '—' }}</span>
              <span class="badge badge-sm" :class="iface.available ? 'badge-success' : 'badge-ghost'">
                {{ iface.available ? 'available' : 'unavailable' }}
              </span>
              <button type="button" class="btn btn-ghost btn-square btn-sm" :aria-label="`View ${iface.grpcService} description`" @click="interfaceDialog = iface">
                <Icon size="20px" aria-hidden="true">info</Icon>
              </button>
            </div>
            <p v-if="iface.summary" class="mt-1 text-sm text-base-content/70">{{ iface.summary }}</p>
          </div>
        </div>
      </section>

      <section v-if="provenance && (provenance.status || provenance.detail || provenance.subjectSha256Hex || provenance.verifiedRepository)" class="border-t border-base-300 pt-4">
        <h4 class="text-sm font-semibold">Build verification</h4>
        <div class="pt-2">
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
      </section>
      </template>
    </div>
    <ModalDialog v-if="discardPrompt" title="Discard changes?" max-width="max-w-md" @close="discardPrompt = false">
      <p class="text-sm">
        This application has unsaved text or number fields. Closing now discards them; toggles and
        choices that already applied stay applied.
      </p>
      <template #footer>
        <button type="button" class="btn btn-ghost btn-sm" @click="discardPrompt = false">Keep editing</button>
        <button type="button" class="btn btn-warning btn-sm" @click="discardAndClose">Discard changes</button>
      </template>
    </ModalDialog>
    <ModalDialog v-if="launchDialog" title="Launch with arguments" max-width="max-w-xl" @close="launchDialog = false">
      <form class="flex flex-col gap-3" @submit.prevent="launch(true)">
        <label v-for="arg in data.app.launchArguments" :key="arg.name" class="form-control">
          <span class="label-text pb-1">{{ arg.name }} <span v-if="arg.required" class="text-error">*</span></span>
          <input v-model="launchArgs[arg.name]" class="input input-bordered input-sm w-full"
            :type="arg.valueType?.includes('INT') ? 'number' : 'text'" :required="arg.required"
            @input="launchDirty = true" />
          <span v-if="arg.description" class="mt-1 text-xs text-base-content/60">{{ arg.description }}</span>
        </label>
        <button class="btn btn-primary btn-sm self-start" type="submit" :disabled="!!busy || !connected">Launch</button>
      </form>
    </ModalDialog>
    <ModalDialog v-if="defaultInfo" title="Default application" max-width="max-w-lg" @close="defaultInfo = false">
      <p class="text-sm">This is a default application and it cannot be stopped, it can only be restarted. Choose another app or none in Settings to stop this app permanently.</p>
    </ModalDialog>
    <ModalDialog v-if="interfaceDialog" :title="interfaceDialog.grpcService" @close="interfaceDialog = null">
      <p v-if="interfaceDialog.summary" class="text-sm">{{ interfaceDialog.summary }}</p>
      <div v-if="descriptions[interfaceDialog.grpcService]?.documentationMarkdown" class="mt-3 text-sm">
        <p v-for="(line, index) in documentationLines(descriptions[interfaceDialog.grpcService].documentationMarkdown)" :key="index"
          :class="{ 'mt-3 font-semibold': line.heading, 'ml-4': line.list, 'min-h-3': !line.text }">
          <span v-if="line.list" aria-hidden="true">• </span>{{ line.text }}
        </p>
      </div>
      <div v-for="method in descriptions[interfaceDialog.grpcService]?.methods || []" :key="method.rpcName" class="mt-4">
        <h4 class="font-semibold">{{ method.rpcName }}</h4>
        <p class="text-sm">{{ method.summary }}</p>
        <div v-if="method.semanticsMarkdown" class="mt-2 text-sm">
          <p v-for="(line, index) in documentationLines(method.semanticsMarkdown)" :key="index"
            :class="{ 'mt-3 font-semibold': line.heading, 'ml-4': line.list, 'min-h-3': !line.text }">
            <span v-if="line.list" aria-hidden="true">• </span>{{ line.text }}
          </p>
        </div>
        <pre v-if="method.requestExampleJson" class="mt-2 overflow-x-auto rounded bg-base-200 p-2 text-xs">{{ method.requestExampleJson }}</pre>
        <pre v-if="method.responseExampleJson" class="mt-2 overflow-x-auto rounded bg-base-200 p-2 text-xs">{{ method.responseExampleJson }}</pre>
      </div>
      <p v-if="!interfaceDialog.summary && !descriptions[interfaceDialog.grpcService]" class="text-sm text-base-content/60">No description available.</p>
    </ModalDialog>
  </ModalDialog>
</template>
