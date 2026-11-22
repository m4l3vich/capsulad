<script setup>
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import ModalDialog from './ModalDialog.vue'
import Icon from './Icon.vue'
import { socket } from '../lib/ws.js'
import { connectWifi, fetchWifi, forgetWifi, scanWifi, subscribeWifi } from '../lib/wifi.js'
import { notify } from '../lib/toast.js'
import { WIFI_SIGNAL_ICONS } from '../lib/iconNames.js'

// Wi-Fi is an explicit surface: the snapshot never scans, joining is a user
// action, and the network that is carrying this session cannot be forgotten
// from the console. The setup access point is described with the exact
// credentials printed on the device story: the live adapter name and the
// password "capsulad".

const view = ref(null)
const { t } = useI18n()
const loadError = ref('')
const busy = ref('')
const scans = ref([])
const scanned = ref(false)
const manageOpen = ref(false)
const forgetting = ref('')
const joinOpen = ref(false)
const joinTarget = ref('')
const connect = reactive({ ssid: '', security: 'wpa2', passphrase: '' })

let off = null
let offReconnect = null
let latestRevision = 0

const data = computed(() => view.value)

const known = computed(() => view.value?.knownNetworks || [])
const currentSsid = computed(() => view.value?.ssid || '')
const fallbackPassword = computed(() => view.value?.apPasswordHint || 'capsulad')
const connectedSsid = computed(() => (view.value?.state === 'connected' ? view.value.ssid || '' : ''))
const joinTitle = computed(() =>
  joinTarget.value ? t('wifi.connectTo', { ssid: joinTarget.value }) : t('wifi.connectHidden')
)

function stateLabel(state) {
  return {
    unavailable: t('wifi.stateNoRadio'),
    disconnected: t('wifi.stateNotConnected'),
    connecting: t('wifi.stateConnecting'),
    connected: t('wifi.stateConnected'),
    ap_fallback: t('wifi.stateApFallback'),
    error: t('wifi.stateError')
  }[state] || t('wifi.stateUnknown')
}

function signalLevel(dbm) {
  const value = Number(dbm)
  if (!Number.isFinite(value) || value === 0) return { icon: WIFI_SIGNAL_ICONS.unknown, label: t('wifi.signalUnknown') }
  if (value >= -55) return { icon: WIFI_SIGNAL_ICONS.strong, label: t('wifi.signalStrong') }
  if (value >= -70) return { icon: WIFI_SIGNAL_ICONS.fair, label: t('wifi.signalFair') }
  if (value >= -80) return { icon: WIFI_SIGNAL_ICONS.weak, label: t('wifi.signalWeak') }
  return { icon: WIFI_SIGNAL_ICONS.veryWeak, label: t('wifi.signalVeryWeak') }
}

function securityLabel(security) {
  const value = String(security || '').toLowerCase()
  if (!value || value === 'open') return t('wifi.openSecurity')
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
    else notify.warning(err.message)
  }
}

// The radio is scanned only from this handler; snapshots and pushes never
// trigger one.
async function scan() {
  if (busy.value) return
  busy.value = 'scan'
  try {
    const next = await scanWifi()
    adopt(next)
    scans.value = next?.scanResults || []
    scanned.value = true
  } catch (err) {
    notify.error(err.message)
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

// The only join surface: a dialog opened from a scan result or the explicit
// hidden-network entry. There is no always-visible form.
function openJoin(network = null) {
  if (network) useScan(network)
  else {
    connect.ssid = ''
    connect.security = 'wpa2'
    connect.passphrase = ''
  }
  joinTarget.value = network?.ssid || ''
  joinOpen.value = true
}

function closeJoin() {
  joinOpen.value = false
  joinTarget.value = ''
  connect.passphrase = ''
}

function validateConnect() {
  const ssid = connect.ssid.trim()
  if (!ssid) return t('wifi.enterNetworkName')
  if (ssid.length > 32) return t('wifi.nameTooLong')
  if (connect.security !== 'open') {
    const passphrase = connect.passphrase
    if (!passphrase) return t('wifi.passwordRequired')
    if (passphrase.length < 8 || passphrase.length > 63) {
      return t('wifi.passwordLength')
    }
  }
  return ''
}

async function submitConnect() {
  if (busy.value) return
  const problem = validateConnect()
  if (problem) {
    notify.error(problem)
    return
  }
  busy.value = 'connect'
  const ssid = connect.ssid.trim()
  const passphrase = connect.security === 'open' ? '' : connect.passphrase
  try {
    const next = await connectWifi({ ssid, passphrase, security: connect.security })
    adopt(next)
    if (joinOpen.value) closeJoin()
  } catch (err) {
    // The dialog stays open so the entered network can be corrected.
    notify.error(err.message)
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
  try {
    const next = await forgetWifi(network.ssid)
    adopt(next)
    if (connect.ssid === network.ssid) connect.ssid = ''
    scans.value = scans.value.filter((entry) => entry.ssid !== network.ssid)
    forgetting.value = ''
  } catch (err) {
    notify.error(err.message)
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
  joinOpen.value = false
  off?.()
  offReconnect?.()
})
</script>

<template>
  <section id="wifi" class="card scroll-mt-24 rounded-box bg-base-100">
    <div class="card-body gap-3 p-4">
      <div class="flex flex-wrap items-center gap-2">
        <Icon size="24px">wifi</Icon>
        <h2 class="flex-grow text-lg font-semibold">{{ $t('wifi.title') }}</h2>
        <button
          type="button"
          class="btn btn-sm btn-ghost"
          :disabled="!known.length"
          @click="openManage"
        >
          {{ $t('wifi.manage') }}
        </button>
      </div>

      <div v-if="!data && !loadError" class="space-y-2">
        <div class="skeleton h-4 w-40"></div>
        <div class="skeleton h-16 w-full"></div>
      </div>

      <div v-else-if="loadError && !data" class="alert alert-error py-2" role="alert">
        <span class="text-sm">{{ $t('wifi.unavailableState', { error: loadError }) }}</span>
        <button type="button" class="btn btn-sm btn-ghost" @click="refresh">{{ $t('common.retry') }}</button>
      </div>

      <template v-else-if="data">
        <div v-if="!data.available" class="flex flex-col gap-1 rounded-box bg-base-200/60 p-3 border-2 border-neutral-300" role="status">
          <p class="text-sm font-medium">{{ $t('wifi.unavailable') }}</p>
          <p class="text-xs text-base-content/60">{{ $t('wifi.unavailableHint') }}</p>
        </div>

        <div v-else-if="data.apActive" class="flex flex-col gap-1 rounded-box bg-orange-100/60 p-3 border-orange-500 border" role="status">
          <p class="text-md font-medium">{{ $t('wifi.apActive') }}</p>
          <p class="text-sm text-base-content/60">
            {{ $t('wifi.apActiveBody') }}
          </p>
        </div>

        <div v-else class="flex flex-col gap-1 rounded-box bg-base-200/60 p-3 border-2 border-neutral-300" role="status">
          <details>
            <summary class="text-sm font-medium cursor-pointer">
              <Icon size="16px" class="align-text-top mr-1">wifi_off</Icon>
              {{ $t('wifi.fallback') }}
            </summary>
            <p class="text-sm mt-2">
              {{ $t('wifi.fallbackBody') }}
            </p>

            <dl class="grid grid-cols-2 justify-self-start gap-1 my-3">
              <dt class="text-base-content/60">{{ $t('wifi.networkName') }}</dt>
              <dd>
                <code class="rounded bg-base-300 px-1 py-0.5 font-mono block">{{ data.apSsid }}</code>
              </dd>

              <dt class="text-base-content/60">{{ $t('wifi.password') }}</dt>
              <dd>
                <code class="rounded bg-base-300 px-1 py-0.5 font-mono block">{{ fallbackPassword }}</code>
              </dd>
            </dl>

            <p class="text-sm">
              {{ $t('wifi.joinPortal') }}
              <code class="rounded bg-base-300 px-1 py-0.5 font-mono">{{ data.apPortalUrl || 'http://192.168.4.1' }}</code>.
            </p>
          </details>
        </div>

        <template v-if="data.available">
          <div class="flex items-center gap-3 rounded-box bg-base-200/60 px-3 py-2.5" role="status">
            <Icon size="22px" class="shrink-0 text-base-content/70">
              {{ connectedSsid ? 'wifi' : 'wifi_off' }}
            </Icon>
            <div class="min-w-0 flex-1">
              <p class="truncate text-sm font-medium">{{ connectedSsid || stateLabel(data.state) }}</p>
              <p v-if="connectedSsid" class="truncate font-mono text-xs text-base-content/60">
                {{ data.ipAddress || $t('common.noAddressYet') }}
              </p>
            </div>
            <span
              v-if="data.state === 'connecting'"
              class="loading loading-spinner loading-xs"
              aria-hidden="true"
            ></span>
          </div>

          <p v-if="data.lastError" class="text-xs text-error">{{ $t('wifi.lastAttempt', { error: data.lastError }) }}</p>
        </template>

        <div v-if="data.available">
          <div class="mb-2 flex flex-wrap items-center justify-between gap-2">
            <h3 class="text-sm font-semibold">{{ $t('wifi.nearby') }}</h3>
            <button
              type="button"
              class="btn btn-sm btn-primary"
              :disabled="busy || !data.scanSupported"
              :aria-busy="busy === 'scan' ? 'true' : 'false'"
              @click="scan"
            >
              <span v-if="busy === 'scan'" class="loading loading-spinner loading-xs" aria-hidden="true"></span>
              {{ busy === 'scan' ? $t('wifi.scanning') : $t('wifi.scan') }}
            </button>
          </div>

          <p v-if="!data.scanSupported" class="text-sm text-base-content/60">
            {{ $t('wifi.cannotScan') }}
          </p>
          <p v-else-if="!scanned" class="text-sm text-base-content/60">
            {{ $t('wifi.noScanYet') }}
          </p>
          <p v-else-if="!scans.length" class="text-sm text-base-content/60">
            {{ $t('wifi.noNetworks') }}
          </p>

          <ul v-else class="max-h-64 space-y-1 overflow-y-auto" :aria-label="$t('wifi.scanResults')">
            <li
              v-for="network in scans"
              :key="network.ssid + network.security + network.frequencyMhz"
              class="flex items-center justify-between gap-3 rounded-box bg-base-200/60 px-3 py-2"
            >
              <div class="min-w-0">
                <p class="truncate text-sm font-medium">
                  {{ network.ssid }}
                  <span v-if="network.ssid === currentSsid" class="badge badge-success badge-sm ml-1">{{ $t('common.current') }}</span>
                </p>
                <p class="flex flex-wrap items-center gap-1 text-xs text-base-content/60">
                  <span
                    class="inline-flex items-center"
                    role="img"
                    :aria-label="signalLevel(network.signalDbm).label"
                    :title="signalLevel(network.signalDbm).label"
                  >
                    <Icon size="16px" aria-hidden="true">{{ signalLevel(network.signalDbm).icon }}</Icon>
                  </span>
                  <span>{{ securityLabel(network.security) }}</span>
                </p>
              </div>
              <button type="button" class="btn btn-xs btn-ghost" @click="openJoin(network)">{{ $t('wifi.connect') }}</button>
            </li>
          </ul>

          <button
            v-if="scanned || !data.scanSupported"
            type="button"
            class="btn btn-sm btn-outline mt-2 w-full"
            @click="openJoin()"
          >
            <Icon size="16px" aria-hidden="true">visibility_off</Icon>
            {{ $t('wifi.hidden') }}
          </button>
        </div>
      </template>
    </div>

    <ModalDialog v-if="manageOpen" :title="$t('wifi.savedNetworks')" max-width="max-w-lg" @close="closeManage">
      <p class="mb-3 text-sm text-base-content/60">
        {{ $t('wifi.savedHint') }}
      </p>
      <p v-if="!known.length" class="text-sm text-base-content/60">{{ $t('wifi.noSaved') }}</p>
      <ul v-else class="max-h-72 space-y-1 overflow-y-auto">
        <li
          v-for="network in known"
          :key="network.ssid"
          class="flex flex-wrap items-center justify-between gap-2 rounded-box bg-base-200/60 px-3 py-2"
        >
          <div class="min-w-0">
            <p class="truncate text-sm font-medium">
              {{ network.ssid }}
              <span v-if="network.connected" class="badge badge-success badge-sm ml-1">{{ $t('common.connected') }}</span>
            </p>
          </div>
          <div class="flex items-center gap-1">
            <template v-if="forgetting === network.ssid">
              <button type="button" class="btn btn-xs btn-error" :disabled="busy === 'forget'" @click="forget(network)">
                {{ $t('wifi.confirmForget') }}
              </button>
              <button type="button" class="btn btn-xs btn-ghost" @click="forgetting = ''">{{ $t('common.cancel') }}</button>
            </template>
            <button
              v-else
              type="button"
              class="btn btn-xs btn-outline"
              :disabled="isProtected(network) || busy === 'forget'"
              :title="isProtected(network) ? $t('wifi.protectedTitle') : $t('wifi.forgetTitle', { ssid: network.ssid })"
              @click="forget(network)"
            >
              {{ $t('wifi.forget') }}
            </button>
          </div>
        </li>
      </ul>

      <template #footer>
        <button type="button" class="btn btn-ghost" @click="closeManage">{{ $t('common.close') }}</button>
      </template>
    </ModalDialog>

    <ModalDialog
      v-if="joinOpen"
      :title="joinTitle"
      :before-close="() => busy !== 'connect'"
      @close="closeJoin"
    >
      <form id="wifi-join-form" class="flex flex-col gap-3" novalidate @submit.prevent="submitConnect">
        <label class="flex w-full flex-col gap-1 text-sm">
          <span class="font-medium">{{ $t('wifi.networkNameLabel') }}</span>
          <input
            v-model="connect.ssid"
            type="text"
            class="input input-bordered input-sm w-full"
            maxlength="32"
            autocomplete="off"
            spellcheck="false"
            :placeholder="$t('wifi.networkNameLabel')"
          />
        </label>

        <label class="flex w-full flex-col gap-1 text-sm">
          <span class="font-medium">{{ $t('wifi.security') }}</span>
          <select v-model="connect.security" class="select select-bordered select-sm w-full">
            <option value="wpa2">WPA2</option>
            <option value="wpa2_wpa3">WPA2/WPA3</option>
            <option value="wpa3">WPA3</option>
            <option value="open">{{ $t('wifi.openNoPassword') }}</option>
          </select>
        </label>

        <label v-if="connect.security !== 'open'" class="flex w-full flex-col gap-1 text-sm">
          <span class="font-medium">{{ $t('wifi.passphrase') }}</span>
          <input
            v-model="connect.passphrase"
            type="password"
            class="input input-bordered input-sm w-full"
            autocomplete="new-password"
            maxlength="63"
            :placeholder="$t('wifi.passphraseHint')"
          />
        </label>

        <p class="text-xs text-base-content/60">
          {{ $t('wifi.leavesAfter') }}
        </p>
      </form>

      <template #footer>
        <button type="button" class="btn btn-ghost" :disabled="busy === 'connect'" @click="closeJoin">
          {{ $t('common.cancel') }}
        </button>
        <button
          type="submit"
          form="wifi-join-form"
          class="btn btn-primary"
          :disabled="busy || !connect.ssid.trim()"
          :aria-busy="busy === 'connect' ? 'true' : 'false'"
        >
          <span v-if="busy === 'connect'" class="loading loading-spinner loading-xs" aria-hidden="true"></span>
          {{ busy === 'connect' ? $t('wifi.connecting') : $t('wifi.connect') }}
        </button>
      </template>
    </ModalDialog>
  </section>
</template>
