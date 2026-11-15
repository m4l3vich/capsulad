import test from 'node:test'
import assert from 'node:assert/strict'
import { nextTick } from 'vue'
import { DEFAULT_TIMEOUT, MAX_TOASTS, dismissToast, notify, toasts } from '../src/lib/toast.js'

// The store is the contract other surfaces code against: dedupe, id slots,
// eviction and dismissal all have to hold without a mounted host.

function reset() {
  notify.clear()
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

test('notify defaults to an info toast with a stable id and default timeout', () => {
  reset()
  const toast = notify('Saved')
  assert.equal(toasts.length, 1)
  assert.equal(toast.kind, 'info')
  assert.equal(toast.message, 'Saved')
  assert.equal(toast.timeout, DEFAULT_TIMEOUT)
  assert.match(toast.id, /^info-\d+$/)
  assert.equal(toasts[0].id, toast.id)
  assert.equal(toasts[0].message, toast.message)
})

test('kind helpers carry their kind', () => {
  reset()
  notify.success('Done')
  notify.error('Failed')
  notify.warning('Careful')
  notify.info('FYI')
  assert.deepEqual(toasts.map((toast) => toast.kind), ['success', 'error', 'warning', 'info'])
})

test('identical kind and message replace instead of stacking', () => {
  reset()
  const first = notify.error('Upload failed')
  const second = notify.error('Upload failed')
  assert.equal(toasts.length, 1)
  assert.equal(second.id, first.id)
  assert.equal(toasts[0].message, 'Upload failed')

  // Same text with another kind is a distinct message.
  notify.warning('Upload failed')
  assert.equal(toasts.length, 2)
})

test('a supplied id replaces that slot in place', () => {
  reset()
  notify.info('Connecting', { id: 'connection-state' })
  notify.warning('Reconnecting', { id: 'connection-state' })
  assert.equal(toasts.length, 1)
  assert.equal(toasts[0].id, 'connection-state')
  assert.equal(toasts[0].kind, 'warning')
  assert.equal(toasts[0].message, 'Reconnecting')

  notify.error('Closed', { id: 'connection-state', timeout: 0 })
  assert.equal(toasts.length, 1)
  assert.equal(toasts[0].kind, 'error')
  assert.equal(toasts[0].timeout, 0)
})

test('dismissToast removes the toast and reports whether it existed', async () => {
  reset()
  const toast = notify('Copied')
  await nextTick()
  assert.equal(dismissToast(toast.id), true)
  assert.equal(toasts.length, 0)
  assert.equal(dismissToast(toast.id), false)
})

test('a toast auto-dismisses after its timeout and clears its timer', async () => {
  reset()
  notify.info('Short lived', { timeout: 20 })
  assert.equal(toasts.length, 1)
  await sleep(60)
  assert.equal(toasts.length, 0)
})

test('timeout 0 keeps the toast until dismissed', async () => {
  reset()
  notify.error('Still broken', { timeout: 0 })
  await sleep(60)
  assert.equal(toasts.length, 1)
  dismissToast(toasts[0].id)
  assert.equal(toasts.length, 0)
})

test('re-firing a duplicate resets its dismissal timer', async () => {
  reset()
  notify.info('Retrying', { timeout: 60 })
  await sleep(30)
  notify.info('Retrying', { timeout: 60 })
  await sleep(40)
  // The first timer would have fired by now; the reset keeps it visible.
  assert.equal(toasts.length, 1)
  await sleep(60)
  assert.equal(toasts.length, 0)
})

test('the stack keeps at most MAX_TOASTS toasts, evicting the oldest', () => {
  reset()
  for (let index = 0; index < MAX_TOASTS + 2; index += 1) {
    notify.info(`Message ${index}`)
  }
  assert.equal(toasts.length, MAX_TOASTS)
  assert.equal(toasts[0].message, 'Message 2')
  assert.equal(toasts[toasts.length - 1].message, `Message ${MAX_TOASTS + 1}`)
})

test('clear drops every toast and pending timer', async () => {
  reset()
  notify.info('One', { timeout: 20 })
  notify.error('Two', { timeout: 20 })
  notify.clear()
  assert.equal(toasts.length, 0)
  await sleep(50)
  assert.equal(toasts.length, 0)
})
