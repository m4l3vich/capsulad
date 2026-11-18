import test from 'node:test'
import assert from 'node:assert/strict'

// The guide pane renders package-authored Markdown as a block model. These
// tests pin that the result is inert data (no HTML string) and that the fetch
// client uses the authenticated same-origin route without inventing paths.

const { guideBlocks } = await import('../src/lib/guide.js')
const { readGuide } = await import('../src/lib/apps.js')

test('guide blocks cover headings, prose, lists, code and tables', () => {
  const blocks = guideBlocks(`# Widget

Intro with **bold** and [setup docs](https://example.invalid/setup).

- press the circle key
- press and hold to reset

1. choose a station
2. press play

\`\`\`sh
capsulad-cli app package .
\`\`\`

| Setting | Meaning |
|---|---|
| delay | reply delay |
`)
  assert.deepEqual(blocks[0], { type: 'heading', level: 1, text: 'Widget' })
  assert.ok(blocks.some((b) => b.type === 'paragraph' && b.text.includes('bold')))
  assert.ok(blocks.some((b) => b.type === 'list' && b.kind === 'ul' && b.items.length === 2))
  assert.ok(blocks.some((b) => b.type === 'list' && b.kind === 'ol' && b.items[0] === 'choose a station'))
  assert.ok(blocks.some((b) => b.type === 'code' && b.text === 'capsulad-cli app package .'))
  const table = blocks.find((b) => b.type === 'table')
  assert.deepEqual(table.headers, ['Setting', 'Meaning'])
  assert.deepEqual(table.rows, [['delay', 'reply delay']])
})

test('guide blocks keep hostile package text inert', () => {
  const blocks = guideBlocks(`# <script>alert(1)</script>

<img src=x onerror=alert(2)>

[click](javascript:alert(3))`)
  const serialized = JSON.stringify(blocks)
  assert.ok(serialized.includes('<script>alert(1)</script>'))
  assert.ok(serialized.includes('click (javascript:alert(3))'))
  for (const block of blocks) {
    assert.equal(Object.prototype.hasOwnProperty.call(block, 'html'), false)
    assert.ok(['heading', 'paragraph', 'list', 'code', 'table'].includes(block.type))
  }
})

test('readGuide uses the same-origin app guide route', async () => {
  const calls = []
  globalThis.fetch = async (url, options = {}) => {
    calls.push({ url, options })
    return {
      ok: true,
      status: 200,
      async json() {
        return {
          app_id: 'org.example.app',
          title: 'Example',
          source: 'package',
          format: 'markdown',
          markdown: '# Example\n\nPress start.'
        }
      }
    }
  }
  const controller = new AbortController()
  const doc = await readGuide('org.example.app', { signal: controller.signal })
  assert.equal(calls[0].url, '/api/apps/org.example.app/guide')
  assert.equal(calls[0].options.credentials, 'same-origin')
  assert.equal(calls[0].options.headers.Accept, 'application/json')
  assert.equal(calls[0].options.signal, controller.signal)
  assert.equal(doc.source, 'package')
  assert.match(doc.markdown, /Press start/)
})

test('readGuide surfaces the device message for missing and unreadable guides', async () => {
  globalThis.fetch = async () => ({
    ok: false,
    status: 404,
    async json() {
      return { error: 'This application does not include a user guide.' }
    }
  })
  await assert.rejects(() => readGuide('org.example.none'), /does not include a user guide/)

  globalThis.fetch = async () => ({
    ok: true,
    status: 200,
    async json() {
      return { markdown: '   \n' }
    }
  })
  await assert.rejects(() => readGuide('org.example.empty'), /empty/)
})