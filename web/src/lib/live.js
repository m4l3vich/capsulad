import { onScopeDispose, ref, shallowRef } from 'vue'
import { socket, topicNames } from './ws.js'
import { t } from './i18n.js'

// Shared live state for every page and control in this tab. The page holds one
// WebSocket and one authoritative settings revision; views and controls only
// describe what they observe and what patch they own.

export const liveState = ref(socket.state === 'closed' ? 'connecting' : socket.state)
socket.on('state', (state) => {
  liveState.value = state
})

function topicNumber(topic) {
  if (typeof topic === 'number') return topic
  return topicNames[topic] ?? 0
}

function viewOf(snapshot, topic) {
  if (!snapshot) return null
  switch (topic) {
    case topicNames.overview: return snapshot.overview || null
    case topicNames.settings: return snapshot.settings || null
    case topicNames.application: return snapshot.application || null
    case topicNames.diagnostics: return snapshot.diagnostics || null
    case topicNames.wakeword: return snapshot.wakeword || null
    case topicNames.access: return snapshot.access || null
    case topicNames.wifi: return snapshot.wifi || null
    default: return null
  }
}

function eventView(event) {
  return event.overview || event.settings || event.application || event.diagnostics ||
    event.wakeword || event.access || event.wifi || null
}

/**
 * Subscribe to one topic view. `data` is the unwrapped view payload (for
 * example OverviewView, SettingsView or ApplicationView), `error` is a string
 * for snapshot/transport failures.
 *
 * `logs: true` appends `event.logs` entries into `data.logs` for the
 * application topic (bounded, oldest dropped). It is off by default so log
 * pages can own their pause/filter behaviour; both kinds of handler share the
 * one wire subscription.
 */
export function useTopic(topic, appId = '', { logs = false } = {}) {
  const topicId = topicNumber(topic)
  const data = shallowRef(null)
  const error = ref('')
  const loading = ref(true)
  // Envelope revision of the last installed full view for this connection.
  // Partial domain events (logs, metrics) never advance it.
  const appliedRevision = ref(0)
  let off = null
  let offReconnect = null
  let alive = true
  let lastFullEnvelopeRevision = 0
  // Bounded partial-log tail that arrived before any full view was accepted.
  let pendingLogs = []

  function stored(view) {
    if (!alive || !view) return
    if (logs && Array.isArray(view.logs) && view.logs.length > 400) {
      return { ...view, logs: view.logs.slice(-400) }
    }
    return view
  }

  function logKey(entry) {
    return `${entry.monotonicTimeNs}|${entry.component}|${entry.appId}|${entry.severity}|${entry.message}`
  }

  // Union of the current view's logs, buffered partial logs and any logs the
  // frame itself carries. Dedupe by full-record identity, keep chronological
  // order, and cap the newest tail.
  function mergedLogs(extra) {
    const seen = new Set()
    const merged = []
    const push = (entry) => {
      if (!entry) return
      const key = logKey(entry)
      if (seen.has(key)) return
      seen.add(key)
      merged.push(entry)
    }
    const pushAll = (list) => {
      if (Array.isArray(list)) for (const entry of list) push(entry)
    }
    pushAll(data.value?.logs)
    pushAll(pendingLogs)
    pushAll(extra)
    pendingLogs = []
    merged.sort((a, b) => Number(a.monotonicTimeNs || 0) - Number(b.monotonicTimeNs || 0))
    return merged.slice(-400)
  }

  function accept(view) {
    const next = stored(view)
    if (!next) return
    data.value = next
    error.value = ''
    loading.value = false
  }

  function acceptFull(view, revision) {
    // The page may read appliedRevision from a sync data watcher, so publish
    // it before installing the view.
    if (revision) appliedRevision.value = revision
    accept(logs ? { ...view, logs: mergedLogs(view.logs) } : view)
  }

  function mergeStaleLogs(view) {
    if (!logs || !data.value) return
    const next = mergedLogs(view.logs)
    data.value = { ...data.value, logs: next }
  }

  function fullStale(revision) {
    return !!revision && !!lastFullEnvelopeRevision && revision < lastFullEnvelopeRevision
  }

  function advanceFull(revision) {
    if (revision) lastFullEnvelopeRevision = Math.max(lastFullEnvelopeRevision, revision)
  }

  function appendLogs(entries) {
    if (!alive || !logs || !entries?.length) return
    if (!data.value) {
      pendingLogs = [...pendingLogs, ...entries].slice(-400)
      return
    }
    data.value = { ...data.value, logs: mergedLogs(entries) }
  }

  async function refresh() {
    if (!alive) return
    try {
      const snapshot = await socket.requestSnapshot(topicId, appId)
      const view = viewOf(snapshot, topicId)
      if (!alive || !view) return
      const revision = Number(snapshot?.revision) || 0
      // A resync response may have been read before a newer full event already
      // applied here. Never roll the full view back to an older envelope, but
      // still union any newer logs it carries.
      if (fullStale(revision)) {
        mergeStaleLogs(view)
        return
      }
      advanceFull(revision)
      acceptFull(view, revision)
      error.value = ''
    } catch (err) {
      if (alive) error.value = err?.message || t('errors.deviceDidNotAnswer')
    } finally {
      if (alive) loading.value = false
    }
  }

  // Revisions are per connection. A reconnected socket starts its counter
  // again, so the first snapshot after reconnect must not be rejected as old.
  offReconnect = socket.on('reconnect', () => {
    lastFullEnvelopeRevision = 0
    appliedRevision.value = 0
    pendingLogs = []
    // A daemon restart resets log monotonic time and envelope counters. Do not
    // merge the old connection's log journal into the new epoch's tail.
    if (alive && logs && data.value) {
      data.value = { ...data.value, logs: [] }
    }
  })

  off = socket.subscribe(topicId, appId, {
    onSnapshot(snapshot) {
      if (!alive) return
      const view = viewOf(snapshot, topicId)
      if (!view) return
      const revision = Number(snapshot?.revision) || 0
      if (fullStale(revision)) {
        mergeStaleLogs(view)
        return
      }
      advanceFull(revision)
      acceptFull(view, revision)
    },
    onEvent(event) {
      if (!alive) return
      // Partial domains do not consume the full-view watermark: a snapshot
      // reserved before this event may still carry newer full fields.
      if (event.logs) {
        appendLogs(event.logs.entries)
        return
      }
      const view = eventView(event)
      if (!view) return
      const revision = Number(event?.revision) || 0
      if (fullStale(revision)) {
        mergeStaleLogs(view)
        return
      }
      advanceFull(revision)
      acceptFull(view, revision)
    },
    onResync() {
      refresh()
    },
    onError(err) {
      if (!alive) return
      error.value = err?.message || t('errors.deviceDidNotAnswer')
      loading.value = false
    }
  })

  onScopeDispose(() => {
    alive = false
    off?.()
    off = null
    offReconnect?.()
    offReconnect = null
  })

  return { data, error, loading, refresh, appliedRevision }
}

// ---------------------------------------------------------------------------
// Settings: one serialized compare-and-swap queue for the whole page.
// ---------------------------------------------------------------------------

const settings = {
  data: shallowRef(null),
  error: ref(''),
  pending: ref(false),
  // Increments only when an authoritative settings view is applied. Update
  // results advance the CAS revision but are not a received view.
  viewEpoch: ref(0)
}

let settingsSubscribers = 0
let settingsOff = null
let settingsOffReconnect = null
// Store revision for compare-and-swap. It is not the transport envelope and
// must survive reconnects so a stale write cannot be issued.
let settingsLastRevision = 0
// Envelope revision for ordering pushes; per connection, reset on reconnect.
let settingsEnvelopeRevision = 0
let draining = false
const queued = new Map()

// Queue identity follows the fields a patch actually owns. Rapid repeats of
// the same control collapse; a different output resource or a different field
// mask keeps its own slot so neither patch can drop the other.
function maskKey(payload) {
  if (!payload || typeof payload !== 'object') return ''
  const fields = Array.isArray(payload.updateFields)
    ? payload.updateFields.filter((field) => typeof field === 'string')
    : []
  if (fields.length) return `f:${[...fields].sort().join(',')}`
  const flags = []
  if (payload.hasMode) flags.push('mode')
  if (payload.hasDelay) flags.push('delay')
  return flags.length ? `h:${flags.join(',')}` : ''
}

function patchKey(group, payload) {
  const parts = [group]
  if (group === 'display' && payload?.resourceId) parts.push(`r:${payload.resourceId}`)
  const mask = maskKey(payload)
  if (mask) parts.push(mask)
  return parts.join('|')
}

function settingsStale(revision) {
  return !!revision && !!settingsEnvelopeRevision && revision < settingsEnvelopeRevision
}

function settingsAdvance(revision) {
  if (revision) settingsEnvelopeRevision = Math.max(settingsEnvelopeRevision, revision)
}

function applySettingsSnapshot(snapshot) {
  const view = snapshot?.settings
  if (!view) return
  const envelope = Number(snapshot.revision) || 0
  if (settingsStale(envelope)) return
  settingsAdvance(envelope)
  // Store revision is independent of the envelope and only moves forward.
  settingsLastRevision = Math.max(settingsLastRevision, Number(view.revision) || 0)
  settings.data.value = view
  settings.viewEpoch.value += 1
  settings.error.value = ''
}

async function resyncSettings() {
  try {
    const snapshot = await socket.requestSnapshot(topicNames.settings)
    if (settingsStale(Number(snapshot?.revision) || 0)) return
    applySettingsSnapshot(snapshot)
  } catch (err) {
    settings.error.value = err?.message || t('errors.settingsUnavailable')
  }
}

function subscribeSettings() {
  if (settingsOff) return
  // A fresh subscription's first snapshot is current for this connection;
  // any earlier envelope value belonged to a dropped subscription.
  settingsEnvelopeRevision = 0
  settingsOffReconnect = socket.on('reconnect', () => {
    // A reconnected socket restarts its envelope counter; the settings store
    // revision (CAS) intentionally does not reset.
    settingsEnvelopeRevision = 0
  })
  settingsOff = socket.subscribe(topicNames.settings, '', {
    onSnapshot(snapshot) {
      applySettingsSnapshot(snapshot)
    },
    onEvent(event) {
      const view = event.settings
      if (!view) return
      const envelope = Number(event.revision) || 0
      if (settingsStale(envelope)) return
      settingsAdvance(envelope)
      settingsLastRevision = Math.max(settingsLastRevision, Number(view.revision) || 0)
      settings.data.value = view
      settings.viewEpoch.value += 1
      settings.error.value = ''
    },
    onResync() {
      resyncSettings()
    }
  })
}

function releaseSettings() {
  settingsSubscribers = Math.max(0, settingsSubscribers - 1)
  if (settingsSubscribers === 0 && settingsOff) {
    settingsOff()
    settingsOff = null
    settingsOffReconnect?.()
    settingsOffReconnect = null
  }
}

async function drainQueue() {
  if (draining) return
  draining = true
  settings.pending.value = true
  try {
    while (queued.size > 0) {
      const [key, job] = queued.entries().next().value
      queued.delete(key)
      if (!settings.data.value) {
        settings.error.value = t('errors.settingsNotReady')
        for (const resolve of job.resolvers) resolve(false)
        continue
      }
      const expectedRevision = Number(settings.data.value.revision) || settingsLastRevision || 0
      try {
        const result = await socket.request({
          settings: { expectedRevision, [job.group]: job.payload }
        })
        const revision = Number(result?.settings?.revision) || 0
        if (revision) {
          // Another client's newer event may already be in `data`; never
          // return the shared revision to an older update result.
          settingsLastRevision = Math.max(settingsLastRevision, revision)
          if (settings.data.value) {
            settings.data.value = {
              ...settings.data.value,
              revision: Math.max(Number(settings.data.value.revision) || 0, revision)
            }
          }
        }
        settings.error.value = ''
        for (const resolve of job.resolvers) resolve(true)
      } catch (err) {
        const message = err?.message || t('errors.updateRejected')
        // A failed CAS means our revision is stale. Re-read instead of
        // guessing a newer local value; drafts stay where the caller owns them.
        await resyncSettings()
        settings.error.value = message
        for (const resolve of job.resolvers) resolve(false)
      }
    }
  } finally {
    draining = false
    settings.pending.value = queued.size > 0
    if (queued.size > 0) drainQueue()
  }
}

/**
 * Share the page's settings view and its single update queue.
 *
 * `update(group, payload)` resolves true on success and false on failure; it
 * never rejects. Payloads queued for the same group collapse to the latest
 * one, and every field-scoped patch is sent against the newest known revision.
 */
export function useSettings() {
  settingsSubscribers += 1
  subscribeSettings()
  onScopeDispose(releaseSettings)

  function update(group, payload) {
    return new Promise((resolve) => {
      const key = patchKey(group, payload)
      const existing = queued.get(key)
      if (existing) {
        // Same mask/resource: merge field writes and keep one resolver set.
        existing.payload = { ...existing.payload, ...payload }
        existing.resolvers.push(resolve)
        return
      }
      queued.set(key, { group, payload, resolvers: [resolve] })
      drainQueue()
    })
  }

  return {
    data: settings.data,
    error: settings.error,
    pending: settings.pending,
    viewEpoch: settings.viewEpoch,
    update
  }
}
