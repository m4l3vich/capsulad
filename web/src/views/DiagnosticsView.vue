<script setup>
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { topicNames, socket } from '../lib/ws.js'
import { useTopic, liveState } from '../lib/live.js'
import { shortId, status } from '../lib/format.js'

// Diagnostics is read-only except for stopping a run. Metrics arrive with the
// diagnostics snapshot and while the topic is subscribed; this page never
// polls. Logs are a bounded browser buffer fed by the snapshot tail plus log
// events, with explicit pause/resume and filters.

const LOG_LIMIT = 400
const PAUSED_LIMIT = 200

const { data, error, loading, refresh, appliedRevision } = useTopic(topicNames.diagnostics)

// Envelope revisions are per connection and per domain. Logs have no revision
// gate (a log event is never made stale by a full view), while metrics and
// full views keep independent watermarks. All reset on reconnect.
let metricsRev = 0
let mergedMetrics = null

const live = computed(() => liveState.value === 'open')

// --- hardware and backends --------------------------------------------------

const device = computed(() => data.value?.device || null)
const backends = computed(() => device.value?.subsystemBackends || [])

function shown(value) {
  return value === undefined || value === null || value === '' ? 'Unavailable' : value
}

function plainPlatform(value) {
  const text = status(value)
  return text ? text.charAt(0).toUpperCase() + text.slice(1) : 'Unavailable'
}

function appLabel(appId) {
  const id = String(appId || '')
  const leaf = id.split('.').pop() || id
  const words = leaf.replace(/[-_]/g, ' ').trim()
  return words ? words.charAt(0).toUpperCase() + words.slice(1) : 'Unknown app'
}

function backendBadge(value) {
  const text = status(value)
  if (!text) return 'badge-neutral'
  if (text.includes('available')) return 'badge-success'
  if (text.includes('unverified') || text.includes('degraded') || text.includes('partial')) return 'badge-warning'
  if (text.includes('unavailable') || text.includes('error') || text.includes('failed') || text.includes('missing')) return 'badge-error'
  return 'badge-neutral'
}

// --- metrics ----------------------------------------------------------------

const metrics = computed(() => data.value?.metrics || null)
const metricsAvailable = computed(() => Boolean(metrics.value?.available))

const cpuPercent = computed(() => {
  const value = Number(metrics.value?.cpuUsagePercent)
  return Number.isFinite(value) ? Math.min(100, Math.max(0, value)) : null
})

const cpuCount = computed(() => Number(metrics.value?.cpuCount) || 0)
const cpuCaption = computed(() => {
  if (!metrics.value?.cpuAvailable) return 'Not reported on this platform'
  if (cpuPercent.value === null) return 'Value not reported'
  return cpuCount.value > 0 ? `${cpuCount.value} cores` : 'Current load'
})

const memoryTotal = computed(() => Number(metrics.value?.memoryTotalBytes) || 0)
const memoryUsed = computed(() => Number(metrics.value?.memoryUsedBytes) || 0)
const memoryAvailable = computed(() => Boolean(metrics.value?.memoryAvailable) && memoryTotal.value > 0)
const memoryPercent = computed(() => (memoryAvailable.value ? clampPercent((memoryUsed.value / memoryTotal.value) * 100) : 0))

const diskTotal = computed(() => Number(metrics.value?.diskTotalBytes) || 0)
const diskFree = computed(() => Number(metrics.value?.diskFreeBytes) || 0)
const diskAvailable = computed(() => Boolean(metrics.value?.diskAvailable) && diskTotal.value > 0)
const diskUsedPercent = computed(() => {
  if (!diskAvailable.value) return 0
  return clampPercent(((diskTotal.value - diskFree.value) / diskTotal.value) * 100)
})

const temperatureAvailable = computed(() => Boolean(metrics.value?.temperatureAvailable))
const temperature = computed(() => {
  const value = Number(metrics.value?.socTemperatureC)
  return Number.isFinite(value) ? value : null
})

// Uptime is only rendered when the backend says it read the host clock; an
// unsupported reader leaving uptimeSeconds at 0 must not look like "on for 0s".
const uptimeAvailable = computed(() => Boolean(metrics.value?.uptimeAvailable))

function clampPercent(value) {
  return Number.isFinite(value) ? Math.min(100, Math.max(0, value)) : 0
}

function formatPercent(value) {
  return `${Number(value).toFixed(1)}%`
}

function formatBytes(value) {
  const bytes = Number(value)
  if (!Number.isFinite(bytes) || bytes < 0) return 'Unavailable'
  if (bytes === 0) return '0 B'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let scaled = bytes
  let unit = 0
  while (scaled >= 1024 && unit < units.length - 1) {
    scaled /= 1024
    unit += 1
  }
  return `${scaled >= 100 || unit === 0 ? Math.round(scaled) : scaled.toFixed(1)} ${units[unit]}`
}

function formatUptime(seconds) {
  const total = Math.max(0, Math.floor(Number(seconds) || 0))
  const days = Math.floor(total / 86400)
  const hours = Math.floor((total % 86400) / 3600)
  const minutes = Math.floor((total % 3600) / 60)
  if (days > 0) return `${days}d ${hours}h`
  if (hours > 0) return `${hours}h ${minutes}m`
  if (minutes > 0) return `${minutes}m`
  return `${total}s`
}

// --- runs -------------------------------------------------------------------

const runs = computed(() => data.value?.runs || [])
const selectedProviders = computed(() => data.value?.selectedProviders || {})
const busyRun = ref('')
const actionPending = computed(() => busyRun.value !== '')
const runNotice = ref('')
const runError = ref('')

function isSelectedRun(run) {
  return Boolean(run.selectedDefault || selectedProviders.value?.[run.appId])
}

async function stopRun(run) {
  if (actionPending.value) return
  const selected = isSelectedRun(run)
  busyRun.value = run.runId
  runError.value = ''
  runNotice.value = ''
  try {
    const result = await socket.request({ stop: { appId: run.appId, runId: run.runId } })
    const stop = result.stop || {}
    const isDefault = Boolean(stop.selectedDefault || selected)
    runNotice.value = isDefault
      ? `${appLabel(run.appId)} was restarted and stays on because it is the default app. Choose another default in Settings to switch it off.`
      : `${appLabel(run.appId)} was stopped.`
    await refresh()
  } catch (err) {
    runError.value = err.message
  } finally {
    busyRun.value = ''
  }
}

// --- logs -------------------------------------------------------------------

const logPane = ref(null)
const logEntries = ref([])
const pausedBuffer = ref([])
const paused = ref(false)
const pendingCount = ref(0)
const atBottom = ref(true)
const severityFilter = ref('all')
const textFilter = ref('')

const severityOptions = [
  { value: 'all', label: 'All levels' },
  { value: 'error', label: 'Error' },
  { value: 'warn', label: 'Warning' },
  { value: 'info', label: 'Info' },
  { value: 'debug', label: 'Debug' }
]

function severityKey(entry) {
  return String(entry.severity || 'info').toLowerCase()
}

function severityBadge(entry) {
  switch (severityKey(entry)) {
    case 'error': return 'badge-error'
    case 'warn':
    case 'warning': return 'badge-warning'
    case 'info': return 'badge-info'
    default: return 'badge-ghost'
  }
}

const filteredLogs = computed(() => {
  const needle = textFilter.value.trim().toLowerCase()
  const severity = severityFilter.value
  return logEntries.value.filter((entry) => {
    if (severity !== 'all' && severityKey(entry) !== severity) return false
    if (!needle) return true
    const haystack = `${entry.appId || ''} ${entry.component || ''} ${entry.message || ''}`.toLowerCase()
    return haystack.includes(needle)
  })
})

// Log records are identified by their content, not by object identity: a
// snapshot and a streamed frame decode fresh objects for the same record, and
// a union must not duplicate them. The monotonic timestamp alone is not unique.
function logKey(entry) {
  return `${entry.monotonicTimeNs}|${entry.component}|${entry.appId}|${entry.severity}|${entry.message}`
}

function mergeEntries(base, incoming) {
  const seen = new Set()
  const merged = []
  for (const entry of [...base, ...incoming]) {
    if (!entry || typeof entry !== 'object') continue
    const key = logKey(entry)
    if (seen.has(key)) continue
    seen.add(key)
    merged.push(entry)
  }
  merged.sort((a, b) => Number(a.monotonicTimeNs || 0) - Number(b.monotonicTimeNs || 0))
  return merged
}

function recomputePending() {
  const visible = new Set(logEntries.value.map(logKey))
  const seen = new Set()
  const pending = []
  for (const entry of pausedBuffer.value) {
    if (!entry || typeof entry !== 'object') continue
    const key = logKey(entry)
    if (visible.has(key) || seen.has(key)) continue
    seen.add(key)
    pending.push(entry)
  }
  pausedBuffer.value = pending.slice(-PAUSED_LIMIT)
  pendingCount.value = pausedBuffer.value.length
}

// Snapshot and full-view logs are merged into the local buffer, never
// replaced: an older reserved snapshot must not discard a newer streamed
// entry. Union + content dedup + monotonic sort + newest cap keeps exactly one
// copy, so the reserved-snapshot race preserves the newer log once. Pause is a
// user choice, so its buffer stays; records already visible are removed from
// the "new" count instead of being dropped.
function mergeLogs(entries) {
  if (!Array.isArray(entries) || !entries.length) return
  logEntries.value = mergeEntries(logEntries.value, entries).slice(-LOG_LIMIT)
  if (paused.value) recomputePending()
  if (atBottom.value) nextTick(scrollToBottom)
}

// useTopic owns full-view acceptance; this watcher consumes whichever logs
// array it installed. Metrics merges reuse the same array reference and
// streamed log events never touch data, so neither triggers a duplicate merge.
watch(
  () => data.value?.logs,
  (entries) => {
    if (Array.isArray(entries)) mergeLogs(entries)
  },
  { flush: 'sync' }
)

// A full view can arrive with a metrics object sampled before a metric event
// already retained here. Keep the newer metrics (no rollback) while the view's
// device/runs/resources fields are adopted normally. When the view is at least
// as new as the retained metrics, it becomes the newest metrics authority.
watch(
  () => data.value,
  (view) => {
    if (!view) return
    const rev = Number(appliedRevision.value) || 0
    if (rev && metricsRev && rev < metricsRev) {
      if (mergedMetrics && view.metrics !== mergedMetrics) {
        data.value = { ...view, metrics: mergedMetrics }
      }
      return
    }
    if (rev) {
      metricsRev = Math.max(metricsRev, rev)
      if (view.metrics) mergedMetrics = view.metrics
    }
  },
  { flush: 'sync' }
)

function appendLogs(entries) {
  if (!entries?.length) return
  if (paused.value) {
    pausedBuffer.value = mergeEntries(pausedBuffer.value, entries).slice(-PAUSED_LIMIT)
    recomputePending()
    return
  }
  mergeLogs(entries)
}

// Metrics-only events use their own revision domain. A log revision never
// suppresses metrics or a snapshot, and a metrics revision never suppresses
// logs. Snapshot and full-view metrics are reconciled against the retained
// newest metrics so an older reserved snapshot cannot roll them back.
function reconcileMetrics(candidate, revision) {
  if (!candidate) return
  const rev = Number(revision) || 0
  if (!rev || !metricsRev || rev >= metricsRev) {
    if (rev) metricsRev = Math.max(metricsRev, rev)
    mergedMetrics = candidate
    if (data.value && data.value.metrics !== candidate) {
      data.value = { ...data.value, metrics: candidate }
    }
    return
  }
  if (mergedMetrics && data.value && data.value.metrics !== mergedMetrics) {
    data.value = { ...data.value, metrics: mergedMetrics }
  }
}

function togglePause() {
  if (!paused.value) {
    paused.value = true
    return
  }
  const resumed = pausedBuffer.value
  pausedBuffer.value = []
  pendingCount.value = 0
  paused.value = false
  if (resumed.length) mergeLogs(resumed)
  nextTick(() => {
    atBottom.value = true
    scrollToBottom()
  })
}

function clearLogs() {
  logEntries.value = []
  pausedBuffer.value = []
  pendingCount.value = 0
}

function scrollToBottom() {
  const pane = logPane.value
  if (!pane) return
  pane.scrollTop = pane.scrollHeight
  atBottom.value = true
}

function onLogScroll() {
  const pane = logPane.value
  if (!pane) return
  atBottom.value = pane.scrollHeight - pane.scrollTop - pane.clientHeight < 48
}

function resetFilters() {
  severityFilter.value = 'all'
  textFilter.value = ''
  nextTick(scrollToBottom)
}

let offLogs = null
let offReconnect = null

onMounted(() => {
  // A second handler on the same topic shares the single wire subscription.
  // useTopic registers in setup, so its handler (and sync watchers) run before
  // this one: the page can safely merge logs and restore newer metrics after a
  // full view was installed or skipped.
  offLogs = socket.subscribe(topicNames.diagnostics, '', {
    onSnapshot: (snapshot) => {
      const view = snapshot?.diagnostics
      if (!view) return
      // Snapshot logs are merged even when useTopic skipped the full fields as
      // stale, so a newer streamed entry is preserved exactly once.
      if (Array.isArray(view.logs)) mergeLogs(view.logs)
      if (view.metrics) reconcileMetrics(view.metrics, snapshot?.revision)
    },
    onEvent: (event) => {
      if (event.logs) {
        // Logs have no revision gate: a log event is never "older" than a
        // snapshot for merging purposes.
        appendLogs(event.logs.entries)
        return
      }
      if (event.diagnosticsMetrics) {
        reconcileMetrics(event.diagnosticsMetrics, event.revision)
        return
      }
      if (event.diagnostics?.metrics) {
        reconcileMetrics(event.diagnostics.metrics, event.revision)
      }
    }
  })
  offReconnect = socket.on('reconnect', () => {
    // Monotonic timestamps and envelope counters restart with the daemon
    // process. Never union across that gap: clear retained and paused entries
    // plus per-domain ordering, keep the user's pause choice, and let the fresh
    // subscription snapshot establish the new baseline.
    metricsRev = 0
    mergedMetrics = null
    logEntries.value = []
    pausedBuffer.value = []
    pendingCount.value = 0
    atBottom.value = true
  })
})

onUnmounted(() => {
  offLogs?.()
  offReconnect?.()
  offLogs = null
  offReconnect = null
  metricsRev = 0
  mergedMetrics = null
  logEntries.value = []
  pausedBuffer.value = []
})
</script>

<template>
  <div class="mx-auto w-full max-w-6xl space-y-6">
    <header class="flex flex-wrap items-end justify-between gap-3">
      <div>
        <p class="text-xs font-semibold uppercase tracking-[0.16em] text-base-content/60">System</p>
        <h1 class="text-2xl font-semibold tracking-tight">Diagnostics</h1>
        <p class="mt-1 max-w-3xl text-sm text-base-content/70">
          Check how the speaker is doing, which apps are running, and what it has been doing recently.
        </p>
      </div>
      <div class="flex items-center gap-2">
        <span class="badge badge-sm" :class="live ? 'badge-success' : 'badge-warning'">{{ live ? 'live' : 'not connected' }}</span>
        <button type="button" class="btn btn-sm btn-ghost" :disabled="loading" @click="refresh">
          <span v-if="loading" class="loading loading-spinner loading-xs" aria-hidden="true"></span>
          Refresh
        </button>
      </div>
    </header>

    <div v-if="error" role="alert" class="alert alert-error py-2">
      <span class="text-sm"><strong class="font-semibold">Refresh failed.</strong> {{ error }}</span>
      <button type="button" class="btn btn-sm btn-outline" :disabled="loading" @click="refresh">Retry</button>
    </div>

    <div v-if="!live && data" role="status" class="alert alert-warning py-2">
      <span class="text-sm">
        The connection to the speaker was lost. These readings are from the last update and will refresh when it
        reconnects.
      </span>
    </div>

    <div v-if="!data && loading" class="grid gap-4 md:grid-cols-2" aria-hidden="true">
      <div class="card border border-base-300 bg-base-100"><div class="card-body gap-3 p-5">
        <div class="skeleton h-4 w-1/3"></div><div class="skeleton h-32 w-full"></div>
      </div></div>
      <div class="card border border-base-300 bg-base-100"><div class="card-body gap-3 p-5">
        <div class="skeleton h-4 w-1/2"></div><div class="skeleton h-32 w-full"></div>
      </div></div>
    </div>

    <div v-else-if="!data" class="alert">
      <span class="text-sm">Diagnostics are unavailable. {{ error || 'The speaker did not send a status update.' }}</span>
    </div>

    <template v-if="data">
      <!-- hardware / software / backends -->
      <section class="card border border-base-300 bg-base-100">
        <div class="card-body gap-4 p-5">
          <h2 class="card-title text-base">About this speaker</h2>

          <dl class="grid gap-x-6 gap-y-3 text-sm sm:grid-cols-3">
            <div>
              <dt class="text-xs font-semibold uppercase tracking-wide text-base-content/60">Model</dt>
              <dd class="mt-0.5">{{ shown(device?.model) }}</dd>
            </div>
            <div>
              <dt class="text-xs font-semibold uppercase tracking-wide text-base-content/60">Software version</dt>
              <dd class="mt-0.5">{{ shown(device?.daemonVersion) }}</dd>
            </div>
            <div>
              <dt class="text-xs font-semibold uppercase tracking-wide text-base-content/60">Platform</dt>
              <dd class="mt-0.5">{{ plainPlatform(device?.targetPlatform) }}</dd>
            </div>
          </dl>

          <details class="rounded border border-base-300 p-3">
            <summary class="cursor-pointer text-sm font-medium">Technical details</summary>

            <dl class="mt-3 grid gap-x-6 gap-y-3 text-sm sm:grid-cols-2 lg:grid-cols-3">
              <div>
                <dt class="text-xs font-semibold uppercase tracking-wide text-base-content/60">SoC</dt>
                <dd class="mt-0.5">{{ shown(device?.soc) }}</dd>
              </div>
              <div>
                <dt class="text-xs font-semibold uppercase tracking-wide text-base-content/60">Board revision</dt>
                <dd class="mt-0.5">{{ shown(device?.boardRevision) }}</dd>
              </div>
              <div>
                <dt class="text-xs font-semibold uppercase tracking-wide text-base-content/60">Kernel</dt>
                <dd class="mt-0.5 break-words font-mono text-xs">{{ shown(device?.kernel) }}</dd>
              </div>
              <div>
                <dt class="text-xs font-semibold uppercase tracking-wide text-base-content/60">Firmware</dt>
                <dd class="mt-0.5">{{ shown(device?.firmware) }}</dd>
              </div>
              <div>
                <dt class="text-xs font-semibold uppercase tracking-wide text-base-content/60">Platform ID</dt>
                <dd class="mt-0.5 font-mono text-xs">{{ shown(device?.targetPlatform) }}</dd>
              </div>
              <div>
                <dt class="text-xs font-semibold uppercase tracking-wide text-base-content/60">Device ID</dt>
                <dd class="mt-0.5 break-all font-mono text-xs">{{ shown(device?.deviceId) }}</dd>
              </div>
              <div>
                <dt class="text-xs font-semibold uppercase tracking-wide text-base-content/60">Boot ID</dt>
                <dd class="mt-0.5 break-all font-mono text-xs">{{ shown(device?.bootId) }}</dd>
              </div>
            </dl>

            <h3 class="mt-4 text-xs font-semibold uppercase tracking-wide text-base-content/60">Hardware drivers</h3>
            <div v-if="backends.length" class="mt-2 overflow-x-auto">
              <table class="table table-sm">
                <thead>
                  <tr class="text-xs uppercase tracking-wide text-base-content/60">
                    <th scope="col">Subsystem</th>
                    <th scope="col">Driver</th>
                    <th scope="col">Status</th>
                    <th scope="col">Detail</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="backend in backends" :key="backend.subsystem">
                    <td class="whitespace-nowrap text-sm">{{ shown(backend.subsystem) }}</td>
                    <td class="font-mono text-xs">{{ shown(backend.backendId) }}</td>
                    <td>
                      <span class="badge badge-sm" :class="backendBadge(backend.status)">{{ status(backend.status) || 'unknown' }}</span>
                    </td>
                    <td class="text-xs text-base-content/70">{{ shown(backend.detail) }}</td>
                  </tr>
                </tbody>
              </table>
            </div>
            <p v-else class="mt-2 text-sm text-base-content/70">No driver status reported.</p>
          </details>
        </div>
      </section>

      <!-- metrics -->
      <section class="card border border-base-300 bg-base-100">
        <div class="card-body gap-4 p-5">
          <div class="flex flex-wrap items-center justify-between gap-2">
            <h2 class="card-title text-base">Health at a glance</h2>
            <span v-if="metricsAvailable && uptimeAvailable" class="text-xs text-base-content/60">
              On for {{ formatUptime(metrics.uptimeSeconds) }}
            </span>
          </div>

          <template v-if="metricsAvailable">
            <div class="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
              <div class="rounded-lg border border-base-300 p-4">
                <h3 class="text-xs font-semibold uppercase tracking-wide text-base-content/60">Processor</h3>
                <p class="mt-2 text-2xl font-semibold tabular-nums">
                  {{ metrics?.cpuAvailable && cpuPercent !== null ? formatPercent(cpuPercent) : 'Unavailable' }}
                </p>
                <progress
                  v-if="metrics?.cpuAvailable && cpuPercent !== null"
                  class="progress progress-primary mt-3 h-1.5"
                  :value="cpuPercent"
                  max="100"
                ></progress>
                <p class="mt-2 text-xs text-base-content/60">{{ cpuCaption }}</p>
              </div>

              <div class="rounded-lg border border-base-300 p-4">
                <h3 class="text-xs font-semibold uppercase tracking-wide text-base-content/60">Memory</h3>
                <p class="mt-2 text-2xl font-semibold tabular-nums">
                  {{ memoryAvailable ? formatBytes(memoryUsed) : 'Unavailable' }}
                </p>
                <progress
                  v-if="memoryAvailable"
                  class="progress progress-primary mt-3 h-1.5"
                  :value="memoryPercent"
                  max="100"
                ></progress>
                <p class="mt-2 text-xs text-base-content/60">
                  {{ memoryAvailable ? `used of ${formatBytes(memoryTotal)} (${formatPercent(memoryPercent)})` : 'Not reported on this platform' }}
                </p>
              </div>

              <div class="rounded-lg border border-base-300 p-4">
                <h3 class="text-xs font-semibold uppercase tracking-wide text-base-content/60">Storage</h3>
                <p class="mt-2 text-2xl font-semibold tabular-nums">
                  {{ diskAvailable ? formatBytes(diskFree) : 'Unavailable' }}
                </p>
                <progress
                  v-if="diskAvailable"
                  class="progress progress-primary mt-3 h-1.5"
                  :value="diskUsedPercent"
                  max="100"
                ></progress>
                <p class="mt-2 text-xs text-base-content/60">
                  {{ diskAvailable ? `free of ${formatBytes(diskTotal)} (${formatPercent(diskUsedPercent)} used)` : 'Not reported on this platform' }}
                </p>
              </div>

              <div class="rounded-lg border border-base-300 p-4">
                <h3 class="text-xs font-semibold uppercase tracking-wide text-base-content/60">Temperature</h3>
                <p class="mt-2 text-2xl font-semibold tabular-nums">
                  {{ temperatureAvailable && temperature !== null ? `${temperature.toFixed(1)} °C` : 'Unavailable' }}
                </p>
                <p class="mt-3 text-xs text-base-content/60">
                  {{ temperatureAvailable && temperature !== null ? 'Device temperature' : 'Not reported on this platform' }}
                </p>
              </div>
            </div>
            <p v-if="metrics?.detail" class="text-xs text-base-content/60">{{ metrics.detail }}</p>
            <p class="text-xs text-base-content/60">
              Readings update automatically while this page is open.
            </p>
          </template>
          <div v-else class="rounded-lg border border-base-300 p-4 text-sm">
            <p class="font-medium">Health readings unavailable</p>
            <p class="mt-1 text-base-content/70">
              {{ metrics?.detail || 'This device does not report processor, memory, storage or temperature readings.' }}
            </p>
          </div>
        </div>
      </section>

      <!-- runs -->
      <section class="card border border-base-300 bg-base-100">
        <div class="card-body gap-4 p-5">
          <div>
            <h2 class="card-title text-base">Running apps</h2>
            <p class="mt-1 max-w-3xl text-sm text-base-content/70">
              Apps marked <span class="badge badge-ghost badge-sm">Default</span> keep themselves running, so their
              button says Restart. To switch one off, choose another default in Settings.
            </p>
          </div>

          <div v-if="runError" role="alert" class="alert alert-error py-2 text-sm"><span>{{ runError }}</span></div>
          <div v-if="runNotice" role="status" class="alert alert-info py-2 text-sm">
            <span>{{ runNotice }}</span>
            <button type="button" class="btn btn-ghost btn-xs" aria-label="Dismiss message" @click="runNotice = ''">Dismiss</button>
          </div>

          <div v-if="runs.length" class="overflow-x-auto">
            <table class="table table-sm">
              <thead>
                <tr class="text-xs uppercase tracking-wide text-base-content/60">
                  <th scope="col">App</th>
                  <th scope="col">Status</th>
                  <th scope="col"><span class="sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="run in runs" :key="run.runId">
                  <td>
                    <div class="flex flex-wrap items-center gap-2">
                      <span class="text-sm font-medium">{{ appLabel(run.appId) }}</span>
                      <span v-if="isSelectedRun(run)" class="badge badge-ghost badge-sm">Default</span>
                    </div>
                    <p class="mt-0.5 font-mono text-[11px] text-base-content/50" :title="`${run.appId} · ${run.runId}`">
                      {{ shortId(run.runId) }}<span v-if="run.pid"> · process {{ run.pid }}</span>
                    </p>
                  </td>
                  <td>
                    <span class="badge badge-sm" :class="status(run.state) === 'ready' ? 'badge-success' : 'badge-neutral'">
                      {{ status(run.state) || 'unknown' }}
                    </span>
                    <p v-if="run.statusDetail" class="mt-1 max-w-xs break-words text-xs text-base-content/60">
                      {{ run.statusDetail }}
                    </p>
                  </td>
                  <td class="whitespace-nowrap text-right">
                    <button
                      type="button"
                      class="btn btn-sm"
                      :class="isSelectedRun(run) ? 'btn-outline' : 'btn-error btn-outline'"
                      :disabled="actionPending"
                      :title="isSelectedRun(run)
                        ? 'Default app: this restarts it. Choose another default in Settings to switch it off.'
                        : 'Stop this app'"
                      @click="stopRun(run)"
                    >
                      <span v-if="busyRun === run.runId" class="loading loading-spinner loading-xs" aria-hidden="true"></span>
                      {{ isSelectedRun(run) ? 'Restart' : 'Stop' }}
                    </button>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
          <p v-else class="rounded border border-base-300 p-4 text-sm text-base-content/70">
            No apps are running right now.
          </p>
        </div>
      </section>

      <!-- logs -->
      <section class="card border border-base-300 bg-base-100">
        <div class="card-body gap-3 p-5">
          <div class="flex flex-wrap items-center justify-between gap-2">
            <div class="flex flex-wrap items-center gap-2">
              <h2 class="card-title text-base">Recent activity</h2>
              <span class="badge badge-sm" :class="paused ? 'badge-warning' : live ? 'badge-success' : 'badge-neutral'">
                {{ paused ? 'Paused' : live ? 'Running' : 'Offline' }}
              </span>
              <span v-if="paused" class="badge badge-outline badge-sm">{{ pendingCount }} new</span>
            </div>
            <div class="flex flex-wrap items-center gap-2">
              <button
                type="button"
                class="btn btn-sm"
                :class="paused ? 'btn-warning' : 'btn-outline'"
                :aria-pressed="paused"
                @click="togglePause"
              >{{ paused ? 'Resume' : 'Pause' }}</button>
              <button type="button" class="btn btn-ghost btn-sm" :disabled="!logEntries.length" @click="clearLogs">Clear</button>
            </div>
          </div>

          <div class="flex flex-col gap-2 sm:flex-row sm:items-center">
            <label class="sr-only" for="log-severity">Filter activity by level</label>
            <select
              id="log-severity"
              v-model="severityFilter"
              class="select select-bordered select-sm w-full sm:w-44"
            >
              <option v-for="option in severityOptions" :key="option.value" :value="option.value">{{ option.label }}</option>
            </select>
            <label class="sr-only" for="log-filter">Search activity by app or message</label>
            <input
              id="log-filter"
              v-model="textFilter"
              type="search"
              class="input input-bordered input-sm w-full sm:max-w-xs"
              placeholder="Search apps or messages"
              autocomplete="off"
              spellcheck="false"
            />
            <span class="text-xs text-base-content/60">
              {{ filteredLogs.length }} of {{ logEntries.length }} shown
            </span>
            <button
              v-if="severityFilter !== 'all' || textFilter"
              type="button"
              class="btn btn-ghost btn-xs"
              @click="resetFilters"
            >Clear filters</button>
          </div>

          <p v-if="paused" class="text-xs text-base-content/70">
            While paused, new entries wait here (up to {{ PAUSED_LIMIT }}) and appear when you resume.
          </p>

          <div
            ref="logPane"
            class="h-80 overflow-y-auto rounded-lg border border-base-300 bg-base-200/40 p-3 font-mono text-xs leading-relaxed"
            role="log"
            aria-live="off"
            aria-label="Recent activity entries"
            tabindex="0"
            @scroll="onLogScroll"
          >
            <ul v-if="filteredLogs.length" class="space-y-1">
              <li
                v-for="entry in filteredLogs"
                :key="logKey(entry)"
                class="flex items-start gap-2 border-b border-base-200 pb-1 last:border-b-0"
              >
                <span class="badge badge-xs shrink-0 uppercase" :class="severityBadge(entry)">{{ severityKey(entry) }}</span>
                <span class="shrink-0 text-base-content/60">
                  {{ entry.component || 'system' }}<span v-if="entry.appId"> — {{ appLabel(entry.appId) }}</span>
                </span>
                <span class="min-w-0 whitespace-pre-wrap break-words">{{ entry.message }}</span>
              </li>
            </ul>
            <p v-else-if="logEntries.length" class="text-base-content/70">Nothing matches this filter.</p>
            <p v-else class="text-base-content/70">Nothing here yet.</p>
          </div>

          <div class="flex flex-wrap items-center justify-between gap-2">
            <p class="text-xs text-base-content/60">
              Keeps the latest {{ LOG_LIMIT }} entries in this browser; the newest is at the bottom. After a
              reconnect the list refreshes.
            </p>
            <button v-if="!atBottom && filteredLogs.length" type="button" class="btn btn-xs btn-outline" @click="scrollToBottom">
              Jump to latest
            </button>
          </div>
        </div>
      </section>
    </template>
  </div>
</template>
