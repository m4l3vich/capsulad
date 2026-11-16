import test from 'node:test'
import assert from 'node:assert/strict'

import { ClientFrame, ErrorCode, ServerFrame, Topic } from '../src/proto/web_pb.js'
import { socket } from '../src/lib/ws.js'

// The transport only inspects WebSocket.OPEN; these tests never open a browser
// connection. They feed decoded server frames into the shared handler, which is
// exactly what onmessage does after a live frame arrives.

if (!globalThis.WebSocket) {
  globalThis.WebSocket = { OPEN: 1, CONNECTING: 0, CLOSING: 2, CLOSED: 3 }
}

function serverFrame(body) {
  return ServerFrame.decode(ServerFrame.encode(body))
}

// Installs a recording transport in the open state and returns a restore
// function. No reconnect timer is scheduled, because the socket never closes.
function openTransport() {
  const previousWs = socket.ws
  const previousState = socket.state
  const previousRetry = socket.retry
  const sent = []
  const transport = {
    readyState: WebSocket.OPEN,
    sent,
    send(bytes) { sent.push(ClientFrame.decode(new Uint8Array(bytes))) },
    close() {}
  }
  const states = []
  const reconnects = []
  const offState = socket.on('state', (state) => states.push(state))
  const offReconnect = socket.on('reconnect', () => reconnects.push(true))
  socket.ws = transport
  socket.setState('open')
  return {
    transport,
    states,
    reconnects,
    restore() {
      for (const waiter of socket.snapshotWaiters.values()) clearTimeout(waiter.timer)
      socket.snapshotWaiters.clear()
      for (const entry of socket.pending.values()) clearTimeout(entry.timer)
      socket.pending.clear()
      socket.ws = previousWs
      socket.setState(previousState)
      socket.retry = previousRetry
      socket.retryTimer = null
      offState()
      offReconnect()
    }
  }
}

test('a refused subscription stays an inline error and leaves the socket open', () => {
  const env = openTransport()
  try {
    const errors = []
    const off = socket.subscribe(Topic.TOPIC_APPLICATION, 'org.capsula.marusia', {
      onSnapshot() {},
      onError(err) { errors.push(err) },
      onResync() {}
    })
    const subscribe = env.transport.sent.at(-1)
    assert.ok(subscribe?.subscribe, 'the subscription reached the transport')
    const epoch = socket.seq

    // What the server sends when the daemon refuses the read: an Error frame
    // correlated with the subscribe request id.
    socket.handleFrame(serverFrame({
      requestId: subscribe.requestId,
      error: { code: ErrorCode.ERROR_CODE_FORBIDDEN, message: 'denied cross-application access' }
    }))
    off()

    assert.equal(socket.state, 'open', 'a refused read must not mark the socket disconnected')
    assert.deepEqual(env.states, ['open'], 'the transport state did not change')
    assert.equal(env.reconnects.length, 0, 'no reconnect was attempted')
    assert.equal(socket.retry, 0)
    assert.equal(socket.retryTimer, null)
    assert.equal(errors.length, 1)
    assert.equal(errors[0].message, 'denied cross-application access')
    assert.equal(errors[0].code, ErrorCode.ERROR_CODE_FORBIDDEN)
    assert.ok(socket.seq > epoch, 'the transport epoch kept counting')
    assert.equal(socket.snapshotWaiters.size, 0, 'the failed subscription stopped waiting')
  } finally {
    env.restore()
  }
})

test('a refused command rejects only its own promise', async () => {
  const env = openTransport()
  try {
    const pending = socket.request({ stop: { appId: 'capsula.ring', runId: 'builtin:capsula.ring' } })
    const command = env.transport.sent.at(-1)
    assert.ok(command?.command, 'the command reached the transport')

    socket.handleFrame(serverFrame({
      requestId: command.requestId,
      error: { code: ErrorCode.ERROR_CODE_CONFLICT, message: 'that run does not belong to this application' }
    }))

    await assert.rejects(pending, (err) => {
      assert.equal(err.message, 'that run does not belong to this application')
      assert.equal(err.code, ErrorCode.ERROR_CODE_CONFLICT)
      return true
    })
    assert.equal(socket.pending.size, 0)
    assert.equal(socket.state, 'open')
    assert.deepEqual(env.states, ['open'])
    assert.equal(env.reconnects.length, 0)
  } finally {
    env.restore()
  }
})

test('a refused request does not disturb a live subscription', () => {
  const env = openTransport()
  try {
    const snapshots = []
    const subscriptionErrors = []
    const off = socket.subscribe(Topic.TOPIC_OVERVIEW, '', {
      onSnapshot(snapshot) { snapshots.push(snapshot) },
      onError(err) { subscriptionErrors.push(err) },
      onResync() {}
    })
    socket.handleFrame(serverFrame({
      requestId: 999999,
      error: { code: ErrorCode.ERROR_CODE_NOT_FOUND, message: 'application is not installed' }
    }))
    assert.equal(socket.state, 'open')
    assert.deepEqual(env.states, ['open'])
    assert.equal(subscriptionErrors.length, 0, 'an unrelated request error is not routed to the subscription')

    // The subscription is still usable: a snapshot for any request id is
    // delivered to the handlers registered for its topic.
    const snapshot = serverFrame({
      requestId: 999998,
      snapshot: { topic: Topic.TOPIC_OVERVIEW, revision: 3, overview: { revision: 0, device: { model: 'Capsula Mini' } } }
    })
    const subscriptionKey = `${Topic.TOPIC_OVERVIEW}|`
    socket.deliverSnapshot(subscriptionKey, snapshot.snapshot)
    assert.equal(snapshots.length, 1)
    assert.equal(snapshots[0].overview.device.model, 'Capsula Mini')
    off()
  } finally {
    env.restore()
  }
})