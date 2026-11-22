<script setup>
import { ref } from 'vue'
import { navigate } from '../router.js'
import { login, runAuthMutation, session } from '../lib/session.js'
import { notify } from '../lib/toast.js'

defineProps({ expired: { type: Boolean, default: false } })

const password = ref('')
const busy = ref(false)

function nextPath() {
  const next = new URLSearchParams(location.search).get('next') || '/'
  return next.startsWith('/') && !next.startsWith('//') ? next : '/'
}

async function submit() {
  if (busy.value || !password.value) return
  busy.value = true
  try {
    const target = await runAuthMutation(() => login(password.value, nextPath()))
    password.value = ''
    // The shell is already switching on the authoritative session transition;
    // only the requested destination is left for this view to apply.
    navigate(target, { replace: true })
  } catch (err) {
    notify.error(err?.message || 'Sign in failed')
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="flex min-h-screen flex-col items-center justify-center gap-4 bg-base-200 p-4">
    <main class="card w-full max-w-sm border border-base-300 bg-base-100 shadow-sm">
      <div class="card-body gap-4">
        <div class="flex items-center gap-2">
          <p class="text-primary rounded px-1 py-1 font-mono text-sm font-semibold tracking-tight">
            capsulad v{{ session.version || '—' }}
          </p>
        </div>

        <div>
          <h1 class="text-lg font-semibold">Sign in to Web UI</h1>
        </div>

        <div v-if="expired" class="alert alert-warning py-2" role="alert">
          <span class="text-sm">The session ended. Please sign in again.</span>
        </div>

        <form class="flex flex-col gap-3" @submit.prevent="submit">
          <label class="form-control w-full">
            <span class="label-text pb-1 text-sm font-medium">Administrator password</span>
            <input
              v-model="password"
              type="password"
              class="input input-bordered w-full"
              autocomplete="current-password"
              required
              autofocus
              :disabled="busy"
            />
          </label>
          <button type="submit" class="btn btn-primary w-full" :disabled="busy || !password">
            <span v-if="busy" class="loading loading-spinner loading-xs"></span>
            {{ busy ? 'Signing in…' : 'Sign in' }}
          </button>
        </form>

        <p class="text-xs leading-relaxed text-base-content/60">
          The certificate is self-signed; compare its fingerprint with
          <code class="rounded bg-base-200 px-1 py-0.5 font-mono select-all">capsulad-cli cert status</code>.
        </p>
      </div>
    </main>
  </div>
</template>
