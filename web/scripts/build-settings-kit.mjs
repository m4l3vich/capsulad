// Build the stylesheet injected into application settings documents.
//
// Source of truth: web/src/settings-kit.css + web/tailwind.settings.config.js
// (Tailwind 3.4.17, daisyUI 4.12.23) and the checked-in settings documents
// that the classes are written against. The result is checked in at
// internal/appui/assets/kit.css and embedded with go:embed, so a Go build and
// the speaker itself need no Node toolchain.
//
//   npm run build:kit    rewrite the embedded stylesheet
//   npm run check:kit    verify the checked-in copy is current
//
// Tailwind content scanning also reads script text, so a complete class string
// written as a JS literal is picked up. Classes assembled with concatenation
// are not; the safelist in the config is the stable contract for packaged apps.

import { readFile, writeFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import postcss from 'postcss'
import tailwindcss from 'tailwindcss'
import autoprefixer from 'autoprefixer'

const root = fileURLToPath(new URL('../', import.meta.url))
const outPath = fileURLToPath(new URL('../../internal/appui/assets/kit.css', import.meta.url))
const entryPath = fileURLToPath(new URL('../src/settings-kit.css', import.meta.url))
const configPath = fileURLToPath(new URL('../tailwind.settings.config.js', import.meta.url))

// Embedded in every settings document response. A ceiling keeps the binary and
// each frame cheap; the hand-written kit it replaces was ~7 KiB, and the full
// daisyUI component set for the documented classes is still a small fraction
// of the 512 KiB document budget.
export const MAX_KIT_BYTES = 160 * 1024

// The kit is redistributed to browsers, so its generated banner carries the
// full MIT copyright and permission notices, not just package names. Versions
// and texts are read from the installed packages so they cannot drift.
const KI = [
  { package: 'tailwindcss', label: 'Tailwind CSS' },
  { package: 'daisyui', label: 'daisyUI' }
]

async function packageNotice({ package: name, label }) {
  const meta = JSON.parse(await readFile(new URL(`../node_modules/${name}/package.json`, import.meta.url), 'utf8'))
  const text = (await readFile(new URL(`../node_modules/${name}/LICENSE`, import.meta.url), 'utf8')).trim()
  return ` * ${label} ${meta.version} — ${meta.license}\n *\n * ${text.split('\n').join('\n * ')}\n *`
}

async function buildBanner() {
  const notices = []
  for (const entry of KI) notices.push(await packageNotice(entry))
  return `/*
 * GENERATED FILE — do not edit.
 * Built by web/scripts/build-settings-kit.mjs from web/src/settings-kit.css,
 * web/tailwind.settings.config.js and web/theme.config.js.
 * Local CSS only, no network references. Run \`npm run build:kit\` after
 * changing a settings document or this kit.
 *
 * Third-party notices; the same texts are served as
 * /LICENSE-tailwindcss.txt and /LICENSE-daisyui.txt with the SPA:
 *
${notices.join('\n')}
 */
`
}

export function escapeSelector(token) {
  return token.replace(/[!"#$%&'()*+,./:;<=>?@[\]^`{|}~]/g, '\\$&')
}

/**
 * Classes referenced by markup or by a literal class string in the document's
 * script. Kept deliberately narrow: complete strings, not expressions.
 */
export function documentClasses(html) {
  const found = new Set()
  for (const match of html.matchAll(/\bclass\s*=\s*(?:"([^"]*)"|'([^']*)')/g)) {
    for (const token of (match[1] ?? match[2]).split(/\s+/)) if (token) found.add(token)
  }
  for (const statement of html.matchAll(/(?:className\s*=|classList\.(?:add|remove|toggle)\s*\()([^;\n]*)/g)) {
    for (const literal of statement[1].matchAll(/'([^']*)'|"([^"]*)"/g)) {
      for (const token of (literal[1] ?? literal[2]).split(/\s+/)) {
        if (token && !token.includes('+') && classShaped(token)) found.add(token)
      }
    }
  }
  // The wrapper is documentation, not a CSS component.
  found.delete('capsulad-ui')
  return [...found]
}

// A comparison such as `kind === 'ok'` sits in the same statement as the
// className assignment. Real classes are daisyUI names, Tailwind utilities or
// variants; filter those literals out instead of reporting them as missing.
const BARE_CLASSES = new Set([
  'flex', 'grid', 'hidden', 'block', 'inline-block', 'inline-flex', 'grow',
  'shrink-0', 'border', 'rounded', 'truncate', 'sr-only', 'relative', 'absolute',
  'sticky', 'uppercase', 'btn', 'badge', 'alert', 'card', 'input', 'select',
  'textarea', 'checkbox', 'toggle', 'range', 'radio', 'label', 'loading', 'link',
  'table', 'join', 'divider'
])

function classShaped(token) {
  return token.includes('-') || token.includes('/') || token.includes(':') || token.includes('[') ||
    BARE_CLASSES.has(token)
}

function assertKitContract(css) {
  const problems = []
  if (/@import\b/.test(css)) problems.push('contains @import')
  if (/url\(\s*['"]?https?:/i.test(css)) problems.push('references a remote URL')
  // daisyUI's spinner uses an inline data: SVG whose xmlns is the (non-fetched)
  // W3C namespace. Only remote url() targets and imports are disallowed.
  if (/@font-face\b/.test(css)) problems.push('declares an external font')
  for (const marker of [':root', '.btn', '.card', '.input', '.alert']) {
    if (!css.includes(marker)) problems.push(`missing ${marker}`)
  }
  for (const marker of ['Copyright (c) Tailwind Labs, Inc.', 'Copyright (c) 2020 Pouya Saadeghi', 'Permission is hereby granted']) {
    if (!css.includes(marker)) problems.push(`missing third-party notice: ${marker}`)
  }
  for (const name of ['capsulad', 'capsulad-dark']) {
    if (!new RegExp(`\\[data-theme=['"]?${name}['"]?\\]`).test(css)) problems.push(`missing theme ${name}`)
  }
  const bytes = Buffer.byteLength(css)
  if (bytes > MAX_KIT_BYTES) problems.push(`${bytes} bytes exceeds the ${MAX_KIT_BYTES}-byte ceiling`)
  return problems
}

export function assertDocumentCoverage(css, documents) {
  const problems = []
  for (const { name, html } of documents) {
    for (const token of documentClasses(html)) {
      const selector = '.' + escapeSelector(token)
      // Tailwind may group selectors: `.a,\n.b {`.
      const pattern = new RegExp(selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '(?=[\\s,{:])')
      if (!pattern.test(css)) problems.push(`${name}: class "${token}" is not in the kit`)
    }
  }
  return problems
}

export async function renderKit() {
  const source = await readFile(entryPath, 'utf8')
  const result = await postcss([
    tailwindcss(configPath),
    autoprefixer()
  ]).process(source, { from: entryPath })

  // Tailwind's preflight and license comments carry documentation URLs. The
  // embedded response is not the place for them; the banner carries the full
  // third-party notices instead.
  const cleaned = postcss.parse(result.css)
  cleaned.walkComments((comment) => comment.remove())
  const css = (await buildBanner()) + cleaned.toString()
  const documents = []
  for (const url of [
    new URL('../../internal/appui/assets/clock.html', import.meta.url),
    new URL('../../internal/appui/assets/ring.html', import.meta.url),
    new URL('../../apps/radio/ui/settings.html', import.meta.url)
  ]) {
    try {
      documents.push({ name: fileURLToPath(url).slice(root.length), html: await readFile(url, 'utf8') })
    } catch (err) {
      if (err.code !== 'ENOENT') throw err
    }
  }

  const problems = [...assertKitContract(css), ...assertDocumentCoverage(css, documents)]
  if (problems.length) {
    throw new Error('settings kit contract failed:\n  ' + problems.join('\n  '))
  }
  return css
}

async function main() {
  const check = process.argv.includes('--check')
  const css = await renderKit()
  if (check) {
    const current = await readFile(outPath, 'utf8').catch(() => '')
    if (current !== css) {
      console.error('internal/appui/assets/kit.css is stale; run `npm run build:kit`.')
      process.exit(1)
    }
    console.log(`settings kit is current (${Buffer.byteLength(css)} bytes)`)
    return
  }
  await writeFile(outPath, css)
  console.log(`wrote ${outPath} (${Buffer.byteLength(css)} bytes)`)
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main().catch((err) => {
    console.error(err.message || err)
    process.exit(1)
  })
}