// Presentation helpers. Status enums arrive as canonical proto names; the UI
// trims prefixes for display without changing stored values.

export function pct(value) {
  return Math.round((Number(value) || 0) * 100)
}

export function fixed(value, digits = 2) {
  return (Number(value) || 0).toFixed(digits)
}

export function dateTime(unixMs) {
  if (!unixMs) return '—'
  try {
    return new Date(unixMs).toLocaleString(undefined, {
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
  if (h > 0) return `${h}h ${String(m).padStart(2, '0')}m ${String(s).padStart(2, '0')}s`
  if (m > 0) return `${m}m ${String(s).padStart(2, '0')}s`
  return `${s}s`
}

export function status(value) {
  if (!value) return ''
  const prefixes = [
    'APP_RUN_STATE_', 'TOUCH_HOLD_MODE_', 'MUTED_INDICATOR_MODE_',
    'STOPWATCH_STATUS_', 'TIMER_STATUS_', 'ALERT_KIND_',
    'BACKEND_', 'RESOURCE_', 'PROVENANCE_STATUS_', 'TARGET_PLATFORM_',
    'PLAYBACK_'
  ]
  let text = String(value)
  for (const prefix of prefixes) text = text.replace(prefix, '')
  return text.replaceAll('_', ' ').toLowerCase()
}

export function resourceKind(value) {
  return status(value).replace(' segment display', ' display')
}

export function clockDays(mask) {
  const value = Number(mask) || 0
  if (!value) return 'one-shot'
  const names = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun']
  const days = names.filter((_, index) => value & (1 << index))
  return days.length ? days.join(', ') : 'one-shot'
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
