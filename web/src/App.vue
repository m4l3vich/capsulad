<script setup>
import { computed, onMounted, onUnmounted, watch } from 'vue'
import { link, navigate, route, startRouter } from './router.js'
import { authMutationSettled, loadSession, logout, runAuthMutation, session, watchAuthSocket } from './lib/session.js'
import { liveState } from './lib/live.js'
import { socket } from './lib/ws.js'
import LoginView from './views/LoginView.vue'
import OverviewView from './views/OverviewView.vue'
import SettingsView from './views/SettingsView.vue'
import AssetsView from './views/AssetsView.vue'
import AccessView from './views/AccessView.vue'
import DiagnosticsView from './views/DiagnosticsView.vue'

let offExpired = null
let recovering = null

const nav = [
  { path: '/', label: 'Overview' },
  { path: '/settings', label: 'Settings' },
  { path: '/assets', label: 'Assets' },
  { path: '/access', label: 'Access' },
  { path: '/diagnostics', label: 'Diagnostics' }
]

const views = {
  '/': OverviewView,
  '/settings': SettingsView,
  '/assets': AssetsView,
  '/access': AccessView,
  '/diagnostics': DiagnosticsView
}

const view = computed(() => views[route.path] || null)

const connection = computed(() => ({
  open: { label: 'Live', dot: 'bg-success' },
  connecting: { label: 'Connecting', dot: 'bg-warning' },
  reconnecting: { label: 'Reconnecting', dot: 'bg-warning' },
  expired: { label: 'Session ended', dot: 'bg-error' },
  closed: { label: 'Offline', dot: 'bg-error' }
}[liveState.value] || { label: 'Offline', dot: 'bg-error' }))

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
          <span class="grid h-7 w-7 place-items-center rounded-lg bg-primary text-[10px] font-bold text-primary-content">C</span>
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
            class="whitespace-nowrap rounded px-1 py-1 font-mono text-sm font-semibold tracking-tight"
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

          <div class="ml-auto flex items-center gap-2">
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
      <div v-if="liveState === 'connecting'" class="alert alert-info mb-4 py-2" role="status">
        <span class="loading loading-spinner loading-xs"></span>
        <span class="text-sm">Connecting to the speaker…</span>
      </div>
      <div v-else-if="liveState === 'reconnecting'" class="alert alert-warning mb-4 py-2" role="status">
        <span class="text-sm">Connection lost. Reconnecting; the page keeps its last snapshot until the device answers.</span>
        <button type="button" class="btn btn-sm btn-ghost" @click="socket.connect()">Retry now</button>
      </div>
      <div v-else-if="liveState === 'closed'" class="alert alert-error mb-4 py-2" role="alert">
        <span class="text-sm">The realtime connection is closed. Live controls are unavailable until it reconnects.</span>
        <button type="button" class="btn btn-sm btn-ghost" @click="socket.connect()">Reconnect</button>
      </div>

      <component :is="view" v-if="view" />

      <section v-else class="card border border-base-300 bg-base-100">
        <div class="card-body items-start gap-3">
          <p class="text-xs font-semibold uppercase tracking-wide text-base-content/60">Not found</p>
          <h1 class="text-xl font-semibold">This page does not exist</h1>
          <p class="text-sm text-base-content/70">The console has Main, Settings, Assets, Access and Diagnostics. Use the navigation above or return to Main.</p>
          <a v-bind="link('/')" class="btn btn-primary btn-sm">Go to Main</a>
        </div>
      </section>
    </main>
  </div>
</template>
