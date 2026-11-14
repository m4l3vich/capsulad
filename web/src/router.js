import { reactive } from 'vue'

// Four appliance destinations and a login screen. Application details are a
// modal on Main rather than a separate route, so the router stays a lookup of
// exact paths with no parameter parsing.

export const route = reactive({ path: '/' })

function normalized(path) {
  const clean = path.replace(/\/+$/, '') || '/'
  return clean
}

function apply() {
  route.path = normalized(location.pathname)
}

export function startRouter() {
  apply()
  window.addEventListener('popstate', apply)
}

export function navigate(path, { replace = false } = {}) {
  const next = normalized(path)
  if (next === location.pathname) return
  if (replace) history.replaceState({}, '', next)
  else history.pushState({}, '', next)
  apply()
  window.scrollTo({ top: 0, behavior: 'instant' })
}

// Anchor helper keeps middle-click and new-tab behaviour.
export function link(path) {
  return {
    href: path,
    onClick(event) {
      if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey || event.button !== 0) return
      event.preventDefault()
      navigate(path)
    }
  }
}
