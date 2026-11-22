<script setup>
import { computed, onMounted, onUnmounted, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { link, navigate, route, startRouter } from './router.js'
import { authMutationSettled, loadSession, logout, runAuthMutation, session, watchAuthSocket } from './lib/session.js'
import { liveState } from './lib/live.js'
import { socket } from './lib/ws.js'
import { dismissToast, notify } from './lib/toast.js'
import ToastHost from './components/ToastHost.vue'
import ThemeSwitch from './components/ThemeSwitch.vue'
import LanguageSwitch from './components/LanguageSwitch.vue'
import LoginView from './views/LoginView.vue'
import OverviewView from './views/OverviewView.vue'
import SettingsView from './views/SettingsView.vue'
import AssetsView from './views/AssetsView.vue'
import AccessView from './views/AccessView.vue'
import DiagnosticsView from './views/DiagnosticsView.vue'

const { t } = useI18n()

let offExpired = null
let recovering = null

const nav = computed(() => [
  { path: '/', label: t('nav.overview') },
  { path: '/settings', label: t('nav.settings') },
  { path: '/files', label: t('nav.files') },
  { path: '/access', label: t('nav.access') },
  { path: '/diagnostics', label: t('nav.diagnostics') }
])

const views = {
  '/': OverviewView,
  '/settings': SettingsView,
  '/files': AssetsView,
  '/access': AccessView,
  '/diagnostics': DiagnosticsView
}

const view = computed(() => views[route.path] || null)

const connection = computed(() => ({
  open: { label: t('connection.connected'), dot: 'bg-success' },
  connecting: { label: t('connection.connecting'), dot: 'bg-warning' },
  reconnecting: { label: t('connection.reconnecting'), dot: 'bg-warning' },
  expired: { label: t('connection.sessionEnded'), dot: 'bg-error' },
  closed: { label: t('connection.offline'), dot: 'bg-error' }
}[liveState.value] || { label: t('connection.offline'), dot: 'bg-error' }))

// Connection feedback lives in the toast stack: one slot, replaced in place as
// the socket moves between states, so it never stacks on repeated retries and
// never duplicates the header dot. Only meaningful while signed in; the login
// screen has no live connection of its own.
const CONNECTION_TOAST_ID = 'connection-state'
const connectionToasts = {
  connecting: () => notify.info(t('connection.connectingToast'), { id: CONNECTION_TOAST_ID }),
  reconnecting: () => notify.warning(
    t('connection.lostToast'),
    { id: CONNECTION_TOAST_ID }
  ),
  // The page cannot act on a closed socket until it reopens, so this one stays.
  closed: () => notify.error(
    t('connection.closedToast'),
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
    {{ $t('common.skipToContent') }}
  </a>

  <div v-if="!session.checked" class="flex min-h-screen items-center justify-center p-4">
    <div class="card w-full max-w-sm border border-base-300 bg-base-100 shadow-sm">
      <div class="card-body gap-4" aria-busy="true" aria-live="polite">
        <div class="flex items-center gap-2 font-semibold tracking-tight">
          capsulad
        </div>
        <div class="skeleton h-3 w-2/3"></div>
        <div class="skeleton h-3 w-1/2"></div>
        <span class="text-xs text-base-content/60">{{ $t('common.checkingSession') }}</span>
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
            :title="$t('nav.capsuladMainPage')"
          >capsulad v{{ session.version || '—' }}</a>

          <nav class="order-last flex w-full items-center gap-1 overflow-x-auto pb-0.5 sm:order-none sm:w-auto sm:pb-0" :aria-label="$t('nav.main')">
            <a
              v-for="item in nav"
              :key="item.path"
              v-bind="link(item.path)"
              class="btn btn-sm btn-ghost shrink-0"
              :class="{ 'btn-active bg-base-200 text-primary': isActive(item.path) }"
              :aria-current="isActive(item.path) ? 'page' : undefined"
            >{{ item.label }}</a>
          </nav>

          <div class="ml-auto flex items-center gap-2 sm:gap-3">
            <span class="inline-flex items-center gap-1.5 text-xs text-base-content/70" role="status" :aria-label="connection.label">
              <span class="inline-block h-2 w-2 rounded-full" :class="connection.dot"></span>
              <span class="hidden sm:inline">{{ connection.label }}</span>
            </span>
            <ThemeSwitch />
            <LanguageSwitch />
            <button type="button" class="btn btn-sm btn-outline" @click="signOut">{{ $t('nav.signOut') }}</button>
          </div>
        </div>
      </div>
    </header>

    <main id="main" class="mx-auto w-full max-w-6xl flex-1 px-3 py-4 sm:px-5">
      <component :is="view" v-if="view" />

      <section v-else class="card border border-base-300 bg-base-100">
        <div class="card-body items-start gap-3">
          <p class="text-xs font-semibold uppercase tracking-wide text-base-content/60">{{ $t('notFound.label') }}</p>
          <h1 class="text-xl font-semibold">{{ $t('notFound.title') }}</h1>
          <p class="text-sm text-base-content/70">{{ $t('notFound.body') }}</p>
          <a v-bind="link('/')" class="btn btn-primary btn-sm">{{ $t('notFound.go') }}</a>
        </div>
      </section>
    </main>
  </div>

  <ToastHost />
</template>
