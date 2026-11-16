import test from 'node:test'
import assert from 'node:assert/strict'

// The browser asset client mirrors the daemon's store rules for immediate
// feedback, and always proves the session to the same-origin API. These tests
// pin the pure validation/formatting contract and the request shape.

const {
  assetPathError,
  defaultTarget,
  deleteAsset,
  formatBytes,
  listAssets,
  MAX_ASSET_BYTES,
  shortHash
} = await import('../src/lib/assets.js')
const { session } = await import('../src/lib/session.js')

test('assetPathError accepts nested relative paths', () => {
  for (const path of ['a.wav', 'audio/replies/hello.wav', 'images/logo.PNG', 'data/set 1/index.json']) {
    assert.equal(assetPathError(path), '', `expected ${path} to be valid`)
  }
})

test('assetPathError rejects traversal, absolute and reserved shapes', () => {
  const invalid = {
    '': 'Enter',
    '   ': 'Enter',
    '/etc/passwd': 'relative',
    '../escape.wav': 'cannot contain',
    'audio/../../escape.wav': 'cannot contain',
    'audio//double.wav': 'cannot contain',
    './dot.wav': 'cannot contain',
    '.': 'cannot contain',
    'audio\\reply.wav': 'path separator',
    '.capsulad-upload-123': 'reserved',
    'audio/.capsulad-upload-9': 'reserved',
    [`${'x'.repeat(256)}.wav`]: 'at most 255'
  }
  for (const [path, hint] of Object.entries(invalid)) {
    const message = assetPathError(path)
    assert.ok(message, `expected ${JSON.stringify(path)} to be rejected`)
    assert.ok(message.includes(hint), `message for ${JSON.stringify(path)} = ${message}`)
  }
})

test('formatting helpers', () => {
  assert.equal(defaultTarget('reply.wav'), 'reply.wav')
  assert.equal(defaultTarget(undefined), '')
  assert.equal(formatBytes(0), '0 B')
  assert.equal(formatBytes(512), '512 B')
  assert.equal(formatBytes(2048), '2.0 KiB')
  assert.equal(formatBytes(MAX_ASSET_BYTES), '32 MiB')
  assert.equal(shortHash(''), '—')
  assert.equal(shortHash('0123456789abcdef0123'), '0123456789abcdef…')
})

test('listAssets reads the catalog and total bytes', async () => {
  const calls = []
  globalThis.fetch = async (url, options = {}) => {
    calls.push({ url, method: options.method || 'GET', credentials: options.credentials })
    return {
      ok: true,
      status: 200,
      async json() {
        return {
          assets: [
            { id: 'audio/a.wav', path: '/assets/audio/a.wav', media_type: 'audio/wav', sha256: 'ab', size_bytes: 4 }
          ],
          total_bytes: 4
        }
      }
    }
  }
  const { assets, totalBytes } = await listAssets()
  assert.equal(calls.length, 1)
  assert.equal(calls[0].url, '/api/assets')
  assert.equal(calls[0].credentials, 'same-origin')
  assert.equal(assets.length, 1)
  assert.equal(assets[0].id, 'audio/a.wav')
  assert.equal(totalBytes, 4)
})

test('deleteAsset sends a CSRF-protected same-origin DELETE', async () => {
  session.csrf = 'csrf-token'
  const calls = []
  globalThis.fetch = async (url, options = {}) => {
    calls.push({ url, options })
    return { ok: true, status: 200, async json() { return { ok: true } } }
  }
  await deleteAsset('audio/replies/hello.wav')
  assert.equal(calls.length, 1)
  assert.equal(calls[0].url, '/api/assets?path=audio%2Freplies%2Fhello.wav')
  assert.equal(calls[0].options.method, 'DELETE')
  assert.equal(calls[0].options.credentials, 'same-origin')
  assert.equal(calls[0].options.headers['X-Capsulad-CSRF'], 'csrf-token')
})

test('a rejected delete surfaces the daemon error', async () => {
  globalThis.fetch = async () => ({
    ok: false,
    status: 400,
    async json() { return { error: 'invalid asset path: path must be relative to the asset store' } }
  })
  await assert.rejects(() => deleteAsset('/etc/passwd'), /path must be relative/)
})