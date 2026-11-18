import test from 'node:test'
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { readFile } from 'node:fs/promises'
import * as fontkit from 'fontkit'
import { collectIconNames, iconNamesInBody, iconNamesInSource } from '../scripts/icon-names.mjs'
import {
  MAX_FONT_BYTES,
  checkIconFont,
  familySpecifiers,
  fillGlyphsFor,
  fullFamilyImports,
  isFullFamilySpecifier,
  missingIconNames
} from '../scripts/check-icon-font.mjs'

// The SPA ships a derived subset of Material Symbols Rounded. These tests bind
// the committed bytes, the declared glyph list and the source icon names
// together, so a new <Icon> without a regenerated font fails CI. Shaping with
// HarfBuzz/fontkit proves the ligatures survived; the browser check covers the
// FILL=1 rendering (fontkit cannot instantiate this variable font here).

const root = new URL('../', import.meta.url)
const manifest = JSON.parse(await readFile(new URL('src/assets/icon-font.json', root), 'utf8'))
const fontBytes = await readFile(new URL(`src/assets/${manifest.font}`, root))
const font = fontkit.create(fontBytes)
const expectedAxes = {
  FILL: { name: 'Fill', min: 0, default: 0, max: 1 },
  GRAD: { name: 'Grade', min: -50, default: 0, max: 200 },
  opsz: { name: 'Optical Size', min: 20, default: 24, max: 48 },
  wght: { name: 'Weight', min: 100, default: 400, max: 700 }
}

test('subset bytes match the generated manifest', () => {
  assert.equal(createHash('sha256').update(fontBytes).digest('hex'), manifest.font_sha256)
  assert.equal(fontBytes.length, manifest.font_bytes)
  assert.ok(fontBytes.length < MAX_FONT_BYTES, `${fontBytes.length} bytes`)
  assert.match(manifest.source.sha256, /^[0-9a-f]{64}$/)
  assert.equal(manifest.source.version, '0.48.3')
  assert.equal(manifest.license, 'Apache-2.0')
})

test('filled alternates are present for FILL=1', () => {
  assert.ok(manifest.fill_glyphs.length > 0)
  for (const name of manifest.fill_glyphs) {
    assert.ok(manifest.glyphs.includes(name.replace(/\.fill$/, '')), `${name} has no base icon`)
  }
})

test('variable axes are intact', () => {
  assert.deepEqual(font.variationAxes, expectedAxes)
  assert.deepEqual(manifest.axes, {
    FILL: [0, 0, 1],
    GRAD: [-50, 0, 200],
    opsz: [20, 24, 48],
    wght: [100, 400, 700]
  })
})

test('every icon name is a single ligature glyph', async () => {
  const names = await collectIconNames()
  assert.deepEqual(names, manifest.glyphs)
  for (const name of names) {
    const run = font.layout(name)
    assert.equal(run.glyphs.length, 1, `${name} shaped to ${run.glyphs.length} glyphs`)
    assert.equal(run.glyphs[0].name, name)
  }
  assert.equal(font.numGlyphs, manifest.glyph_count)
})

test('a non-ligature word is not one glyph', () => {
  // Guards against a vacuous "everything is one glyph" check.
  assert.ok(font.layout('roboto').glyphs.length > 1)
})

test('missing glyphs are reported, including the old fluorescent name', () => {
  const missing = missingIconNames(['wifi', 'fluorescent'], manifest.glyphs)
  assert.deepEqual(missing, ['fluorescent'])
  assert.throws(
    () => iconNamesInBody('{{ unregisteredIcon.value }}', 'fixture.vue:1'),
    /unrecognized <Icon> body/
  )
  assert.deepEqual(iconNamesInBody("{{ connected ? 'wifi' : 'wifi_off' }}", 'fixture.vue:1'), ['wifi', 'wifi_off'])
})

test('fill alternates follow the pinned source glyph set', () => {
  const source = new Set(['wifi', 'wifi.fill', 'mic', 'settings.fill'])
  assert.deepEqual(fillGlyphsFor(['wifi', 'mic'], source), ['wifi.fill'])
  assert.deepEqual(missingIconNames(['wifi.fill'], []), ['wifi.fill'])
})

test('the checked-in subset passes the build gate', async () => {
  const result = await checkIconFont()
  assert.equal(result.icons, manifest.glyphs.length)
  assert.equal(result.fillGlyphs, manifest.fill_glyphs.length)
})

test('no source imports all three full font families', async () => {
  assert.deepEqual(await fullFamilyImports(), [])
})

test('full-family references are rejected in every form, subpaths included', async () => {
  assert.deepEqual(await fullFamilyImports(), [])

  // The bare specifier is only one way to reintroduce the 5.4 MB family; a
  // subpath stylesheet or a @font-face url() does the same thing.
  const rejected = [
    "import 'material-symbols'",
    "import { round } from 'material-symbols'",
    "import 'material-symbols/rounded.css'",
    "import 'material-symbols/index.css'",
    "export { default } from 'material-symbols/sharp.scss'",
    "const css = require('material-symbols/outlined.css')",
    "await import('material-symbols/rounded.css')",
    '@import "material-symbols/index.css";',
    '@use "material-symbols/rounded";',
    'src: url(material-symbols/material-symbols-rounded.woff2) format("woff2");',
    "src: url('../node_modules/material-symbols/material-symbols-rounded.woff2');"
  ]
  for (const source of rejected) {
    assert.notDeepEqual(familySpecifiers(source), [], source)
  }
  assert.ok(isFullFamilySpecifier('material-symbols/rounded.css'))

  // The local subset is a relative path, and prose that names the package is
  // not a reference: both must stay allowed.
  const allowed = [
    "import '../assets/material-symbols-rounded.css'",
    "import './assets/material-symbols-rounded.css'",
    "src: url('./material-symbols-rounded-subset.woff2') format('woff2');",
    '/* Mirrors material-symbols/rounded.css; see LICENSE-material-symbols.txt. */'
  ]
  for (const source of allowed) {
    assert.deepEqual(familySpecifiers(source), [], source)
  }
})

test('a self-closing <Icon /> fails instead of shipping a missing glyph', () => {
  assert.throws(
    () => iconNamesInSource('<template>\n  <Icon size="16px" />\n</template>', 'fixture.vue'),
    /fixture\.vue:2: self-closing <Icon \.\.\. \/> renders no glyph/
  )
  // The stray tag must not be swallowed as the opener of the next paired icon.
  assert.throws(
    () => iconNamesInSource('<template>\n<Icon />\n<Icon>wifi</Icon>\n</template>', 'fixture.vue'),
    /fixture\.vue:2: self-closing <Icon/
  )
  assert.throws(
    () => iconNamesInSource('<template>\n  <Icon size="16px">\n</template>', 'fixture.vue'),
    /fixture\.vue:2: <Icon> has no closing <\/Icon>/
  )
  assert.deepEqual(
    iconNamesInSource('<template>\n  <Icon>wifi</Icon>\n</template>', 'fixture.vue'),
    ['wifi']
  )
})

test('commented-out template markup contributes no glyphs', () => {
  const source = [
    '<template>',
    '  <!-- <Icon>retired_icon</Icon> <Icon /> -->',
    '  <Icon>wifi</Icon>',
    '</template>'
  ].join('\n')
  assert.deepEqual(iconNamesInSource(source, 'fixture.vue'), ['wifi'])
})

test('the licence copied into the served tree matches the source copy', async () => {
  // public/ is copied verbatim into internal/webui/dist, which is what a
  // released binary ships; the two copies must not drift.
  const shipped = await readFile(new URL('public/LICENSE-material-symbols.txt', root), 'utf8')
  const sourceTree = await readFile(new URL('src/assets/LICENSE-material-symbols.txt', root), 'utf8')
  assert.equal(shipped, sourceTree)
  assert.match(shipped, /Apache License/)
})
