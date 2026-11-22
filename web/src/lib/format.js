import { getLocale, t } from './i18n.js'

// Presentation helpers. Status enums arrive as canonical proto names; the UI
// trims prefixes for display without changing stored values. Every number and
// date rendered here follows the active console locale.

/** Locale-sensitive decimal number, used wherever a value is shown with a fixed precision. */
export function formatNumber(value, digits = 0) {
  return new Intl.NumberFormat(getLocale(), {
    minimumFractionDigits: digits,
    maximumFractionDigits: digits
  }).format(Number(value) || 0)
}

export function pct(value) {
  return Math.round((Number(value) || 0) * 100)
}

/** Human byte size using binary units, e.g. "1.4 MiB". */
export function formatBytes(bytes) {
  const value = Number(bytes) || 0
  if (value < 1024) return `${formatNumber(value, 0)} B`
  const units = ['KiB', 'MiB', 'GiB']
  let scaled = value / 1024
  let index = 0
  while (scaled >= 1024 && index < units.length - 1) {
    scaled /= 1024
    index += 1
  }
  return `${formatNumber(scaled, scaled >= 10 ? 0 : 1)} ${units[index]}`
}

export function fixed(value, digits = 2) {
  return (Number(value) || 0).toFixed(digits)
}

export function hasValidKeyScopes(scopes) {
  return Array.isArray(scopes) && scopes.length > 0 &&
    scopes.every(scope => scope === 'status' || scope === 'assistant' || scope === 'operator')
}

/** Human label for the recorded grant; malformed or missing grants give no access. */
export function keyScopes(scopes) {
  if (!hasValidKeyScopes(scopes)) {
    return Array.isArray(scopes) && scopes.length > 0
      ? t('format.noAccessList', { scopes: scopes.join(', ') })
      : t('format.noAccess')
  }
  if (scopes.includes('operator')) return t('format.fullAdmin')
  if (scopes.includes('assistant')) return t('format.assistantControls')
  return t('format.statusOnly')
}

export function dateTime(unixMs) {
  if (!unixMs) return '—'
  try {
    return new Date(unixMs).toLocaleString(getLocale(), {
      year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit'
    })
  } catch (_) {
    return '—'
  }
}

export function durationMs(ms) {
  const total = Math.round((Number(ms) || 0) / 1000)
  const h = Math.floor(total / 3600)
  const m = Math.floor((total % 3600) / 60)
  const s = total % 60
  const hours = t('format.hoursShort')
  const minutes = t('format.minutesShort')
  const seconds = t('format.secondsShort')
  if (h > 0) return `${h}${hours} ${String(m).padStart(2, '0')}${minutes} ${String(s).padStart(2, '0')}${seconds}`
  if (m > 0) return `${m}${minutes} ${String(s).padStart(2, '0')}${seconds}`
  return `${s}${seconds}`
}

export function status(value) {
  if (!value) return ''
  const prefixes = [
    'APP_RUN_STATE_', 'APP_', 'TOUCH_HOLD_MODE_', 'MUTED_INDICATOR_MODE_',
    'STOPWATCH_STATUS_', 'TIMER_STATUS_', 'ALERT_KIND_',
    'BACKEND_', 'RESOURCE_', 'PROVENANCE_STATUS_', 'PROVENANCE_', 'TARGET_PLATFORM_',
    'PLAYBACK_'
  ]
  let text = String(value)
  for (const prefix of prefixes) text = text.replace(prefix, '')
  return text.replaceAll('_', ' ').toLowerCase()
}

export function resourceKind(value) {
  return status(value).replace(' segment display', ' display')
}

export function hasDay(mask, day) {
  return Boolean((Number(mask) || 0) & (1 << day))
}

export function logLine(entry) {
  return `${String(entry.severity || 'INFO').padEnd(5)} ${entry.component || 'daemon'}${entry.appId ? `[${entry.appId}]` : ''} ${entry.message || ''}`
}

export function shortId(value) {
  const text = String(value || '')
  return text.length > 12 ? text.slice(0, 12) : text
}

export function safeJson(value) {
  if (value === undefined || value === null || value === '') return ''
  try {
    const parsed = JSON.parse(value)
    if (typeof parsed === 'string') return parsed
    return String(parsed)
  } catch (_) {
    return String(value)
  }
}
