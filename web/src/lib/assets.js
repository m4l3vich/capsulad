import { session } from './session.js'
import { t } from './i18n.js'

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
  if (!path) return t('errors.assetPathRequired')
  if (path.includes('\0')) return t('errors.assetPathControl')
  if (path.includes('\\')) return t('errors.assetPathSeparator')
  if (path.startsWith('/')) return t('errors.assetPathAbsolute')
  if (path.length > 4096) return t('errors.assetPathTooLong')
  for (const part of path.split('/')) {
    if (!part || part === '.' || part === '..') {
      return t('errors.assetPathComponents')
    }
    if (part.startsWith(STAGING_PREFIX)) return t('errors.assetPathReserved')
    if (part.length > 255) return t('errors.assetPathComponentLength')
  }
  return ''
}

/** The destination a file picker selection suggests by default. */
export function defaultTarget(name) {
  return String(name ?? '').trim()
}

export function shortHash(hash) {
  const text = String(hash ?? '')
  if (!text) return '—'
  return text.length > 16 ? `${text.slice(0, 16)}…` : text
}

async function readError(resp) {
  try {
    const body = await resp.json()
    return body.error || t('errors.requestFailed', { status: resp.status })
  } catch (_) {
    return t('errors.requestFailed', { status: resp.status })
  }
}

/** List the catalog. Returns { assets, totalBytes, freeBytes, freeAvailable }. */
export async function listAssets() {
  const resp = await fetch('/api/assets', {
    credentials: 'same-origin',
    headers: { Accept: 'application/json' }
  })
  if (!resp.ok) throw new Error(await readError(resp))
  const body = await resp.json()
  return {
    assets: Array.isArray(body.assets) ? body.assets : [],
    totalBytes: Number(body.total_bytes) || 0,
    freeBytes: Number(body.free_bytes) || 0,
    freeAvailable: Boolean(body.free_available)
  }
}

/** Create one folder in the store. Existing directories are accepted. */
export async function createFolder(target) {
  const resp = await fetch(`/api/assets/folder?path=${encodeURIComponent(target)}`, {
    method: 'POST',
    credentials: 'same-origin',
    headers: { Accept: 'application/json', 'X-Capsulad-CSRF': session.csrf }
  })
  if (!resp.ok) throw new Error(await readError(resp))
  const body = await resp.json()
  return String(body.folder || target)
}

/** Same-origin download URL for one published file. */
export function downloadURL(target) {
  return `/api/assets/file?path=${encodeURIComponent(target)}`
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
      else reject(new Error(body.error || t('errors.uploadFailed', { status: xhr.status })))
    }
    xhr.onerror = () => reject(new Error(t('errors.connectionLostUpload')))
    xhr.onabort = () => reject(new Error(t('errors.uploadCancelled')))
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