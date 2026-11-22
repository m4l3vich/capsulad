import { formatBytes } from './format.js'
import { session } from './session.js'
import { t } from './i18n.js'

// Browser admin client for installed application packages. The device owns
// validation, the running-app refusal and the atomic swap; these helpers only
// carry the archive and mirror the limits for immediate feedback.

// Mirrors artifact.MaxArchiveBytes: the 64 MiB unpacked package budget plus
// the container slack the daemon allows for a maximum-size archive.
export const MAX_PACKAGE_BYTES = 65 * 1024 * 1024

/** Reject an unusable choice before the browser starts sending it. Empty string means valid. */
export function packageFileError(file) {
  if (!file) return t('install.chooseArchive')
  if (Number(file.size) === 0) return t('install.emptyArchive')
  if (Number(file.size) > MAX_PACKAGE_BYTES) {
    return t('install.archiveTooLarge', {
      size: formatBytes(file.size),
      max: formatBytes(MAX_PACKAGE_BYTES)
    })
  }
  return ''
}

/**
 * Reject a URL the device would refuse before opening a request. Only the
 * scheme and structure are checked here; the daemon owns the download.
 */
export function packageURLError(value) {
  const text = String(value || '').trim()
  if (!text) return t('install.urlRequired')
  let parsed
  try {
    parsed = new URL(text)
  } catch (_) {
    return t('install.urlAbsolute')
  }
  if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
    return t('install.urlScheme')
  }
  if (!parsed.hostname) return t('install.urlHost')
  // WHATWG URL folds "https:///pkg" into a host named "pkg"; the daemon's
  // parser does not, so require the literal authority separator here too.
  if (!/^https?:\/\/[^/\s]+/i.test(text)) return t('install.urlHost')
  if (parsed.username || parsed.password) return t('install.urlCredentials')
  if (parsed.hash) return t('install.urlFragment')
  return ''
}

function describeOutcome(result) {
  const name = result.displayName || result.appId
  if (!result.replaced) return t('install.outcomeInstalled', { name, version: result.version })
  if (result.previousVersion && result.previousVersion !== result.version) {
    return t('install.outcomeUpdated', {
      name,
      previous: result.previousVersion,
      version: result.version
    })
  }
  return t('install.outcomeReinstalled', { name, version: result.version })
}

/** The sentence shown after a successful install. */
export function installOutcome(result) {
  return describeOutcome(result)
}

async function readError(resp) {
  try {
    const body = await resp.json()
    return body.error || t('errors.requestFailed', { status: resp.status })
  } catch (_) {
    return t('errors.requestFailed', { status: resp.status })
  }
}

/** Normalize the daemon's install reply into the shape the page renders. */
function installResult(body) {
  return {
    appId: String(body.app_id),
    displayName: String(body.display_name || ''),
    version: String(body.version || ''),
    previousVersion: String(body.previous_version || ''),
    replaced: Boolean(body.replaced)
  }
}

/**
 * Install or replace one package. XMLHttpRequest is used instead of fetch
 * because only it reports upload progress; the archive is the raw request body
 * and the device streams it straight into its staging tree. Resolves with
 * { appId, displayName, version, previousVersion, replaced }.
 *
 * An AbortSignal cancels the upload; the promise then rejects with the same
 * "install cancelled" message the URL route uses.
 */
export function installPackage(file, { onProgress, signal } = {}) {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(new Error(t('errors.installCancelled')))
      return
    }
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
    const onAbort = () => xhr.abort()
    const cleanup = () => signal?.removeEventListener('abort', onAbort)
    signal?.addEventListener('abort', onAbort, { once: true })
    xhr.onload = () => {
      cleanup()
      let body = {}
      try {
        body = JSON.parse(xhr.responseText)
      } catch (_) {
        body = {}
      }
      if (xhr.status >= 200 && xhr.status < 300 && body.app_id) {
        resolve(installResult(body))
        return
      }
      reject(new Error(body.error || t('errors.installFailed', { status: xhr.status })))
    }
    xhr.onerror = () => {
      cleanup()
      reject(new Error(t('errors.connectionLostInstall')))
    }
    xhr.onabort = () => {
      cleanup()
      reject(new Error(t('errors.installCancelled')))
    }
    xhr.send(file)
  })
}

/**
 * Ask the daemon to download and install a package from an http(s) URL. The
 * daemon fetches and validates it; the browser only waits, so there is no byte
 * progress to report. Resolves with the same shape as installPackage.
 */
export async function installPackageFromURL(url, { signal } = {}) {
  let resp
  try {
    resp = await fetch('/api/apps/url', {
      method: 'POST',
      credentials: 'same-origin',
      headers: {
        'Content-Type': 'application/json',
        Accept: 'application/json',
        'X-Capsulad-CSRF': session.csrf
      },
      body: JSON.stringify({ url }),
      signal
    })
  } catch (err) {
    if (err?.name === 'AbortError') throw new Error(t('errors.installCancelled'))
    throw new Error(t('errors.connectionLostInstall'))
  }
  let body = {}
  try {
    body = await resp.json()
  } catch (_) {
    body = {}
  }
  if (!resp.ok || !body.app_id) {
    throw new Error(body.error || t('errors.installFailed', { status: resp.status }))
  }
  return installResult(body)
}

/**
 * The applications filesystem capacity for the install dialog. `available`
 * false means the platform cannot stat it; the byte fields are then zero.
 */
export async function appStorage({ signal } = {}) {
  const resp = await fetch('/api/apps/storage', {
    credentials: 'same-origin',
    headers: { Accept: 'application/json' },
    signal
  })
  let body = {}
  try {
    body = await resp.json()
  } catch (_) {
    body = {}
  }
  if (!resp.ok) throw new Error(body.error || t('errors.storageUnavailable', { status: resp.status }))
  return {
    available: Boolean(body.available),
    totalBytes: Number(body.total_bytes) || 0,
    freeBytes: Number(body.free_bytes) || 0,
    usedBytes: Number(body.used_bytes) || 0
  }
}

/** One installed package's size and the free space its removal would release. */
export async function appPackageStorage(appId, { signal } = {}) {
  const resp = await fetch(`/api/apps/${encodeURIComponent(appId)}/storage`, {
    credentials: 'same-origin',
    headers: { Accept: 'application/json' },
    signal
  })
  let body = {}
  try {
    body = await resp.json()
  } catch (_) {
    body = {}
  }
  if (!resp.ok) throw new Error(body.error || t('errors.storageUnavailable', { status: resp.status }))
  return {
    sizeBytes: Number(body.size_bytes) || 0,
    freeBytes: Number(body.free_bytes) || 0,
    totalBytes: Number(body.total_bytes) || 0,
    available: Boolean(body.available)
  }
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

/**
 * Read one application's optional user guide. The package root README.md is
 * injected as inert Markdown; built-in apps use their embedded guide. A 404
 * means the app has no guide and is shown as such, not as a broken page.
 */
export async function readGuide(appId, { signal } = {}) {
  const resp = await fetch(`/api/apps/${encodeURIComponent(appId)}/guide`, {
    credentials: 'same-origin',
    headers: { Accept: 'application/json' },
    signal
  })
  let body = {}
  try {
    body = await resp.json()
  } catch (_) {
    body = {}
  }
  if (!resp.ok) throw new Error(body.error || t('errors.guideUnavailable', { status: resp.status }))
  const markdown = typeof body.markdown === 'string' ? body.markdown : ''
  if (!markdown.trim()) throw new Error(t('errors.guideEmpty'))
  return {
    appId: String(body.app_id || appId),
    title: String(body.title || ''),
    source: String(body.source || ''),
    markdown
  }
}
