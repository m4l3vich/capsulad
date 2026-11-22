// Collect every icon name the SPA can render.
//
// Static names come from the text between <Icon>…</Icon>. Conditional text
// (the Wi-Fi connected/unavailable pair) contributes its quoted names. The
// remaining expressions are dynamic, so they must be registered here and are
// backed by the shared lists in src/lib/iconNames.js; an unregistered
// expression fails the check instead of silently missing a glyph.
//
// A self-closing `<Icon ... />` or a tag without `</Icon>` has no slot text, so
// it can never render a glyph: both fail with a source line instead of being
// skipped. Commented-out template markup is ignored rather than parsed.
//
// `node scripts/icon-names.mjs` prints the sorted JSON list for
// scripts/build-icon-font.py; `check-icon-font.mjs` and the icon-font test use
// the exported functions.

import { readdir, readFile } from 'node:fs/promises'
import { join, relative } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import {
  SETTINGS_SECTION_ICONS,
  TOAST_ICONS,
  WIFI_SIGNAL_ICONS,
  THEME_ICONS
} from '../src/lib/iconNames.js'

const sourceRoot = fileURLToPath(new URL('../src/', import.meta.url))

// Whitespace-normalized <Icon> body -> the shared list that supplies it.
const DYNAMIC_EXPRESSIONS = new Map([
  ['{{ look(toast).icon }}', Object.values(TOAST_ICONS)],
  ['{{ signalLevel(network.signalDbm).icon }}', Object.values(WIFI_SIGNAL_ICONS)],
  ['{{ section.icon }}', Object.values(SETTINGS_SECTION_ICONS)],
  ['{{ currentThemeIcon }}', Object.values(THEME_ICONS)],
  ['{{ theme.icon }}', Object.values(THEME_ICONS)]
])

export const ICON_NAME_PATTERN = /^[a-z0-9_]+$/

export function isIconName(value) {
  return ICON_NAME_PATTERN.test(value)
}

async function vueFiles(root) {
  const out = []
  for (const entry of await readdir(root, { withFileTypes: true })) {
    const path = join(root, entry.name)
    if (entry.isDirectory()) out.push(...await vueFiles(path))
    else if (entry.name.endsWith('.vue')) out.push(path)
  }
  return out
}

// Reduce one <Icon> body to the names it can render. Throws for an
// unrecognized body so a new dynamic source cannot pass unnoticed.
export function iconNamesInBody(body, where) {
  const normalized = body.trim().replace(/\s+/g, ' ')
  if (isIconName(normalized)) return [normalized]

  const dynamic = DYNAMIC_EXPRESSIONS.get(normalized)
  if (dynamic) return dynamic

  const quoted = [...normalized.matchAll(/'([^']+)'|"([^"]+)"/g)]
    .map((match) => match[1] || match[2])
    .filter(isIconName)
  if (quoted.length) return quoted

  throw new Error(
    `${where}: unrecognized <Icon> body ${JSON.stringify(normalized)}; ` +
    'register the dynamic expression in scripts/icon-names.mjs or use a named glyph'
  )
}

// Vue templates may keep commented-out markup. A commented <Icon> never
// renders, so blank the comments before extraction; replacing every character
// except the newline preserves byte offsets and therefore line numbers.
function withoutComments(source) {
  return source.replace(/<!--[\s\S]*?-->/g, (comment) => comment.replace(/[^\n]/g, ' '))
}

function lineOf(source, index) {
  return source.slice(0, index).split('\n').length
}

// Every <Icon> must be a paired tag with a body. A self-closing <Icon ... />
// has no slot text, so it contributes no glyph and would ship a missing icon;
// an unclosed <Icon> is the same mistake spelled differently. Both fail here
// instead of depending on the paired-tag regex to notice an empty body.
export function iconNamesInSource(source, where) {
  const visible = withoutComments(source)

  // The self-closing scan runs before pairing: `<Icon ... />` would otherwise
  // be consumed as the opening tag of the next paired icon.
  for (const match of visible.matchAll(/<Icon\b[^>]*\/>/g)) {
    throw new Error(
      `${where}:${lineOf(visible, match.index)}: self-closing <Icon ... /> renders no glyph; ` +
      'use <Icon>name</Icon> so the font subset carries the icon'
    )
  }

  const names = new Set()
  const paired = []
  for (const match of visible.matchAll(/<Icon\b[^>]*>([\s\S]*?)<\/Icon>/g)) {
    paired.push([match.index, match.index + match[0].length])
    const line = lineOf(visible, match.index)
    for (const name of iconNamesInBody(match[1], `${where}:${line}`)) names.add(name)
  }

  for (const match of visible.matchAll(/<Icon\b/g)) {
    if (paired.some(([start, end]) => match.index >= start && match.index < end)) continue
    throw new Error(
      `${where}:${lineOf(visible, match.index)}: <Icon> has no closing </Icon>; ` +
      'a tag without slot text renders no glyph'
    )
  }
  return [...names]
}

export async function collectIconNames(root = sourceRoot) {
  const names = new Set()
  for (const values of DYNAMIC_EXPRESSIONS.values()) {
    for (const name of values) names.add(name)
  }

  for (const file of await vueFiles(root)) {
    const source = await readFile(file, 'utf8')
    for (const name of iconNamesInSource(source, relative(sourceRoot, file))) names.add(name)
  }
  return [...names].sort()
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const names = await collectIconNames()
  console.log(JSON.stringify(names))
}
