import { session } from './session.js'

async function requestBoot(appId, options = {}) {
  const response = await fetch(`/api/apps/${encodeURIComponent(appId)}/boot`, {
    credentials: 'same-origin',
    ...options,
    headers: { Accept: 'application/json', ...options.headers }
  })
  const body = await response.json()
  if (!response.ok) {
    const error = new Error(body.error || `Request failed (${response.status})`)
    error.status = response.status
    throw error
  }
  return body
}

// Returns { enabled, editable, revision }. Fetch again after a 409 conflict.
export function getAppBoot(appId) {
  return requestBoot(appId)
}

// Saves for the next daemon start; does not start or stop the application.
export function setAppBoot(appId, enabled, revision) {
  return requestBoot(appId, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json', 'X-Capsulad-CSRF': session.csrf },
    body: JSON.stringify({ enabled, revision })
  })
}
