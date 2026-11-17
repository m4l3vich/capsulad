<script setup>
import { computed, onMounted, onUnmounted, watch } from 'vue'
import { link, navigate, route, startRouter } from './router.js'
import { authMutationSettled, loadSession, logout, runAuthMutation, session, watchAuthSocket } from './lib/session.js'
import { liveState } from './lib/live.js'
import { socket } from './lib/ws.js'
import { dismissToast, notify } from './lib/toast.js'
import ToastHost from './components/ToastHost.vue'
import LoginView from './views/LoginView.vue'
import OverviewView from './views/OverviewView.vue'
import AppsView from './views/AppsView.vue'
import SettingsView from './views/SettingsView.vue'
import AssetsView from './views/AssetsView.vue'
import AccessView from './views/AccessView.vue'
import DiagnosticsView from './views/DiagnosticsView.vue'

let offExpired = null
let recovering = null

const nav = [
  { path: '/', label: 'Overview' },
  { path: '/apps', label: 'Apps' },
  { path: '/settings', label: 'Settings' },
  { path: '/assets', label: 'Assets' },
  { path: '/access', label: 'Access' },
  { path: '/diagnostics', label: 'Diagnostics' }
]

const views = {
  '/': OverviewView,
  '/apps': AppsView,
  '/settings': SettingsView,
  '/assets': AssetsView,
  '/access': AccessView,
  '/diagnostics': DiagnosticsView
}

const view = computed(() => views[route.path] || null)

const connection = computed(() => ({
  open: { label: 'Connected', dot: 'bg-success' },
  connecting: { label: 'Connecting', dot: 'bg-warning' },
  reconnecting: { label: 'Reconnecting', dot: 'bg-warning' },
  expired: { label: 'Session ended', dot: 'bg-error' },
  closed: { label: 'Offline', dot: 'bg-error' }
}[liveState.value] || { label: 'Offline', dot: 'bg-error' }))

// Connection feedback lives in the toast stack: one slot, replaced in place as
// the socket moves between states, so it never stacks on repeated retries and
// never duplicates the header dot. Only meaningful while signed in; the login
// screen has no live connection of its own.
const CONNECTION_TOAST_ID = 'connection-state'
const connectionToasts = {
  connecting: () => notify.info('Connecting to the speaker…', { id: CONNECTION_TOAST_ID }),
  reconnecting: () => notify.warning(
    'Connection lost. Reconnecting; the page keeps its last snapshot until the device answers.',
    { id: CONNECTION_TOAST_ID }
  ),
  // The page cannot act on a closed socket until it reopens, so this one stays.
  closed: () => notify.error(
    'The realtime connection is closed. Live controls are unavailable until it reconnects.',
    { id: CONNECTION_TOAST_ID, timeout: 0 }
  )
}

watch(
  [liveState, () => session.authenticated],
  ([state, authenticated]) => {
    if (!authenticated || state === 'open') {
      dismissToast(CONNECTION_TOAST_ID)
      return
    }
    connectionToasts[state]?.()
  },
  { immediate: true }
)

function isActive(path) {
  return route.path === path
}

// A password change closes every live socket, often before its HTTP response
// (carrying this browser's fresh cookie) has been processed. Wait for the auth
// mutation to settle, then re-read the session; only a genuinely absent
// session turns into the login screen.
function recoverSession() {
  if (recovering) return recovering
  session.expired = true
  recovering = (async () => {
    await authMutationSettled()
    await loadSession()
    if (session.authenticated) {
      session.expired = false
      socket.connect()
      return
    }
    socket.close()
  })().finally(() => {
    recovering = null
  })
  return recovering
}

// Resolve the initial route before the first render so a deep link does not
// flash Main.
startRouter()

onMounted(async () => {
  offExpired = socket.on('expired', recoverSession)
  await loadSession()
})

onUnmounted(() => {
  offExpired?.()
  stopAuthSocket?.()
})

// The session store is authoritative: the shell owns the socket on every
// authenticated transition, including the initial already-authenticated load.
// Login must not depend on an emit from a view the shell unmounts first.
const stopAuthSocket = watchAuthSocket(socket)

watch(
  [() => session.authenticated, () => route.path],
  ([authenticated, path]) => {
    if (authenticated && path === '/login') navigate('/', { replace: true })
  }
)

async function signOut() {
  try {
    await runAuthMutation(() => logout())
  } catch (_) {
    // A revoked session behaves the same for the browser.
  }
  socket.close()
  session.authenticated = false
  session.expired = false
  navigate('/login', { replace: true })
}
</script>

<template>
  <a
    class="fixed left-3 top-3 z-50 -translate-y-24 rounded bg-primary px-3 py-2 text-sm text-primary-content transition-transform focus:translate-y-0"
    href="#main"
  >
    Skip to content
  </a>

  <div v-if="!session.checked" class="flex min-h-screen items-center justify-center p-4">
    <div class="card w-full max-w-sm border border-base-300 bg-base-100 shadow-sm">
      <div class="card-body gap-4" aria-busy="true" aria-live="polite">
        <div class="flex items-center gap-2 font-semibold tracking-tight">
          capsulad
        </div>
        <div class="skeleton h-3 w-2/3"></div>
        <div class="skeleton h-3 w-1/2"></div>
        <span class="text-xs text-base-content/60">Checking the session…</span>
      </div>
    </div>
  </div>

  <LoginView v-else-if="!session.authenticated" :expired="session.expired" />

  <div v-else class="flex min-h-screen flex-col">
    <header class="sticky top-0 z-30 border-b border-base-300 bg-base-100/95 backdrop-blur">
      <div class="mx-auto w-full max-w-6xl px-3 sm:px-5">
        <div class="flex flex-wrap items-center gap-x-3 gap-y-1 py-2">
          <a
            v-bind="link('/')"
            class="whitespace-nowrap rounded px-1 py-1 font-mono text-sm font-semibold tracking-tight text-primary"
            title="capsulad main page"
          >capsulad v{{ session.version || '—' }}</a>

          <nav class="order-last flex w-full items-center gap-1 overflow-x-auto pb-0.5 sm:order-none sm:w-auto sm:pb-0" aria-label="Main">
            <a
              v-for="item in nav"
              :key="item.path"
              v-bind="link(item.path)"
              class="btn btn-sm btn-ghost btn-primary shrink-0"
              :class="{ 'btn-soft bg-base-200': isActive(item.path) }"
              :aria-current="isActive(item.path) ? 'page' : undefined"
            >{{ item.label }}</a>
          </nav>

          <div class="ml-auto flex items-center gap-3">
            <span class="inline-flex items-center gap-1.5 text-xs text-base-content/70" role="status" :aria-label="connection.label">
              <span class="inline-block h-2 w-2 rounded-full" :class="connection.dot"></span>
              <span class="hidden sm:inline">{{ connection.label }}</span>
            </span>
            <button type="button" class="btn btn-sm btn-outline" @click="signOut">Sign out</button>
          </div>
        </div>
      </div>
    </header>

    <main id="main" class="mx-auto w-full max-w-6xl flex-1 px-3 py-4 sm:px-5">
      <component :is="view" v-if="view" />

      <section v-else class="card border border-base-300 bg-base-100">
        <div class="card-body items-start gap-3">
          <p class="text-xs font-semibold uppercase tracking-wide text-base-content/60">Not found</p>
          <h1 class="text-xl font-semibold">This page does not exist</h1>
          <p class="text-sm text-base-content/70">The console has Overview, Apps, Settings, Assets, Access and Diagnostics. Use the navigation above or return to Overview.</p>
          <a v-bind="link('/')" class="btn btn-primary btn-sm">Go to Overview</a>
        </div>
      </section>
    </main>
  </div>

  <ToastHost />
</template>
