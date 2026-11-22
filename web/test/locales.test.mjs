import test from 'node:test'
import assert from 'node:assert/strict'
import enGB from '../src/locales/en-GB.json'
import ruRU from '../src/locales/ru-RU.json'
import { AVAILABLE_LOCALES, getLocale, i18n, setLocale } from '../src/lib/i18n.js'
import { formatBytes, formatNumber, keyScopes, status } from '../src/lib/format.js'

// Both catalogues are complete and shipped together. A missing key on either
// side is a build failure, not a silent English fallback in Russian (or vice
// versa). Placeholders must match too, or a translation drops a live value.

function walk(node, prefix = '') {
  const out = new Map()
  for (const [key, value] of Object.entries(node)) {
    const path = prefix ? `${prefix}.${key}` : key
    if (value && typeof value === 'object' && !Array.isArray(value)) {
      for (const [child, childValue] of walk(value, path)) out.set(child, childValue)
    } else {
      out.set(path, value)
    }
  }
  return out
}

function placeholders(text) {
  return [...new Set([...String(text).matchAll(/\{([a-zA-Z0-9_]+)\}/g)]
    .map((match) => match[1]))]
    .sort()
    .join(',')
}

test('both catalogues expose exactly the same keys and placeholders', () => {
  const en = walk(enGB)
  const ru = walk(ruRU)
  assert.deepEqual([...ru.keys()].sort(), [...en.keys()].sort())

  for (const [path, value] of en) {
    assert.equal(typeof value, 'string', `en ${path} is not a string`)
    assert.notEqual(value.trim(), '', `en ${path} is empty`)
    assert.equal(typeof ru.get(path), 'string', `ru ${path} is not a string`)
    assert.notEqual(String(ru.get(path)).trim(), '', `ru ${path} is empty`)
    assert.equal(placeholders(ru.get(path)), placeholders(value), `placeholder mismatch at ${path}`)
  }
})

test('the registry offers English (United Kingdom) and Russian', () => {
  assert.deepEqual(AVAILABLE_LOCALES.map((entry) => entry.id), ['en-GB', 'ru-RU'])
  assert.deepEqual(AVAILABLE_LOCALES.map((entry) => entry.short), ['EN', 'RU'])
})

test('protocol statuses normalise to the catalogue mapping keys', () => {
  assert.equal(status('APP_READY'), 'ready')
  assert.equal(status('APP_RUN_STATE_READY'), 'ready')
  assert.equal(status('APP_STARTING'), 'starting')
  assert.equal(status('APP_EXITED'), 'exited')
  assert.equal(status('PROVENANCE_VERIFIED'), 'verified')
  assert.equal(status('PROVENANCE_VERIFICATION_FAILED'), 'verification failed')
  assert.equal(status('PROVENANCE_UNSUPPORTED_RUNTIME'), 'unsupported runtime')
})

test('switching locale changes messages and locale-sensitive formatting', () => {
  try {
    setLocale('ru-RU')
    assert.equal(getLocale(), 'ru-RU')
    assert.equal(keyScopes(['status']), 'только статус')
    // Russian uses a comma decimal separator, proving Intl follows the locale.
    assert.equal(formatBytes(2048), '2,0 KiB')
    // Shared fixed-precision formatting also follows the locale.
    assert.equal(formatNumber(12.345, 2), '12,35')
    // Protocol status badges are translated, not raw enum text.
    assert.equal(i18n.global.t('diagnostics.runStates.ready'), 'Готово')
    assert.equal(i18n.global.t('diagnostics.runStates.failed'), 'Сбой')
    assert.equal(i18n.global.t('application.provenanceStatuses.verified'), 'Проверено')
    assert.equal(i18n.global.t('application.provenanceStatuses.verificationFailed'), 'Проверка не пройдена')
    // one / few / many, selected by vue-i18n's plural rule.
    assert.equal(i18n.global.t('training.missingWord', { count: 1 }, 1), 'ещё 1 запись слова активации')
    assert.equal(i18n.global.t('training.missingWord', { count: 3 }, 3), 'ещё 3 записи слова активации')
    assert.equal(i18n.global.t('training.missingWord', { count: 5 }, 5), 'ещё 5 записей слова активации')
  } finally {
    setLocale('en-GB')
  }
  assert.equal(getLocale(), 'en-GB')
  assert.equal(keyScopes(['status']), 'status only')
  assert.equal(formatBytes(2048), '2.0 KiB')
})
