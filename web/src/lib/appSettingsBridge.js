// Parent side of the application settings bridge.
//
// The child is an untrusted, opaque-origin iframe. Every message is accepted
// only when it comes from that exact frame, carries the per-frame random
// channel token, stays within size limits, and describes settings patches for
// keys declared by the app whose page hosts the frame. There is no generic
// command, service, app-bus or cross-app operation.

export const BRIDGE_PREFIX = 'capsulad.settings-ui/1:'
export const BRIDGE_API_VERSION = 1
export const MAX_CHILD_MESSAGE_BYTES = 64 << 10
export const MAX_PATCH_CHANGES = 64
export const MAX_PATCH_VALUE_BYTES = 8 << 10
export const MAX_PATCH_TOTAL_BYTES = 32 << 10
export const MAX_QUEUE = 16

const HEX32 = /^[0-9a-f]{32}$/

export const FRAME_THEMES = Object.freeze(['light', 'dark'])

/** The frame needs only the resolved palette, not the user's mode. */
export function normalizeTheme(value) {
  return value === 'dark' ? 'dark' : 'light'
}

function themeFragment(url, theme) {
  const base = String(url).split('#')[0]
  return base + '#capsulad-theme=' + normalizeTheme(theme)
}

export function createChannelToken() {
  const bytes = new Uint8Array(16)
  if (globalThis.crypto?.getRandomValues) {
    globalThis.crypto.getRandomValues(bytes)
  } else {
    // Only reached in non-browser test environments without WebCrypto. This
    // is still a per-frame value, never a security boundary decision on its
    // own; the source check always runs too.
    for (let i = 0; i < bytes.length; i += 1) bytes[i] = Math.floor(Math.random() * 256)
  }
  return [...bytes].map((b) => b.toString(16).padStart(2, '0')).join('')
}

function isPlainObject(value) {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

/** True only for messages in the settings protocol namespace. */
export function isBridgeProtocolMessage(raw) {
  return isPlainObject(raw) && typeof raw.type === 'string' && raw.type.startsWith(BRIDGE_PREFIX)
}

function byteLength(text) {
  if (typeof TextEncoder !== 'undefined') return new TextEncoder().encode(text).length
  return unescape(encodeURIComponent(text)).length
}

/**
 * Validate a child frame message. Returns {kind, ...} on success or {error}.
 * `settingKeys` is the declared setting-key allowlist for this app.
 */
export function validateChildMessage(raw, { token, appId, settingKeys }) {
  if (!isPlainObject(raw)) return { error: 'message is not an object' }
  if (typeof raw.type !== 'string' || !raw.type.startsWith(BRIDGE_PREFIX)) return { error: 'unknown message type' }
  let encoded
  try {
    encoded = JSON.stringify(raw)
  } catch (_) {
    return { error: 'message is not serializable' }
  }
  if (byteLength(encoded) > MAX_CHILD_MESSAGE_BYTES) return { error: 'message exceeds the frame limit' }
  if (typeof raw.channel !== 'string' || !HEX32.test(raw.channel) || raw.channel !== token) {
    return { error: 'channel does not match this frame' }
  }
  if (raw.appId !== undefined && raw.appId !== appId) return { error: 'message names a different application' }

  const kind = raw.type.slice(BRIDGE_PREFIX.length)
  if (kind === 'ready') return { kind: 'ready' }

  if (kind === 'resize') {
    const height = raw.height
    if (!Number.isInteger(height) || height < 20 || height > 4000) return { error: 'invalid frame height' }
    return { kind: 'resize', height }
  }

  if (kind === 'patch') {
    if (!Number.isInteger(raw.requestId) || raw.requestId < 1 || raw.requestId > 1e9) {
      return { error: 'patch needs a request id' }
    }
    if (!Array.isArray(raw.changes) || raw.changes.length === 0 || raw.changes.length > MAX_PATCH_CHANGES) {
      return { error: 'patch needs 1 to ' + MAX_PATCH_CHANGES + ' changes' }
    }
    let total = 0
    const changes = []
    for (const change of raw.changes) {
      if (!isPlainObject(change)) return { error: 'change is not an object' }
      const key = change.key
      if (typeof key !== 'string' || !settingKeys.has(key)) {
        return { error: 'change names an undeclared setting' }
      }
      const hasValue = typeof change.valueJson === 'string'
      const clear = change.clear === true
      if (hasValue === clear) {
        return { error: 'change needs exactly one of valueJson or clear' }
      }
      if (hasValue) {
        if (byteLength(change.valueJson) > MAX_PATCH_VALUE_BYTES) return { error: 'setting value is too large' }
        try {
          JSON.parse(change.valueJson)
        } catch (_) {
          return { error: 'setting value is not valid JSON' }
        }
        total += byteLength(change.valueJson)
        changes.push({ key, valueJson: change.valueJson })
      } else {
        if (change.clear !== true) return { error: 'clear must be true' }
        changes.push({ key, clear: true })
      }
    }
    if (total > MAX_PATCH_TOTAL_BYTES) return { error: 'patch is too large' }
    return { kind: 'patch', requestId: raw.requestId, changes }
  }

  return { error: 'unknown message type' }
}

function normalizeValues(values) {
  if (!Array.isArray(values)) return []
  return values.map((v) => ({
    key: String(v.key || ''),
    valueJson: typeof v.valueJson === 'string' ? v.valueJson : '',
    hasValue: !!v.hasValue
  }))
}

/**
 * Hosts one settings iframe for one mounted application page. It owns the
 * channel token, serializes patches, and invalidates everything on dispose so
 * a stale/navigated frame cannot touch a newer page.
 */
export class SettingsFrameHost {
  constructor({ iframe, appId, schema, revision, values, applyChanges, onStatus, theme: frameTheme }) {
    this.iframe = iframe
    this.appId = appId
    this.schema = schema || { settings: [] }
    this.revision = Number(revision) || 0
    this.values = normalizeValues(values)
    this.theme = normalizeTheme(frameTheme)
    this.token = createChannelToken()
    this.keys = new Set((this.schema.settings || []).map((s) => s.key))
    this.applyChanges = applyChanges
    this.onStatus = onStatus || (() => {})
    this.queue = []
    this.inflight = false
    this.mounted = true
    this.ready = false
    this.navigated = false
    this.pendingURL = ''
    this.initSent = false
    this.violated = false

    this.handleMessage = this.handleMessage.bind(this)
    this.handleLoad = this.handleLoad.bind(this)
    window.addEventListener('message', this.handleMessage)
    this.iframe.addEventListener('load', this.handleLoad)
    // Deterministic barrier: the first load is always a local about:blank
    // document that consumes the navigation step; init is only posted on the
    // second load, for the authenticated URL we chose.
    this.iframe.src = 'about:blank#capsulad-settings'
  }

  // arm records the authenticated document URL after the listener/token
  // exist. Navigation happens on the barrier's first load.
  arm(url) {
    if (!this.mounted || this.navigated || !url) return false
    this.pendingURL = themeFragment(url, this.theme)
    return true
  }

  handleLoad() {
    if (!this.mounted || this.violated) return
    if (!this.navigated) {
      this.navigated = true
      if (this.pendingURL) {
        this.iframe.src = this.pendingURL
      } else {
        this.onStatus?.({ error: 'The settings document URL is unavailable.' })
      }
      return
    }
    if (this.initSent) {
      // Any further load is a navigation or reload. The injected document has
      // no legitimate reason to leave its own URL, and re-initializing would
      // hand the token/schema/values to a document we no longer control.
      this.violated = true
      this.ready = false
      this.queue = []
      this.onStatus?.({ error: 'The settings frame navigated away; reload this application page.' })
      try { this.iframe.src = 'about:blank' } catch (_) { /* frame detached */ }
      return
    }
    this.initSent = true
    this.post({
      type: BRIDGE_PREFIX + 'init',
      channel: this.token,
      appId: this.appId,
      apiVersion: BRIDGE_API_VERSION,
      schema: this.schema,
      revision: this.revision,
      values: this.values,
      theme: this.theme
    })
  }

  handleMessage(event) {
    if (!this.mounted || this.violated || !this.initSent) return
    if (event.source !== this.iframe.contentWindow) return
    // A sandboxed frame without allow-same-origin has an opaque origin.
    // Reject any message that claims a real origin.
    if (event.origin !== 'null') return
    // The frame may post its own unrelated chatter (the app document, a
    // browser instrumentation hook). Only our namespaced protocol is
    // actionable; ignoring anything else cannot grant a privilege.
    const raw = event.data
    if (!isBridgeProtocolMessage(raw)) return
    const result = validateChildMessage(raw, {
      token: this.token,
      appId: this.appId,
      settingKeys: this.keys
    })
    if (result.error) {
      this.onStatus?.({ error: result.error })
      return
    }
    if (result.kind === 'ready') {
      this.ready = true
      this.onStatus?.({ ready: true })
      this.pump()
      return
    }
    if (result.kind === 'resize') {
      if (this.iframe.style.height !== result.height + 'px') this.iframe.style.height = result.height + 'px'
      return
    }
    if (result.kind === 'patch') {
      if (!this.ready) {
        this.sendResult(result.requestId, { ok: false, error: 'frame is not ready' })
        return
      }
      if (this.queue.length >= MAX_QUEUE) {
        this.sendResult(result.requestId, { ok: false, error: 'too many pending updates; retry after the next state' })
        return
      }
      this.queue.push(result)
      this.pump()
    }
  }

  async pump() {
    if (!this.mounted || this.inflight || !this.ready || this.queue.length === 0) return
    const next = this.queue.shift()
    this.inflight = true
    try {
      const result = await this.applyChanges(next.changes, this.revision)
      if (!this.mounted) return
      if (result?.ok) {
        this.revision = Number(result.revision) || this.revision
        this.values = normalizeValues(result.values || this.values)
        this.sendResult(next.requestId, {
          ok: true,
          revision: this.revision,
          message: result.message || 'Saved.'
        })
        this.sendState()
      } else {
        // The caller refreshes the authoritative snapshot and calls
        // updateState() before the child retries.
        if (typeof result?.revision === 'number') this.revision = result.revision
        if (result?.values) this.values = normalizeValues(result.values)
        this.sendResult(next.requestId, {
          ok: false,
          revision: this.revision,
          error: result?.error || 'The settings update was rejected.'
        })
        this.sendState()
      }
    } catch (err) {
      if (this.mounted) {
        this.sendResult(next.requestId, { ok: false, error: err?.message || 'The settings update failed.' })
      }
    } finally {
      this.inflight = false
      if (this.mounted) this.pump()
    }
  }

  /**
   * Forward a resolved palette into an open frame. This is an attribute swap
   * in the child: no reload, no init, no state/results are replayed, so drafts,
   * pending patches, focus and scroll position are untouched. Before init the
   * theme rides on the URL fragment and init itself.
   */
  setTheme(theme) {
    const next = normalizeTheme(theme)
    if (!this.mounted || this.violated || this.theme === next) return
    this.theme = next
    if (!this.initSent) return
    this.post({
      type: BRIDGE_PREFIX + 'theme',
      channel: this.token,
      appId: this.appId,
      theme: next
    })
  }

  updateState({ revision, values }) {
    if (!this.mounted) return
    this.revision = Number(revision) || this.revision
    this.values = normalizeValues(values)
    this.sendState()
  }

  sendState() {
    if (!this.mounted || !this.ready) return
    this.post({
      type: BRIDGE_PREFIX + 'state',
      channel: this.token,
      appId: this.appId,
      revision: this.revision,
      schema: this.schema,
      values: this.values
    })
  }

  sendResult(requestId, { ok, revision, message, error }) {
    if (!this.mounted) return
    this.post({
      type: BRIDGE_PREFIX + 'result',
      channel: this.token,
      appId: this.appId,
      requestId,
      ok: !!ok,
      revision: Number(revision) || 0,
      message: message || '',
      error: error || ''
    })
  }

  post(body) {
    this.iframe.contentWindow?.postMessage(body, '*')
  }

  dispose() {
    if (!this.mounted) return
    this.mounted = false
    this.ready = false
    this.navigated = true
    this.pendingURL = ''
    this.violated = true
    this.queue = []
    window.removeEventListener('message', this.handleMessage)
    this.iframe.removeEventListener('load', this.handleLoad)
  }
}
