// Verify that the checked-in icon font covers every icon the SPA can render.
//
// The font is a derived asset: scripts/build-icon-font.py regenerates it from
// the pinned material-symbols package and records the glyph list, source hash
// and resulting hash in src/assets/icon-font.json. This check is the fast gate
// used by `npm run build` and CI; it never needs Python.

import { createHash } from 'node:crypto'
import { existsSync } from 'node:fs'
import { readdir, readFile } from 'node:fs/promises'
import { join, relative } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import * as fontkit from 'fontkit'
import { collectIconNames } from './icon-names.mjs'

const root = fileURLToPath(new URL('../', import.meta.url))
const manifestPath = join(root, 'src/assets/icon-font.json')
const fontPath = join(root, 'src/assets/material-symbols-rounded-subset.woff2')
const sourceRoot = join(root, 'node_modules/material-symbols')
const sourcePackagePath = join(sourceRoot, 'package.json')
const sourceFontPath = join(sourceRoot, 'material-symbols-rounded.woff2')
const srcRoot = join(root, 'src')

export const MAX_FONT_BYTES = 64 * 1024

// Names the source uses but the font manifest does not provide. Kept pure so
// the failure path is testable without touching the real manifest.
export function missingIconNames(required, available) {
  const known = new Set(available)
  return required.filter((name) => !known.has(name))
}

// Which icons need a filled alternate for this exact source font. The font
// swaps to `<name>.fill` through GSUB FeatureVariations at FILL >= 0.99, so the
// subset must carry it or a browser renders a different design.
export function fillGlyphsFor(names, sourceGlyphs) {
  return names
    .filter((name) => sourceGlyphs.has(`${name}.fill`))
    .map((name) => `${name}.fill`)
}

export function glyphNames(font) {
  const names = []
  for (let id = 0; id < font.numGlyphs; id++) names.push(font.getGlyph(id).name)
  return names
}

async function files(dir) {
  const out = []
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name)
    if (entry.isDirectory()) out.push(...await files(path))
    else out.push(path)
  }
  return out
}

// Every reference form that loads font CSS or font bytes. `url()` matters as
// much as an import: a @font-face pointing at the package WOFF2 re-ships a
// full 5.4 MB family without any module specifier.
const REFERENCE_PATTERNS = [
  /(?:^|\n)\s*(?:import|export)\b[^'"\n]*?\bfrom\s*['"]([^'"]+)['"]/g,
  /(?:^|\n)\s*import\s*['"]([^'"]+)['"]/g,
  /\bimport\(\s*['"]([^'"]+)['"]\s*\)/g,
  /\brequire\(\s*['"]([^'"]+)['"]\s*\)/g,
  /@(?:import|use|forward)\s+(?:url\(\s*)?['"]?([^'")\s;]+)/g,
  /url\(\s*['"]?([^'")]+?)['"]?\s*\)/g
]

// A full family is the bare package specifier, any subpackage path
// (`material-symbols/rounded.css`, `/index.css`, the `.woff2` files, the scss
// entry points) or an explicit node_modules path to it. The local subset
// stylesheet is a relative path and is the only allowed font reference.
export function isFullFamilySpecifier(specifier) {
  const bare = specifier.replace(/^\.\//, '')
  return bare === 'material-symbols' || bare.startsWith('material-symbols/') ||
    /(?:^|\/)node_modules\/material-symbols(?:\/|$)/.test(specifier)
}

// Pure so the guard is testable without touching the real source tree.
export function familySpecifiers(source) {
  const found = new Set()
  for (const pattern of REFERENCE_PATTERNS) {
    for (const match of source.matchAll(pattern)) {
      if (isFullFamilySpecifier(match[1])) found.add(match[1])
    }
  }
  return [...found]
}

// The scanned tree is `src/` plus the root `index.html`: a stylesheet <link>
// there reaches the served page without passing through a module graph.
export async function fullFamilyImports() {
  const found = []
  const paths = (await files(srcRoot)).filter((file) => /\.(vue|js|mjs|ts|css|scss|html)$/.test(file))
  const html = join(root, 'index.html')
  if (existsSync(html)) paths.push(html)
  for (const file of paths) {
    const specifiers = familySpecifiers(await readFile(file, 'utf8'))
    if (specifiers.length) found.push({ file: relative(root, file), specifiers })
  }
  return found
}

export async function checkIconFont() {
  const manifest = JSON.parse(await readFile(manifestPath, 'utf8'))
  const bytes = await readFile(fontPath)
  const actualHash = createHash('sha256').update(bytes).digest('hex')
  if (actualHash !== manifest.font_sha256) {
    throw new Error(
      `icon font hash mismatch: ${actualHash} != manifest ${manifest.font_sha256}; run npm run fonts`
    )
  }
  if (bytes.length !== manifest.font_bytes || bytes.length > MAX_FONT_BYTES) {
    throw new Error(
      `icon font size ${bytes.length} does not match manifest ${manifest.font_bytes} (limit ${MAX_FONT_BYTES})`
    )
  }

  const names = await collectIconNames()
  const missing = missingIconNames(names, manifest.glyphs)
  if (missing.length) {
    throw new Error(
      `icon font is missing ${missing.join(', ')}; add the glyph or the subset cannot render it`
    )
  }
  const stale = manifest.glyphs.filter((name) => !names.includes(name))
  if (stale.length) {
    throw new Error(`icon font still contains unused icons: ${stale.join(', ')}; run npm run fonts`)
  }

  // The font itself must carry every declared glyph, including the `.fill`
  // alternates that GSUB FeatureVariations selects at FILL >= 0.99. If the
  // source package is available, also prove the fill list is complete for the
  // exact pinned font: fontTools silently drops unknown names, so a base-only
  // subset would otherwise render a different glyph.
  const subset = fontkit.create(bytes)
  if (subset.numGlyphs !== manifest.glyph_count) {
    throw new Error(`icon font has ${subset.numGlyphs} glyphs, manifest says ${manifest.glyph_count}`)
  }
  const present = new Set(glyphNames(subset))
  const absent = [...manifest.glyphs, ...manifest.fill_glyphs].filter((name) => !present.has(name))
  if (absent.length) {
    throw new Error(`icon font does not contain ${absent.join(', ')}; run npm run fonts`)
  }

  let sourceVersion
  let sourceBytes
  try {
    sourceVersion = JSON.parse(await readFile(sourcePackagePath, 'utf8')).version
    sourceBytes = await readFile(sourceFontPath)
  } catch {
    throw new Error('material-symbols is not installed; run npm ci in web/')
  }
  if (sourceVersion !== manifest.source.version) {
    throw new Error(
      `material-symbols ${sourceVersion} does not match manifest ${manifest.source.version}; run npm run fonts`
    )
  }
  const sourceHash = createHash('sha256').update(sourceBytes).digest('hex')
  if (sourceHash !== manifest.source.sha256) {
    throw new Error('material-symbols rounded font changed; run npm run fonts')
  }
  const sourcePresent = new Set(glyphNames(fontkit.create(sourceBytes)))
  const expectedFills = fillGlyphsFor(names, sourcePresent)
  const missingFills = missingIconNames(expectedFills, manifest.fill_glyphs)
  if (missingFills.length) {
    throw new Error(
      `icon font is missing filled alternates ${missingFills.join(', ')}; run npm run fonts`
    )
  }
  const staleFills = manifest.fill_glyphs.filter((name) => !expectedFills.includes(name))
  if (staleFills.length) {
    throw new Error(`icon font has stale filled alternates ${staleFills.join(', ')}; run npm run fonts`)
  }

  const imports = await fullFamilyImports()
  if (imports.length) {
    const where = imports.map(({ file, specifiers }) => `${file} (${specifiers.join(', ')})`)
    throw new Error(
      `full material-symbols family referenced in ${where.join(', ')}; ` +
      'import the local subset stylesheet instead'
    )
  }

  return {
    icons: names.length,
    fillGlyphs: manifest.fill_glyphs.length,
    glyphs: manifest.glyph_count,
    fontBytes: bytes.length,
    fontSha256: actualHash
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    const result = await checkIconFont()
    console.log(
      `icon font ok: ${result.icons} icons (${result.fillGlyphs} filled alternates), ` +
      `${result.glyphs} glyphs, ${result.fontBytes} bytes (sha256 ${result.fontSha256})`
    )
  } catch (error) {
    console.error(`icon font check failed: ${error.message}`)
    process.exitCode = 1
  }
}
