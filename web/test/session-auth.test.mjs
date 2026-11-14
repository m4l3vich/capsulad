import test from 'node:test'
import assert from 'node:assert/strict'
import { nextTick } from 'vue'

// The shell must connect the socket from the authoritative session store, not
// from an event emitted by the login view after its await. By then the shell
// has already replaced that view, and Vue drops the emit. This test pins the
// consumer-visible contract: the session watcher connects the socket during
// login and closes it on logout, with no view callback involved.

let authenticated = false
const requests = []

function reply(body, status = 200) {
  return {
    ok: status < 400,
    status,
    async json() {
      return body
    }
  }
}

globalThis.fetch = async (url, options = {}) => {
  requests.push({ url, method: options.method || 'GET' })
  if (url === '/api/session') {
    return reply({
      authenticated,
      csrf: authenticated ? 'csrf' : '',
      daemon_version: 'test',
      platform: 'simulator',
      user: authenticated ? 'admin' : ''
    })
  }
  if (url === '/api/login') {
    authenticated = true
    return reply({ ok: true, next: '/settings' })
  }
  if (url === '/api/logout') {
    authenticated = false
    return reply({ ok: true })
  }
  throw new Error(`unexpected fetch ${url}`)
}

function fakeSocket() {
  return {
    connects: 0,
    closes: 0,
    connect() {
      this.connects += 1
    },
    close() {
      this.closes += 1
    }
  }
}

test('login and logout drive the shell socket from session state alone', async () => {
  const { session, loadSession, login, logout, runAuthMutation, watchAuthSocket } = await import('../src/lib/session.js')
  const socket = fakeSocket()
  const stop = watchAuthSocket(socket)

  await loadSession()
  assert.equal(session.checked, true)
  assert.equal(session.authenticated, false)
  assert.equal(socket.connects, 0, 'signed-out load does not open a socket')

  const target = await runAuthMutation(() => login('secret', '/settings'))
  await nextTick()
  assert.equal(target, '/settings')
  assert.equal(session.authenticated, true)
  assert.equal(socket.connects, 1, 'the authenticated transition connects without a view event')

  await runAuthMutation(() => logout())
  await nextTick()
  assert.equal(session.authenticated, false)
  assert.equal(socket.closes, 1)

  stop()
  assert.deepEqual(requests.map((request) => request.url), ['/api/session', '/api/login', '/api/session', '/api/logout'])
})
