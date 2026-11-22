import test from 'node:test'
import assert from 'node:assert/strict'
import { keyScopes } from '../src/lib/format.js'

// The Access page describes the same list the MCP frontend can mint into, so a
// status key must never be shown as an administration credential or the other
// way round.

test('only an explicit status grant reads as status only', () => {
  assert.equal(keyScopes(undefined), 'no access')
  assert.equal(keyScopes([]), 'no access')
  assert.equal(keyScopes(['status']), 'status only')
})

test('an assistant key reads as assistant controls and is not full administration', () => {
  assert.equal(keyScopes(['assistant']), 'assistant controls')
  assert.equal(keyScopes(['assistant', 'status']), 'assistant controls')
})

test('an operator key reads as full administration, even beside status', () => {
  assert.equal(keyScopes(['operator']), 'full administration')
  assert.equal(keyScopes(['operator', 'assistant', 'status']), 'full administration')
})

test('an unknown scope invalidates the entire recorded grant', () => {
  assert.equal(keyScopes(['experimental']), 'no access: experimental')
  assert.equal(keyScopes(['operator', 'experimental']), 'no access: operator, experimental')
})
