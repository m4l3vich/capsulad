import test from 'node:test'
import assert from 'node:assert/strict'
import { deviceHeadline } from '../src/lib/device.js'

// The overview headline is the device's three independent identity facts. A
// missing field must degrade to a readable sentence, never to "undefined" or a
// blank header; the supported profiles are the only hardware names allowed.

test('supported boards name revision, vendor and software', () => {
  assert.equal(deviceHeadline({
    boardRevision: 'Capsula-mini-4',
    model: 'Capsula Mini Rockchip',
    soc: 'Rockchip RK3308',
    targetPlatform: 'CAPSULA_MINI_ROCKCHIP_STOCK',
    firmware: 'Capsula OS'
  }), 'Capsula-mini-4 (Rockchip, stock)')

  assert.equal(deviceHeadline({
    boardRevision: 'Capsula-mini-6',
    soc: 'Amlogic A113X (A113D, meson-axg)',
    targetPlatform: 'CAPSULA_MINI_AMLOGIC_STOCK'
  }), 'Capsula-mini-6 (Amlogic, stock)')

  assert.equal(deviceHeadline({
    boardRevision: 'Capsula-mini-6',
    soc: 'Amlogic A113X',
    targetPlatform: 'CAPSULA_MINI_AMLOGIC_ALPINE'
  }), 'Capsula-mini-6 (Amlogic, Alpine Linux)')
})

test('the simulator reports the host and its simulator software', () => {
  assert.equal(deviceHeadline({
    boardRevision: 'Simulator',
    soc: 'virtual',
    targetPlatform: 'TARGET_PLATFORM_UNSPECIFIED',
    firmware: 'capsulad-simulator'
  }), 'Simulator (host, simulator)')

  // A snapshot that carries the backend platform literal must render the same
  // software type, not fall through to the firmware description.
  assert.equal(deviceHeadline({
    boardRevision: 'Simulator',
    soc: 'virtual',
    targetPlatform: 'simulator',
    firmware: 'capsulad-simulator'
  }), 'Simulator (host, simulator)')

  assert.equal(deviceHeadline({ soc: 'simulator', targetPlatform: 'TARGET_PLATFORM_UNSPECIFIED' }),
    'Unknown device (host, simulator)')
})

test('revision falls back to the probed model before giving up', () => {
  assert.equal(deviceHeadline({
    model: 'VK Capsula Mini (model probe unavailable)',
    soc: 'Amlogic A113X',
    targetPlatform: 'CAPSULA_MINI_AMLOGIC_STOCK'
  }), 'VK Capsula Mini (model probe unavailable) (Amlogic, stock)')
})

test('a missing platform falls back to the firmware, then to simulator', () => {
  assert.equal(deviceHeadline({ boardRevision: 'X', soc: 'Rockchip RK3308', firmware: 'Buildroot 2024' }),
    'X (Rockchip, Buildroot 2024)')
  assert.equal(deviceHeadline({ boardRevision: 'X', soc: 'Rockchip RK3308' }),
    'X (Rockchip, simulator)')
})

test('a missing device stays readable', () => {
  assert.equal(deviceHeadline(undefined), 'Unknown device (unknown SoC, simulator)')
  assert.equal(deviceHeadline(null), 'Unknown device (unknown SoC, simulator)')
  assert.equal(deviceHeadline({}), 'Unknown device (unknown SoC, simulator)')
  assert.equal(deviceHeadline({ boardRevision: '  ', model: '  ', soc: '  ' }),
    'Unknown device (unknown SoC, simulator)')
})
