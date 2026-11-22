import { createI18n } from 'vue-i18n'
import enGB from '../locales/en-GB.js'
import ruRU from '../locales/ru-RU.js'

// The console ships two complete catalogues. A locale is a first-class UI
// choice: it is persisted in localStorage, applied to <html lang>, and used by
// the formatters in lib/format.js. Adding a locale means importing its
// catalogue, adding it to MESSAGES, and adding an AVAILABLE_LOCALES entry.

export const LOCALE_STORAGE_KEY = 'capsulad-locale'
export const DEFAULT_LOCALE = 'en-GB'

export const AVAILABLE_LOCALES = Object.freeze([
  { id: 'en-GB', labelKey: 'language.enGB', short: 'EN' },
  { id: 'ru-RU', labelKey: 'language.ruRU', short: 'RU' }
])

const MESSAGES = {
  'en-GB': enGB,
  'ru-RU': ruRU
}

function normalizeLocale(value) {
  return AVAILABLE_LOCALES.some((entry) => entry.id === value) ? value : DEFAULT_LOCALE
}

function readStoredLocale() {
  if (typeof window === 'undefined' || !window.localStorage) return DEFAULT_LOCALE
  try {
    return normalizeLocale(window.localStorage.getItem(LOCALE_STORAGE_KEY))
  } catch (_) {
    // Private mode or blocked storage: the documented default needs no
    // persistence.
    return DEFAULT_LOCALE
  }
}

const initialLocale = readStoredLocale()

/**
 * vue-i18n does not infer Slavic plural categories on its own. Russian uses
 * one/few/many, selected from the absolute count and its last two digits.
 */
function russianPlural(choice) {
  const value = Math.abs(Number(choice) || 0)
  const mod10 = value % 10
  const mod100 = value % 100
  if (mod10 === 1 && mod100 !== 11) return 0
  if (mod10 >= 2 && mod10 <= 4 && (mod100 < 12 || mod100 > 14)) return 1
  return 2
}

export const i18n = createI18n({
  legacy: false,
  locale: initialLocale,
  fallbackLocale: DEFAULT_LOCALE,
  globalInjection: true,
  pluralRules: {
    'ru-RU': russianPlural
  },
  messages: MESSAGES
})

/** Translation function for non-component modules such as the API helpers. */
export const t = i18n.global.t

/** The active locale id, read by locale-sensitive formatters. */
export function getLocale() {
  return i18n.global.locale.value
}

function applyDocumentLang() {
  if (typeof document === 'undefined') return
  document.documentElement.lang = getLocale()
}

/** Switch locale, persist the choice and update <html lang>. */
export function setLocale(value) {
  const next = normalizeLocale(value)
  i18n.global.locale.value = next
  if (typeof window !== 'undefined' && window.localStorage) {
    try {
      window.localStorage.setItem(LOCALE_STORAGE_KEY, next)
    } catch (_) {
      // The choice still applies to this page; only persistence is absent.
    }
  }
  applyDocumentLang()
  if (typeof window !== 'undefined') {
    window.dispatchEvent(new CustomEvent('capsulad-locale', { detail: next }))
  }
  return next
}

if (typeof window !== 'undefined') {
  // Keep another tab of the same console in step.
  window.addEventListener('storage', (event) => {
    if (event.key !== LOCALE_STORAGE_KEY) return
    const next = normalizeLocale(event.newValue)
    if (next === getLocale()) return
    i18n.global.locale.value = next
    applyDocumentLang()
  })
}

applyDocumentLang()
