import { session } from './session.js'

// Browser admin client for the shared read-only asset store. The same
// directory is mounted at /assets inside every app sandbox, so these calls
// require a session, same-origin intent and the session CSRF token. The daemon
// owns validation; the helpers here mirror it for immediate feedback.

export const MAX_ASSET_BYTES = 32 * 1024 * 1024

// Mirrors the daemon's staging-file prefix; such names are never published.
const STAGING_PREFIX = '.capsulad-upload-'

/** Client-side copy of the store's path rules. Empty string means valid. */
export function assetPathError(value) {
  const path = String(value ?? '').trim()
  if (!path) return 'Enter a destination path.'
  if (path.includes('\0')) return 'The path contains a control character.'
  if (path.includes('\\')) return 'Use / as the path separator.'
  if (path.startsWith('/')) return 'Use a path relative to the asset store, not an absolute path.'
  if (path.length > 4096) return 'The path is too long.'
  for (const part of path.split('/')) {
    if (!part || part === '.' || part === '..') {
      return 'The path cannot contain empty, "." or ".." components.'
    }
    if (part.startsWith(STAGING_PREFIX)) return 'That name is reserved for uploads in progress.'
    if (part.length > 255) return 'Each path component must be at most 255 characters.'
  }
  return ''
}

/** The destination a file picker selection suggests by default. */
export function defaultTarget(name) {
  return String(name ?? '').trim()
}

export function formatBytes(bytes) {
  const value = Number(bytes) || 0
  if (value < 1024) return `${value} B`
  const units = ['KiB', 'MiB', 'GiB']
  let scaled = value / 1024
  let index = 0
  while (scaled >= 1024 && index < units.length - 1) {
    scaled /= 1024
    index += 1
  }
  return `${scaled >= 10 ? scaled.toFixed(0) : scaled.toFixed(1)} ${units[index]}`
}

export function shortHash(hash) {
  const text = String(hash ?? '')
  if (!text) return '—'
  return text.length > 16 ? `${text.slice(0, 16)}…` : text
}

async function readError(resp) {
  try {
    const body = await resp.json()
    return body.error || `request failed (${resp.status})`
  } catch (_) {
    return `request failed (${resp.status})`
  }
}

/** List the catalog. Returns { assets, totalBytes }. */
export async function listAssets() {
  const resp = await fetch('/api/assets', {
    credentials: 'same-origin',
    headers: { Accept: 'application/json' }
  })
  if (!resp.ok) throw new Error(await readError(resp))
  const body = await resp.json()
  return {
    assets: Array.isArray(body.assets) ? body.assets : [],
    totalBytes: Number(body.total_bytes) || 0
  }
}

/**
 * Upload one file as the raw request body. XMLHttpRequest is used instead of
 * fetch because only it reports upload progress. Resolves with the created
 * catalog entry.
 */
export function uploadAsset(file, target, { onProgress } = {}) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    xhr.open('POST', `/api/assets?path=${encodeURIComponent(target)}`)
    xhr.withCredentials = true
    xhr.setRequestHeader('Accept', 'application/json')
    xhr.setRequestHeader('X-Capsulad-CSRF', session.csrf)
    xhr.setRequestHeader('Content-Type', file.type || 'application/octet-stream')
    if (xhr.upload && typeof onProgress === 'function') {
      xhr.upload.onprogress = (event) => {
        if (event.lengthComputable && event.total > 0) onProgress(event.loaded / event.total)
      }
    }
    xhr.onload = () => {
      let body = {}
      try {
        body = JSON.parse(xhr.responseText)
      } catch (_) {
        body = {}
      }
      if (xhr.status >= 200 && xhr.status < 300 && body.asset) resolve(body.asset)
      else reject(new Error(body.error || `upload failed (${xhr.status})`))
    }
    xhr.onerror = () => reject(new Error('upload failed: the connection to the speaker was lost'))
    xhr.onabort = () => reject(new Error('upload cancelled'))
    xhr.send(file)
  })
}

/** Delete one regular file. Directories are never removed. */
export async function deleteAsset(target) {
  const resp = await fetch(`/api/assets?path=${encodeURIComponent(target)}`, {
    method: 'DELETE',
    credentials: 'same-origin',
    headers: { Accept: 'application/json', 'X-Capsulad-CSRF': session.csrf }
  })
  if (!resp.ok) throw new Error(await readError(resp))
}