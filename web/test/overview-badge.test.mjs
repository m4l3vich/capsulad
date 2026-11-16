import test from 'node:test'
import assert from 'node:assert/strict'
import {
  CONNECTED_BADGE_LABEL,
  UNKNOWN_DEVICE_NAME,
  connectedBadge,
  connectedBadgeTitle,
  connectedDevices
} from '../src/stores/overviewBluetooth.js'

// The Bluetooth speaker card header shows a green chip only while the overview
// snapshot reports a linked paired device, and its tooltip names them. The view
// binds v-if/title/aria-label straight to this descriptor, so the descriptor is
// the whole contract: null means "render nothing", `title` is what a sighted
// user hovers and what a screen reader announces.

const device = (name, connected, deviceId = `${name?.toLowerCase?.() || 'dev'}-id`) => ({ name, connected, deviceId })

test('badge is present and names both connected paired devices', () => {
  const badge = connectedBadge([
    device('Pixel 7', true),
    device('WH-1000XM4', false),
    device('Kitchen Echo', true)
  ])

  assert.ok(badge, 'expected a badge while a paired device is connected')
  assert.equal(badge.label, CONNECTED_BADGE_LABEL)
  assert.equal(badge.label, 'connected')
  assert.deepEqual(badge.names, ['Pixel 7', 'Kitchen Echo'])
  // Tooltip and accessible name are the same text: names joined with ", ".
  assert.equal(badge.title, 'Connected: Pixel 7, Kitchen Echo')
  assert.match(badge.title, /Pixel 7, Kitchen Echo/)
})

test('badge is absent when no paired device is connected or the snapshot is unavailable', () => {
  assert.equal(connectedBadge([device('Pixel 7', false), device('WH-1000XM4')]), null)
  assert.equal(connectedBadge([]), null)
  assert.equal(connectedBadge(undefined), null)
  assert.equal(connectedBadge(null), null)
  // A partial or malformed snapshot must not produce a chip either.
  assert.equal(connectedBadge({}), null)
  assert.equal(connectedBadge('Pixel 7'), null)
})

test('only an explicit connected flag counts, in snapshot order', () => {
  const devices = connectedDevices([
    device('Pixel 7', true),
    device('Truthy', 'yes'),
    device('Numeric', 1),
    { name: 'Missing flag' },
    device('WH-1000XM4', true)
  ])
  assert.deepEqual(devices.map((entry) => entry.name), ['Pixel 7', 'WH-1000XM4'])
})

test('unnamed connected devices fall back to their id, then to a placeholder', () => {
  const badge = connectedBadge([
    { name: '   ', deviceId: 'aa:bb:cc:dd:ee:ff', connected: true },
    { connected: true }
  ])
  assert.ok(badge)
  assert.deepEqual(badge.names, ['aa:bb:cc:dd:ee:ff', UNKNOWN_DEVICE_NAME])
  assert.equal(badge.title, `Connected: aa:bb:cc:dd:ee:ff, ${UNKNOWN_DEVICE_NAME}`)
})

test('tooltip text joins names with ", " and is empty for no names', () => {
  assert.equal(connectedBadgeTitle(['A']), 'Connected: A')
  assert.equal(connectedBadgeTitle(['A', 'B', 'C']), 'Connected: A, B, C')
  assert.equal(connectedBadgeTitle([]), '')
  assert.equal(connectedBadgeTitle(undefined), '')
  assert.equal(connectedBadgeTitle(['  ', '']), '')
})