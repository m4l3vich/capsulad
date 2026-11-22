import { reactive } from 'vue'

// Browser-local appearance choice for the console and the embedded application
// settings frames. `System` is the default and follows the operating system
// live; Light and Dark are explicit and stop following it until the user picks
// System again.
//
// `public/theme-boot.js` applies the same choice before first paint. The
// storage key, mode names and theme attributes are the contract between the
// two files; this module owns every change after boot.

export const THEME_STORAGE_KEY = 'capsulad.theme'

export const THEME_MODES = Object.freeze([
  { id: 'light', label: 'Light', icon: 'light_mode' },
  { id: 'dark', label: 'Dark', icon: 'dark_mode' },
  { id: 'system', label: 'System', icon: 'devices' }
])

const LIGHT_THEME = 'capsulad'
const DARK_THEME = 'capsulad-dark'
const media = typeof window !== 'undefined' && window.matchMedia
  ? window.matchMedia('(prefers-color-scheme: dark)')
  : null

function normalizeMode(value) {
  return value === 'light' || value === 'dark' || value === 'system' ? value : 'system'
}

function readStoredMode() {
  try {
    const stored = window.localStorage.getItem(THEME_STORAGE_KEY)
    return stored ? normalizeMode(stored) : 'system'
  } catch (_) {
    // Private mode or blocked storage: System is the documented default and
    // needs no persistence.
    return 'system'
  }
}

function resolve(mode) {
  if (mode === 'light') return 'light'
  if (mode === 'dark') return 'dark'
  return media?.matches ? 'dark' : 'light'
}

const bootMode = typeof document !== 'undefined'
  ? document.documentElement.dataset.themeMode
  : ''
const initialMode = bootMode ? normalizeMode(bootMode) : readStoredMode()

export const theme = reactive({
  mode: initialMode,
  resolved: resolve(initialMode)
})

function updateMeta(dark) {
  if (typeof document === 'undefined') return
  const themeColor = document.querySelector('meta[name="theme-color"]')
  if (themeColor) themeColor.setAttribute('content', dark ? '#0b1220' : '#f8fafc')
  const colorScheme = document.querySelector('meta[name="color-scheme"]')
  if (colorScheme) colorScheme.setAttribute('content', dark ? 'dark' : 'light')
}

function apply() {
  if (typeof document === 'undefined') return
  const dark = theme.resolved === 'dark'
  const root = document.documentElement
  root.setAttribute('data-theme', dark ? DARK_THEME : LIGHT_THEME)
  root.setAttribute('data-theme-mode', theme.mode)
  updateMeta(dark)
}

/** The persisted mode is Light, Dark or System; System is the default. */
export function setThemeMode(mode) {
  const next = normalizeMode(mode)
  theme.mode = next
  theme.resolved = resolve(next)
  try {
    window.localStorage.setItem(THEME_STORAGE_KEY, next)
  } catch (_) {
    // The choice still applies to this page; only persistence is unavailable.
  }
  apply()
}

if (media) {
  const onSystemChange = () => {
    if (theme.mode !== 'system') return
    theme.resolved = resolve('system')
    apply()
  }
  if (media.addEventListener) media.addEventListener('change', onSystemChange)
  else if (media.addListener) media.addListener(onSystemChange)
}

if (typeof window !== 'undefined') {
  // Keep a second tab of the same console in step. Storage events never fire
  // for the tab that wrote the value.
  window.addEventListener('storage', (event) => {
    if (event.key !== THEME_STORAGE_KEY) return
    const next = normalizeMode(event.newValue)
    if (next === theme.mode) return
    theme.mode = next
    theme.resolved = resolve(next)
    apply()
  })
}

// The boot script normally ran first; this also covers a document loaded
// without it (for example an isolated component test).
apply()