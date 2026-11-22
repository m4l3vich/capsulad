<script setup>
import { computed, nextTick, onUnmounted, ref, watch } from 'vue'
import { MediaAction, Topic } from '../proto/web_pb.js'
import { liveState, useTopic } from '../lib/live.js'
import { coldPlayLaunch, hasLiveRun, hasReadyRun, isColdPlay } from '../lib/appActions.js'
import { socket } from '../lib/ws.js'
import { formatBytes } from '../lib/format.js'
import { deviceHeadline } from '../lib/device.js'
import { appPackageStorage, installOutcome, removePackage } from '../lib/apps.js'
import VolumeControl from '../components/VolumeControl.vue'
import MicrophoneControl from '../components/MicrophoneControl.vue'
import ModalDialog from '../components/ModalDialog.vue'
import AppInstallModal from '../components/AppInstallModal.vue'
import ApplicationView from './ApplicationView.vue'
import Icon from '../components/Icon.vue'
import { notify } from '../lib/toast.js'
import { connectedBadge } from '../stores/overviewBluetooth.js'

const { data, error, loading, refresh } = useTopic('overview')
// const data = ref(null)
// const error = ref(null)
// const loading = true
// const refresh = () => { }
const busy = ref('')
const selectedApp = ref('')
const discoverable = ref(false)
const incoming = ref(false)
const showInstallModal = ref(false)

const bluetooth = computed(() => data.value?.bluetooth || null)
const bluetoothReady = computed(() =>
  !!bluetooth.value?.present && !!bluetooth.value?.powered && liveState.value === 'open')
// Header status for this card: a green chip naming every paired device the
// snapshot reports connected. Null (no chip at all) whenever none is linked or
// the snapshot has not arrived, so the header never shows a stale "connected".
const bluetoothConnected = computed(() => connectedBadge(data.value?.pairedDevices))
const apps = computed(() => data.value?.apps || [])
const runs = computed(() => data.value?.runs || [])
// The overview snapshot is the Now Playing projection: only players that
// report PLAYBACK_PLAYING, and only providers that still have one. A connected
// but paused source, an idle app and a provider whose players cannot be read
// are absent, so the provider tabs and player list never offer a row the user
// cannot act on. With none left the card falls back to "Nothing is playing."
const providers = computed(() => data.value?.providers || [])
const selected = computed(() => data.value?.selectedProviders || {})

// A refresh failure keeps the previous snapshot on screen, so it surfaces as a
// toast rather than replacing the view. Toast once per distinct failure.
let lastRefreshError = ''
watch(error, (value) => {
  if (!value) {
    lastRefreshError = ''
    return
  }
  if (!data.value || value === lastRefreshError) return
  lastRefreshError = value
  notify.warning(`The last refresh failed: ${value}`)
})

const runsByApp = computed(() => {
  const map = new Map()
  for (const run of runs.value) {
    const list = map.get(run.appId) || []
    list.push(run)
    map.set(run.appId, list)
  }
  return map
})

function appRuns(appId) {
  return runsByApp.value.get(appId) || []
}

function appReady(appId) {
  return hasReadyRun(appRuns(appId))
}

function appLive(appId) {
  return hasLiveRun(appRuns(appId))
}

function syncBluetoothPolicy() {
  if (busy.value.startsWith('bt:')) return
  discoverable.value = !!data.value?.bluetooth?.discoverable
  incoming.value = !!data.value?.bluetooth?.incomingConnections
}

watch(
  () => [data.value?.bluetooth?.discoverable, data.value?.bluetooth?.incomingConnections],
  syncBluetoothPolicy,
  { immediate: true }
)

// A backend may coerce one policy while the other command is in flight (for
// example, blocking incoming also clears discoverability). Re-read both from
// the authoritative snapshot once the command settles.
watch(busy, (value) => {
  if (!value) syncBluetoothPolicy()
})

async function run(key, command, message) {
  if (busy.value) return false
  if (liveState.value !== 'open') {
    notify.error('The speaker is not connected.')
    return false
  }
  busy.value = key
  try {
    await socket.request(command)
    if (message) notify.success(message)
    return true
  } catch (err) {
    notify.error(err?.message || 'The device rejected the request.')
    return false
  } finally {
    busy.value = ''
  }
}

async function setDiscoverable(next) {
  const previous = discoverable.value
  discoverable.value = next
  const ok = await run(
    'bt:discoverable',
    { bluetooth: { setDiscoverable: { discoverable: next } } },
    next ? 'This speaker is now visible to nearby devices' : 'This speaker is hidden from nearby devices'
  )
  if (!ok) {
    discoverable.value = previous
    return
  }
  await refresh()
}

async function setIncoming(next) {
  const previous = incoming.value
  incoming.value = next
  const ok = await run(
    'bt:incoming',
    { bluetooth: { setIncomingConnections: { incomingConnections: next } } },
    next ? 'Incoming connections are allowed' : 'Incoming connections are blocked'
  )
  if (!ok) {
    incoming.value = previous
    return
  }
  await refresh()
}

function missingArgument(app) {
  return (app.launchArguments || []).find((arg) => arg.required && !String(arg.value || '').trim())
}

async function launch(app) {
  const missing = missingArgument(app)
  if (missing) {
    selectedApp.value = app.appId
    notify.error(`${app.displayName} needs ${missing.name}; set it in Settings.`)
    return
  }
  const args = (app.launchArguments || [])
    .filter((arg) => String(arg.value || '').trim())
    .map((arg) => ({ name: arg.name, value: arg.value }))
  const ok = await run(`launch:${app.appId}`, { launch: { appId: app.appId, arguments: args } },
    `Launch requested for ${app.displayName}`)
  if (ok) await refresh()
}

function isRunning(appId) {
  return (runsByApp.value.get(appId) || []).length > 0
}

function selectedRole(appId) {
  return !!selected.value[appId]
}

function hasLaunchArguments(app) {
  return (app.launchArguments || []).length > 0
}

// A required argument has no safe default; the plain Launch entry would just
// bounce the user to Settings, so only the argument form is offered.
function requiresLaunchArguments(app) {
  return (app.launchArguments || []).some((arg) => arg.required)
}

function showLifecycle(app) {
  if (isRunning(app.appId)) return !!app.canStop
  return true
}

function openLaunchArguments(app) {
  closeMenu()
  selectedApp.value = app.appId
}

// Framework stop restarts a selected-default app instead of leaving it down,
// which is exactly the Restart affordance the menu offers. Every live run is
// addressed by ID so a multi-instance app stops completely.
async function stopRuns(app) {
  const live = appRuns(app.appId)
  if (!live.length) return
  closeMenu()
  if (busy.value) return
  if (liveState.value !== 'open') {
    notify.error('The speaker is not connected.')
    return
  }
  const restart = selectedRole(app.appId)
  const key = `stop:${app.appId}`
  busy.value = key
  try {
    for (const runItem of live) {
      await socket.request({ stop: { appId: app.appId, runId: runItem.runId } })
    }
    notify.success(`${restart ? 'Restart' : 'Stop'} requested for ${app.displayName}`)
  } catch (err) {
    notify.error(err?.message || 'The device rejected the request.')
  } finally {
    if (busy.value === key) busy.value = ''
    // A partial stop is possible when several runs were addressed; the
    // snapshot, not the loop, is the authority on what is still running.
    await refresh()
  }
}

function onInstalled(result) {
  showInstallModal.value = false
  notify.success(installOutcome(result))
  refresh()
}

// --- uninstall confirmation -------------------------------------------------

const deleteTarget = ref(null)
const deleteStorage = ref(null)
const deleteStorageLoading = ref(false)
const deleteStorageError = ref('')
const deleteBusy = ref(false)
let deleteController = null

function canDelete(app) {
  return !!app.canUninstall && !app.systemApp
}

async function openDelete(app) {
  closeMenu()
  deleteTarget.value = app
  deleteStorage.value = null
  deleteStorageError.value = ''
  deleteStorageLoading.value = true
  deleteController?.abort()
  const controller = new AbortController()
  deleteController = controller
  try {
    const measured = await appPackageStorage(app.appId, { signal: controller.signal })
    if (deleteTarget.value?.appId === app.appId) deleteStorage.value = measured
  } catch (err) {
    if (err?.name === 'AbortError') return
    if (deleteTarget.value?.appId === app.appId) {
      deleteStorageError.value = err?.message || 'The package size could not be read.'
    }
  } finally {
    if (deleteTarget.value?.appId === app.appId) deleteStorageLoading.value = false
  }
}

function beforeDeleteClose() {
  return !deleteBusy.value
}

function closeDelete() {
  if (deleteBusy.value) return
  deleteController?.abort()
  deleteController = null
  deleteTarget.value = null
  deleteStorage.value = null
  deleteStorageError.value = ''
  deleteStorageLoading.value = false
}

async function confirmDelete() {
  const app = deleteTarget.value
  if (!app || deleteBusy.value) return
  deleteBusy.value = true
  try {
    await removePackage(app.appId)
    notify.success(`Removed ${app.displayName || app.appId}. Its settings were kept.`)
    deleteBusy.value = false
    closeDelete()
    await refresh()
  } catch (err) {
    notify.error(err?.message || 'The device rejected the removal.')
  } finally {
    deleteBusy.value = false
  }
}

// Declared manifest actions arrive with the overview listing; an app that
// declares none simply has no menu to offer.
function actionsOf(app) {
  return app.actions || []
}

function canRunAction(app, action) {
  if (appReady(app.appId)) return true
  // The shipped radio's Play is a public launch while no run exists. Every
  // other action needs a ready run; sending one earlier is rejected.
  return isColdPlay(action, app, appRuns(app.appId))
}

function actionHint(app, action) {
  if (appReady(app.appId)) return ''
  if (appLive(app.appId)) return 'The application is starting or stopping; try again when it is ready.'
  if (isColdPlay(action, app, appRuns(app.appId))) {
    return 'Not running: Play launches the radio with its saved station; it still exits when idle.'
  }
  return 'Launch the application before using this action.'
}

async function runAction(app, action) {
  closeMenu()
  const label = action.label || action.actionId
  if (!canRunAction(app, action)) {
    notify.info(actionHint(app, action))
    return
  }
  if (action.confirmBeforeRun && !window.confirm(`${label}?`)) return
  if (appReady(app.appId)) {
    await run(
      `action:${app.appId}:${action.actionId}`,
      { action: { appId: app.appId, actionId: action.actionId } },
      `${label} sent to ${app.displayName}`
    )
    return
  }
  await coldPlay(app, action, label)
}

// The overview listing has no app settings values, so a cold Play fetches the
// same application snapshot the detail page uses and seeds the radio's
// `station` launch argument from the saved setting only.
async function coldPlay(app, action, label) {
  if (busy.value) return
  if (liveState.value !== 'open') {
    notify.error('The speaker is not connected.')
    return
  }
  const key = `action:${app.appId}:${action.actionId}`
  busy.value = key
  try {
    const snapshot = await socket.requestSnapshot(Topic.TOPIC_APPLICATION, app.appId)
    const view = snapshot?.application
    if (appReady(app.appId)) {
      await socket.request({ action: { appId: app.appId, actionId: action.actionId } })
      notify.info(`${label} sent to ${app.displayName}`)
      await refresh()
      return
    }
    if (appLive(app.appId)) {
      notify.info(actionHint(app, action))
      return
    }
    const plan = coldPlayLaunch(
      action,
      app,
      appRuns(app.appId),
      view?.schema?.settings || [],
      view?.values || []
    )
    if (!plan) {
      notify.info('Launch the application before using this action.')
      return
    }
    if (plan.error) {
      notify.error(plan.error)
      return
    }
    await socket.request({ launch: { appId: app.appId, arguments: plan.arguments } })
    notify.success(`${label} requested`)
    await refresh()
  } catch (err) {
    notify.error(err?.message || 'The device rejected the request.')
  } finally {
    if (busy.value === key) busy.value = ''
  }
}

// A provider is either an application (addressed by app_id) or the framework's
// own Bluetooth source, which has no app id and is addressed by its kind. The
// selection is keyed on that identity so both shapes coexist.
function providerKey(provider) {
  return provider.appId ? `app:${provider.appId}` : `framework:${provider.kind}`
}

// One popover at a time: the media card shows its first provider, and the
// badge menu switches which provider it renders. The selection falls back to
// the first provider when the chosen one disappears.
const activeProviderKey = ref('')
const activeProvider = computed(() => {
  const list = providers.value
  if (!list.length) return null
  return list.find((provider) => providerKey(provider) === activeProviderKey.value) || list[0]
})
const otherProviders = computed(() =>
  providers.value.filter((provider) =>
    !activeProvider.value || providerKey(provider) !== providerKey(activeProvider.value)))

function selectProvider(provider) {
  activeProviderKey.value = providerKey(provider)
  closeMenu()
}

function playerCount(provider) {
  const count = (provider.players || []).length
  return `${count} player${count === 1 ? '' : 's'}`
}

// --- popover menus ---------------------------------------------------------

const openMenu = ref('')
let menuTrigger = null

function toggleMenu(key, event) {
  if (openMenu.value === key) {
    closeMenu()
    return
  }
  openMenu.value = key
  menuTrigger = event?.currentTarget || null
  // Move focus into the freshly rendered menu so it is usable from the
  // keyboard; Escape still returns focus to the trigger.
  nextTick(() => {
    menuTrigger?.parentElement?.querySelector('[role="menu"] button:not([disabled])')?.focus()
  })
}

function closeMenu(restoreFocus = false) {
  const trigger = menuTrigger
  openMenu.value = ''
  menuTrigger = null
  if (restoreFocus) trigger?.focus()
}

// A menu closes on any press outside its own trigger/menu wrapper. Presses on
// another trigger close this menu first, then the trigger's click opens that
// one.
function onMenuPointerDown(event) {
  const owner = event.target instanceof Element ? event.target.closest('[data-menu]') : null
  if (owner?.getAttribute('data-menu') === openMenu.value) return
  closeMenu()
}

function onMenuKeyDown(event) {
  if (event.key !== 'Escape' || !openMenu.value) return
  event.preventDefault()
  closeMenu(true)
}

watch(openMenu, (key) => {
  if (key) {
    document.addEventListener('pointerdown', onMenuPointerDown)
    document.addEventListener('keydown', onMenuKeyDown)
    return
  }
  document.removeEventListener('pointerdown', onMenuPointerDown)
  document.removeEventListener('keydown', onMenuKeyDown)
})

onUnmounted(() => {
  document.removeEventListener('pointerdown', onMenuPointerDown)
  document.removeEventListener('keydown', onMenuKeyDown)
})

const media = {
  play: MediaAction.MEDIA_ACTION_PLAY,
  pause: MediaAction.MEDIA_ACTION_PAUSE,
  next: MediaAction.MEDIA_ACTION_NEXT,
  previous: MediaAction.MEDIA_ACTION_PREVIOUS
}

function supports(player, action) {
  return (player.supportedActions || []).includes(action)
}

function playing(player) {
  return String(player.playbackState || '').includes('PLAYING')
}

function showPlay(player) {
  if (playing(player)) return supports(player, 'PAUSE') && !supports(player, 'PLAY')
  return supports(player, 'PLAY')
}

function showPause(player) {
  return playing(player) && supports(player, 'PAUSE')
}

async function mediaAction(provider, player, action) {
  const ok = await run(`media:${providerKey(provider)}:${player.playerId}:${action}`, {
    media: {
      kind: provider.kind, appId: provider.appId, playerId: player.playerId, action: media[action]
    }
  })
  // Media transitions are asynchronous on the provider; one explicit resync
  // shows the accepted state without polling.
  if (ok) await refresh()
}

function trackTime(ms) {
  const total = Math.max(0, Math.round((Number(ms) || 0) / 1000))
  const minutes = Math.floor(total / 60)
  const seconds = total % 60
  if (minutes >= 60) {
    return `${Math.floor(minutes / 60)}:${String(minutes % 60).padStart(2, '0')}:${String(seconds).padStart(2, '0')}`
  }
  return `${minutes}:${String(seconds).padStart(2, '0')}`
}
</script>

<template>
  <div class="flex flex-col gap-4">
    <div class="flex flex-wrap items-end justify-between gap-2">
      <h1 v-if="!data && !error && loading" class="text-xl font-semibold leading-tight my-2 text-primary flex items-center gap-4">
        <span class="loading loading-spinner loading-md"></span>
        Still loading…
      </h1>
      <h1 v-else class="text-xl font-semibold leading-tight my-2" :title="data?.device?.deviceId || 'Unknown device'">
        {{ deviceHeadline(data?.device) }}
      </h1>
    </div>

    <div v-if="!data && !error" class="grid gap-4 md:grid-cols-4" aria-busy="true">
      <div v-for="n in 4" :key="n" class="card border border-base-300 bg-base-100">
        <div class="card-body gap-3">
          <div class="skeleton h-3 w-1/3"></div>
          <div class="skeleton h-7 w-1/2"></div>
        </div>
      </div>
    </div>

    <div v-else-if="error && !data" class="alert alert-error" role="alert">
      <span class="text-sm">The speaker snapshot is unavailable: {{ error }}</span>
      <button type="button" class="btn btn-sm btn-ghost" @click="refresh">Try again</button>
    </div>

    <template v-else-if="data">
      <!-- Compact control bar: connectivity, media, volume and microphone. -->
      <section class="grid gap-4 sm:grid-cols-1 md:grid-cols-3">
        <!-- Audio settings -->
        <div class="card p-4 h-full flex min-w-0 flex-col gap-2 justify-between border border-base-300 bg-base-100">
          <div class="flex items-center gap-2">
            <Icon size="16px">volume_up</Icon>
            <span class="text-xs font-medium uppercase tracking-wide">Master volume</span>
          </div>
          <VolumeControl />
          <MicrophoneControl />
        </div>

        <!-- Now playing -->
        <div class="card p-4 h-full flex min-w-0 flex-col gap-2 justify-between border border-base-300 bg-base-100">
          <div class="flex items-center gap-2">
            <Icon size="16px">video_library</Icon>
            <span class="text-xs font-medium uppercase tracking-wide flex-grow">Now playing</span>
            <div v-if="otherProviders.length" class="relative" data-menu="providers">
              <button
                type="button"
                class="badge badge-primary badge-sm cursor-pointer"
                aria-haspopup="menu"
                :aria-expanded="openMenu === 'providers'"
                :title="`Show the other ${otherProviders.length} media provider${otherProviders.length === 1 ? '' : 's'}`"
                @click="toggleMenu('providers', $event)"
              >+{{ otherProviders.length }} more</button>
              <ul
                v-if="openMenu === 'providers'"
                role="menu"
                aria-label="Other media providers"
                class="absolute right-0 top-full z-30 mt-1 w-56 rounded-box border border-base-300 bg-base-100 p-1 text-sm shadow-lg"
              >
                <li v-for="provider in otherProviders" :key="providerKey(provider)" role="none">
                  <button
                    type="button"
                    role="menuitem"
                    class="flex w-full flex-col items-start gap-0.5 rounded px-2 py-1.5 text-left hover:bg-base-200"
                    @click="selectProvider(provider)"
                  >
                    <span class="max-w-full truncate font-medium">{{ provider.name || provider.appId || 'Media' }}</span>
                    <span class="text-[11px] text-base-content/60">
                      {{ playerCount(provider) }}
                    </span>
                  </button>
                </li>
              </ul>
            </div>
          </div>
          <p v-if="!providers.length" class="text-sm my-auto">Nothing is playing.</p>
          <div v-else class="flex min-w-0 flex-col gap-1">
            <p v-if="providers.length > 1" class="truncate text-[11px] text-base-content/60">
              {{ activeProvider.name || 'Media app' }}
            </p>
            <ul class="flex flex-col gap-2">
              <li v-for="player in activeProvider.players" :key="player.playerId" class="flex min-w-0 flex-col gap-1.5">
                <div class="flex min-w-0 items-start justify-between items-center gap-2">
                  <div class="min-w-0">
                    <p class="truncate text-sm font-medium">{{ player.title || player.mediaName || 'Untitled media' }}</p>
                    <p class="truncate text-xs text-base-content/60">
                      {{ [player.artist, player.album].filter(Boolean).join(' · ') || player.sourceName || player.playerId }}
                    </p>
                  </div>
                  <span class="text-[11px] tabular-nums">
                    {{ trackTime(player.positionMs) }} / {{ trackTime(player.durationMs) }}
                  </span>
                </div>
                <div class="flex flex-wrap gap-2 items-center">
                  <button
                    v-if="showPlay(player) && !showPause(player)"
                    type="button"
                    class="btn btn-primary btn-xs"
                    :disabled="!!busy || liveState !== 'open'"
                    @click="mediaAction(activeProvider, player, 'play')"
                  >
                    <Icon size="20px">play_arrow</Icon>
                  </button>
                  <button
                    v-if="showPause(player)"
                    type="button"
                    class="btn btn-primary btn-xs"
                    :disabled="!!busy || liveState !== 'open'"
                    @click="mediaAction(activeProvider, player, 'pause')"
                  >
                    <Icon size="20px">pause</Icon>
                  </button>
                  <progress
                    class="progress progress-primary h-1.5 flex-1"
                    :value="Number(player.positionMs) || 0"
                    :max="Number(player.durationMs)"
                  ></progress>
                  <button
                    type="button"
                    class="btn btn-ghost btn-xs"
                    :disabled="!!busy || liveState !== 'open' || !supports(player, 'SKIP_PREVIOUS')"
                    @click="mediaAction(activeProvider, player, 'previous')"
                  >
                    <Icon size="20px">skip_previous</Icon>
                  </button>
                  <button
                    type="button"
                    class="btn btn-ghost btn-xs"
                    :disabled="!!busy || liveState !== 'open' || !supports(player, 'SKIP_NEXT')"
                    @click="mediaAction(activeProvider, player, 'next')"
                  >
                    <Icon size="20px">skip_next</Icon>
                  </button>
                </div>
              </li>
            </ul>
          </div>
        </div>

        <!-- Bluetooth speaker -->
        <div class="card p-4 h-full flex min-w-0 flex-col gap-2 justify-between border border-base-300 bg-base-100">
          <div class="flex items-center gap-2">
            <Icon size="16px">bluetooth</Icon>
            <span class="text-xs font-medium uppercase tracking-wide flex-grow">Bluetooth speaker</span>
            <span
              v-if="bluetoothConnected"
              class="badge badge-success badge-sm shrink-0"
              role="status"
              :title="bluetoothConnected.title"
              :aria-label="bluetoothConnected.title"
            >{{ bluetoothConnected.label }}</span>
            <input
              type="checkbox"
              class="toggle toggle-primary toggle-sm"
              role="switch"
              :checked="incoming"
              :disabled="!bluetoothReady || busy === 'bt:incoming'"
              @change="setIncoming($event.target.checked)"
            />
          </div>
          <template v-if="bluetooth?.present">
            <p v-if="bluetooth.name" class="truncate text-sm">
              Device name: <code class="rounded bg-base-200 px-1 py-0.5 font-mono">{{ bluetooth.name }}</code>
            </p>
            <hr/>
            <label class="flex flex-col gap-1 text-sm" :title="!incoming ? 'Enable speaker mode first.' : ''">
              <span class="flex items-center justify-between gap-3">
                <span :class="{ 'text-base-content/60': !incoming }">Pairing mode</span>
                <input
                  type="checkbox"
                  class="toggle toggle-primary toggle-sm"
                  role="switch"
                  :checked="discoverable"
                  :disabled="!bluetoothReady || !incoming || busy === 'bt:discoverable'"
                  @change="setDiscoverable($event.target.checked)"
                />
              </span>
            </label>
            <p v-if="!bluetoothReady" class="text-xs text-warning">
              {{ liveState !== 'open' ? 'Disconnected: policy changes are paused.' : 'Turn Bluetooth on to change these policies.' }}
            </p>
            <p v-if="data.bluetoothError" class="text-xs text-error">{{ data.bluetoothError }}</p>
          </template>
          <p v-else class="text-xs text-base-content/60">{{ bluetooth?.detail || 'Bluetooth is not available.' }}</p>
        </div>
      </section>

      <!-- Installed applications list -->
      <section class="card border border-base-300 bg-base-100">
        <div class="card-body gap-3 p-4">
          <div class="flex items-center gap-2">
            <Icon size="20px">widgets</Icon>
            <h2 class="text-sm font-semibold uppercase tracking-wide">Installed applications</h2>
            <span class="badge badge-soft badge-primary badge-sm">{{ apps.length }}</span>

            <span class="flex-grow"/>

            <button class="btn btn-xs btn-primary" @click="showInstallModal = true">
              <Icon size="16px">system_update_alt</Icon>
              Install an app
            </button>
          </div>
          <div v-if="apps.length" class="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
            <article
              v-for="app in apps"
              :key="app.appId"
              class="flex min-w-0 flex-col gap-3 rounded-lg border border-base-300 p-3"
            >
              <div class="min-w-0 flex justify-between">
                <div>
                  <p class="truncate text-sm font-semibold" :title="app.appId">{{ app.displayName }}</p>
                  <p class="truncate text-xs text-base-content/60 data-value">{{ app.version || '—' }}</p>
                </div>

                <div class="flex flex-wrap gap-1.5">
                  <span v-if="app.systemApp" class="badge badge-outline badge-sm opacity-50">built-in</span>
                  <span v-if="selectedRole(app.appId)" class="badge badge-primary badge-sm">default</span>
                </div>
              </div>
              <div class="mt-auto flex flex-wrap items-center justify-between gap-2">
                <span v-if="isRunning(app.appId)" class="badge badge-success badge-md" role="status">Running</span>
                <button
                  v-else
                  type="button"
                  class="btn btn-primary btn-xs"
                  :disabled="!!busy || liveState !== 'open'"
                  @click="launch(app)"
                >
                  <Icon size="16px">play_arrow</Icon>
                  Launch
                </button>
                <div class="flex items-center gap-1">
                  <button
                    type="button"
                    class="btn btn-ghost btn-xs"
                    :aria-label="`Settings for ${app.displayName}`"
                    @click="selectedApp = app.appId"
                  >
                    <Icon size="16px">settings</Icon>
                    Settings
                  </button>
                  <div class="relative" :data-menu="`actions:${app.appId}`">
                    <button
                      type="button"
                      class="btn btn-ghost btn-xs"
                      aria-haspopup="menu"
                      :aria-expanded="openMenu === `actions:${app.appId}`"
                      :aria-label="`Actions for ${app.displayName}`"
                      :title="`Run a declared action for ${app.displayName}`"
                      @click="toggleMenu(`actions:${app.appId}`, $event)"
                    >
                      <Icon size="16px">more_vert</Icon>
                      Actions
                    </button>
                    <ul
                      v-if="openMenu === `actions:${app.appId}`"
                      role="menu"
                      :aria-label="`Actions for ${app.displayName}`"
                      class="absolute right-0 top-full z-30 mt-1 w-56 rounded-box border border-base-300 bg-base-100 p-1 text-sm shadow-lg"
                    >
                      <template v-if="isRunning(app.appId) && app.canStop">
                        <li>
                          <button
                            type="button"
                            role="menuitem"
                            class="flex w-full items-start gap-0.5 rounded px-2 py-1.5 text-left hover:bg-base-200 disabled:opacity-50 text-error"
                            :disabled="!!busy || liveState !== 'open'"
                            :title="selectedRole(app.appId) ? 'Stopping a selected default restarts it.' : 'Stop the application.'"
                            @click="stopRuns(app)"
                          >
                            <Icon size="20px" class="mr-1">{{ selectedRole(app.appId) ? 'refresh' : 'stop' }}</Icon>
                            {{ selectedRole(app.appId) ? 'Restart' : 'Stop' }}
                          </button>
                        </li>
                      </template>
                      <template v-else-if="!isRunning(app.appId)">
                        <li v-if="!requiresLaunchArguments(app)">
                          <button
                            type="button"
                            role="menuitem"
                            class="flex w-full items-start gap-0.5 rounded px-2 py-1.5 text-left hover:bg-base-200 disabled:opacity-50 text-primary"
                            :disabled="!!busy || liveState !== 'open'"
                            @click="launch(app)"
                          >
                            <Icon size="20px" class="mr-1">play_arrow</Icon>
                            Launch
                          </button>
                        </li>
                        <li v-if="hasLaunchArguments(app)">
                          <button
                            type="button"
                            role="menuitem"
                            class="flex w-full items-start gap-0.5 rounded px-2 py-1.5 text-left hover:bg-base-200 disabled:opacity-50 text-primary"
                            :disabled="!!busy || liveState !== 'open'"
                            title="Review the launch arguments in Settings before starting the app."
                            @click="openLaunchArguments(app)"
                          >
                            <Icon size="20px" class="mr-1">list</Icon>
                            Launch with arguments
                          </button>
                        </li>
                      </template>

                      <hr v-if="showLifecycle(app)" class="my-1"/>

                      <li v-for="action in actionsOf(app)" :key="action.actionId" role="none">
                        <button
                          type="button"
                          role="menuitem"
                          class="flex w-full flex-col items-start gap-0.5 rounded px-2 py-1.5 text-left hover:bg-base-200 disabled:opacity-50"
                          :disabled="!!busy || liveState !== 'open' || !canRunAction(app, action)"
                          :title="actionHint(app, action)"
                          @click="runAction(app, action)"
                        >
                          <span class="font-medium">{{ action.label || action.actionId }}</span>
                          <span v-if="action.description" class="text-[11px] font-normal text-base-content/60">
                            {{ action.description }}
                          </span>
                          <span v-if="actionHint(app, action)" class="text-[11px] font-normal text-base-content/60">
                            {{ actionHint(app, action) }}
                          </span>
                        </button>
                      </li>
                      <li v-if="!actionsOf(app).length" class="menu-disabled px-2 py-1.5 opacity-50 w-full">
                        This app doesn't declare any actions.
                      </li>

                      <template v-if="canDelete(app)">
                        <hr class="my-1"/>
                        <li>
                          <button
                            type="button"
                            role="menuitem"
                            class="flex w-full items-start gap-0.5 rounded px-2 py-1.5 text-left hover:bg-base-200 disabled:opacity-50 text-error"
                            :disabled="!!busy || isRunning(app.appId)"
                            :title="isRunning(app.appId) ? 'Stop the app before removing its package.' : 'Remove the package but keep its settings.'"
                            @click="openDelete(app)"
                          >
                            <Icon size="20px" class="mr-1">delete</Icon>
                            Delete this app
                          </button>
                        </li>
                      </template>
                    </ul>
                  </div>
                </div>
              </div>
            </article>
          </div>
          <p v-else class="text-sm text-base-content/60">No applications are installed.</p>
        </div>
      </section>

    </template>

    <ApplicationView
      v-if="selectedApp"
      :key="selectedApp"
      :app-id="selectedApp"
      @close="selectedApp = ''"
    />

    <AppInstallModal
      v-if="showInstallModal"
      @close="showInstallModal = false"
      @installed="onInstalled"
    />

    <ModalDialog
      v-if="deleteTarget"
      max-width="max-w-md"
      title="Delete this app"
      :before-close="beforeDeleteClose"
      @close="closeDelete"
    >
      <p class="text-sm">
        Remove <span class="font-semibold">{{ deleteTarget.displayName || deleteTarget.appId }}</span>
        (<span class="data-value">{{ deleteTarget.appId }}</span>) from the applications filesystem?
      </p>
      <p class="mt-2 text-sm">
        <template v-if="deleteStorageLoading">Measuring the package…</template>
        <template v-else-if="deleteStorage">
          The package uses {{ formatBytes(deleteStorage.sizeBytes) }}, so deleting it frees that much space.
        </template>
        <template v-else>The package size could not be read.</template>
      </p>
      <p class="mt-2 text-sm text-base-content/60">
        The app's settings are kept, so reinstalling it restores its configuration.
      </p>
      <p v-if="deleteStorageError" class="mt-2 text-sm text-warning">{{ deleteStorageError }}</p>
      <p v-else-if="deleteStorage?.available" class="mt-2 text-xs text-base-content/60">
        Free space now: {{ formatBytes(deleteStorage.freeBytes) }} of {{ formatBytes(deleteStorage.totalBytes) }}.
      </p>
      <template #footer>
        <button type="button" class="btn btn-ghost btn-sm" :disabled="deleteBusy" @click="closeDelete">
          Cancel
        </button>
        <button type="button" class="btn btn-error btn-sm" :disabled="deleteBusy" @click="confirmDelete">
          <span v-if="deleteBusy" class="loading loading-spinner loading-sm"></span>
          Delete
        </button>
      </template>
    </ModalDialog>
  </div>
</template>
