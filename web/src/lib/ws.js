import { ClientFrame, ServerFrame, Topic } from '../proto/web_pb.js'
import { t } from './i18n.js'

// One binary WebSocket per session. Frames are capsulad.web.v1 protobuf, every
// request is correlated by id, subscriptions own bounded snapshots plus
// revisioned events, and a server resync notice forces a fresh snapshot
// instead of letting the UI act on a stream with a hole.

export const topicNames = {
  overview: Topic.TOPIC_OVERVIEW,
  settings: Topic.TOPIC_SETTINGS,
  application: Topic.TOPIC_APPLICATION,
  diagnostics: Topic.TOPIC_DIAGNOSTICS,
  wakeword: Topic.TOPIC_WAKEWORD,
  access: Topic.TOPIC_ACCESS,
  wifi: Topic.TOPIC_WIFI
}

class CapsuleSocket {
  constructor() {
    this.ws = null
    this.seq = 0
    this.pending = new Map()
    this.snapshotWaiters = new Map()
    this.subs = new Map()
    this.state = 'closed'
    this.manualClose = false
    this.retry = 0
    this.retryTimer = null
    this.hello = null
    this.listeners = { state: new Set(), expired: new Set(), reconnect: new Set() }
    this.trainingHandlers = new Map()
  }

  on(event, fn) {
    this.listeners[event]?.add(fn)
    return () => this.listeners[event]?.delete(fn)
  }

  emit(event, value) {
    for (const fn of this.listeners[event] || []) fn(value)
  }

  setState(state) {
    this.state = state
    this.emit('state', state)
  }

  connect() {
    this.manualClose = false
    if (this.ws && (this.ws.readyState === WebSocket.OPEN || this.ws.readyState === WebSocket.CONNECTING)) return
    this.setState(this.retry > 0 ? 'reconnecting' : 'connecting')
    const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:'
    const ws = new WebSocket(`${scheme}//${location.host}/ws`)
    ws.binaryType = 'arraybuffer'
    this.ws = ws

    ws.onopen = () => {
      this.retry = 0
      this.setState('open')
      this.sendRaw({ hello: {} })
      for (const key of this.subs.keys()) {
        const [topic, appId] = key.split('|')
        this.sendSubscribe(Number(topic), appId)
      }
      this.emit('reconnect')
    }

    ws.onmessage = (event) => {
      let frame
      try {
        frame = ServerFrame.decode(new Uint8Array(event.data))
      } catch (_) {
        return
      }
      this.handleFrame(frame)
    }

    ws.onclose = (event) => {
      this.ws = null
      this.rejectAll(new Error(t('errors.connectionClosed')))
      if (this.manualClose) {
        this.setState('closed')
        return
      }
      if (event.code === 1008) {
        this.setState('expired')
        this.emit('expired')
        return
      }
      this.setState('reconnecting')
      this.scheduleReconnect()
    }

    ws.onerror = () => {
      // onclose carries the actionable state.
    }
  }

  scheduleReconnect() {
    if (this.retryTimer) return
    const delay = [1000, 2000, 4000, 8000, 10000][Math.min(this.retry, 4)]
    this.retry += 1
    this.retryTimer = setTimeout(() => {
      this.retryTimer = null
      this.connect()
    }, delay)
  }

  close() {
    this.manualClose = true
    if (this.retryTimer) {
      clearTimeout(this.retryTimer)
      this.retryTimer = null
    }
    if (this.ws) this.ws.close(1000, 'client closed')
    this.ws = null
    this.setState('closed')
  }

  sendRaw(obj) {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return false
    const body = { ...obj }
    if (body.requestId === undefined) body.requestId = ++this.seq
    this.ws.send(ClientFrame.encode(body))
    return body.requestId
  }

  handleFrame(frame) {
    if (frame.hello) {
      this.hello = frame.hello
      return
    }
    if (frame.snapshot) {
      const waiter = this.snapshotWaiters.get(frame.requestId)
      if (waiter) {
        this.snapshotWaiters.delete(frame.requestId)
        clearTimeout(waiter.timer)
        this.deliverSnapshot(waiter.key, frame.snapshot)
      }
      this.settle(frame.requestId, frame.snapshot)
      return
    }
    if (frame.result) {
      this.settle(frame.requestId, frame.result)
      return
    }
    if (frame.error) {
      this.fail(frame.requestId, frame.error)
      return
    }
    if (frame.progress) {
      this.trainingHandlers.get(frame.progress.trainingId)?.(frame.progress)
      const entry = this.pending.get(frame.requestId)
      if (entry?.onProgress) entry.onProgress(frame.progress)
      return
    }
    if (frame.event) {
      this.handleEvent(frame.event)
    }
  }

  deliverSnapshot(key, snapshot) {
    const set = this.subs.get(key)
    if (!set) return
    for (const handler of set) handler.onSnapshot?.(snapshot)
  }

  handleEvent(event) {
    if (event.resyncRequired && !event.topic) {
      // The server emits a topic-less resync when the outbound queue overflowed.
      // It cannot name which topic lost events, so every live subscription must
      // re-request its snapshot; ignoring it would leave the view stale.
      for (const set of this.subs.values()) {
        for (const handler of set) handler.onResync?.()
      }
      return
    }
    const appId = event.appId || ''
    const key = `${event.topic}|${appId}`
    const set = this.subs.get(key)
    if (!set || set.size === 0) return
    for (const handler of set) {
      if (event.resyncRequired) handler.onResync?.()
      else handler.onEvent?.(event)
    }
  }

  settle(id, value) {
    const entry = this.pending.get(id)
    if (!entry) return
    this.pending.delete(id)
    clearTimeout(entry.timer)
    entry.cleanup?.()
    entry.resolve(value)
    entry.onProgress = null
  }

  notifySubscribe(key, error) {
    const handlers = this.subs.get(key)
    if (!handlers) return
    for (const handler of handlers) {
      try {
        handler.onError?.(error)
      } catch (_) {
        // A page handler must not break the shared transport.
      }
    }
  }

  fail(id, error) {
    const failure = Object.assign(new Error(error.message || t('errors.requestFailedPlain')), { code: error.code })
    // A per-request failure settles exactly that request (or subscription) and
    // nothing else. Transport state and its epoch belong to onclose: an RPC the
    // daemon refused must stay an inline error on the page, never a reason to
    // declare the socket disconnected or to reconnect.
    // A rejected subscription has no pending command entry, but its view must
    // still leave the loading state instead of waiting forever.
    const waiter = this.snapshotWaiters.get(id)
    this.snapshotWaiters.delete(id)
    if (waiter) {
      clearTimeout(waiter.timer)
      this.notifySubscribe(waiter.key, failure)
    }
    const entry = this.pending.get(id)
    if (!entry) return
    this.pending.delete(id)
    clearTimeout(entry.timer)
    entry.cleanup?.()
    entry.reject(failure)
  }

  rejectAll(err) {
    for (const [, entry] of this.pending) {
      clearTimeout(entry.timer)
      entry.cleanup?.()
      entry.reject(err)
    }
    this.pending.clear()
    for (const [, waiter] of this.snapshotWaiters) clearTimeout(waiter.timer)
    this.snapshotWaiters.clear()
  }

  onTraining(trainingId, fn) {
    this.trainingHandlers.set(trainingId, fn)
    return () => this.trainingHandlers.delete(trainingId)
  }

  // Cancellable request. Aborting rejects with an AbortError and sends a
  // CancelOperation naming `request:<id>`, so the daemon can cancel the
  // in-flight device call instead of only discarding the browser promise.
  request(command, { onProgress, timeout = 30000, signal } = {}) {
    return new Promise((resolve, reject) => {
      if (signal?.aborted) {
        reject(abortError())
        return
      }
      const id = ++this.seq
      let settled = false
      const entry = { resolve, reject, onProgress }
      const cleanup = () => {
        if (signal) signal.removeEventListener('abort', onAbort)
      }
      entry.cleanup = cleanup
      const finish = (fn, value) => {
        if (settled) return
        settled = true
        cleanup()
        fn(value)
      }
      // Both cancellation paths tell the daemon to cancel the owned
      // operation, so a timed-out recording/action does not keep running.
      const sendCancel = () => {
        if (this.ws && this.ws.readyState === WebSocket.OPEN) {
          this.ws.send(ClientFrame.encode({
            requestId: ++this.seq,
            cancel: { operationId: `request:${id}` }
          }))
        }
      }
      const onAbort = () => {
        if (settled) return
        this.pending.delete(id)
        clearTimeout(entry.timer)
        entry.onProgress = null
        entry.cleanup = null
        finish(reject, abortError())
        sendCancel()
      }
      entry.timer = setTimeout(() => {
        this.pending.delete(id)
        entry.onProgress = null
        entry.cleanup = null
        finish(reject, new Error(t('errors.requestTimedOut')))
        sendCancel()
      }, timeout)
      this.pending.set(id, entry)
      if (signal) signal.addEventListener('abort', onAbort, { once: true })
      const body = { requestId: id, command }
      if (!this.ws || this.ws.readyState !== WebSocket.OPEN) {
        this.pending.delete(id)
        clearTimeout(entry.timer)
        finish(reject, new Error(t('errors.notConnectedDevice')))
        return
      }
      this.ws.send(ClientFrame.encode(body))
    })
  }

  requestSnapshot(topic, appId = '', timeout = 15000) {
    return new Promise((resolve, reject) => {
      const id = ++this.seq
      const entry = { resolve, reject }
      entry.timer = setTimeout(() => {
        this.pending.delete(id)
        reject(new Error(t('errors.snapshotTimedOut')))
      }, timeout)
      this.pending.set(id, entry)
      if (!this.ws || this.ws.readyState !== WebSocket.OPEN) {
        this.pending.delete(id)
        clearTimeout(entry.timer)
        reject(new Error(t('errors.notConnectedDevice')))
        return
      }
      this.ws.send(ClientFrame.encode({ requestId: id, snapshotRequest: { topic, appId } }))
    })
  }

  sendSubscribe(topic, appId) {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return
    const id = ++this.seq
    const waiter = {
      key: `${topic}|${appId}`,
      timer: setTimeout(() => {
        if (this.snapshotWaiters.get(id) !== waiter) return
        this.snapshotWaiters.delete(id)
        this.notifySubscribe(waiter.key, new Error(t('errors.subscriptionTimedOut')))
      }, 15000)
    }
    this.snapshotWaiters.set(id, waiter)
    this.ws.send(ClientFrame.encode({ requestId: id, subscribe: { topic, appId } }))
  }

  subscribe(topic, appId, handler) {
    const key = `${topic}|${appId || ''}`
    let set = this.subs.get(key)
    if (!set) {
      set = new Set()
      this.subs.set(key, set)
    }
    const first = set.size === 0
    set.add(handler)
    if (first) this.sendSubscribe(topic, appId || '')
    return () => {
      const current = this.subs.get(key)
      if (!current) return
      current.delete(handler)
      if (current.size === 0) {
        this.subs.delete(key)
        if (this.ws?.readyState === WebSocket.OPEN) {
          this.ws.send(ClientFrame.encode({ requestId: ++this.seq, unsubscribe: { topic, appId: appId || '' } }))
        }
      }
    }
  }
}

function abortError() {
  const err = new Error(t('errors.requestAborted'))
  err.name = 'AbortError'
  return err
}

export const socket = new CapsuleSocket()
