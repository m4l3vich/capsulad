import test from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import { existsSync } from 'node:fs'
import { readFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'

// The generator is the only writer of the checked-in subset, and `--require
// NAME` is its documented failure path: a name the pinned font does not provide
// must fail with that name before anything is written. Python tooling is not
// part of `npm ci`, so this test skips unless the pinned venv
// (scripts/requirements-fonts.txt) is importable; set CAPSULAD_FONT_PYTHON to
// point at another interpreter with the pinned packages.

const web = fileURLToPath(new URL('../', import.meta.url))
const script = fileURLToPath(new URL('../scripts/build-icon-font.py', import.meta.url))
const fontPath = new URL('../src/assets/material-symbols-rounded-subset.woff2', import.meta.url)
const manifestPath = new URL('../src/assets/icon-font.json', import.meta.url)
const sourcePackagePath = new URL('../node_modules/material-symbols/package.json', import.meta.url)

function python() {
  const candidates = [
    process.env.CAPSULAD_FONT_PYTHON,
    fileURLToPath(new URL('../../build/font-venv/bin/python', import.meta.url)),
    'python3'
  ].filter(Boolean)
  for (const candidate of candidates) {
    if (candidate !== 'python3' && !existsSync(candidate)) continue
    const probe = spawnSync(
      candidate,
      ['-c', 'import fontTools, uharfbuzz, brotli; assert fontTools.version == "4.68.0"'],
      { cwd: web, encoding: 'utf8' }
    )
    if (probe.status === 0) return candidate
  }
  return null
}

async function sha(path) {
  return createHash('sha256').update(await readFile(path)).digest('hex')
}

const interpreter = python()
const reason = !interpreter
  ? 'install build/font-venv from web/scripts/requirements-fonts.txt or set CAPSULAD_FONT_PYTHON'
  : !existsSync(sourcePackagePath)
    ? 'run npm ci in web/ so the pinned material-symbols font is available'
    : false

test('--require fails for a name the pinned font lacks, without writing', { skip: reason }, async () => {
  const fontBefore = await sha(fontPath)
  const manifestBefore = await sha(manifestPath)

  const result = spawnSync(
    interpreter,
    [script, '--check', '--require', 'no_such_material_symbol_glyph'],
    { cwd: web, encoding: 'utf8' }
  )

  assert.equal(result.status, 1, `expected failure, got ${result.status}: ${result.stdout}${result.stderr}`)
  assert.match(result.stderr, /source font has no glyph for: no_such_material_symbol_glyph/)
  assert.equal(await sha(fontPath), fontBefore, 'the failing run rewrote the subset')
  assert.equal(await sha(manifestPath), manifestBefore, 'the failing run rewrote the manifest')
})