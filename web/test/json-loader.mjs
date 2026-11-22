// Node does not import JSON modules without an import attribute (and Node 24
// removed the older `assert` spelling). Vite handles JSON natively, so the
// tests need this small loader to load the same catalogue files the bundle
// compiles. Keep it test-only; the production bundle never uses this path.

import { readFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'

export async function load(url, context, nextLoad) {
  if (url.endsWith('.json')) {
    const source = await readFile(fileURLToPath(url), 'utf8')
    return {
      format: 'module',
      source: `export default ${source}\n`,
      shortCircuit: true
    }
  }
  return nextLoad(url, context)
}
