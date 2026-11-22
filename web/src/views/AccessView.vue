<script setup>
import { computed, nextTick, onUnmounted, reactive, ref, watch } from 'vue'
import { topicNames, socket } from '../lib/ws.js'
import { useTopic } from '../lib/live.js'
import { changePassword, runAuthMutation } from '../lib/session.js'
import { dateTime, hasValidKeyScopes, keyScopes } from '../lib/format.js'
import { notify } from '../lib/toast.js'

// Access is the only page that changes credentials. Password changes revoke
// every other browser session and close every live socket, including this
// one; the daemon reissues this browser's cookie in the same response. The
// password request runs inside session.js's auth-mutation lifecycle, so the
// shell's socket-expiry handler waits for the reissued cookie before probing
// /api/session and owns the single reconnect.

const { data, error: loadError, loading, refresh } = useTopic(topicNames.access)

// A refresh failure keeps the last received state on screen; announce it once
// per distinct failure instead of pinning a banner above the page.
let lastLoadError = ''
watch(loadError, (value) => {
  if (!value) {
    lastLoadError = ''
    return
  }
  if (!data.value || value === lastLoadError) return
  lastLoadError = value
  notify.warning(`Refresh failed: ${value} Showing the last received access state.`)
})

// --- password ---------------------------------------------------------------

const password = reactive({ current: '', next: '', confirm: '' })
const passwordTouched = reactive({ current: false, next: false, confirm: false })
const passwordAttempted = ref(false)
const passwordBusy = ref(false)

const passwordIssues = computed(() => ({
  current: password.current ? '' : 'Enter the current administrator password.',
  next: password.next.length >= 8 ? '' : 'Use at least 8 characters.',
  confirm: password.confirm === password.next ? '' : 'The two new passwords do not match.'
}))
const passwordValid = computed(() => !Object.values(passwordIssues.value).some(Boolean))

function showPasswordIssue(field) {
  return (passwordAttempted.value || passwordTouched[field]) ? passwordIssues.value[field] : ''
}

function resetPasswordDrafts() {
  password.current = ''
  password.next = ''
  password.confirm = ''
  for (const field of Object.keys(passwordTouched)) passwordTouched[field] = false
  passwordAttempted.value = false
}

async function submitPassword() {
  if (!passwordValid.value) {
    passwordAttempted.value = true
    return
  }
  passwordBusy.value = true
  try {
    const message = await runAuthMutation(() =>
      changePassword(password.current, password.next, password.confirm))
    notify.success(message)
    resetPasswordDrafts()
    // The shell's expired handler awaits the same in-flight mutation, then
    // reloads the session with the reissued cookie and reconnects the socket.
  } catch (err) {
    notify.error(err.message)
  } finally {
    passwordBusy.value = false
  }
}

// --- static API keys --------------------------------------------------------

const KEY_NAME = /^[A-Za-z0-9_.-]{1,64}$/

const keyName = ref('')
const keyBusy = ref(false)
const keyNameAttempted = ref(false)
const newKey = ref(null)
const secretCopied = ref(false)

const keyNameDraft = computed(() => keyName.value.trim())
const keyNameIssue = computed(() => {
  if (!keyNameDraft.value) return 'Enter a name for the key.'
  if (!KEY_NAME.test(keyNameDraft.value)) return 'Use 1–64 ASCII letters, digits, dots, underscores or hyphens.'
  return ''
})
const keyNameValid = computed(() => !keyNameIssue.value)

async function createKey() {
  if (!keyNameValid.value) {
    keyNameAttempted.value = true
    return
  }
  keyBusy.value = true
  try {
    const result = await socket.request({ createApiKey: { name: keyNameDraft.value } })
    newKey.value = result.apiKey || null
    secretCopied.value = false
    keyName.value = ''
    keyNameAttempted.value = false
    notify.success('API key created; copy it now, it is not shown again')
    await refresh()
  } catch (err) {
    notify.error(err.message)
  } finally {
    keyBusy.value = false
  }
}

async function copySecret() {
  if (!newKey.value?.value) return
  try {
    await navigator.clipboard.writeText(newKey.value.value)
    secretCopied.value = true
    notify.success('API key copied to the clipboard')
  } catch (_) {
    notify.warning('Clipboard unavailable; select the key text and copy it manually')
  }
}

function dismissSecret() {
  newKey.value = null
  secretCopied.value = false
}

// --- revoke confirmation ----------------------------------------------------

const revokeDialog = ref(null)
const revokeTarget = ref(null)
const revokeBusy = ref(false)

const keys = computed(() => data.value?.keys || [])
const activeKeyCount = computed(() => keys.value.filter((key) => !key.revoked && hasValidKeyScopes(key.scopes)).length)

function askRevoke(key) {
  revokeTarget.value = key
  nextTick(() => {
    if (!revokeDialog.value?.open) revokeDialog.value?.showModal()
  })
}

function closeRevoke() {
  revokeDialog.value?.close()
}

async function confirmRevoke() {
  const key = revokeTarget.value
  if (!key || revokeBusy.value) return
  revokeBusy.value = true
  try {
    await socket.request({ revokeApiKey: { name: key.name } })
    if (newKey.value?.name === key.name) dismissSecret()
    closeRevoke()
    notify.success(`API key “${key.name}” revoked`)
    await refresh()
  } catch (err) {
    notify.error(err.message)
  } finally {
    revokeBusy.value = false
  }
}

function onRevokeClosed() {
  revokeTarget.value = null
}

function onRevokeCancel(event) {
  // Keep the dialog open while the revoke request is in flight so its result
  // (or error) is visible.
  if (revokeBusy.value) event.preventDefault()
}

// --- certificate ------------------------------------------------------------

const status = computed(() => data.value?.status || null)
const fingerprint = computed(() => status.value?.serverFingerprint || '')
const expiryMs = computed(() => Number(status.value?.serverExpiryUnixMs) || 0)
const expiryDays = computed(() => {
  if (!expiryMs.value) return null
  return Math.ceil((expiryMs.value - Date.now()) / 86400000)
})
const expiryLabel = computed(() => {
  const days = expiryDays.value
  if (days === null) return ''
  return days < 0 ? `expired ${Math.abs(days)} d ago` : `in ${days} d`
})
const certNames = computed(() => [
  ...(status.value?.serverDnsNames || []),
  ...(status.value?.serverIpAddresses || [])
])
const listeners = computed(() => status.value?.listenAddresses || [])

onUnmounted(() => {
  // The plaintext key never outlives the page that revealed it.
  newKey.value = null
})
</script>

<template>
  <div class="mx-auto w-full max-w-6xl space-y-6">
    <header class="flex flex-wrap items-end justify-between gap-3">
      <div>
        <h1 class="text-2xl font-semibold tracking-tight">Access</h1>
        <p class="mt-1 max-w-3xl text-sm text-base-content/70">
          Manage the administrator password and the API keys that scripts and MCP clients use.
        </p>
      </div>
    </header>

    <div v-if="!data && loadError" role="alert" class="alert alert-error">
      <span><strong class="font-semibold">Access state unavailable.</strong> {{ loadError }}</span>
      <button type="button" class="btn btn-sm btn-outline" @click="refresh">Try again</button>
    </div>

    <div v-if="!data && !loadError" class="grid gap-4 md:grid-cols-2" aria-hidden="true">
      <div class="card border border-base-300 bg-base-100"><div class="card-body gap-3 p-5">
        <div class="skeleton h-4 w-1/3"></div><div class="skeleton h-9 w-full"></div><div class="skeleton h-9 w-full"></div>
      </div></div>
      <div class="card border border-base-300 bg-base-100"><div class="card-body gap-3 p-5">
        <div class="skeleton h-4 w-1/2"></div><div class="skeleton h-24 w-full"></div>
      </div></div>
    </div>

    <template v-if="data">
      <div class="grid items-start gap-6 lg:grid-cols-[minmax(0,1.15fr)_minmax(0,0.85fr)]">
        <!-- password -->
        <section class="card border border-base-300 bg-base-100">
          <div class="card-body gap-4 p-5">
            <div>
              <h2 class="card-title text-base">Administrator password</h2>
              <p class="mt-1 text-sm text-base-content/70">
                Changing it signs out every other browser and briefly reconnects this page.
              </p>
            </div>

            <form class="space-y-3" novalidate :aria-busy="passwordBusy" @submit.prevent="submitPassword">
              <label class="block">
                <span class="mb-1 block text-sm font-medium">Current password</span>
                <input
                  v-model="password.current"
                  type="password"
                  class="input input-bordered w-full"
                  :class="{ 'input-error': showPasswordIssue('current') }"
                  autocomplete="current-password"
                  required
                  :aria-invalid="showPasswordIssue('current') ? 'true' : undefined"
                  aria-describedby="current-password-error"
                  :disabled="passwordBusy"
                  @blur="passwordTouched.current = true"
                />
                <p v-if="showPasswordIssue('current')" id="current-password-error" class="mt-1 text-xs text-error">
                  {{ passwordIssues.current }}
                </p>
              </label>

              <div class="grid gap-3 sm:grid-cols-2">
                <label class="block">
                  <span class="mb-1 block text-sm font-medium">New password</span>
                  <input
                    v-model="password.next"
                    type="password"
                    class="input input-bordered w-full"
                    :class="{ 'input-error': showPasswordIssue('next') }"
                    autocomplete="new-password"
                    minlength="8"
                    required
                    :aria-invalid="showPasswordIssue('next') ? 'true' : undefined"
                    aria-describedby="next-password-error"
                    :disabled="passwordBusy"
                    @blur="passwordTouched.next = true"
                  />
                  <p v-if="showPasswordIssue('next')" id="next-password-error" class="mt-1 text-xs text-error">
                    {{ passwordIssues.next }}
                  </p>
                </label>
                <label class="block">
                  <span class="mb-1 block text-sm font-medium">Confirm new password</span>
                  <input
                    v-model="password.confirm"
                    type="password"
                    class="input input-bordered w-full"
                    :class="{ 'input-error': showPasswordIssue('confirm') }"
                    autocomplete="new-password"
                    minlength="8"
                    required
                    :aria-invalid="showPasswordIssue('confirm') ? 'true' : undefined"
                    aria-describedby="confirm-password-error"
                    :disabled="passwordBusy"
                    @blur="passwordTouched.confirm = true"
                  />
                  <p v-if="showPasswordIssue('confirm')" id="confirm-password-error" class="mt-1 text-xs text-error">
                    {{ passwordIssues.confirm }}
                  </p>
                </label>
              </div>

              <div class="flex flex-wrap items-center gap-3 pt-1">
                <button type="submit" class="btn btn-primary btn-sm" :disabled="passwordBusy">
                  <span v-if="passwordBusy" class="loading loading-spinner loading-xs" aria-hidden="true"></span>
                  Change password
                </button>
              </div>
              <details class="text-xs text-base-content/60">
                <summary class="cursor-pointer">Password recovery</summary>
                <div>
                  Run
                  <code class="mt-1 inline-block break-all font-mono border rounded px-1 bg-base-200 select-all">capsulad-cli auth set-password</code>
                  in speaker's shell
                </div>
              </details>
            </form>
          </div>
        </section>

        <!-- certificate + status -->
        <div class="space-y-6">
          <section class="card border border-base-300 bg-base-100">
            <div class="card-body gap-4 p-5">
              <div class="flex flex-wrap items-start justify-between gap-2">
                <h2 class="card-title text-base">Secure certificate</h2>
                <a href="/api/cert.pem" download="capsulad-server.crt" class="btn btn-sm btn-outline">Download certificate</a>
              </div>

              <p class="text-sm text-base-content/70">
                This speaker uses its own self-signed certificate. Download it and trust it on your devices to
                remove the browser warning.
              </p>

              <div class="text-sm">
                <p class="text-xs font-semibold uppercase tracking-wide text-base-content/60">Valid until</p>
                <p class="mt-1">
                  <template v-if="expiryMs">{{ dateTime(expiryMs) }}</template>
                  <span v-else>Unavailable</span>
                  <span v-if="expiryLabel" class="ml-1" :class="expiryDays < 0 ? 'text-error' : 'text-base-content/60'">{{ expiryLabel }}</span>
                </p>
              </div>

              <details class="rounded border border-base-300 p-3">
                <summary class="cursor-pointer text-sm font-medium">Certificate details</summary>
                <dl class="mt-3 space-y-3 text-sm">
                  <div>
                    <dt class="text-xs font-semibold uppercase tracking-wide text-base-content/60">SHA-256 fingerprint</dt>
                    <dd class="mt-1 break-all font-mono text-xs">{{ fingerprint || 'Unavailable' }}</dd>
                  </div>
                  <div>
                    <dt class="text-xs font-semibold uppercase tracking-wide text-base-content/60">Certificate names</dt>
                    <dd class="mt-1 break-words font-mono text-xs">{{ certNames.length ? certNames.join(' ') : 'None reported' }}</dd>
                  </div>
                  <div>
                    <dt class="text-xs font-semibold uppercase tracking-wide text-base-content/60">Listeners</dt>
                    <dd class="mt-1 break-words font-mono text-xs">{{ listeners.length ? listeners.join(' ') : 'None reported' }}</dd>
                  </div>
                </dl>
                <p class="mt-3 text-xs text-base-content/60">
                  Renewing the certificate is done on the speaker and needs a restart.
                </p>
              </details>
            </div>
          </section>
        </div>
      </div>

      <!-- static keys -->
      <section class="card border border-base-300 bg-base-100">
        <div class="card-body gap-4 p-5">
          <div class="flex flex-wrap items-start justify-between gap-3">
            <div>
              <h2 class="card-title text-base">Static API keys</h2>
              <p class="mt-1 max-w-3xl text-sm text-base-content/70">
                For scripts, monitoring tools and MCP clients. This page mints status keys, which can only read device
                status and the server certificate. For speaker controls, use
                <code class="rounded bg-base-200 px-1 py-0.5 font-mono text-xs">capsulad-cli auth key create NAME --scope assistant</code>.
                No key can sign in here or change the administrator password. Name a key after the place that will use it.
                Read the <a
                  class="link link-primary"
                  href="https://github.com/m4l3vich/capsulad/blob/master/docs/MCP.md"
                  target="_blank"
                  rel="noreferrer"
                >API documentation</a>.
              </p>
            </div>
          </div>

          <form class="flex flex-col gap-2 sm:flex-row sm:items-start" novalidate @submit.prevent="createKey">
            <label class="block w-full sm:max-w-sm">
              <span class="mb-1 block text-sm font-medium">Key name</span>
              <input
                v-model="keyName"
                type="text"
                class="input input-bordered w-full"
                :class="{ 'input-error': (keyNameAttempted || keyNameDraft) && keyNameIssue }"
                placeholder="home-monitor"
                maxlength="64"
                autocomplete="off"
                spellcheck="false"
                :aria-invalid="(keyNameAttempted || keyNameDraft) && keyNameIssue ? 'true' : undefined"
                aria-describedby="key-name-hint"
                :disabled="keyBusy"
                @blur="keyNameAttempted = true"
              />
              <p id="key-name-hint" class="mt-1 text-xs" :class="(keyNameAttempted || keyNameDraft) && keyNameIssue ? 'text-error' : 'text-base-content/60'">
                {{ (keyNameAttempted || keyNameDraft) && keyNameIssue ? keyNameIssue : 'Use letters, numbers, dots, hyphens or underscores (up to 64).' }}
              </p>
            </label>
            <button
              type="submit"
              class="btn btn-primary btn-sm sm:mt-6"
              :disabled="keyBusy || !keyNameValid"
            >
              <span v-if="keyBusy" class="loading loading-spinner loading-xs" aria-hidden="true"></span>
              Create key
            </button>
          </form>

          <div v-if="newKey" role="status" class="alert alert-warning items-start">
            <div class="w-full space-y-2">
              <p class="text-sm">
                Copy <strong class="font-semibold">{{ newKey.name }}</strong> now — this key is shown only once and
                cannot be looked up again.
              </p>
              <code class="block break-all rounded border border-warning/40 bg-base-100 p-2 font-mono text-xs">{{ newKey.value }}</code>
              <div class="flex flex-wrap gap-2">
                <button type="button" class="btn btn-xs btn-outline" @click="copySecret">{{ secretCopied ? 'Copied' : 'Copy key' }}</button>
                <button type="button" class="btn btn-xs btn-ghost" @click="dismissSecret">Done, clear it</button>
              </div>
              <p class="text-xs">“Done” removes the key from this page. Store it where the script can read it.</p>
            </div>
          </div>

          <div v-if="keys.length" class="overflow-x-auto">
            <table class="table table-sm">
              <thead>
                <tr class="text-xs uppercase tracking-wide text-base-content/60">
                  <th scope="col">Name</th>
                  <th scope="col">Starts with</th>
                  <th scope="col">Scope</th>
                  <th scope="col">Added</th>
                  <th scope="col">Status</th>
                  <th scope="col"><span class="sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="key in keys" :key="key.name">
                  <td class="font-mono text-xs">{{ key.name }}</td>
                  <td class="font-mono text-xs">{{ key.prefix ? key.prefix + '…' : '—' }}</td>
                  <td class="whitespace-nowrap">
                    <span
                      class="badge badge-sm"
                      :class="hasValidKeyScopes(key.scopes) && key.scopes.includes('operator') ? 'badge-warning' : 'badge-ghost'"
                    >{{ keyScopes(key.scopes) }}</span>
                  </td>
                  <td class="whitespace-nowrap text-xs">{{ dateTime(key.createdUnixMs) }}</td>
                  <td>
                    <span class="badge badge-sm" :class="key.revoked || !hasValidKeyScopes(key.scopes) ? 'badge-neutral' : 'badge-success'">
                      {{ key.revoked ? 'revoked' : hasValidKeyScopes(key.scopes) ? 'active' : 'inactive' }}
                    </span>
                  </td>
                  <td class="text-right">
                    <button
                      v-if="!key.revoked"
                      type="button"
                      class="btn btn-xs btn-error btn-outline"
                      @click="askRevoke(key)"
                    >Revoke</button>
                    <span v-else class="text-xs text-base-content/50">no longer valid</span>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
          <p v-else class="rounded border border-base-300 p-4 text-sm text-base-content/70">
            No automation keys yet. Create a status key here only if a script needs to read device status.
          </p>

          <!-- TODO: actually remove keys from the list 7d after revocation -->
          <p class="text-xs text-base-content/60">
            Revoking a key cannot be undone. The entry stays in this list for 7 days so you can see it was revoked.
          </p>
        </div>
      </section>
    </template>

    <dialog ref="revokeDialog" class="modal" aria-labelledby="revoke-key-title" @close="onRevokeClosed" @cancel="onRevokeCancel">
      <div class="modal-box">
        <h3 id="revoke-key-title" class="text-lg font-semibold">Revoke API key</h3>
        <p class="mt-2 text-sm text-base-content/70">
          Revoke <code class="font-mono font-semibold">{{ revokeTarget?.name }}</code>?
          Any script using it will stop working immediately. This cannot be undone.
        </p>
        <div class="modal-action">
          <button type="button" class="btn btn-sm btn-ghost" :disabled="revokeBusy" @click="closeRevoke">Cancel</button>
          <button type="button" class="btn btn-sm btn-error" :disabled="revokeBusy" @click="confirmRevoke">
            <span v-if="revokeBusy" class="loading loading-spinner loading-xs" aria-hidden="true"></span>
            Revoke key
          </button>
        </div>
      </div>
      <form method="dialog" class="modal-backdrop">
        <button aria-label="Close revoke dialog" :disabled="revokeBusy">close</button>
      </form>
    </dialog>
  </div>
</template>
