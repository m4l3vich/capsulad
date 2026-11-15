import { reactive } from 'vue'

// Transient feedback is owned by a module-level store: callers can report
// progress or failure before the host mounts (boot, login, socket transitions)
// and the same stack survives every view change.

export const DEFAULT_TIMEOUT = 5000
export const MAX_TOASTS = 5

const KINDS = new Set(['info', 'success', 'warning', 'error'])

// The rendered stack. ToastHost reads this; callers never mutate it directly.
export const toasts = reactive([])

// Timers are kept out of the reactive objects so the only identity the UI and
// the callers share is the toast id.
const timers = new Map()
let sequence = 0

function normalizeKind(kind) {
  return KINDS.has(kind) ? kind : 'info'
}

function normalizeTimeout(value) {
  if (value === undefined) return DEFAULT_TIMEOUT
  const ms = Number(value)
  // 0 (or a nonsense negative/NaN) means the toast stays until dismissed.
  if (!Number.isFinite(ms) || ms <= 0) return 0
  return ms
}

function clearTimer(id) {
  const timer = timers.get(id)
  if (timer === undefined) return
  clearTimeout(timer)
  timers.delete(id)
}

function removeAt(index) {
  const [removed] = toasts.splice(index, 1)
  if (removed) clearTimer(removed.id)
}

function armTimer(toast) {
  clearTimer(toast.id)
  if (toast.timeout <= 0) return
  const timer = setTimeout(() => {
    timers.delete(toast.id)
    dismissToast(toast.id)
  }, toast.timeout)
  // Never keep a Node test process (or a background page) alive for a toast.
  timer.unref?.()
  timers.set(toast.id, timer)
}

function publish(kind, message, options = {}) {
  const text = message === undefined || message === null ? '' : String(message)
  if (!text) return null
  const resolvedKind = normalizeKind(kind)
  const timeout = normalizeTimeout(options.timeout)
  const requestedId = options.id === undefined || options.id === null ? '' : String(options.id)

  // A caller-supplied id is the state slot for that message (for example the
  // connection banner). Otherwise identical kind+message replaces itself
  // rather than stacking duplicates on repeated failures.
  let index = requestedId
    ? toasts.findIndex((toast) => toast.id === requestedId)
    : -1
  if (index === -1 && !requestedId) {
    index = toasts.findIndex((toast) => toast.kind === resolvedKind && toast.message === text)
  }

  if (index !== -1) {
    // Keep the existing id so the host patches the live node in place. An
    // unchanged message therefore is not announced again by screen readers.
    const next = { id: toasts[index].id, kind: resolvedKind, message: text, timeout }
    toasts.splice(index, 1, next)
    armTimer(next)
    return next
  }

  sequence += 1
  const toast = {
    id: requestedId || `${resolvedKind}-${sequence}`,
    kind: resolvedKind,
    message: text,
    timeout
  }
  toasts.push(toast)
  // Cap the stack; the oldest entry leaves first so new failures stay visible.
  while (toasts.length > MAX_TOASTS) removeAt(0)
  armTimer(toast)
  return toast
}

export function dismissToast(id) {
  const index = toasts.findIndex((toast) => toast.id === id)
  if (index === -1) return false
  removeAt(index)
  return true
}

function clear() {
  for (const timer of timers.values()) clearTimeout(timer)
  timers.clear()
  toasts.splice(0, toasts.length)
}

export function notify(message, options) {
  return publish('info', message, options)
}

notify.success = (message, options) => publish('success', message, options)
notify.error = (message, options) => publish('error', message, options)
notify.warning = (message, options) => publish('warning', message, options)
notify.info = (message, options) => publish('info', message, options)
notify.clear = clear
