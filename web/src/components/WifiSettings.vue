<script setup>
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import ModalDialog from './ModalDialog.vue'
import { socket } from '../lib/ws.js'
import { connectWifi, fetchWifi, forgetWifi, scanWifi, subscribeWifi } from '../lib/wifi.js'

// Wi-Fi is an explicit surface: the snapshot never scans, joining is a user
// action, and the network that is carrying this session cannot be forgotten
// from the console. The setup access point is described with the exact
// credentials printed on the device story: the live adapter name and the
// password "capsulad".

const view = ref(null)
const loadError = ref('')
const actionError = ref('')
const busy = ref('')
const scans = ref([])
const scanned = ref(false)
const manageOpen = ref(false)
const forgetting = ref('')
const connect = reactive({ ssid: '', security: 'wpa2', passphrase: '' })

let off = null
let offReconnect = null
let latestRevision = 0

const data = computed(() => view.value)

const known = computed(() => view.value?.knownNetworks || [])
const currentSsid = computed(() => view.value?.ssid || '')
const fallbackPassword = computed(() => view.value?.apPasswordHint || 'capsulad')

function stateLabel(state) {
  return {
    unavailable: 'No radio',
    disconnected: 'Not connected',
    connecting: 'Connecting',
    connected: 'Connected',
    ap_fallback: 'Setup access point',
    error: 'Error'
  }[state] || 'Unknown'
}

function stateBadge(state) {
  return {
    connected: 'badge-success',
    connecting: 'badge-warning',
    ap_fallback: 'badge-warning',
    error: 'badge-error',
    unavailable: 'badge-ghost'
  }[state] || 'badge-ghost'
}

function signalLabel(dbm) {
  const value = Number(dbm)
  if (!Number.isFinite(value) || value === 0) return '—'
  if (value >= -55) return 'strong'
  if (value >= -70) return 'fair'
  return 'weak'
}

function securityLabel(security) {
  const value = String(security || '').toLowerCase()
  if (!value || value === 'open') return 'Open'
  if (value === 'wpa3') return 'WPA3'
  if (value === 'wpa2_wpa3') return 'WPA2/WPA3'
  if (value === 'wpa') return 'WPA'
  return 'WPA2'
}

function isProtected(network) {
  if (!network) return true
  return Boolean(network.connected) || network.ssid === currentSsid.value
}

function adopt(next) {
  if (!next) return
  const revision = Number(next.revision) || 0
  if (revision < latestRevision) return
  latestRevision = revision
  view.value = next
  loadError.value = ''
}

async function refresh() {
  try {
    adopt(await fetchWifi())
  } catch (err) {
    if (!view.value) loadError.value = err.message
    else actionError.value = err.message
  }
}

// The radio is scanned only from this handler; snapshots and pushes never
// trigger one.
async function scan() {
  if (busy.value) return
  busy.value = 'scan'
  actionError.value = ''
  try {
    const next = await scanWifi()
    adopt(next)
    scans.value = next?.scanResults || []
    scanned.value = true
  } catch (err) {
    actionError.value = err.message
    await refresh()
  } finally {
    busy.value = ''
  }
}

function useScan(network) {
  connect.ssid = network.ssid || ''
  const security = String(network.security || '').toLowerCase()
  connect.security = ['open', 'wpa', 'wpa3', 'wpa2_wpa3'].includes(security) ? security : 'wpa2'
  connect.passphrase = ''
}

function validateConnect() {
  const ssid = connect.ssid.trim()
  if (!ssid) return 'Enter the network name, including for a hidden network.'
  if (ssid.length > 32) return 'A network name is at most 32 characters.'
  if (connect.security !== 'open') {
    const passphrase = connect.passphrase
    if (!passphrase) return 'Enter the Wi-Fi password for this network.'
    if (passphrase.length < 8 || passphrase.length > 63) {
      return 'A WPA password must be 8 to 63 characters.'
    }
  }
  return ''
}

async function submitConnect() {
  if (busy.value) return
  const problem = validateConnect()
  if (problem) {
    actionError.value = problem
    return
  }
  busy.value = 'connect'
  actionError.value = ''
  const ssid = connect.ssid.trim()
  const passphrase = connect.security === 'open' ? '' : connect.passphrase
  try {
    const next = await connectWifi({ ssid, passphrase, security: connect.security })
    adopt(next)
  } catch (err) {
    actionError.value = err.message
    await refresh()
  } finally {
    // Never keep a submitted passphrase in component state.
    connect.passphrase = ''
    busy.value = ''
  }
}

function openManage() {
  forgetting.value = ''
  manageOpen.value = true
}

function closeManage() {
  forgetting.value = ''
  manageOpen.value = false
}

async function forget(network) {
  if (!network || busy.value || isProtected(network)) return
  if (forgetting.value !== network.ssid) {
    forgetting.value = network.ssid
    return
  }
  busy.value = 'forget'
  actionError.value = ''
  try {
    const next = await forgetWifi(network.ssid)
    adopt(next)
    if (connect.ssid === network.ssid) connect.ssid = ''
    scans.value = scans.value.filter((entry) => entry.ssid !== network.ssid)
    forgetting.value = ''
  } catch (err) {
    actionError.value = err.message
    await refresh()
  } finally {
    busy.value = ''
  }
}

function handleReconnect() {
  // A restarted daemon restarts the Wi-Fi snapshot revision counter, so the
  // previous high-water mark would reject every new snapshot/event. Reset the
  // guard before reading the new manager state.
  latestRevision = 0
  refresh()
}

onMounted(() => {
  // Subscribe first so an event emitted while the snapshot is in flight is not
  // lost; the revision guard drops the older of the two.
  off = subscribeWifi({
    onSnapshot: (snap) => adopt(snap.wifi),
    onEvent: (event) => adopt(event.wifi),
    onResync: refresh
  })
  refresh()
  offReconnect = socket.on('reconnect', handleReconnect)
})

onUnmounted(() => {
  connect.passphrase = ''
  manageOpen.value = false
  off?.()
  offReconnect?.()
})
</script>

<template>
  <section id="wifi" class="card scroll-mt-24 bg-base-100 shadow-sm">
    <div class="card-body gap-4">
      <div class="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 class="card-title text-lg">Wi-Fi</h2>
          <p class="text-sm opacity-70">
            The speaker tries saved networks in priority order. Scans run only when you press Scan.
          </p>
        </div>
        <div class="flex flex-wrap items-center gap-2">
          <span v-if="data" class="badge" :class="stateBadge(data.state)">{{ stateLabel(data.state) }}</span>
          <button
            type="button"
            class="btn btn-sm btn-outline"
            :disabled="!known.length"
            @click="openManage"
          >
            Manage networks
          </button>
        </div>
      </div>

      <div v-if="!data && !loadError" class="space-y-2">
        <div class="skeleton h-4 w-40"></div>
        <div class="skeleton h-16 w-full"></div>
      </div>

      <div v-else-if="loadError && !data" class="alert alert-error" role="alert">
        <span>Wi-Fi state unavailable: {{ loadError }}</span>
        <button type="button" class="btn btn-sm btn-ghost" @click="refresh">Try again</button>
      </div>

      <template v-else-if="data">
        <div v-if="actionError" class="alert alert-error" role="alert">{{ actionError }}</div>

        <div v-if="!data.available" class="alert alert-info" role="status">
          <div class="space-y-1">
            <p><strong>Wi-Fi is unavailable on this speaker.</strong></p>
            <p class="text-sm">Check that Wi-Fi is enabled on the speaker, then try again.</p>
            <details class="text-xs opacity-70">
              <summary class="cursor-pointer">Technical details</summary>
              <p class="mt-1">{{ data.detail || 'No usable Wi-Fi interface was reported.' }}</p>
            </details>
          </div>
        </div>

        <div v-else-if="data.apActive" class="alert alert-warning" role="status">
          <div class="space-y-1">
            <p>
              <strong>Setup access point active.</strong>
              The speaker could not join a saved network, so it is broadcasting
              <span class="font-mono">{{ data.apSsid || 'its device name' }}</span>
              with password <code class="font-mono">{{ fallbackPassword }}</code>.
            </p>
            <p class="text-sm">
              Join that network, then open
              <a v-if="data.apPortalUrl" :href="data.apPortalUrl" target="_blank" rel="noopener" class="link">{{ data.apPortalUrl }}</a>
              <span v-else>http://192.168.4.1</span>
              to pick a network. The portal can only scan and connect.
            </p>
          </div>
        </div>

        <div class="grid gap-3 sm:grid-cols-2">
          <div class="rounded-box bg-base-200 px-3 py-2">
            <p class="text-xs uppercase tracking-wide opacity-60">Network</p>
            <p class="truncate font-medium">{{ data.ssid || '—' }}</p>
          </div>
          <div class="rounded-box bg-base-200 px-3 py-2">
            <p class="text-xs uppercase tracking-wide opacity-60">Address</p>
            <p class="truncate font-mono text-sm">{{ data.ipAddress || '—' }}</p>
          </div>
        </div>

        <details class="text-xs opacity-70">
          <summary class="cursor-pointer">Technical details</summary>
          <p class="mt-1">Interface: <span class="font-mono">{{ data.interface || '—' }}</span></p>
          <p v-if="data.detail" class="mt-1">{{ data.detail }}</p>
        </details>

        <p v-if="data.lastError" class="text-sm text-error">Last attempt: {{ data.lastError }}</p>
        <p class="text-xs opacity-60">
          The connected network cannot be forgotten or disconnected from this console; the session runs over it.
        </p>

        <div class="divider my-0"></div>

        <div v-if="data.available">
          <div class="mb-2 flex flex-wrap items-center justify-between gap-2">
            <h3 class="font-medium">Nearby networks</h3>
            <button
              type="button"
              class="btn btn-sm btn-primary"
              :disabled="busy || !data.scanSupported"
              :aria-busy="busy === 'scan' ? 'true' : 'false'"
              @click="scan"
            >
              <span v-if="busy === 'scan'" class="loading loading-spinner loading-xs" aria-hidden="true"></span>
              {{ busy === 'scan' ? 'Scanning…' : 'Scan' }}
            </button>
          </div>

          <p v-if="!data.scanSupported" class="text-sm opacity-70">
            This adapter cannot scan; connect by typing the network name below.
          </p>
          <p v-else-if="!scanned" class="text-sm opacity-70">
            No scan yet. Press Scan to list nearby networks.
          </p>
          <p v-else-if="!scans.length" class="text-sm opacity-70">
            No networks found. Move closer to the router, or type a hidden network's name below.
          </p>

          <ul v-else class="max-h-64 space-y-1 overflow-y-auto" aria-label="Scan results">
            <li
              v-for="network in scans"
              :key="network.ssid + network.security + network.frequencyMhz"
              class="flex items-center justify-between gap-3 rounded-box border border-base-300 px-3 py-2"
            >
              <div class="min-w-0">
                <p class="truncate text-sm font-medium">
                  {{ network.ssid }}
                  <span v-if="network.ssid === currentSsid" class="badge badge-success badge-sm ml-1">current</span>
                </p>
                <p class="text-xs opacity-60">
                  {{ securityLabel(network.security) }} · {{ signalLabel(network.signalDbm) }}
                  <span v-if="network.frequencyMhz"> · {{ network.frequencyMhz }} MHz</span>
                </p>
              </div>
              <button type="button" class="btn btn-xs btn-ghost" @click="useScan(network)">Use</button>
            </li>
          </ul>
        </div>

        <form v-if="data.available" class="space-y-3" novalidate @submit.prevent="submitConnect">
          <h3 class="font-medium">Join a network</h3>
          <div class="grid gap-3 sm:grid-cols-2">
            <label class="form-control w-full">
              <span class="label-text mb-1">Network name</span>
              <input
                v-model="connect.ssid"
                type="text"
                class="input input-bordered w-full"
                maxlength="32"
                autocomplete="off"
                spellcheck="false"
                placeholder="Type a name, including hidden networks"
              />
            </label>
            <label class="form-control w-full">
              <span class="label-text mb-1">Security</span>
              <select v-model="connect.security" class="select select-bordered w-full">
                <option value="wpa2">WPA2</option>
                <option value="wpa2_wpa3">WPA2/WPA3</option>
                <option value="wpa3">WPA3</option>
                <option value="open">Open (no password)</option>
              </select>
            </label>
          </div>
          <label v-if="connect.security !== 'open'" class="form-control w-full">
            <span class="label-text mb-1">Password</span>
            <input
              v-model="connect.passphrase"
              type="password"
              class="input input-bordered w-full"
              autocomplete="new-password"
              maxlength="63"
              placeholder="8 to 63 characters"
            />
          </label>
          <div class="flex flex-wrap items-center gap-2">
            <button
              type="submit"
              class="btn btn-primary"
              :disabled="busy || !connect.ssid.trim()"
              :aria-busy="busy === 'connect' ? 'true' : 'false'"
            >
              <span v-if="busy === 'connect'" class="loading loading-spinner loading-xs" aria-hidden="true"></span>
              {{ busy === 'connect' ? 'Connecting…' : 'Connect' }}
            </button>
            <span class="text-xs opacity-60">The speaker leaves its current network only after the new one associates.</span>
          </div>
        </form>
      </template>
    </div>

    <ModalDialog v-if="manageOpen" title="Saved networks" @close="closeManage">
      <p class="mb-3 text-sm opacity-70">
        The active network stays protected while the console is connected; forget other saved networks here.
      </p>
      <p v-if="!known.length" class="text-sm opacity-70">No saved networks yet.</p>
      <ul v-else class="max-h-72 space-y-1 overflow-y-auto">
        <li
          v-for="network in known"
          :key="network.ssid"
          class="flex flex-wrap items-center justify-between gap-2 rounded-box border border-base-300 px-3 py-2"
        >
          <div class="min-w-0">
            <p class="truncate text-sm font-medium">
              {{ network.ssid }}
              <span v-if="isProtected(network)" class="badge badge-success badge-sm ml-1">active</span>
            </p>
            <p class="text-xs opacity-60">
              {{ securityLabel(network.security) }}<span v-if="!network.hasPassword"> · password not stored</span>
            </p>
          </div>
          <div class="flex items-center gap-1">
            <template v-if="forgetting === network.ssid">
              <button type="button" class="btn btn-xs btn-error" :disabled="busy === 'forget'" @click="forget(network)">
                Confirm forget
              </button>
              <button type="button" class="btn btn-xs btn-ghost" @click="forgetting = ''">Cancel</button>
            </template>
            <button
              v-else
              type="button"
              class="btn btn-xs btn-outline"
              :disabled="isProtected(network) || busy === 'forget'"
              :title="isProtected(network) ? 'The current network cannot be forgotten here' : `Forget ${network.ssid}`"
              @click="forget(network)"
            >
              Forget
            </button>
          </div>
        </li>
      </ul>

      <template #footer>
        <button type="button" class="btn btn-ghost" @click="closeManage">Close</button>
      </template>
    </ModalDialog>
  </section>
</template>
