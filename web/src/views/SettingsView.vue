<script setup>
import { computed, nextTick, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import WifiSettings from '../components/WifiSettings.vue'
import WakewordSettings from '../components/WakewordSettings.vue'
import TimezonePicker from '../components/TimezonePicker.vue'
import VolumeControl from '../components/VolumeControl.vue'
import MicrophoneControl from '../components/MicrophoneControl.vue'
import ModalDialog from '../components/ModalDialog.vue'
import { socket } from '../lib/ws.js'
import { useSettings, useTopic } from '../lib/live.js'
import { pct } from '../lib/format.js'

// Settings is a compact appliance console: every control is either an
// immediate bounded update (select, slider, toggle) or a local draft submitted
// by its own button. All authority lives in the settings snapshot; this view
// only mirrors it and never rewrites a preference the user did not touch.

const { data, error, pending, update } = useSettings()
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
const actionError = ref('')
const btOverride = ref(null)
const unpairTarget = ref(null)

const values = computed(() => data.value?.values || {})
const apps = computed(() => data.value?.apps || [])
const ambient = computed(() => Boolean(data.value?.ambientAvailable))
const btStatus = computed(() => btOverride.value || data.value?.bluetooth || null)
const pairedDevices = computed(() => overviewData.value?.pairedDevices || [])

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
  if (ok) actionError.value = ''
  else actionError.value = error.value || 'The speaker rejected that change.'
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
  if (ok) {
    actionError.value = ''
  } else {
    actionError.value = error.value || failure
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
    wakewordModel: v.wakewordModelId || '',
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
  actionError.value = ''
  try {
    const result = await socket.request({ bluetooth: { [action]: payload } })
    if (result?.bluetooth?.status) {
      btOverride.value = result.bluetooth.status
      ui.bluetooth.incoming = !!result.bluetooth.status.incomingConnections
      ui.bluetooth.discoverable = !!result.bluetooth.status.discoverable
    }
  } catch (err) {
    actionError.value = err.message || failure
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
  refreshOverview().catch(() => {})
}

function askUnpair(device) {
  if (busy.value) return
  unpairTarget.value = device
}

async function confirmUnpair() {
  const device = unpairTarget.value
  if (!device || busy.value) return
  busy.value = 'unpair'
  actionError.value = ''
  try {
    await socket.request({ bluetooth: { unpair: { address: device.address } } })
    unpairTarget.value = null
    await refreshOverview()
  } catch (err) {
    actionError.value = err.message
  } finally {
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
    actionError.value = 'Use up to 63 letters, numbers or hyphens, starting and ending with a letter or number.'
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
    title: 'Voice assistant',
    keywords: ['voice', 'assistant', 'wakeword', 'summon', 'talk', 'touch', 'wake word', 'training']
  },
  {
    id: 'display-ring',
    title: 'Display & ring',
    keywords: ['display', 'ring', 'led', 'segment', 'brightness', 'ambient', 'auto', 'default app']
  },
  {
    id: 'microphone',
    title: 'Microphone',
    keywords: ['microphone', 'mic', 'mute', 'hide', 'delay', 'indicator', 'privacy']
  },
  {
    id: 'bluetooth',
    title: 'Bluetooth',
    keywords: ['bluetooth', 'pair', 'unpair', 'discoverable', 'incoming', 'speaker']
  },
  {
    id: 'wifi',
    title: 'Wi-Fi',
    keywords: ['wifi', 'wi-fi', 'network', 'ssid', 'password', 'scan', 'portal', 'wireless']
  },
  {
    id: 'device',
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
  <div class="space-y-4">
    <header class="space-y-3">
      <div class="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 class="text-2xl font-semibold tracking-tight">Settings</h1>
          <p class="max-w-2xl text-sm opacity-70">
            Selects, sliders and toggles apply immediately. Text fields stay as your draft until you press
            their button, even when another session or the speaker changes something.
          </p>
          <p v-if="pending" class="mt-1 flex items-center gap-2 text-xs opacity-60" role="status">
            <span class="loading loading-spinner loading-xs" aria-hidden="true"></span>
            Applying…
          </p>
        </div>
        <label class="input input-bordered flex w-full items-center gap-2 sm:w-72">
          <svg class="h-4 w-4 opacity-60" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true">
            <path fill-rule="evenodd" d="M9 3a6 6 0 104.472 10.007l3.26 3.26a1 1 0 001.415-1.414l-3.26-3.26A6 6 0 009 3zm-4 6a4 4 0 118 0 4 4 0 01-8 0z" clip-rule="evenodd" />
          </svg>
          <input
            v-model="query"
            type="search"
            class="grow"
            placeholder="Filter settings"
            aria-label="Filter settings sections"
            autocomplete="off"
          />
        </label>
      </div>

      <nav class="flex flex-wrap items-center gap-1" aria-label="Settings sections">
        <a
          v-for="section in sections"
          :key="section.id"
          :href="`#${section.id}`"
          class="btn btn-xs btn-ghost"
          :class="visible[section.id] ? '' : 'opacity-40'"
          @click.prevent="jumpTo(section.id)"
        >{{ section.title }}</a>
      </nav>
    </header>

    <div v-if="actionError" class="alert alert-error" role="alert">
      <span>{{ actionError }}</span>
    </div>

    <div v-if="!data && !error" class="grid gap-3 md:grid-cols-2">
      <div class="card bg-base-100 shadow-sm"><div class="card-body gap-2">
        <div class="skeleton h-4 w-32"></div>
        <div class="skeleton h-20 w-full"></div>
      </div></div>
      <div class="card bg-base-100 shadow-sm"><div class="card-body gap-2">
        <div class="skeleton h-4 w-40"></div>
        <div class="skeleton h-20 w-full"></div>
      </div></div>
    </div>

    <div v-else-if="!data" class="alert alert-error" role="alert">
      <span>
        <strong>Settings unavailable.</strong>
        {{ error }} The console reconnects automatically when the speaker is reachable.
      </span>
    </div>

    <template v-else>
      <div v-show="!visibleCount" class="alert" role="status">
        <span>No settings match “{{ query }}”.</span>
        <button type="button" class="btn btn-sm btn-ghost" @click="clearSearch">Clear filter</button>
      </div>

      <div v-show="visible['voice-assistant']" class="space-y-3">
        <section id="voice-assistant" class="card scroll-mt-24 bg-base-100 shadow-sm">
          <div class="card-body gap-4">
            <div>
              <h2 class="card-title text-lg">Voice assistant</h2>
              <p class="text-sm opacity-70">
                The assistant app answers wakeword and touch summons. Wakeword detection and voice training live
                with the assistant below.
              </p>
            </div>

            <div class="grid gap-3 md:grid-cols-2">
              <label class="form-control w-full">
                <span class="label-text mb-1">Default assistant</span>
                <select
                  v-model="ui.assistant.app"
                  class="select select-bordered w-full"
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
              <label class="form-control w-full">
                <span class="label-text mb-1">Touch and hold</span>
                <select
                  v-model="ui.assistant.touch"
                  class="select select-bordered w-full"
                  @change="applyAssistant(['touch_hold_mode'])"
                >
                  <option value="summon">Hold to summon</option>
                  <option value="talk">Hold to talk</option>
                </select>
              </label>
            </div>
            <p v-if="!providerApps('voice_assistant').length" class="text-xs opacity-60">
              No installed app provides a voice assistant. Install one to choose it here.
            </p>
            <p class="text-xs opacity-60">
              <template v-if="ui.assistant.touch === 'talk'">
                Release to end listening.
              </template>
              <template v-else>
                Let the assistant decide when you have finished.
              </template>
            </p>

            <div class="divider my-0"></div>
            <div id="wakeword" class="scroll-mt-24">
              <WakewordSettings />
            </div>
          </div>
        </section>
      </div>

      <div v-show="visible['display-ring']" class="space-y-3">
        <section id="display-ring" class="card scroll-mt-24 bg-base-100 shadow-sm">
          <div class="card-body gap-5">
            <div>
              <h2 class="card-title text-lg">Display &amp; ring</h2>
              <p class="text-sm opacity-70">
                Default apps own their output. Brightness is either set by you or adjusted to the room light.
              </p>
            </div>

            <div v-for="kind in ['display', 'ring']" :key="kind" class="space-y-3">
              <div class="flex items-center gap-2">
                <h3 class="font-medium">{{ kind === 'display' ? 'Segment display' : 'LED ring' }}</h3>
                <span v-if="!data[kind]" class="badge badge-ghost badge-sm">not reported</span>
              </div>

              <div v-if="data[kind]" class="grid gap-3 md:grid-cols-2">
                <label class="form-control w-full">
                  <span class="label-text mb-1">Default application</span>
                  <select
                    v-model="ui[kind].app"
                    class="select select-bordered w-full"
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

                <div class="space-y-2">
                  <div class="flex items-center justify-between gap-2">
                    <span class="label-text">Brightness</span>
                    <span class="badge badge-ghost badge-sm">{{ ui[kind].mode === 'auto' ? 'auto' : `${ui[kind].brightness}%` }}</span>
                  </div>
                  <input
                    v-model.number="ui[kind].brightness"
                    type="range"
                    min="0"
                    max="100"
                    step="1"
                    class="range range-primary range-sm w-full"
                    :disabled="ui[kind].mode === 'auto'"
                    :aria-label="`${kind === 'display' ? 'Segment display' : 'LED ring'} constant brightness`"
                    @pointerdown="beginDrag(kind)"
                    @pointerup="endDrag(kind)"
                    @change="endDrag(kind)"
                    @input="markResource(kind, ['brightness'])"
                  />
                  <label class="label cursor-pointer justify-start gap-2 py-0">
                    <input
                      type="checkbox"
                      class="checkbox checkbox-sm"
                      :checked="ui[kind].mode === 'auto'"
                      :disabled="!ambient && ui[kind].mode !== 'auto'"
                      @change="toggleAuto(kind, $event)"
                    />
                    <span class="label-text">Automatically adjust to room light</span>
                  </label>
                  <p v-if="!ambient" class="text-xs text-warning">
                    This speaker has no room-light sensor, so automatic adjustment is off. Your set brightness
                    is used.
                  </p>
                  <p v-else-if="ui[kind].mode === 'auto'" class="text-xs opacity-60">
                    The slider is off while automatic adjustment is on.
                  </p>
                </div>
              </div>
              <p v-if="!providerApps(providerToken(kind)).length" class="text-xs opacity-60">
                No installed app provides this output. Choose none or install one.
              </p>
            </div>
          </div>
        </section>
      </div>

      <div v-show="visible['microphone']" class="space-y-3">
        <section id="microphone" class="card scroll-mt-24 bg-base-100 shadow-sm">
          <div class="card-body gap-4">
            <div>
              <h2 class="card-title text-lg">Microphone</h2>
              <p class="text-sm opacity-70">
                Muting stops the microphone everywhere. The muted light can hide after a delay without
                changing the mute state.
              </p>
            </div>

            <MicrophoneControl />

            <div class="divider my-0"></div>

            <label class="form-control w-full md:max-w-sm">
              <span class="label-text mb-1">Hide the muted light</span>
              <select v-model="ui.indicator" class="select select-bordered w-full" @change="applyIndicator">
                <option v-for="option in indicatorOptions" :key="option.value" :value="option.value">
                  {{ option.label }}
                </option>
              </select>
            </label>
            <p class="text-xs opacity-60">
              “Never” is the default: the muted light stays visible until the microphone is unmuted. Changing
              this never changes the saved delay.
            </p>
          </div>
        </section>
      </div>

      <div v-show="visible['bluetooth']" class="space-y-3">
        <section id="bluetooth" class="card scroll-mt-24 bg-base-100 shadow-sm">
          <div class="card-body gap-4">
            <div class="flex flex-wrap items-start justify-between gap-2">
              <div>
                <h2 class="card-title text-lg">Bluetooth</h2>
                <p class="text-sm opacity-70">Choose how phones, tablets and speakers connect over Bluetooth.</p>
              </div>
              <div class="flex items-center gap-2">
                <span v-if="btStatus" class="badge" :class="btStatus.powered ? 'badge-success' : 'badge-ghost'">
                  {{ btStatus.powered ? 'Bluetooth on' : 'Bluetooth off' }}
                </span>
                <span v-if="btStatus?.name" class="font-mono text-xs opacity-60">{{ btStatus.name }}</span>
              </div>
            </div>

            <div v-if="data.bluetoothError" class="alert alert-warning" role="status">{{ data.bluetoothError }}</div>

            <div class="space-y-2">
              <label class="label cursor-pointer justify-start gap-3 py-0">
                <input
                  type="checkbox"
                  class="toggle toggle-primary"
                  :checked="ui.bluetooth.incoming"
                  :disabled="!btStatus?.present || busy === 'bluetooth'"
                  @change="toggleIncoming"
                />
                <span>
                  <span class="label-text block">Speaker mode</span>
                  <span class="block text-xs opacity-60">
                    Accept audio from paired devices. Turning this off does not disconnect a device already
                    connected.
                  </span>
                </span>
              </label>
              <label class="label cursor-pointer justify-start gap-3 py-0">
                <input
                  type="checkbox"
                  class="toggle toggle-primary"
                  :checked="ui.bluetooth.discoverable"
                  :disabled="!btStatus?.present || busy === 'bluetooth'"
                  @change="toggleDiscoverable"
                />
                <span>
                  <span class="label-text block">Pair new devices</span>
                  <span class="block text-xs opacity-60">
                    Let nearby devices find and pair with this speaker.
                  </span>
                </span>
              </label>
            </div>

            <div class="divider my-0"></div>

            <div>
              <div class="mb-2 flex items-center justify-between gap-2">
                <h3 class="font-medium">Paired devices</h3>
                <button type="button" class="btn btn-xs btn-ghost" :disabled="!overviewData" @click="reloadOverview">
                  Refresh
                </button>
              </div>
              <p v-if="overviewError" class="text-sm text-error" role="alert">{{ overviewError }}</p>
              <p v-else-if="!overviewData" class="text-sm opacity-70">Loading paired devices…</p>
              <p v-else-if="!pairedDevices.length" class="text-sm opacity-70">No paired devices.</p>
              <ul v-else class="space-y-1">
                <li
                  v-for="device in pairedDevices"
                  :key="device.address"
                  class="flex flex-wrap items-center justify-between gap-2 rounded-box border border-base-300 px-3 py-2"
                >
                  <div class="min-w-0">
                    <p class="truncate text-sm font-medium">
                      {{ device.name || 'Unnamed device' }}
                      <span v-if="device.connected" class="badge badge-success badge-sm ml-1">connected</span>
                    </p>
                    <p class="font-mono text-xs opacity-60">{{ device.address }}</p>
                  </div>
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
      </div>

      <div v-show="visible['wifi']">
        <WifiSettings />
      </div>

      <div v-show="visible['device']" class="space-y-3">
        <section id="device" class="card scroll-mt-24 bg-base-100 shadow-sm">
          <div class="card-body gap-5">
            <div>
              <h2 class="card-title text-lg">Time &amp; device</h2>
              <p class="text-sm opacity-70">Time zone, speaker name, volume and the boot chime.</p>
            </div>

            <div class="space-y-3">
              <h3 class="font-medium">Time zone</h3>
              <TimezonePicker />
            </div>

            <div class="divider my-0"></div>

            <div class="space-y-3">
              <h3 class="font-medium">Speaker name</h3>
              <div class="flex flex-col gap-2 sm:flex-row sm:items-end">
                <label class="form-control w-full sm:max-w-xs">
                  <span class="label-text mb-1">Name on your network</span>
                  <input
                    v-model="drafts.hostname"
                    type="text"
                    class="input input-bordered w-full"
                    maxlength="63"
                    autocomplete="off"
                    spellcheck="false"
                    @input="dirty.hostname = true"
                  />
                </label>
                <button
                  type="button"
                  class="btn btn-sm btn-outline sm:mb-0.5"
                  :disabled="!dirty.hostname"
                  @click="saveHostname"
                >
                  Save name
                </button>
              </div>
              <p class="text-xs opacity-60">
                Other devices on your home network can use this name to find the speaker.
              </p>
              <details class="text-xs opacity-70">
                <summary class="cursor-pointer">Technical details</summary>
                <p class="mt-1">
                  A single DNS label, advertised as <code>&lt;name&gt;.local</code>.
                  <template v-if="data.discovery?.advertised">
                    Currently advertised as <code>{{ data.discovery.advertised }}</code>.
                  </template>
                  <template v-else-if="data.discovery && !data.discovery.supported">
                    mDNS advertising is unavailable; the name is stored for when a responder exists.
                  </template>
                </p>
              </details>
            </div>

            <div class="divider my-0"></div>

            <VolumeControl />

            <label class="label cursor-pointer justify-start gap-3 py-0">
              <input
                type="checkbox"
                class="toggle toggle-primary"
                :checked="ui.startup"
                @change="toggleStartup"
              />
              <span>
                <span class="label-text block">Startup sound</span>
                <span class="block text-xs opacity-60">Play the chime on every boot.</span>
              </span>
            </label>
          </div>
        </section>
      </div>
    </template>

    <ModalDialog v-if="unpairTarget" title="Unpair device" @close="unpairTarget = null">
      <p class="text-sm">
        Unpair <strong>{{ unpairTarget.name || unpairTarget.address }}</strong>? It will be forgotten by
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
