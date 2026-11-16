import { Topic } from '../proto/web_pb.js'
import { socket } from './ws.js'

// Small adapter over the one authenticated WebSocket. Wi-Fi snapshots never
// scan; scanWifi() is the only call that submits the explicit scan command.
export const wifiTopic = Topic.TOPIC_WIFI

export function subscribeWifi(handlers) {
  return socket.subscribe(wifiTopic, '', handlers)
}

export async function fetchWifi() {
  const snap = await socket.requestSnapshot(wifiTopic)
  return snap.wifi
}

// A command resolves with the whole Result. Its `wifi` field is a WifiResult
// { message, wifi: WifiView }, so helpers must unwrap the nested view: callers
// adopt a WifiView (revision, knownNetworks, scanResults) exactly like the
// snapshot path does. A missing view is a protocol violation, never an empty
// view, so a broken contract fails loudly instead of rendering "no networks".
function wifiView(result, command) {
  const view = result?.wifi?.wifi
  if (!view) throw new Error(`malformed Wi-Fi ${command} response: missing WifiView`)
  return view
}

export async function scanWifi() {
  const result = await socket.request({ wifi: { scan: {} } }, { timeout: 40000 })
  return wifiView(result, 'scan')
}

export async function connectWifi({ ssid, passphrase, security }) {
  const result = await socket.request(
    { wifi: { connect: { ssid, passphrase, security } } },
    { timeout: 60000 }
  )
  return wifiView(result, 'connect')
}

export async function forgetWifi(ssid) {
  const result = await socket.request({ wifi: { forget: { ssid } } }, { timeout: 30000 })
  return wifiView(result, 'forget')
}
