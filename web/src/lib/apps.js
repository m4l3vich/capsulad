import { formatBytes } from './format.js'
import { session } from './session.js'

// Browser admin client for installed application packages. The device owns
// validation, the running-app refusal and the atomic swap; these helpers only
// carry the archive and mirror the limits for immediate feedback.

// Mirrors artifact.MaxArchiveBytes: the 64 MiB unpacked package budget plus
// the container slack the daemon allows for a maximum-size archive.
export const MAX_PACKAGE_BYTES = 65 * 1024 * 1024

/** Reject an unusable choice before the browser starts sending it. Empty string means valid. */
export function packageFileError(file) {
  if (!file) return 'Choose a package archive.'
  if (Number(file.size) === 0) return 'The archive is empty.'
  if (Number(file.size) > MAX_PACKAGE_BYTES) {
    return `The archive is ${formatBytes(file.size)}; the speaker accepts at most ${formatBytes(MAX_PACKAGE_BYTES)}.`
  }
  return ''
}

function describeOutcome(result) {
  const name = result.displayName || result.appId
  if (!result.replaced) return `Installed ${name} ${result.version}.`
  if (result.previousVersion && result.previousVersion !== result.version) {
    return `Updated ${name} from ${result.previousVersion} to ${result.version}.`
  }
  return `Reinstalled ${name} ${result.version}.`
}

/** The sentence shown after a successful install. */
export function installOutcome(result) {
  return describeOutcome(result)
}

async function readError(resp) {
  try {
    const body = await resp.json()
    return body.error || `request failed (${resp.status})`
  } catch (_) {
    return `request failed (${resp.status})`
  }
}

/**
 * Install or replace one package. XMLHttpRequest is used instead of fetch
 * because only it reports upload progress; the archive is the raw request body
 * and the device streams it straight into its staging tree. Resolves with
 * { appId, displayName, version, previousVersion, replaced }.
 */
export function installPackage(file, { onProgress } = {}) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    xhr.open('POST', '/api/apps')
    xhr.withCredentials = true
    xhr.setRequestHeader('Accept', 'application/json')
    xhr.setRequestHeader('X-Capsulad-CSRF', session.csrf)
    // Any non-form content type keeps the body raw: the session middleware
    // only parses form-encoded requests.
    xhr.setRequestHeader('Content-Type', 'application/gzip')
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
      if (xhr.status >= 200 && xhr.status < 300 && body.app_id) {
        resolve({
          appId: String(body.app_id),
          displayName: String(body.display_name || ''),
          version: String(body.version || ''),
          previousVersion: String(body.previous_version || ''),
          replaced: Boolean(body.replaced)
        })
        return
      }
      reject(new Error(body.error || `install failed (${xhr.status})`))
    }
    xhr.onerror = () => reject(new Error('install failed: the connection to the speaker was lost'))
    xhr.onabort = () => reject(new Error('install cancelled'))
    xhr.send(file)
  })
}

/** Remove one installed package. App settings are kept by the device. */
export async function removePackage(appId) {
  const resp = await fetch(`/api/apps/${encodeURIComponent(appId)}`, {
    method: 'DELETE',
    credentials: 'same-origin',
    headers: { Accept: 'application/json', 'X-Capsulad-CSRF': session.csrf }
  })
  if (!resp.ok) throw new Error(await readError(resp))
}
