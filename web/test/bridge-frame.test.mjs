import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import vm from 'node:vm'

// Regression for the injected child bridge: its envelope must keep the
// namespaced protocol type (not the body's own `type`), and it must not post
// before init has locked the per-frame channel. Either bug makes the host
// reject every child message, so the settings frame never becomes ready.

const source = fs.readFileSync(new URL('../../internal/appui/assets/bridge.js', import.meta.url), 'utf8')
const TOKEN = '0123456789abcdef0123456789abcdef'

test('bridge posts only namespaced messages after init', () => {
  const posted = []
  const listeners = {}
  const parent = { postMessage: (message) => posted.push(message) }
  const windowObject = {
    addEventListener(type, fn) {
      ;(listeners[type] ||= []).push(fn)
    }
  }
  const documentObject = {
    documentElement: { scrollHeight: 480 },
    body: { scrollHeight: 480 }
  }
  const context = vm.createContext({
    window: windowObject,
    document: documentObject,
    parent,
    console
  })

  vm.runInContext(source, context)

  // DOMContentLoaded / ResizeObserver ordering must not leak a pre-init post.
  windowObject.CapsuladUI.resize()
  assert.deepEqual(posted, [])

  listeners.message[0]({
    source: parent,
    data: {
      type: 'capsulad.settings-ui/1:init',
      channel: TOKEN,
      appId: 'org.example.clock',
      apiVersion: 1,
      schema: { settings: [] },
      revision: 3,
      values: []
    }
  })

  assert.equal(posted[0].type, 'capsulad.settings-ui/1:ready')
  assert.equal(posted[0].channel, TOKEN)
  assert.equal(posted[0].type.startsWith('capsulad.settings-ui/1:'), true)
  // appId travels host->child on init; child status/patch messages carry only
  // the channel (validateChildMessage treats appId as optional).
  assert.equal(windowObject.CapsuladUI.appId, 'org.example.clock')

  windowObject.CapsuladUI.patch([{ key: 'effect', valueJson: '"static"' }])
  const patch = posted.find((message) => message.type === 'capsulad.settings-ui/1:patch')
  assert.ok(patch, 'patch keeps the protocol namespace')
  assert.equal(patch.channel, TOKEN)
  assert.equal(patch.requestId, 1)
  assert.deepEqual(patch.changes, [{ key: 'effect', valueJson: '"static"' }])
})
