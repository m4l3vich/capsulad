<script setup>
import { computed, ref, watch } from 'vue'
import { MediaAction } from '../proto/web_pb.js'
import { liveState, useTopic } from '../lib/live.js'
import { socket } from '../lib/ws.js'
import { status } from '../lib/format.js'
import VolumeControl from '../components/VolumeControl.vue'
import MicrophoneControl from '../components/MicrophoneControl.vue'
import ApplicationView from './ApplicationView.vue'
import Icon from '../components/Icon.vue'

const { data, error, loading, refresh } = useTopic('overview')
// const data = ref(null)
// const error = ref(null)
// const loading = true
// const refresh = () => { }
const actionError = ref('')
const statusMessage = ref('')
const busy = ref('')
const selectedApp = ref('')
const discoverable = ref(false)
const incoming = ref(false)

const bluetooth = computed(() => data.value?.bluetooth || null)
const bluetoothReady = computed(() =>
  !!bluetooth.value?.present && !!bluetooth.value?.powered && liveState.value === 'open')
const apps = computed(() => data.value?.apps || [])
const runs = computed(() => data.value?.runs || [])
const providers = computed(() => data.value?.providers || [])
const selected = computed(() => data.value?.selectedProviders || {})

const runsByApp = computed(() => {
  const map = new Map()
  for (const run of runs.value) {
    const list = map.get(run.appId) || []
    list.push(run)
    map.set(run.appId, list)
  }
  return map
})

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
    actionError.value = 'The speaker is not connected.'
    return false
  }
  busy.value = key
  actionError.value = ''
  statusMessage.value = ''
  try {
    await socket.request(command)
    if (message) statusMessage.value = message
    return true
  } catch (err) {
    actionError.value = err?.message || 'The device rejected the request.'
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
    actionError.value = `${app.displayName} needs ${missing.name}; set it in Settings.`
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
  const ok = await run(`media:${provider.appId}:${player.playerId}:${action}`, {
    media: { appId: provider.appId, playerId: player.playerId, action: media[action] }
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
      <h1 v-if="!data && !error && loading" class="text-xl font-semibold leading-tight my-2 text-primary flex align-center gap-4">
        <span class="loading loading-spinner loading-md"></span>
        Still loading…
      </h1>
      <h1 v-else class="text-xl font-semibold leading-tight my-2" :title="data?.device?.deviceId || 'Unknown device'">
        {{ data?.device?.model || 'Unknown device' }}
      </h1>
    </div>

    <div v-if="actionError" class="alert alert-error py-2" role="alert">
      <span class="text-sm">{{ actionError }}</span>
      <button type="button" class="btn btn-sm btn-ghost" @click="actionError = ''">Dismiss</button>
    </div>
    <div v-else-if="statusMessage" class="alert alert-info py-2" role="status">
      <span class="text-sm">{{ statusMessage }}</span>
      <button type="button" class="btn btn-sm btn-ghost" @click="statusMessage = ''">Dismiss</button>
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
      <div v-if="error" class="alert alert-warning py-2" role="status">
        <span class="text-sm">The last refresh failed: {{ error }}</span>
        <button type="button" class="btn btn-sm btn-ghost" @click="refresh">Retry</button>
      </div>

      <!-- Compact control bar: connectivity, media, volume and microphone. -->
      <section class="grid gap-4 sm:grid-cols-1 md:grid-cols-3">
        <!-- Bluetooth speaker -->
        <div class="card p-4 h-full flex min-w-0 flex-col gap-2 justify-between border border-base-300 bg-base-100">
          <div class="flex items-center gap-2">
            <Icon size="16px">bluetooth</Icon>
            <span class="text-xs font-medium uppercase tracking-wide flex-grow">Bluetooth speaker</span>
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

        <!-- Now playing -->
        <div class="card p-4 h-full flex min-w-0 flex-col gap-2 justify-between border border-base-300 bg-base-100">
          <div class="flex items-center justify-between gap-2">
            <span class="text-xs font-medium uppercase tracking-wide text-base-content/60">Now playing</span>
          </div>
          <p v-if="!providers.length" class="text-sm my-auto">Nothing is playing.</p>
          <div v-else class="flex min-w-0 flex-col gap-2">
            <div v-for="provider in providers" :key="provider.appId" class="flex min-w-0 flex-col gap-1">
              <p v-if="providers.length > 1" class="truncate text-[11px] text-base-content/60">
                {{ provider.name || 'Media app' }}
              </p>
              <p v-if="provider.error" class="text-xs text-error">{{ provider.error }}</p>
              <p v-else-if="!(provider.players || []).length" class="text-xs text-base-content/60">
                No active player.
              </p>
              <ul v-else class="flex flex-col gap-2">
                <li v-for="player in provider.players" :key="player.playerId" class="flex min-w-0 flex-col gap-1.5">
                  <div class="flex min-w-0 items-start justify-between gap-2">
                    <div class="min-w-0">
                      <p class="truncate text-sm font-medium">{{ player.title || player.mediaName || 'Untitled media' }}</p>
                      <p class="truncate text-xs text-base-content/60">
                        {{ [player.artist, player.album].filter(Boolean).join(' · ') || player.sourceName || player.playerId }}
                      </p>
                    </div>
                    <span class="badge badge-sm shrink-0" :class="playing(player) ? 'badge-success' : 'badge-ghost'">
                      {{ status(player.playbackState) || 'idle' }}
                    </span>
                  </div>
                  <div v-if="Number(player.durationMs) > 0" class="flex items-center gap-2">
                    <progress
                      class="progress progress-primary h-1.5 flex-1"
                      :value="Number(player.positionMs) || 0"
                      :max="Number(player.durationMs)"
                    ></progress>
                    <span class="text-[11px] tabular-nums text-base-content/60">
                      {{ trackTime(player.positionMs) }} / {{ trackTime(player.durationMs) }}
                    </span>
                  </div>
                  <div class="flex flex-wrap gap-1">
                    <button
                      v-if="supports(player, 'SKIP_PREVIOUS')"
                      type="button"
                      class="btn btn-ghost btn-xs"
                      :disabled="!!busy || liveState !== 'open'"
                      @click="mediaAction(provider, player, 'previous')"
                    >Previous</button>
                    <button
                      v-if="showPlay(player) && !showPause(player)"
                      type="button"
                      class="btn btn-primary btn-xs"
                      :disabled="!!busy || liveState !== 'open'"
                      @click="mediaAction(provider, player, 'play')"
                    >Play</button>
                    <button
                      v-if="showPause(player)"
                      type="button"
                      class="btn btn-primary btn-xs"
                      :disabled="!!busy || liveState !== 'open'"
                      @click="mediaAction(provider, player, 'pause')"
                    >Pause</button>
                    <button
                      v-if="supports(player, 'SKIP_NEXT')"
                      type="button"
                      class="btn btn-ghost btn-xs"
                      :disabled="!!busy || liveState !== 'open'"
                      @click="mediaAction(provider, player, 'next')"
                    >Next</button>
                  </div>
                </li>
              </ul>
            </div>
          </div>
        </div>

        <!-- Audio settings -->
        <div class="card p-4 h-full flex min-w-0 flex-col gap-2 justify-between border border-base-300 bg-base-100">
          <div class="flex items-center gap-2">
            <Icon size="16px">volume_up</Icon>
            <span class="text-xs font-medium uppercase tracking-wide">Master volume</span>
          </div>
          <VolumeControl />
          <MicrophoneControl />
        </div>
      </section>

      <!-- Installed applications: Launch stays here, Settings opens the modal. -->
      <section class="card border border-base-300 bg-base-100">
        <div class="card-body gap-2 p-4">
          <div class="flex items-center justify-between gap-2">
            <h2 class="text-sm font-semibold uppercase tracking-wide text-base-content/60">Installed applications</h2>
            <span class="badge badge-ghost badge-sm">{{ apps.length }}</span>
          </div>
          <div v-if="apps.length" class="overflow-x-auto">
            <table class="table table-sm">
              <thead>
                <tr>
                  <th scope="col">Application</th>
                  <th scope="col" class="hidden sm:table-cell">Version</th>
                  <th scope="col" class="hidden md:table-cell">State</th>
                  <th scope="col" class="text-right">Actions</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="app in apps" :key="app.appId" class="align-top">
                  <td>
                    <div class="flex flex-wrap items-center gap-1.5">
                      <button
                        type="button"
                        class="link link-hover text-left font-medium"
                        :title="app.appId"
                        @click="selectedApp = app.appId"
                      >{{ app.displayName }}</button>
                      <span v-if="app.systemApp" class="badge badge-outline badge-xs">built-in</span>
                      <span v-if="selectedRole(app.appId)" class="badge badge-primary badge-xs">default</span>
                    </div>
                  </td>
                  <td class="hidden sm:table-cell data-value">{{ app.version || '—' }}</td>
                  <td class="hidden md:table-cell">
                    <span v-if="isRunning(app.appId)" class="badge badge-success badge-sm">Running</span>
                    <span v-else class="text-xs text-base-content/60">Stopped</span>
                  </td>
                  <td>
                    <div class="flex justify-end gap-1">
                      <button
                        type="button"
                        class="btn btn-primary btn-xs"
                        :disabled="!!busy || liveState !== 'open'"
                        @click="launch(app)"
                      >Launch</button>
                      <button type="button" class="btn btn-ghost btn-xs" @click="selectedApp = app.appId">Settings</button>
                    </div>
                  </td>
                </tr>
              </tbody>
            </table>
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
  </div>
</template>
