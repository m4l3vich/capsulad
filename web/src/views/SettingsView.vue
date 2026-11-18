<script setup>
import { computed, defineAsyncComponent, nextTick, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import WifiSettings from '../components/WifiSettings.vue'
import WakewordSettings from '../components/WakewordSettings.vue'
import VolumeControl from '../components/VolumeControl.vue'
import MicrophoneControl from '../components/MicrophoneControl.vue'
import ModalDialog from '../components/ModalDialog.vue'
import Icon from '../components/Icon.vue'
import { socket } from '../lib/ws.js'
import { useSettings, useTopic } from '../lib/live.js'
import { pct } from '../lib/format.js'
import { notify } from '../lib/toast.js'
import { SETTINGS_SECTION_ICONS } from '../lib/iconNames.js'

// The timezone map carries its own large module; keep it out of the initial
// bundle and load it the first time this view renders the picker.
const TimezonePicker = defineAsyncComponent(() => import('../components/TimezonePicker.vue'))

// Settings is a compact appliance console: every control is either an
// immediate bounded update (select, slider, toggle) or a local draft submitted
// by its own button. All authority lives in the settings snapshot; this view
// only mirrors it and never rewrites a preference the user did not touch.

const { data, error, update } = useSettings()
const {
  data: overviewData,
  error: overviewError,
  refresh: refreshOverview
} = useTopic('overview')

const ui = reactive({
  display: { app: '', brightness: 100, mode: 'const' },
  ring: { app: '', brightness: 100, mode: 'const' },
  assistant: { app: '', touch: 'summon' },
  indicator: 'none',
  bluetooth: { incoming: true, discoverable: false },
  startup: false
})
const drafts = reactive({ hostname: '' })
const dirty = reactive({ hostname: false })
const dragging = reactive({ display: false, ring: false })
const resourcePending = { display: new Set(), ring: new Set() }
const resourceInFlight = { display: new Map(), ring: new Map() }
const resourceTimers = { display: null, ring: null }
const busy = ref('')
const btOverride = ref(null)
const unpairTarget = ref(null)
// The advertised name dialog keeps its own draft: an incoming overview or
// settings snapshot must never overwrite what the administrator is typing.
const adapterNameOpen = ref(false)
const adapterNameDraft = ref('')
const adapterNameError = ref('')
const adapterNameSaving = ref(false)
// BlueZ/HCI advertises at most 248 UTF-8 bytes; the daemon validates the same
// rule, and this copy exists only to keep Save disabled before the round trip.
const ADAPTER_NAME_MAX_BYTES = 248

const values = computed(() => data.value?.values || {})
const apps = computed(() => data.value?.apps || [])
const ambient = computed(() => Boolean(data.value?.ambientAvailable))
const btStatus = computed(() => btOverride.value || data.value?.bluetooth || null)
const adapterNameBytes = computed(() =>
  new TextEncoder().encode(adapterNameDraft.value.trim()).length
)
const adapterNameValid = computed(() => {
  const name = adapterNameDraft.value.trim()
  if (!name || adapterNameBytes.value > ADAPTER_NAME_MAX_BYTES) return false
  // Same control-character refusal as the daemon: NUL and friends would be
  // mangled by BlueZ's alias and the provisioning AP SSID.
  return !/[\u0000-\u001f\u007f-\u009f]/.test(name)
})
const pairedDevices = computed(() => overviewData.value?.pairedDevices || [])
// What the speaker answers to on the network: the advertised name already
// carries its `.local` suffix. Without one, only the saved hostname is honest;
// an unsaved draft is not a name the speaker answers to yet.
const advertisedName = computed(() => {
  const advertised = String(data.value?.discovery?.advertised || '').trim()
  if (advertised) return advertised
  const saved = String(values.value.hostname || '').trim()
  return `${saved || 'capsula'}.local`
})
// Naming can be unavailable on the running platform; that is not the same as
// the name being discoverable, so the copy must not promise it.
const namingUnsupported = computed(() => Boolean(data.value?.discovery) && !data.value.discovery.supported)

// Provider roles are manifest hints used only to populate these dropdowns;
// they are not entitlements. A saved app is never replaced or hidden: when it
// is not eligible (or no longer installed) it stays as the selected option.
const PROVIDER_ROLE = {
  voice_assistant: 'voice assistant',
  segment_display_provider: 'display',
  led_ring_controller: 'ring'
}

function hasProvide(app, token) {
  return Array.isArray(app?.provides) && app.provides.includes(token)
}

function providerApps(token) {
  return apps.value.filter((app) => hasProvide(app, token))
}

function providerLabel(app) {
  return `${app.displayName || app.appId}${app.systemApp ? ' (built-in)' : ''}`
}

function providerToken(kind) {
  return kind === 'ring' ? 'led_ring_controller' : 'segment_display_provider'
}

function savedProvider(token, savedId) {
  if (!savedId) return null
  if (providerApps(token).some((app) => app.appId === savedId)) return null
  const app = apps.value.find((entry) => entry.appId === savedId)
  const role = PROVIDER_ROLE[token] || 'provider'
  const label = app ? providerLabel(app) : `${savedId} (not installed)`
  return { appId: savedId, label: `${label} — saved; not a ${role} provider` }
}

let hashHandled = false

// Snapshot-carried problems are state, not events: announce each new message
// once instead of on every push that repeats it.
let lastBluetoothError = ''

watch(() => data.value?.bluetoothError || '', (message) => {
  if (!message) {
    lastBluetoothError = ''
    return
  }
  if (message === lastBluetoothError) return
  lastBluetoothError = message
  notify.warning(message)
}, { immediate: true })

let lastOverviewError = ''

watch(overviewError, (message) => {
  const text = String(message || '')
  if (!text) {
    lastOverviewError = ''
    return
  }
  if (text === lastOverviewError) return
  lastOverviewError = text
  notify.warning(text)
})

watch(data, (view) => {
  if (!view) return
  btOverride.value = null
  sync(view)
  if (!hashHandled) scrollToHash()
}, { immediate: true })

function modeKey(mode) {
  const text = String(mode || '')
  if (text.includes('IMMEDIATE')) return 'immediate'
  if (text.includes('UNTIL_UNMUTED')) return 'none'
  if (text.includes('DELAY')) return 'delay'
  return 'none'
}

function presetFor(delayMs) {
  const seconds = Math.round(Number(delayMs || 0) / 1000)
  return [60, 600, 1800].includes(seconds) ? seconds : 0
}

function delayLabel(delayMs) {
  const seconds = Math.max(0, Math.round(Number(delayMs || 0) / 1000))
  if (seconds >= 60) {
    const minutes = Math.floor(seconds / 60)
    const rest = seconds % 60
    return rest ? `${minutes} min ${rest} s` : `${minutes} min`
  }
  return `${seconds} s`
}

function resourceOwned(kind, field) {
  if (resourcePending[kind].has(field)) return true
  if ((resourceInFlight[kind].get(field) || 0) > 0) return true
  return field === 'brightness' && dragging[kind]
}

function sync(view) {
  const v = view.values || {}
  for (const kind of ['display', 'ring']) {
    const resource = view[kind]
    if (!resource) continue
    const id = resource.resourceId
    const authApp = v.defaultAppByResource?.[id] || ''
    const authMode = v.brightnessMode?.[id] === 'auto' ? 'auto' : 'const'
    const level = v.displayBrightness?.[id]
    const authBrightness = level === undefined ? 100 : pct(level)
    // A queued/in-flight local edit owns its field until the request settles;
    // an unrelated settings push must not revert and then submit that edit.
    if (!resourceOwned(kind, 'default_app')) ui[kind].app = authApp
    if (!resourceOwned(kind, 'brightness_mode')) ui[kind].mode = authMode
    if (!resourceOwned(kind, 'brightness')) ui[kind].brightness = authBrightness
  }
  ui.assistant.app = v.defaultAssistantAppId || ''
  ui.assistant.touch = String(v.touchHoldMode || '').includes('TALK') ? 'talk' : 'summon'
  const indicator = v.mutedMicrophoneIndicator
  const key = modeKey(indicator?.mode)
  ui.indicator = key === 'delay' ? String(presetFor(indicator?.delayMs) || 'custom') : key
  ui.bluetooth.incoming = !!v.bluetoothIncomingConnections
  ui.bluetooth.discoverable = !!v.bluetoothDiscoverable
  ui.startup = !!v.startupSoundEnabled
  if (!dirty.hostname) drafts.hostname = v.hostname || ''
}

// One client-side chain keeps this view's update order deterministic; the
// shared settings store still owns the CAS revision and its single in-flight
// request.
let chain = Promise.resolve()

function send(group, payload) {
  chain = chain.then(() => update(group, payload)).catch(() => false)
  return chain
}

async function apply(group, payload) {
  const ok = await send(group, payload)
  if (!ok) notify.error(error.value || 'The speaker rejected that change.')
  return ok
}

// ---- Display and ring -----------------------------------------------------

function resourcePayload(kind, fields) {
  const resource = data.value?.[kind]
  const v = values.value
  const id = resource?.resourceId || ''
  const appId = fields.includes('default_app')
    ? ui[kind].app
    : (v.defaultAppByResource?.[id] || '')
  const brightness = fields.includes('brightness')
    ? ui[kind].brightness / 100
    : (v.displayBrightness?.[id] ?? 1)
  const mode = fields.includes('brightness_mode')
    ? (ui[kind].mode === 'auto' ? 2 : 1)
    : (v.brightnessMode?.[id] === 'auto' ? 2 : 1)
  return { resourceId: id, defaultApp: appId, brightness, brightnessMode: mode, updateFields: fields }
}

function beginFields(kind, fields) {
  const counts = resourceInFlight[kind]
  for (const field of fields) counts.set(field, (counts.get(field) || 0) + 1)
}

function endFields(kind, fields) {
  const counts = resourceInFlight[kind]
  for (const field of fields) {
    const next = (counts.get(field) || 1) - 1
    if (next > 0) counts.set(field, next)
    else counts.delete(field)
  }
}

async function sendResource(kind, fields, failure) {
  beginFields(kind, fields)
  const ok = await send('display', resourcePayload(kind, fields))
  endFields(kind, fields)
  if (!ok) {
    notify.error(error.value || failure)
    // The store re-reads on failure; with ownership cleared, sync restores the
    // authoritative value for these fields.
    if (data.value) sync(data.value)
  }
  return ok
}

async function flushResource(kind) {
  if (!resourcePending[kind].size) return
  const fields = [...resourcePending[kind]]
  resourcePending[kind].clear()
  await sendResource(kind, fields, 'Brightness was not applied.')
  // A slider still moving accumulates the next patch; send it after the
  // bounded interval rather than blocking on every pixel of travel.
  if (resourcePending[kind].size) scheduleResource(kind)
}

function scheduleResource(kind) {
  if (resourceTimers[kind]) return
  resourceTimers[kind] = setTimeout(() => {
    resourceTimers[kind] = null
    flushResource(kind)
  }, 100)
}

function markResource(kind, fields) {
  if (!data.value?.[kind]) return
  for (const field of fields) resourcePending[kind].add(field)
  scheduleResource(kind)
}

// Discrete choices are not debounced: a queued-in-flight field stays
// user-owned until the request settles, so an unrelated push cannot revert it.
function applyResourceNow(kind, fields) {
  if (!data.value?.[kind]) return
  sendResource(kind, fields, 'That choice was not applied.')
}

function beginDrag(kind) {
  dragging[kind] = true
}

function endDrag(kind) {
  dragging[kind] = false
  markResource(kind, ['brightness'])
}

function toggleAuto(kind, event) {
  ui[kind].mode = event.target.checked ? 'auto' : 'const'
  applyResourceNow(kind, ['brightness_mode'])
}

// ---- Voice assistant ------------------------------------------------------

function applyAssistant(fields) {
  const v = values.value
  const payload = {
    defaultAssistantApp: fields.includes('default_assistant_app')
      ? ui.assistant.app
      : (v.defaultAssistantAppId || ''),
    wakewordEnabled: !!v.wakewordEnabled,
    touchHold: fields.includes('touch_hold_mode')
      ? (ui.assistant.touch === 'talk' ? 2 : 1)
      : (String(v.touchHoldMode || '').includes('TALK') ? 2 : 1),
    updateFields: fields
  }
  apply('assistant', payload)
}

// ---- Muted indicator ------------------------------------------------------

const indicatorOptions = computed(() => {
  const options = [
    { value: 'none', label: 'Never (keep the light visible)' },
    { value: '60', label: 'After 1 minute' },
    { value: '600', label: 'After 10 minutes' },
    { value: '1800', label: 'After 30 minutes' }
  ]
  const indicator = values.value.mutedMicrophoneIndicator
  if (ui.indicator === 'immediate') {
    options.push({ value: 'immediate', label: 'Immediately (current setting)' })
  }
  if (ui.indicator === 'custom') {
    options.push({
      value: 'custom',
      label: `${delayLabel(indicator?.delayMs)} (current setting)`
    })
  }
  return options
})

function applyIndicator() {
  const choice = ui.indicator
  // A stored non-preset value stays selected as a labelled current setting;
  // choosing it again never rewrites the delay.
  if (choice === 'custom') return
  let payload
  if (choice === 'none') payload = { mode: 3, hasMode: true, hasDelay: false, delayS: 0 }
  else if (choice === 'immediate') payload = { mode: 1, hasMode: true, hasDelay: false, delayS: 0 }
  else payload = { mode: 2, hasMode: true, hasDelay: true, delayS: Number(choice) }
  apply('mutedIndicator', payload)
}

// ---- Bluetooth ------------------------------------------------------------

async function bluetoothCommand(action, payload, failure) {
  if (busy.value) {
    // A toggle pressed while another adapter command is running never became a
    // request; restore the authoritative values instead of faking the change.
    ui.bluetooth.incoming = !!values.value.bluetoothIncomingConnections
    ui.bluetooth.discoverable = !!values.value.bluetoothDiscoverable
    return
  }
  busy.value = 'bluetooth'
  try {
    const result = await socket.request({ bluetooth: { [action]: payload } })
    if (result?.bluetooth?.status) {
      btOverride.value = result.bluetooth.status
      ui.bluetooth.incoming = !!result.bluetooth.status.incomingConnections
      ui.bluetooth.discoverable = !!result.bluetooth.status.discoverable
    }
  } catch (err) {
    notify.error(err.message || failure)
    // The command failed, so the requested value never became authoritative.
    ui.bluetooth.incoming = !!values.value.bluetoothIncomingConnections
    ui.bluetooth.discoverable = !!values.value.bluetoothDiscoverable
  } finally {
    busy.value = ''
  }
}

function toggleIncoming(event) {
  ui.bluetooth.incoming = event.target.checked
  bluetoothCommand(
    'setIncomingConnections',
    { incomingConnections: ui.bluetooth.incoming },
    'Speaker mode was not changed.'
  )
}

function toggleDiscoverable(event) {
  ui.bluetooth.discoverable = event.target.checked
  bluetoothCommand(
    'setDiscoverable',
    { discoverable: ui.bluetooth.discoverable },
    'Pair new devices was not changed.'
  )
}

function reloadOverview() {
  // A failed re-read is already exposed through overviewError; swallow the
  // rejection so a click cannot surface as an unhandled promise.
  refreshOverview().catch(() => { })
}

function askUnpair(device) {
  if (busy.value) return
  unpairTarget.value = device
}

async function confirmUnpair() {
  const device = unpairTarget.value
  if (!device || busy.value) return
  busy.value = 'unpair'
  try {
    await socket.request({ bluetooth: { unpair: { deviceId: device.deviceId } } })
    unpairTarget.value = null
    await refreshOverview()
  } catch (err) {
    notify.error(err.message || 'The device could not be unpaired.')
  } finally {
    busy.value = ''
  }
}

function openAdapterName() {
  if (!btStatus.value?.present || busy.value) return
  adapterNameDraft.value = btStatus.value?.name || ''
  adapterNameError.value = ''
  adapterNameOpen.value = true
}

function closeAdapterName() {
  adapterNameOpen.value = false
  adapterNameDraft.value = ''
  adapterNameError.value = ''
}

async function saveAdapterName() {
  if (adapterNameSaving.value) return
  const name = adapterNameDraft.value.trim()
  if (!adapterNameValid.value) {
    adapterNameError.value = `Use 1 to ${ADAPTER_NAME_MAX_BYTES} bytes with no control characters.`
    return
  }
  adapterNameSaving.value = true
  busy.value = 'bluetooth'
  adapterNameError.value = ''
  try {
    const result = await socket.request({ bluetooth: { setAdapterName: { name } } })
    // The result carries the adapter state the manager observed after the
    // write; adopt exactly that, never the draft.
    if (result?.bluetooth?.status) btOverride.value = result.bluetooth.status
    const shown = result?.bluetooth?.status?.name || name
    adapterNameOpen.value = false
    adapterNameDraft.value = ''
    notify.success(`Device name changed to ${shown}.`)
  } catch (err) {
    // Keep the dialog open so the administrator can correct the name.
    adapterNameError.value = err.message || 'The device name was not changed.'
    notify.error(adapterNameError.value)
  } finally {
    adapterNameSaving.value = false
    busy.value = ''
  }
}

// ---- Startup and hostname -------------------------------------------------

function toggleStartup(event) {
  ui.startup = event.target.checked
  apply('startupSound', { enabled: ui.startup })
}

const HOSTNAME = /^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$/

async function saveHostname() {
  const hostname = drafts.hostname.trim()
  if (!HOSTNAME.test(hostname)) {
    notify.error('Use up to 63 letters, numbers or hyphens, starting and ending with a letter or number.')
    return
  }
  const ok = await apply('hostname', { hostname })
  if (ok) dirty.hostname = false
}

onUnmounted(() => {
  clearTimeout(resourceTimers.display)
  clearTimeout(resourceTimers.ring)
})

// ---- Section filtering ----------------------------------------------------

const sections = [
  {
    id: 'voice-assistant',
    icon: SETTINGS_SECTION_ICONS.voiceAssistant,
    title: 'Voice assistant',
    keywords: ['voice', 'assistant', 'summon', 'talk', 'touch']
  },
  {
    id: 'wakeword',
    icon: SETTINGS_SECTION_ICONS.wakeword,
    title: 'Wakeword',
    keywords: ['wakeword', 'wake word', 'detection', 'training', 'recordings', 'model']
  },
  {
    id: 'display-ring',
    icon: SETTINGS_SECTION_ICONS.displayRing,
    title: 'Display & ring',
    keywords: ['display', 'ring', 'led', 'segment', 'brightness', 'ambient', 'auto', 'default app']
  },
  {
    id: 'microphone',
    icon: SETTINGS_SECTION_ICONS.microphone,
    title: 'Microphone',
    keywords: ['microphone', 'mic', 'mute', 'hide', 'delay', 'indicator', 'privacy']
  },
  {
    id: 'bluetooth',
    icon: SETTINGS_SECTION_ICONS.bluetooth,
    title: 'Bluetooth',
    keywords: ['bluetooth', 'pair', 'unpair', 'discoverable', 'incoming', 'speaker', 'name', 'rename']
  },
  {
    id: 'wifi',
    icon: SETTINGS_SECTION_ICONS.wifi,
    title: 'Wi-Fi',
    keywords: ['wifi', 'wi-fi', 'network', 'ssid', 'password', 'scan', 'portal', 'wireless']
  },
  {
    id: 'device',
    icon: SETTINGS_SECTION_ICONS.device,
    title: 'Time & device',
    keywords: ['device', 'timezone', 'time', 'zone', 'clock', 'hostname', 'name', 'volume', 'sound', 'startup']
  }
]

const query = ref('')

const visible = computed(() => {
  const tokens = query.value.trim().toLowerCase().split(/\s+/).filter(Boolean)
  const map = {}
  for (const section of sections) {
    const haystack = `${section.title} ${section.keywords.join(' ')}`.toLowerCase()
    map[section.id] = !tokens.length || tokens.every((token) => haystack.includes(token))
  }
  return map
})

const visibleCount = computed(() => Object.values(visible.value).filter(Boolean).length)

// Deep links such as /settings#wakeword can arrive before the snapshot renders
// the sections; retry once on mount and once when data first appears.
function scrollToHash() {
  const id = String(location.hash || '').replace(/^#/, '')
  if (!id) {
    hashHandled = true
    return
  }
  nextTick(() => {
    const target = document.getElementById(id)
    if (target) {
      target.scrollIntoView({ block: 'start' })
      hashHandled = true
    }
  })
}

onMounted(scrollToHash)

function clearSearch() {
  query.value = ''
}

// Vue flushes the v-show update on a microtask, after the browser would have
// handled a raw hash jump, so jump explicitly once the section is visible.
function jumpTo(id) {
  query.value = ''
  nextTick(() => {
    const target = document.getElementById(id)
    if (target) target.scrollIntoView({ block: 'start' })
  })
}
</script>

<template>
  <div class="mx-auto w-full max-w-2xl flex flex-col gap-5">
    <div class="flex flex-wrap items-end justify-between gap-2">
      <h1 class="my-2 flex items-center gap-2 text-xl font-semibold leading-tight">
        Settings
      </h1>
      <label class="input input-sm input-bordered flex w-full items-center gap-2 sm:w-64">
        <Icon size="16px">search</Icon>
        <input
          v-model="query"
          type="search"
          class="grow text-sm"
          placeholder="Filter settings"
          aria-label="Filter settings sections"
          autocomplete="off"
        />
      </label>
    </div>

    <nav class="flex flex-wrap items-center gap-x-4 gap-y-1" aria-label="Settings sections">
      <button
        v-for="section in sections"
        :key="section.id"
        type="button"
        class="btn btn-ghost btn-sm"
        :class="visible[section.id] ? '' : 'opacity-40'"
        @click="jumpTo(section.id)"
      >
        <Icon size="20px">{{ section.icon }}</Icon>
        {{ section.title }}
      </button>
    </nav>

    <div v-if="!data && !error" class="flex flex-col gap-5" aria-busy="true">
      <div v-for="n in 4" :key="n" class="rounded-box bg-base-100">
        <div class="flex flex-col gap-3 p-4">
          <div class="skeleton h-3 w-1/3"></div>
          <div class="skeleton h-8 w-full"></div>
          <div class="skeleton h-8 w-2/3"></div>
        </div>
      </div>
    </div>

    <div v-else-if="!data" class="alert alert-error" role="alert">
      <span class="text-sm">
        <strong>Settings unavailable.</strong>
        {{ error }} The web UI reconnects automatically when the device is reachable.
      </span>
    </div>

    <template v-else>
      <div v-show="!visibleCount" class="alert py-2" role="status">
        <span class="text-sm">No settings match “{{ query }}”.</span>
        <button type="button" class="btn btn-sm btn-ghost" @click="clearSearch">Clear filter</button>
      </div>

      <section
        v-show="visible['voice-assistant']"
        id="voice-assistant"
        class="scroll-mt-24 rounded-box bg-base-100"
      >
        <div class="flex flex-col gap-3 p-4 sm:p-5">
          <div class="flex items-center gap-2">
            <Icon size="20px" class="text-base-content/60">record_voice_over</Icon>
            <h2 class="text-lg font-semibold">Voice assistant</h2>
          </div>
          <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <label class="flex flex-col gap-1 text-sm">
              <div class="alert flex-grow">
                Choose an app that will launch when you hold the assistant button on the speaker.
              </div>
              <span class="font-medium mt-2">Default assistant app</span>
              <select
                v-model="ui.assistant.app"
                class="select select-bordered select-sm w-full"
                @change="applyAssistant(['default_assistant_app'])"
              >
                <option value="">— none —</option>
                <option
                  v-if="savedProvider('voice_assistant', ui.assistant.app)"
                  :value="ui.assistant.app"
                >
                  {{ savedProvider('voice_assistant', ui.assistant.app).label }}
                </option>
                <option v-for="app in providerApps('voice_assistant')" :key="app.appId" :value="app.appId">
                  {{ providerLabel(app) }}
                </option>
              </select>
            </label>
            <label class="flex flex-col gap-1 text-sm">
              <div class="alert flex-grow">
                <template v-if="ui.assistant.touch === 'summon'">
                  "Hold to summon" — hold the button, the assistant activates, you release it — assistant continues to listen.
                </template>
                <template v-if="ui.assistant.touch === 'talk'">
                  "Hold to talk" — hold the button, the assistant activates, you release it — assistant stops listening.
                </template>
              </div>
              <span class="font-medium mt-2">Touch and hold</span>
              <select
                v-model="ui.assistant.touch"
                class="select select-bordered select-sm w-full"
                @change="applyAssistant(['touch_hold_mode'])"
              >
                <option value="summon">Hold to summon</option>
                <option value="talk">Hold to talk</option>
              </select>
            </label>
          </div>
          <p v-if="!providerApps('voice_assistant').length" class="text-xs text-base-content/60">
            No installed app provides a voice assistant.
          </p>
        </div>
      </section>

      <div v-show="visible['wakeword']" id="wakeword" class="scroll-mt-24">
        <WakewordSettings />
      </div>

      <section
        v-show="visible['display-ring']"
        id="display-ring"
        class="scroll-mt-24 rounded-box bg-base-100"
      >
        <div class="flex flex-col gap-4 p-4 sm:p-5">
          <div class="flex items-center gap-2">
            <Icon size="20px" class="text-base-content/60">display_settings</Icon>
            <h2 class="text-lg font-semibold">Display &amp; ring</h2>
          </div>

          <div
            v-for="kind in ['display', 'ring']"
            :key="kind"
            class="flex flex-col gap-3 rounded-box bg-base-200/60 p-3 sm:p-4"
          >
            <div class="flex items-center gap-2">
              <h3 class="flex-grow text-base font-semibold">
                {{ kind === 'display' ? 'Segment display' : 'LED ring' }}
              </h3>
              <span v-if="!data[kind]" class="text-xs text-base-content/60">not reported</span>
            </div>

            <template v-if="data[kind]">
              <label class="flex flex-col gap-1.5 text-sm sm:flex-row sm:items-center sm:justify-between sm:gap-3">
                <span class="font-medium">Default application</span>
                <select
                  v-model="ui[kind].app"
                  class="select select-bordered select-sm w-full sm:w-64"
                  @change="applyResourceNow(kind, ['default_app'])"
                >
                  <option value="">— none —</option>
                  <option
                    v-if="savedProvider(providerToken(kind), ui[kind].app)"
                    :value="ui[kind].app"
                  >
                    {{ savedProvider(providerToken(kind), ui[kind].app).label }}
                  </option>
                  <option v-for="app in providerApps(providerToken(kind))" :key="app.appId" :value="app.appId">
                    {{ providerLabel(app) }}
                  </option>
                </select>
              </label>

              <div class="flex flex-col gap-2">
                <div class="flex items-center justify-between gap-2">
                  <span class="text-sm font-medium">Brightness</span>
                  <span class="text-sm tabular-nums text-base-content/60">
                    {{ ui[kind].mode === 'auto' ? 'auto' : `${ui[kind].brightness}%` }}
                  </span>
                </div>
                <div class="flex flex-col gap-2 sm:flex-row sm:items-center sm:gap-3">
                  <input
                    v-model.number="ui[kind].brightness"
                    type="range"
                    min="0"
                    max="100"
                    step="1"
                    class="range range-primary range-sm w-full sm:flex-1"
                    :disabled="ui[kind].mode === 'auto'"
                    :aria-label="`${kind === 'display' ? 'Segment display' : 'LED ring'} constant brightness`"
                    @pointerdown="beginDrag(kind)"
                    @pointerup="endDrag(kind)"
                    @change="endDrag(kind)"
                    @input="markResource(kind, ['brightness'])"
                  />
                  <label class="flex shrink-0 cursor-pointer items-center gap-2">
                    <input
                      type="checkbox"
                      class="checkbox checkbox-sm"
                      aria-label="Automatic brightness"
                      :checked="ui[kind].mode === 'auto'"
                      :disabled="!ambient && ui[kind].mode !== 'auto'"
                      @change="toggleAuto(kind, $event)"
                    />
                    <span class="text-sm">Automatic</span>
                  </label>
                </div>
                <p v-if="!ambient" class="text-xs text-warning">
                  No room-light sensor: your brightness is used.
                </p>
                <p v-else-if="ui[kind].mode === 'auto'" class="text-xs text-base-content/60">
                  The slider is off while automatic adjustment is on.
                </p>
              </div>
            </template>
            <p v-if="!providerApps(providerToken(kind)).length" class="text-xs text-base-content/60">
              No installed app provides this output.
            </p>
          </div>
        </div>
      </section>

      <section
        v-show="visible['microphone']"
        id="microphone"
        class="scroll-mt-24 rounded-box bg-base-100"
      >
        <div class="flex flex-col gap-3 p-4 sm:p-5">
          <div class="flex items-center gap-2">
            <Icon size="20px" class="text-base-content/60">mic</Icon>
            <h2 class="text-lg font-semibold">Microphone</h2>
          </div>
          <div class="flex flex-col gap-3">
            <MicrophoneControl class="[&>label]:flex-row-reverse [&>label]:justify-between" />
            <label class="flex flex-col gap-1.5 text-sm sm:flex-row sm:items-center sm:justify-between sm:gap-3">
              <span class="font-medium">Hide the muted light</span>
              <select
                v-model="ui.indicator"
                class="select select-bordered select-sm w-full sm:w-64"
                @change="applyIndicator"
              >
                <option v-for="option in indicatorOptions" :key="option.value" :value="option.value">
                  {{ option.label }}
                </option>
              </select>
            </label>
          </div>
          <p class="text-xs text-base-content/60">
            “Never” keeps the muted light visible until you unmute; changing this never changes the saved delay.
          </p>
        </div>
      </section>

      <section
        v-show="visible['bluetooth']"
        id="bluetooth"
        class="scroll-mt-24 rounded-box bg-base-100"
      >
        <div class="flex flex-col gap-4 p-4 sm:p-5">
          <div class="flex flex-col gap-1">
            <div class="flex items-center gap-2">
              <Icon size="20px" class="text-base-content/60">bluetooth</Icon>
              <h2 class="text-lg font-semibold">Bluetooth</h2>
            </div>
            <div class="flex items-center justify-between gap-2">
              <span class="text-sm font-medium">Device name</span>
              <span class="flex min-w-0 items-center gap-1">
                <span class="truncate font-mono text-xs text-base-content/60">
                  {{ btStatus?.name || '—' }}
                </span>
                <button
                  type="button"
                  class="btn btn-ghost btn-sm btn-square"
                  aria-label="Change Bluetooth device name"
                  :disabled="!btStatus?.present || !!busy"
                  @click="openAdapterName"
                >
                  <Icon size="18px">edit</Icon>
                </button>
              </span>
            </div>
          </div>

          <div class="flex flex-col gap-3">
            <label class="flex cursor-pointer items-start justify-between gap-3">
              <span class="min-w-0">
                <span class="block text-sm font-medium">Speaker mode</span>
                <span class="mt-0.5 block text-xs text-base-content/60">
                  Accept audio from already-paired devices. Turning this off keeps a connected device connected.
                </span>
              </span>
              <input
                type="checkbox"
                class="toggle toggle-primary toggle-sm mt-1 shrink-0"
                :checked="ui.bluetooth.incoming"
                :disabled="!btStatus?.present || busy === 'bluetooth'"
                @change="toggleIncoming"
              />
            </label>
            <label class="flex cursor-pointer items-start justify-between gap-3">
              <span class="min-w-0">
                <span class="block text-sm font-medium">Pair new devices</span>
                <span class="mt-0.5 block text-xs text-base-content/60">Let nearby devices find and pair with this speaker.</span>
              </span>
              <input
                type="checkbox"
                class="toggle toggle-primary toggle-sm mt-1 shrink-0"
                :checked="ui.bluetooth.discoverable"
                :disabled="!btStatus?.present || busy === 'bluetooth'"
                @change="toggleDiscoverable"
              />
            </label>
          </div>

          <div class="flex flex-col gap-2">
            <div class="flex items-center gap-2">
              <h3 class="flex-grow text-sm font-semibold">Paired devices</h3>
              <button type="button" class="btn btn-ghost btn-xs" :disabled="!overviewData" @click="reloadOverview">
                Refresh
              </button>
            </div>
            <p v-if="!overviewData && overviewError" class="text-xs text-base-content/60" role="status">
              Paired devices are unavailable.
            </p>
            <p v-else-if="!overviewData" class="text-xs text-base-content/60">Loading paired devices…</p>
            <p v-else-if="!pairedDevices.length" class="text-xs text-base-content/60">No paired devices.</p>
            <ul v-else class="divide-y divide-base-300">
              <li
                v-for="device in pairedDevices"
                :key="device.deviceId"
                class="flex flex-wrap items-center justify-between gap-2 py-2 first:pt-0 last:pb-0"
              >
                <p class="flex min-w-0 items-center gap-1.5 truncate text-sm font-medium">
                  {{ device.name || 'Unnamed device' }}
                  <span v-if="device.connected" class="badge badge-success badge-sm">connected</span>
                </p>
                <button
                  type="button"
                  class="btn btn-xs btn-outline"
                  :disabled="busy === 'unpair'"
                  @click="askUnpair(device)"
                >
                  Unpair
                </button>
              </li>
            </ul>
          </div>
        </div>
      </section>

      <div v-show="visible['wifi']">
        <WifiSettings />
      </div>

      <section
        v-show="visible['device']"
        id="device"
        class="scroll-mt-24 rounded-box bg-base-100"
      >
        <div class="flex flex-col gap-4 p-4 sm:p-5">
          <div class="flex items-center gap-2">
            <Icon size="20px" class="text-base-content/60">schedule</Icon>
            <h2 class="text-lg font-semibold">Time &amp; device</h2>
          </div>

          <div class="flex flex-col gap-4">
            <TimezonePicker class="w-full" />

            <div class="flex flex-col gap-2 rounded-box bg-base-200/60 p-3 sm:p-4">
              <h3 class="text-base font-semibold">Speaker name</h3>
              <div class="flex flex-col gap-2 sm:flex-row sm:items-center">
                <input
                  v-model="drafts.hostname"
                  type="text"
                  class="input input-bordered input-sm w-full sm:max-w-xs"
                  maxlength="63"
                  autocomplete="off"
                  spellcheck="false"
                  aria-label="Speaker name on your network"
                  @input="dirty.hostname = true"
                />
                <button
                  type="button"
                  class="btn btn-sm btn-outline"
                  :disabled="!dirty.hostname"
                  @click="saveHostname"
                >
                  Save name
                </button>
              </div>
              <p v-if="namingUnsupported" class="text-xs text-base-content/60">
                The speaker name is saved as
                <code class="font-mono">{{ advertisedName }}</code>
                and will be advertised when network naming is available.
              </p>
              <p v-else class="text-xs text-base-content/60">
                The speaker can be discovered as
                <code class="font-mono">{{ advertisedName }}</code>
                on the network.
              </p>
            </div>

            <div class="flex flex-col gap-3 rounded-box bg-base-200/60 p-3 sm:p-4">
              <h3 class="text-base font-semibold">Sound and boot</h3>
              <VolumeControl />
              <label class="flex cursor-pointer items-start justify-between gap-3">
                <span class="min-w-0">
                  <span class="block text-sm font-medium">Startup sound</span>
                  <span class="block text-xs text-base-content/60">Play the chime on every boot.</span>
                </span>
                <input
                  type="checkbox"
                  class="toggle toggle-primary toggle-sm mt-1 shrink-0"
                  :checked="ui.startup"
                  @change="toggleStartup"
                />
              </label>
            </div>
          </div>
        </div>
      </section>
    </template>

    <ModalDialog
      v-if="adapterNameOpen"
      title="Change device name"
      max-width="max-w-md"
      @close="closeAdapterName"
    >
      <form class="flex flex-col gap-3" @submit.prevent="saveAdapterName">
        <label class="flex flex-col gap-1.5 text-sm">
          <span class="font-medium">Device name</span>
          <input
            v-model="adapterNameDraft"
            type="text"
            class="input input-bordered w-full"
            maxlength="248"
            autocomplete="off"
            spellcheck="false"
            autofocus
            aria-label="Bluetooth device name"
            aria-describedby="bluetooth-name-help"
            :aria-invalid="adapterNameError ? 'true' : 'false'"
          />
        </label>
        <p id="bluetooth-name-help" class="text-xs text-base-content/60">
          Shown to nearby devices when pairing and in their Bluetooth menu.
          <span class="tabular-nums">{{ adapterNameBytes }} / {{ ADAPTER_NAME_MAX_BYTES }} bytes</span>
        </p>
        <p v-if="adapterNameError" class="text-sm text-error" role="alert">{{ adapterNameError }}</p>
        <div class="modal-action m-0">
          <button type="button" class="btn btn-ghost" @click="closeAdapterName">Cancel</button>
          <button
            type="submit"
            class="btn btn-primary"
            :disabled="adapterNameSaving || !adapterNameValid"
            :aria-busy="adapterNameSaving ? 'true' : 'false'"
          >
            <span v-if="adapterNameSaving" class="loading loading-spinner loading-xs" aria-hidden="true"></span>
            Save
          </button>
        </div>
      </form>
    </ModalDialog>

    <ModalDialog v-if="unpairTarget" title="Unpair device" @close="unpairTarget = null">
      <p class="text-sm">
        Unpair <strong>{{ unpairTarget.name || 'Unnamed device' }}</strong>? It will be forgotten by
        Bluetooth and must pair again to reconnect.
      </p>
      <template #footer>
        <button type="button" class="btn btn-ghost" @click="unpairTarget = null">Cancel</button>
        <button
          type="button"
          class="btn btn-error"
          :disabled="busy === 'unpair'"
          :aria-busy="busy === 'unpair' ? 'true' : 'false'"
          @click="confirmUnpair"
        >
          <span v-if="busy === 'unpair'" class="loading loading-spinner loading-xs" aria-hidden="true"></span>
          Unpair
        </button>
      </template>
    </ModalDialog>
  </div>
</template>
