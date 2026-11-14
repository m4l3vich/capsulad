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

export async function scanWifi() {
  const result = await socket.request({ wifi: { scan: {} } }, { timeout: 40000 })
  return result.wifi
}

export async function connectWifi({ ssid, passphrase, security }) {
  const result = await socket.request(
    { wifi: { connect: { ssid, passphrase, security } } },
    { timeout: 60000 }
  )
  return result.wifi
}

export async function forgetWifi(ssid) {
  const result = await socket.request({ wifi: { forget: { ssid } } }, { timeout: 30000 })
  return result.wifi
}
