import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import vm from 'node:vm'
import { THEME_MODES, THEME_STORAGE_KEY, setThemeMode, theme } from '../src/lib/theme.js'

// The appearance choice is applied by a classic boot script before first paint
// and owned afterwards by src/lib/theme.js. These cover the shared contract:
// System is the default, explicit choices are honoured, and an explicit choice
// does not follow the operating system.

const bootSource = fs.readFileSync(new URL('../public/theme-boot.js', import.meta.url), 'utf8')

function boot({ stored, prefersDark, storageThrows = false }) {
  const attributes = {}
  const meta = {}
  const sandbox = {
    window: {
      localStorage: {
        getItem() {
          if (storageThrows) throw new Error('blocked')
          return stored ?? null
        }
      },
      matchMedia: () => ({ matches: prefersDark })
    },
    document: {
      documentElement: {
        setAttribute(name, value) {
          attributes[name] = value
        }
      },
      querySelector(selector) {
        return { setAttribute: (name, value) => { meta[selector + ':' + name] = value } }
      }
    }
  }
  vm.runInNewContext(bootSource, sandbox)
  return { attributes, meta }
}

test('a fresh browser follows the OS and remembers System', () => {
  const dark = boot({ prefersDark: true })
  assert.equal(dark.attributes['data-theme'], 'capsulad-dark')
  assert.equal(dark.attributes['data-theme-mode'], 'system')
  assert.equal(dark.meta['meta[name="color-scheme"]:content'], 'dark')

  const light = boot({ prefersDark: false })
  assert.equal(light.attributes['data-theme'], 'capsulad')
  assert.equal(light.attributes['data-theme-mode'], 'system')
})

test('an explicit choice ignores the OS palette', () => {
  const light = boot({ stored: 'light', prefersDark: true })
  assert.equal(light.attributes['data-theme'], 'capsulad')
  assert.equal(light.attributes['data-theme-mode'], 'light')
  assert.equal(light.meta['meta[name="color-scheme"]:content'], 'light')

  const dark = boot({ stored: 'dark', prefersDark: false })
  assert.equal(dark.attributes['data-theme'], 'capsulad-dark')
  assert.equal(dark.attributes['data-theme-mode'], 'dark')
})

test('unavailable storage still boots into System', () => {
  const result = boot({ prefersDark: true, storageThrows: true })
  assert.equal(result.attributes['data-theme'], 'capsulad-dark')
  assert.equal(result.attributes['data-theme-mode'], 'system')
})

test('the switch offers exactly Light, Dark and System, defaulting to System', () => {
  assert.deepEqual(THEME_MODES.map((mode) => mode.id), ['light', 'dark', 'system'])
  assert.equal(THEME_STORAGE_KEY, 'capsulad.theme')
  // Node has no document/window: apply() and persistence are no-ops, but the
  // state machine is the same one the browser uses.
  assert.equal(theme.mode, 'system')
  setThemeMode('dark')
  assert.equal(theme.mode, 'dark')
  assert.equal(theme.resolved, 'dark')
  setThemeMode('light')
  assert.equal(theme.resolved, 'light')
  setThemeMode('nonsense')
  assert.equal(theme.mode, 'system')
  assert.equal(theme.resolved, 'light')
})

test('the shell loads the boot script from the daemon origin, not inline', () => {
  const html = fs.readFileSync(new URL('../index.html', import.meta.url), 'utf8')
  const head = html.slice(0, html.indexOf('</head>'))
  assert.match(head, /<script src="\/theme-boot\.js"><\/script>/)
  assert.match(head, /data-theme="capsulad"/)
  assert.ok(!/<script(?![^>]*\bsrc=)[^>]*>/.test(head), 'CSP allows only same-origin scripts')
})