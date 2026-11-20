import test from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import {
  MAX_KIT_BYTES,
  assertDocumentCoverage,
  documentClasses,
  renderKit
} from '../scripts/build-settings-kit.mjs'

// The embedded stylesheet is a derived asset: the daemon serves it from
// go:embed and the speaker has no Node toolchain, so the checked-in bytes must
// equal what the local Tailwind + daisyUI build produces and must cover every
// class the packaged documents use.

const kitURL = new URL('../../internal/appui/assets/kit.css', import.meta.url)
const checkedIn = await readFile(kitURL, 'utf8')
const generated = await renderKit()

const documents = [
  ['internal/appui/assets/clock.html', new URL('../../internal/appui/assets/clock.html', import.meta.url)],
  ['internal/appui/assets/ring.html', new URL('../../internal/appui/assets/ring.html', import.meta.url)],
  ['apps/radio/ui/settings.html', new URL('../../apps/radio/ui/settings.html', import.meta.url)]
]

test('checked-in kit matches the local build', () => {
  assert.equal(checkedIn, generated, 'run `npm run build:kit` after changing the kit or a settings document')
})

test('kit is daisyUI with both shared themes and no network reference', () => {
  for (const marker of ['.btn', '.card', '.input', '.textarea', '.select', '.checkbox', '.alert', '.badge']) {
    assert.ok(checkedIn.includes(marker), `missing ${marker}`)
  }
  assert.match(checkedIn, /\[data-theme=['"]?capsulad['"]?\]/)
  assert.match(checkedIn, /\[data-theme=['"]?capsulad-dark['"]?\]/)
  assert.match(checkedIn, /color-scheme:\s*dark/)
  assert.ok(!checkedIn.includes('@import'))
  assert.ok(!/url\(\s*['"]?https?:/i.test(checkedIn))
  assert.ok(!checkedIn.includes('@font-face'))
  assert.ok(Buffer.byteLength(checkedIn) <= MAX_KIT_BYTES)
})

test('release notices carry full Tailwind and daisyUI MIT texts at installed versions', async () => {
  const notices = await readFile(new URL('../public/THIRD-PARTY.md', import.meta.url), 'utf8')
  const tailwind = await readFile(new URL('../public/LICENSE-tailwindcss.txt', import.meta.url), 'utf8')
  const daisyui = await readFile(new URL('../public/LICENSE-daisyui.txt', import.meta.url), 'utf8')
  const tailwindVersion = JSON.parse(await readFile(new URL('../node_modules/tailwindcss/package.json', import.meta.url), 'utf8')).version
  const daisyuiVersion = JSON.parse(await readFile(new URL('../node_modules/daisyui/package.json', import.meta.url), 'utf8')).version

  for (const text of [tailwind, daisyui]) {
    assert.match(text, /^MIT License/)
    assert.match(text, /Permission is hereby granted/)
    assert.match(text, /THE SOFTWARE IS PROVIDED "AS IS"/)
  }
  assert.match(tailwind, /Copyright \(c\) Tailwind Labs, Inc\./)
  assert.match(daisyui, /Copyright \(c\) 2020 Pouya Saadeghi/)
  assert.ok(notices.includes(`**${tailwindVersion}**`))
  assert.ok(notices.includes(`**${daisyuiVersion}**`))
  assert.match(notices, /LICENSE-tailwindcss\.txt/)
  assert.match(notices, /LICENSE-daisyui\.txt/)
  assert.match(checkedIn, /Copyright \(c\) Tailwind Labs, Inc\./)
  assert.match(checkedIn, /Copyright \(c\) 2020 Pouya Saadeghi/)
})

test('every packaged settings class resolves in the kit', async () => {
  for (const [name, url] of documents) {
    const html = await readFile(url, 'utf8')
    assert.ok(documentClasses(html).length > 0, `${name} declares no classes`)
  }
  assert.deepEqual(assertDocumentCoverage(checkedIn, await Promise.all(
    documents.map(async ([name, url]) => ({ name, html: await readFile(url, 'utf8') }))
  )), [])
})