#!/usr/bin/env node
// The daemon serves the SPA with `script-src 'self'`, so the built bundle must
// never depend on the eval family. vue-i18n's runtime message compiler used to
// call `new Function` for every message; the JSON catalogues are now
// precompiled by @intlify/unplugin-vue-i18n. This check is the build-time
// regression guard for that contract.

import { readdirSync, readFileSync, statSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

const assetsDir = fileURLToPath(new URL('../../internal/webui/dist/assets/', import.meta.url))

const PATTERNS = [
  { name: 'new Function', re: /\bnew\s+Function\s*\(/ },
  { name: 'eval call', re: /(?:^|[^A-Za-z0-9_$])eval\s*\(/ },
  { name: 'unsafe-eval dependency', re: /unsafe-eval/ }
]

let files
try {
  files = readdirSync(assetsDir).filter((name) => name.endsWith('.js')).sort()
} catch (err) {
  console.error(`check-csp: cannot read ${assetsDir}: ${err.message}`)
  console.error('check-csp: run `npm run build` first')
  process.exit(1)
}

if (files.length === 0) {
  console.error(`check-csp: no JavaScript assets under ${assetsDir}`)
  process.exit(1)
}

const failures = []
let bytes = 0
for (const name of files) {
  const path = `${assetsDir}${name}`
  bytes += statSync(path).size
  const source = readFileSync(path, 'utf8')
  for (const { name: label, re } of PATTERNS) {
    const match = re.exec(source)
    if (match) failures.push(`${name}: ${label} near ${JSON.stringify(match[0])}`)
  }
}

if (failures.length > 0) {
  console.error('check-csp: built assets require eval; the daemon CSP forbids it')
  for (const failure of failures) console.error(`  ${failure}`)
  process.exit(1)
}

console.log(`check-csp: ok (${files.length} JS assets, ${bytes} bytes, no new Function/eval)`)
