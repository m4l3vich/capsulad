import test from 'node:test'
import assert from 'node:assert/strict'
import { effectScope, nextTick } from 'vue'
import { ServerFrame, Topic } from '../src/proto/web_pb.js'
import { socket } from '../src/lib/ws.js'
import { useTopic, useSettings } from '../src/lib/live.js'

// Ordering is carried by ServerFrame.snapshot.revision / event.revision, not
// by the nested view payload (many views have revision 0). These tests feed
// real generated frames through the shared handlers.

function frame(body) {
  return ServerFrame.decode(ServerFrame.encode(body))
}

// Protobuf floats are float32 on the wire, so decoded values use tolerant
// comparison. Revision and identity assertions stay exact.
function assertFloat(actual, expected, message) {
  assert.ok(
    Math.abs(actual - expected) <= 1e-6,
    message || `expected ${actual} to be within 1e-6 of ${expected}`
  )
}

function overviewSnapshot(revision, model) {
  return frame({
    snapshot: {
      topic: Topic.TOPIC_OVERVIEW,
      revision,
      overview: { revision: 0, device: { model } }
    }
  }).snapshot
}

function overviewEvent(revision, model) {
  return frame({
    event: {
      topic: Topic.TOPIC_OVERVIEW,
      revision,
      overview: { revision: 0, device: { model } }
    }
  }).event
}

function settingsSnapshot(envelope, storeRevision, masterVolume) {
  return frame({
    snapshot: {
      topic: Topic.TOPIC_SETTINGS,
      revision: envelope,
      settings: { revision: storeRevision, values: { masterVolume } }
    }
  }).snapshot
}

function settingsEvent(envelope, storeRevision, masterVolume) {
  return frame({
    event: {
      topic: Topic.TOPIC_SETTINGS,
      revision: envelope,
      settings: { revision: storeRevision, values: { masterVolume } }
    }
  }).event
}

function applicationSnapshot(envelope, displayName, logs) {
  return frame({
    snapshot: {
      topic: Topic.TOPIC_APPLICATION,
      appId: 'org.example.app',
      revision: envelope,
      application: {
        revision: 0,
        app: { appId: 'org.example.app', displayName, version: '1' },
        logs
      }
    }
  }).snapshot
}

function applicationLogEvent(envelope, message) {
  return frame({
    event: {
      topic: Topic.TOPIC_APPLICATION,
      appId: 'org.example.app',
      revision: envelope,
      logs: { entries: [{ monotonicTimeNs: 2, component: 'app', appId: 'org.example.app', severity: 'INFO', message }] }
    }
  }).event
}

function diagnosticsSnapshot(envelope, model) {
  return frame({
    snapshot: {
      topic: Topic.TOPIC_DIAGNOSTICS,
      revision: envelope,
      diagnostics: { revision: 0, device: { model } }
    }
  }).snapshot
}

function diagnosticsMetricsEvent(envelope) {
  return frame({
    event: {
      topic: Topic.TOPIC_DIAGNOSTICS,
      revision: envelope,
      diagnosticsMetrics: { available: true, cpuUsagePercent: 12.5 }
    }
  }).event
}

test('a stale envelope snapshot cannot overwrite a newer topic event', async () => {
  const originalSubscribe = socket.subscribe
  const originalRequestSnapshot = socket.requestSnapshot
  let handler = null
  socket.subscribe = (topic, appId, nextHandler) => {
    handler = nextHandler
    return () => {}
  }

  const scope = effectScope()
  const view = scope.run(() => useTopic('overview'))
  try {
    handler.onEvent(overviewEvent(7, 'new'))
    assert.equal(view.data.value.device.model, 'new')

    handler.onSnapshot(overviewSnapshot(6, 'stale'))
    assert.equal(view.data.value.device.model, 'new', 'stale snapshot is ignored')

    socket.requestSnapshot = async () => overviewSnapshot(5, 'older-refresh')
    await view.refresh()
    assert.equal(view.data.value.device.model, 'new', 'stale resync is ignored')

    // Envelope revisions are per connection.
    socket.emit('reconnect')
    handler.onSnapshot(overviewSnapshot(1, 'after-reconnect'))
    assert.equal(view.data.value.device.model, 'after-reconnect')
  } finally {
    scope.stop()
    socket.subscribe = originalSubscribe
    socket.requestSnapshot = originalRequestSnapshot
  }
})

test('a newer partial log survives an older reserved-envelope application snapshot', () => {
  const originalSubscribe = socket.subscribe
  let handler = null
  socket.subscribe = (topic, appId, nextHandler) => {
    handler = nextHandler
    return () => {}
  }

  const scope = effectScope()
  const view = scope.run(() => useTopic('application', 'org.example.app', { logs: true }))
  try {
    // Reserved snapshot rev 10 is still reading; a newer log arrives first.
    handler.onEvent(applicationLogEvent(11, 'new-log'))
    assert.equal(view.data.value, null)

    handler.onSnapshot(applicationSnapshot(10, 'snapshot-app', []))
    assert.equal(view.data.value.app.displayName, 'snapshot-app', 'full snapshot still updates full state')
    assert.equal(view.data.value.logs.length, 1)
    assert.equal(view.data.value.logs[0].message, 'new-log')
    assert.equal(view.appliedRevision.value, 10)

    // The same partial log must not be counted twice after later snapshots.
    handler.onSnapshot(applicationSnapshot(12, 'next-app', [
      { monotonicTimeNs: 2, component: 'app', appId: 'org.example.app', severity: 'INFO', message: 'new-log' }
    ]))
    assert.equal(view.data.value.logs.length, 1)
    assert.equal(view.data.value.logs[0].message, 'new-log')

    // An older full frame must not roll back full fields, but its logs still
    // merge into the journal.
    handler.onSnapshot(applicationSnapshot(9, 'should-not-apply', [
      { monotonicTimeNs: 1, component: 'app', appId: 'org.example.app', severity: 'INFO', message: 'old-log' }
    ]))
    assert.equal(view.data.value.app.displayName, 'next-app')
    assert.deepEqual(view.data.value.logs.map((entry) => entry.message), ['old-log', 'new-log'])
    assert.equal(view.appliedRevision.value, 12)

    // A new connection is a new log epoch: the old journal is dropped instead
    // of being merged and sorted against the restarted monotonic clock.
    socket.emit('reconnect')
    assert.equal(view.appliedRevision.value, 0)
    assert.deepEqual(view.data.value.logs, [])
    handler.onSnapshot(applicationSnapshot(1, 'after-restart', [
      { monotonicTimeNs: 1, component: 'app', appId: 'org.example.app', severity: 'INFO', message: 'fresh-log' }
    ]))
    assert.equal(view.data.value.app.displayName, 'after-restart')
    assert.deepEqual(view.data.value.logs.map((entry) => entry.message), ['fresh-log'])
    assert.equal(view.appliedRevision.value, 1)
  } finally {
    scope.stop()
    socket.subscribe = originalSubscribe
  }
})

test('partial metrics do not advance the full diagnostics watermark', () => {
  const originalSubscribe = socket.subscribe
  let handler = null
  socket.subscribe = (topic, appId, nextHandler) => {
    handler = nextHandler
    return () => {}
  }

  const scope = effectScope()
  const view = scope.run(() => useTopic('diagnostics'))
  try {
    handler.onEvent(diagnosticsMetricsEvent(12))
    assert.equal(view.data.value, null, 'metrics are page-owned for logs:false')
    assert.equal(view.appliedRevision.value, 0)

    handler.onSnapshot(diagnosticsSnapshot(10, 'snapshot-health'))
    assert.equal(view.data.value.device.model, 'snapshot-health')
    assert.equal(view.appliedRevision.value, 10)

    // The partial event's number does not make a lower-envelope full event
    // look older than the accepted full view.
    handler.onEvent(frame({
      event: {
        topic: Topic.TOPIC_DIAGNOSTICS,
        revision: 9,
        diagnostics: { revision: 0, device: { model: 'stale-full' } }
      }
    }).event)
    assert.equal(view.data.value.device.model, 'snapshot-health')
  } finally {
    scope.stop()
    socket.subscribe = originalSubscribe
  }
})

test('settings ordering uses the envelope while CAS revision stays monotonic', async () => {
  const originalSubscribe = socket.subscribe
  const originalRequest = socket.request
  const originalRequestSnapshot = socket.requestSnapshot
  let handler = null
  socket.subscribe = (topic, appId, nextHandler) => {
    if (topic === Topic.TOPIC_SETTINGS) handler = nextHandler
    return () => {}
  }
  socket.requestSnapshot = async () => settingsSnapshot(9, 1, 0.5)

  const calls = []
  const waits = []
  socket.request = (command) => {
    calls.push(command)
    return new Promise((resolve, reject) => waits.push({ resolve, reject }))
  }

  const scope = effectScope()
  let settings
  try {
    scope.run(() => {
      settings = useSettings()
    })
    handler.onSnapshot(settingsSnapshot(9, 1, 0.5))
    assertFloat(settings.data.value.values.masterVolume, 0.5)

    handler.onEvent(settingsEvent(11, 2, 0.7))
    const epochAfterEvent = settings.viewEpoch.value
    handler.onSnapshot(settingsSnapshot(10, 3, 0.1))
    assertFloat(settings.data.value.values.masterVolume, 0.7, 'stale snapshot is ignored')
    assert.equal(settings.data.value.revision, 2, 'store revision is not rolled back')
    assert.equal(settings.viewEpoch.value, epochAfterEvent, 'stale snapshot is not an applied view')

    const before = calls.length
    const first = settings.update('display', {
      resourceId: 'display.segment.front',
      updateFields: ['default_app'],
      defaultApp: 'a'
    })
    const second = settings.update('display', {
      resourceId: 'light.ring.main',
      updateFields: ['default_app'],
      defaultApp: 'b'
    })
    const third = settings.update('display', {
      resourceId: 'display.segment.front',
      updateFields: ['brightness'],
      brightness: 0.4
    })
    assert.equal(calls.length, before + 1, 'first patch is in flight')
    waits[before].resolve({ settings: { revision: 4 } })
    assert.equal(await first, true)
    await nextTick()
    assert.equal(calls.length, before + 2)
    waits[before + 1].resolve({ settings: { revision: 5 } })
    assert.equal(await second, true)
    await nextTick()
    assert.equal(calls.length, before + 3)
    waits[before + 2].resolve({ settings: { revision: 6 } })
    assert.equal(await third, true)

    assert.equal(calls[before].settings.display.resourceId, 'display.segment.front')
    assert.equal(calls[before + 1].settings.display.resourceId, 'light.ring.main')
    assert.equal(calls[before + 2].settings.display.resourceId, 'display.segment.front')
    assert.deepEqual(calls[before + 2].settings.display.updateFields, ['brightness'])

    // Different muted-indicator masks keep separate jobs; neither is dropped.
    const maskStart = calls.length
    const mode = settings.update('mutedIndicator', { mode: 2, hasMode: true })
    const delay = settings.update('mutedIndicator', { delayS: 30, hasDelay: true })
    assert.equal(calls.length, maskStart + 1)
    waits[maskStart].resolve({ settings: { revision: 7 } })
    assert.equal(await mode, true)
    await nextTick()
    assert.equal(calls.length, maskStart + 2)
    waits[maskStart + 1].resolve({ settings: { revision: 8 } })
    assert.equal(await delay, true)
    assert.equal(calls[maskStart].settings.mutedIndicator.hasMode, true)
    assert.equal(calls[maskStart + 1].settings.mutedIndicator.hasDelay, true)

    // Identical mask/resource coalesces while another patch is in flight.
    const gateStart = calls.length
    const gate = settings.update('display', {
      resourceId: 'light.ring.main',
      updateFields: ['default_app'],
      defaultApp: 'gate'
    })
    assert.equal(calls.length, gateStart + 1)
    const volumeStart = calls.length
    const lower = settings.update('volume', { masterVolume: 0.3 })
    const higher = settings.update('volume', { masterVolume: 0.6 })
    assert.equal(calls.length, volumeStart, 'volume patches wait behind the in-flight display patch')
    waits[gateStart].resolve({ settings: { revision: 9 } })
    assert.equal(await gate, true)
    await nextTick()
    assert.equal(calls.length, volumeStart + 1, 'one coalesced volume patch')
    assert.equal(calls[volumeStart].settings.volume.masterVolume, 0.6)
    waits[volumeStart].resolve({ settings: { revision: 10 } })
    assert.deepEqual(await Promise.all([lower, higher]), [true, true])
    assert.equal(settings.data.value.revision, 10)

    // An authoritative view can arrive with the same numeric volume while a
    // control's request is pending; the epoch is the marker VolumeControl
    // reconciles on, so it must advance even without a value change.
    const epochBeforeSameValue = settings.viewEpoch.value
    handler.onEvent(settingsEvent(20, 20, 0.7))
    assert.equal(settings.viewEpoch.value, epochBeforeSameValue + 1)
    assertFloat(settings.data.value.values.masterVolume, 0.7)
  } finally {
    scope.stop()
    socket.subscribe = originalSubscribe
    socket.request = originalRequest
    socket.requestSnapshot = originalRequestSnapshot
  }
})
