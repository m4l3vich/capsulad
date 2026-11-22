<script setup>
import { computed, nextTick, onUnmounted, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
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
const { t } = useI18n()

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
  notify.warning(t('access.refreshFailed', { error: value }))
})

// --- password ---------------------------------------------------------------

const password = reactive({ current: '', next: '', confirm: '' })
const passwordTouched = reactive({ current: false, next: false, confirm: false })
const passwordAttempted = ref(false)
const passwordBusy = ref(false)

const passwordIssues = computed(() => ({
  current: password.current ? '' : t('access.currentRequired'),
  next: password.next.length >= 8 ? '' : t('access.minLength'),
  confirm: password.confirm === password.next ? '' : t('access.mismatch')
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
  if (!keyNameDraft.value) return t('access.keyNameRequired')
  if (!KEY_NAME.test(keyNameDraft.value)) return t('access.keyNameInvalid')
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
    notify.success(t('access.keyCreated'))
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
    notify.success(t('access.keyCopied'))
  } catch (_) {
    notify.warning(t('access.clipboardFailed'))
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
    notify.success(t('access.keyRevoked', { name: key.name }))
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
  return days < 0
    ? t('access.expiryPast', { days: Math.abs(days) })
    : t('access.expiryFuture', { days })
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
        <h1 class="text-2xl font-semibold tracking-tight">{{ $t('access.title') }}</h1>
        <p class="mt-1 max-w-3xl text-sm text-base-content/70">
          {{ $t('access.intro') }}
        </p>
      </div>
    </header>

    <div v-if="!data && loadError" role="alert" class="alert alert-error">
      <span><strong class="font-semibold">{{ $t('access.stateUnavailable') }}</strong> {{ loadError }}</span>
      <button type="button" class="btn btn-sm btn-outline" @click="refresh">{{ $t('common.retry') }}</button>
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
              <h2 class="card-title text-base">{{ $t('access.passwordTitle') }}</h2>
              <p class="mt-1 text-sm text-base-content/70">
                {{ $t('access.passwordIntro') }}
              </p>
            </div>

            <form class="space-y-3" novalidate :aria-busy="passwordBusy" @submit.prevent="submitPassword">
              <label class="block">
                <span class="mb-1 block text-sm font-medium">{{ $t('access.currentPassword') }}</span>
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
                  <span class="mb-1 block text-sm font-medium">{{ $t('access.newPassword') }}</span>
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
                  <span class="mb-1 block text-sm font-medium">{{ $t('access.confirmPassword') }}</span>
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
                  {{ $t('access.changePassword') }}
                </button>
              </div>
              <details class="text-xs text-base-content/60">
                <summary class="cursor-pointer">{{ $t('access.recovery') }}</summary>
                <div>
                  {{ $t('access.recoveryRun') }}
                  <code class="mt-1 inline-block break-all font-mono border rounded px-1 bg-base-200 select-all">capsulad-cli auth set-password</code>
                  {{ $t('access.recoveryShell') }}
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
                <h2 class="card-title text-base">{{ $t('access.certificateTitle') }}</h2>
                <div class="flex flex-wrap gap-2">
                  <a href="/api/cert.pem" download="capsulad-server.crt" class="btn btn-sm btn-outline">{{ $t('access.downloadCertificate') }}</a>
                  <a href="/api/ca.pem" download="capsulad-ca.crt" class="btn btn-sm btn-outline">{{ $t('access.downloadCA') }}</a>
                </div>
              </div>

              <p class="text-sm text-base-content/70">
                {{ $t('access.certIntro') }}
              </p>

              <p class="text-xs text-base-content/60">
                {{ $t('access.caIntro') }}
              </p>

              <div class="text-sm">
                <p class="text-xs font-semibold uppercase tracking-wide text-base-content/60">{{ $t('access.validUntil') }}</p>
                <p class="mt-1">
                  <template v-if="expiryMs">{{ dateTime(expiryMs) }}</template>
                  <span v-else>{{ $t('common.unavailable') }}</span>
                  <span v-if="expiryLabel" class="ml-1" :class="expiryDays < 0 ? 'text-error' : 'text-base-content/60'">{{ expiryLabel }}</span>
                </p>
              </div>

              <details class="rounded border border-base-300 p-3">
                <summary class="cursor-pointer text-sm font-medium">{{ $t('access.certDetails') }}</summary>
                <dl class="mt-3 space-y-3 text-sm">
                  <div>
                    <dt class="text-xs font-semibold uppercase tracking-wide text-base-content/60">{{ $t('access.sha256') }}</dt>
                    <dd class="mt-1 break-all font-mono text-xs">{{ fingerprint || $t('common.unavailable') }}</dd>
                  </div>
                  <div>
                    <dt class="text-xs font-semibold uppercase tracking-wide text-base-content/60">{{ $t('access.certNames') }}</dt>
                    <dd class="mt-1 break-words font-mono text-xs">{{ certNames.length ? certNames.join(' ') : $t('access.noneReported') }}</dd>
                  </div>
                  <div>
                    <dt class="text-xs font-semibold uppercase tracking-wide text-base-content/60">{{ $t('access.listeners') }}</dt>
                    <dd class="mt-1 break-words font-mono text-xs">{{ listeners.length ? listeners.join(' ') : $t('access.noneReported') }}</dd>
                  </div>
                </dl>
                <p class="mt-3 text-xs text-base-content/60">
                  {{ $t('access.renewHint') }}
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
              <h2 class="card-title text-base">{{ $t('access.keysTitle') }}</h2>
              <p class="mt-1 max-w-3xl text-sm text-base-content/70">
                {{ $t('access.keysIntro1') }}
                <code class="rounded bg-base-200 px-1 py-0.5 font-mono text-xs">capsulad-cli auth key create NAME --scope assistant</code>.
                {{ $t('access.keysIntro2') }}
                <a
                  class="link link-primary"
                  href="https://github.com/m4l3vich/capsulad/blob/master/docs/MCP.md"
                  target="_blank"
                  rel="noreferrer"
                >{{ $t('access.apiDocs') }}</a>.
              </p>
            </div>
          </div>

          <form class="flex flex-col gap-2 sm:flex-row sm:items-start" novalidate @submit.prevent="createKey">
            <label class="block w-full sm:max-w-sm">
              <span class="mb-1 block text-sm font-medium">{{ $t('access.keyName') }}</span>
              <input
                v-model="keyName"
                type="text"
                class="input input-bordered w-full"
                :class="{ 'input-error': (keyNameAttempted || keyNameDraft) && keyNameIssue }"
                :placeholder="$t('access.keyNamePlaceholder')"
                maxlength="64"
                autocomplete="off"
                spellcheck="false"
                :aria-invalid="(keyNameAttempted || keyNameDraft) && keyNameIssue ? 'true' : undefined"
                aria-describedby="key-name-hint"
                :disabled="keyBusy"
                @blur="keyNameAttempted = true"
              />
              <p id="key-name-hint" class="mt-1 text-xs" :class="(keyNameAttempted || keyNameDraft) && keyNameIssue ? 'text-error' : 'text-base-content/60'">
                {{ (keyNameAttempted || keyNameDraft) && keyNameIssue ? keyNameIssue : $t('access.keyNameHint') }}
              </p>
            </label>
            <button
              type="submit"
              class="btn btn-primary btn-sm sm:mt-6"
              :disabled="keyBusy || !keyNameValid"
            >
              <span v-if="keyBusy" class="loading loading-spinner loading-xs" aria-hidden="true"></span>
              {{ $t('access.createKey') }}
            </button>
          </form>

          <div v-if="newKey" role="status" class="alert alert-warning items-start">
            <div class="w-full space-y-2">
              <p class="text-sm">
                {{ $t('access.copyNow', { name: newKey.name }) }}
              </p>
              <code class="block break-all rounded border border-warning/40 bg-base-100 p-2 font-mono text-xs">{{ newKey.value }}</code>
              <div class="flex flex-wrap gap-2">
                <button type="button" class="btn btn-xs btn-outline" @click="copySecret">{{ secretCopied ? $t('access.copied') : $t('access.copyKey') }}</button>
                <button type="button" class="btn btn-xs btn-ghost" @click="dismissSecret">{{ $t('access.clearIt') }}</button>
              </div>
              <p class="text-xs">{{ $t('access.clearHint') }}</p>
            </div>
          </div>

          <div v-if="keys.length" class="overflow-x-auto">
            <table class="table table-sm">
              <thead>
                <tr class="text-xs uppercase tracking-wide text-base-content/60">
                  <th scope="col">{{ $t('common.name') }}</th>
                  <th scope="col">{{ $t('access.startsWith') }}</th>
                  <th scope="col">{{ $t('common.scope') }}</th>
                  <th scope="col">{{ $t('common.added') }}</th>
                  <th scope="col">{{ $t('common.status') }}</th>
                  <th scope="col"><span class="sr-only">{{ $t('common.actions') }}</span></th>
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
                      {{ key.revoked ? $t('access.revoked') : hasValidKeyScopes(key.scopes) ? $t('access.active') : $t('access.inactive') }}
                    </span>
                  </td>
                  <td class="text-right">
                    <button
                      v-if="!key.revoked"
                      type="button"
                      class="btn btn-xs btn-error btn-outline"
                      @click="askRevoke(key)"
                    >{{ $t('access.revoke') }}</button>
                    <span v-else class="text-xs text-base-content/50">{{ $t('access.noLongerValid') }}</span>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
          <p v-else class="rounded border border-base-300 p-4 text-sm text-base-content/70">
            {{ $t('access.noKeys') }}
          </p>

          <p class="text-xs text-base-content/60">
            {{ $t('access.revokeHint') }}
          </p>
        </div>
      </section>
    </template>

    <dialog ref="revokeDialog" class="modal" aria-labelledby="revoke-key-title" @close="onRevokeClosed" @cancel="onRevokeCancel">
      <div class="modal-box">
        <h3 id="revoke-key-title" class="text-lg font-semibold">{{ $t('access.revokeTitle') }}</h3>
        <p class="mt-2 text-sm text-base-content/70">
          {{ $t('access.revokeBody', { name: revokeTarget?.name }) }}
        </p>
        <div class="modal-action">
          <button type="button" class="btn btn-sm btn-ghost" :disabled="revokeBusy" @click="closeRevoke">{{ $t('common.cancel') }}</button>
          <button type="button" class="btn btn-sm btn-error" :disabled="revokeBusy" @click="confirmRevoke">
            <span v-if="revokeBusy" class="loading loading-spinner loading-xs" aria-hidden="true"></span>
            {{ $t('access.revokeKey') }}
          </button>
        </div>
      </div>
      <form method="dialog" class="modal-backdrop">
        <button :aria-label="$t('access.closeRevokeDialog')" :disabled="revokeBusy">{{ $t('access.closeLower') }}</button>
      </form>
    </dialog>
  </div>
</template>
