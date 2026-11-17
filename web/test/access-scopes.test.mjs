import test from 'node:test'
import assert from 'node:assert/strict'
import { keyScopes } from '../src/lib/format.js'

// The Access page describes the same list the MCP frontend can mint into, so a
// status key must never be shown as an administration credential or the other
// way round.

test('legacy and explicit status keys read as status only', () => {
  assert.equal(keyScopes(undefined), 'status only')
  assert.equal(keyScopes([]), 'status only')
  assert.equal(keyScopes(['status']), 'status only')
})

test('an operator key reads as full administration, even beside status', () => {
  assert.equal(keyScopes(['operator']), 'full administration')
  assert.equal(keyScopes(['operator', 'status']), 'full administration')
})

test('an unknown scope is shown verbatim rather than hidden', () => {
  assert.equal(keyScopes(['experimental']), 'experimental')
})
