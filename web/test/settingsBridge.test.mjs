import test from 'node:test'
import assert from 'node:assert/strict'
import {
  BRIDGE_PREFIX,
  validateChildMessage,
  createChannelToken,
  isBridgeProtocolMessage
} from '../src/lib/appSettingsBridge.js'

const TOKEN = '0123456789abcdef0123456789abcdef'
const keys = new Set(['effect', 'brightness'])
const base = { token: TOKEN, appId: 'org.example.app', settingKeys: keys }

test('channel tokens are per-frame 128-bit hex', () => {
  const a = createChannelToken()
  const b = createChannelToken()
  assert.match(a, /^[0-9a-f]{32}$/)
  assert.match(b, /^[0-9a-f]{32}$/)
  assert.notEqual(a, b)
})

test('unrelated frame chatter is ignored, not treated as protocol', () => {
  assert.equal(isBridgeProtocolMessage({ type: BRIDGE_PREFIX + 'ready' }), true)
  assert.equal(isBridgeProtocolMessage({ type: 'ready' }), false)
  assert.equal(isBridgeProtocolMessage({ type: 'resize' }), false)
  assert.equal(isBridgeProtocolMessage('page instrumentation'), false)
  assert.equal(isBridgeProtocolMessage(null), false)
})

test('only the exact source token and app are accepted', () => {
  const msg = { type: BRIDGE_PREFIX + 'ready', channel: TOKEN }
  assert.equal(validateChildMessage(msg, base).kind, 'ready')
  assert.match(validateChildMessage({ ...msg, channel: 'f'.repeat(32) }, base).error, /channel/)
  assert.match(validateChildMessage({ ...msg, appId: 'org.example.other' }, base).error, /application/)
  assert.match(validateChildMessage({ type: 'other:ready', channel: TOKEN }, base).error, /type/)
  assert.match(validateChildMessage(null, base).error, /object/)
})

test('declared settings only, exactly one payload form', () => {
  const patch = (changes) => ({
    type: BRIDGE_PREFIX + 'patch',
    channel: TOKEN,
    appId: 'org.example.app',
    requestId: 1,
    changes
  })
  assert.deepEqual(validateChildMessage(patch([{ key: 'effect', valueJson: '"static"' }]), base).changes, [
    { key: 'effect', valueJson: '"static"' }
  ])
  assert.equal(validateChildMessage(patch([{ key: 'effect', clear: true }]), base).kind, 'patch')
  assert.match(validateChildMessage(patch([{ key: 'speed', valueJson: '1' }]), base).error, /undeclared/)
  assert.match(validateChildMessage(patch([{ key: 'effect' }]), base).error, /exactly one/)
  assert.match(validateChildMessage(patch([{ key: 'effect', valueJson: '"a"', clear: true }]), base).error, /exactly one/)
  assert.match(validateChildMessage(patch([{ key: 'effect', valueJson: '{' }]), base).error, /JSON/)
  assert.match(validateChildMessage(patch([{ key: 'effect', valueJson: 'x'.repeat(9000) }]), base).error, /large/)
  assert.match(validateChildMessage(patch(Array.from({ length: 65 }, () => ({ key: 'effect', clear: true }))), base).error, /1 to 64/)
})

test('resize is bounded and cannot inject DOM access', () => {
  const msg = (height) => ({ type: BRIDGE_PREFIX + 'resize', channel: TOKEN, height })
  assert.equal(validateChildMessage(msg(4000), base).height, 4000)
  assert.match(validateChildMessage(msg(4001), base).error, /height/)
  assert.match(validateChildMessage(msg(-1), base).error, /height/)
  assert.match(validateChildMessage({ ...msg(300), appId: 'org.example.other' }, base).error, /application/)
})
