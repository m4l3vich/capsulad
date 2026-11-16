// Header status for the Bluetooth speaker card.
//
// The daemon reports one row per paired device and marks the link with an
// explicit `connected: true`. Nothing else in the overview snapshot says
// whether a speaker is currently attached, so the badge is derived from those
// rows alone: an absent, malformed or empty `pairedDevices` list means "no
// badge", never a guess.

export const CONNECTED_BADGE_LABEL = 'connected'
export const UNKNOWN_DEVICE_NAME = 'Unknown device'

function deviceName(device) {
  const name = String(device?.name || '').trim()
  if (name) return name
  const id = String(device?.deviceId || '').trim()
  return id || UNKNOWN_DEVICE_NAME
}

/** Paired devices the snapshot says are connected, in snapshot order. */
export function connectedDevices(pairedDevices) {
  if (!Array.isArray(pairedDevices)) return []
  return pairedDevices.filter((device) => device?.connected === true)
}

/**
 * The tooltip/accessible name: every connected device name, joined with ", ".
 * Returns '' for an empty list so callers can treat it as "no badge".
 */
export function connectedBadgeTitle(names) {
  const list = (Array.isArray(names) ? names : []).map(String).filter((name) => name.trim())
  if (!list.length) return ''
  return `Connected: ${list.join(', ')}`
}

/**
 * Badge descriptor, or null when the snapshot has no connected paired device.
 * `label` is the visible chip text; `title` is shared by `title` and
 * `aria-label` so sighted and screen-reader users learn the same device names.
 */
export function connectedBadge(pairedDevices) {
  const devices = connectedDevices(pairedDevices)
  if (!devices.length) return null
  const names = devices.map(deviceName)
  return { label: CONNECTED_BADGE_LABEL, names, title: connectedBadgeTitle(names) }
}