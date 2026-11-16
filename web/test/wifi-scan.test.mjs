import test from 'node:test'
import assert from 'node:assert/strict'

import { ClientFrame, ServerFrame } from '../src/proto/web_pb.js'
import { socket } from '../src/lib/ws.js'
import { connectWifi, forgetWifi, scanWifi } from '../src/lib/wifi.js'

// The transport only inspects WebSocket.OPEN; these tests never open a browser
// connection. They feed decoded server frames into the shared handler, which is
// exactly what onmessage does after a live frame arrives.
//
// Every Wi-Fi command is answered with a capsulad.web.v1.Result whose `wifi`
// field is a WifiResult { message, wifi: WifiView }. The helpers must unwrap
// that nested view; returning the wrapper made the page read an undefined
// revision and an empty scan list, so a successful scan looked like no
// networks at all.

if (!globalThis.WebSocket) {
  globalThis.WebSocket = { OPEN: 1, CONNECTING: 0, CLOSING: 2, CLOSED: 3 }
}

function serverFrame(body) {
  return ServerFrame.decode(ServerFrame.encode(body))
}

const wifiView = {
  revision: 42,
  available: true,
  state: 'connected',
  interface: 'wlan0',
  ssid: 'home-net',
  knownNetworks: [{ ssid: 'home-net', connected: true, hasPassword: true }],
  scanResults: [
    { ssid: 'home-net', security: 'wpa2', signalDbm: -41, frequencyMhz: 5180, connected: true },
    { ssid: 'neighbour', security: 'wpa2', signalDbm: -73, frequencyMhz: 2412, connected: false }
  ],
  scanSupported: true
}

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
  socket.ws = transport
  socket.setState('open')
  return {
    transport,
    restore() {
      for (const waiter of socket.snapshotWaiters.values()) clearTimeout(waiter.timer)
      socket.snapshotWaiters.clear()
      for (const entry of socket.pending.values()) clearTimeout(entry.timer)
      socket.pending.clear()
      socket.ws = previousWs
      socket.setState(previousState)
      socket.retry = previousRetry
      socket.retryTimer = null
    }
  }
}

// Starts a Wi-Fi command, answers it with the daemon's real Result shape, and
// returns whatever the helper resolved to.
async function commandResult(start, wifiResult) {
  const env = openTransport()
  try {
    const pending = start()
    const request = env.transport.sent.at(-1)
    assert.ok(request, 'the command reached the transport')
    socket.handleFrame(serverFrame({
      requestId: request.requestId,
      result: { wifi: wifiResult ?? { message: 'ok', wifi: wifiView } }
    }))
    return await pending
  } finally {
    env.restore()
  }
}

test('scanWifi resolves to the WifiView so scan results reach the page', async () => {
  const view = await commandResult(() => scanWifi(), {
    message: 'scan complete',
    wifi: wifiView
  })

  assert.equal(view.revision, wifiView.revision)
  assert.equal(view.ssid, 'home-net')
  assert.equal(view.scanResults?.length, 2, 'scan results survived unwrapping')
  assert.equal(view.scanResults[1].ssid, 'neighbour')
})

test('connectWifi resolves to the WifiView', async () => {
  const view = await commandResult(() =>
    connectWifi({ ssid: 'home-net', passphrase: 'secret', security: 'wpa2' })
  )

  assert.equal(view.revision, wifiView.revision)
  assert.equal(view.state, 'connected')
  assert.equal(view.knownNetworks?.[0].ssid, 'home-net')
})

test('forgetWifi resolves to the WifiView', async () => {
  const view = await commandResult(() => forgetWifi('home-net'))

  assert.equal(view.revision, wifiView.revision)
  assert.equal(view.knownNetworks?.[0].ssid, 'home-net')
})

test('a command result without a WifiView is a protocol violation', async () => {
  await assert.rejects(
    () => commandResult(() => scanWifi(), { message: 'scan complete' }),
    (err) => {
      assert.ok(err instanceof Error)
      assert.match(err.message, /wifi/i)
      return true
    }
  )
})