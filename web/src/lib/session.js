import { reactive, watch } from 'vue'
import { t } from './i18n.js'

// Session state and the small HTTP auth surface. Auth mutations are tracked as
// one in-flight promise: a password change can close the live socket before
// its HTTP response (with the fresh cookie) is processed, so the socket-expiry
// recovery waits for that mutation before re-reading the session. Every
// mutation advances an epoch, which prevents an older `/api/session` response
// from overwriting a newer login/logout/password result.

export const session = reactive({
  checked: false,
  authenticated: false,
  expired: false,
  csrf: '',
  version: '',
  platform: '',
  user: ''
})

let epoch = 0
let authMutation = null

/**
 * Run one auth mutation at a time. The work starts synchronously, before a
 * WebSocket close event from the same mutation can be observed.
 */
export function runAuthMutation(work) {
  if (authMutation) return authMutation
  const started = (async () => work())()
  const settled = started.finally(() => {
    if (authMutation === settled) authMutation = null
  })
  authMutation = settled
  return settled
}

/** Resolves once no login/logout/password request is in flight. */
export function authMutationSettled() {
  return authMutation ? authMutation.catch(() => {}) : Promise.resolve()
}

/**
 * The shell owns the live socket through the authoritative session state, not
 * through a callback from a view that the shell may unmount first. Returns the
 * watcher stop handle.
 */
export function watchAuthSocket(sock) {
  return watch(
    () => session.authenticated,
    (authenticated) => {
      if (!session.checked) return
      if (authenticated) {
        session.expired = false
        sock.connect()
        return
      }
      sock.close()
    },
    { immediate: true }
  )
}

function applySession(data) {
  session.authenticated = !!data.authenticated
  session.csrf = data.csrf || ''
  session.version = data.daemon_version || ''
  session.platform = data.platform || ''
  session.user = data.user || ''
}

async function readSession() {
  const resp = await fetch('/api/session', {
    credentials: 'same-origin',
    headers: { Accept: 'application/json' }
  })
  if (!resp.ok) throw new Error(t('errors.sessionEndpoint'))
  return resp.json()
}

export async function loadSession() {
  const startedAt = epoch
  try {
    const data = await readSession()
    if (startedAt !== epoch) return
    applySession(data)
  } catch (_) {
    // A transient read failure does not prove the cookie is invalid; keep the
    // last known identity but stop the login skeleton.
    if (startedAt === epoch) session.authenticated = false
  } finally {
    if (startedAt === epoch) session.checked = true
  }
}

function jsonHeaders() {
  return { 'Content-Type': 'application/json', Accept: 'application/json', 'X-Capsulad-CSRF': session.csrf }
}

async function readError(resp) {
  try {
    const body = await resp.json()
    return body.error || t('errors.requestFailed', { status: resp.status })
  } catch (_) {
    return t('errors.requestFailed', { status: resp.status })
  }
}

export async function login(password, next = '/') {
  epoch += 1
  const resp = await fetch('/api/login', {
    method: 'POST',
    credentials: 'same-origin',
    headers: jsonHeaders(),
    body: JSON.stringify({ password, next })
  })
  if (!resp.ok) throw new Error(await readError(resp))
  const body = await resp.json()
  const data = await readSession()
  applySession(data)
  session.checked = true
  session.expired = false
  return body.next || next
}

export async function logout() {
  epoch += 1
  const resp = await fetch('/api/logout', {
    method: 'POST',
    credentials: 'same-origin',
    headers: jsonHeaders()
  })
  if (!resp.ok) throw new Error(await readError(resp))
  session.authenticated = false
  session.expired = false
  session.csrf = ''
  session.user = ''
}

export async function changePassword(current, next, confirm) {
  epoch += 1
  const resp = await fetch('/api/password', {
    method: 'POST',
    credentials: 'same-origin',
    headers: jsonHeaders(),
    body: JSON.stringify({ current, next, confirm })
  })
  if (!resp.ok) throw new Error(await readError(resp))
  const body = await resp.json()
  // This response also sets a fresh cookie for this browser; read it before
  // resolving so the app can reconnect without guessing at timing.
  const data = await readSession()
  applySession(data)
  session.checked = true
  session.expired = false
  return body.message || t('errors.passwordChanged')
}
