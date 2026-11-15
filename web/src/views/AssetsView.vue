<script setup>
import { computed, onMounted, ref } from 'vue'
import ModalDialog from '../components/ModalDialog.vue'
import {
  assetPathError,
  defaultTarget,
  deleteAsset,
  formatBytes,
  listAssets,
  MAX_ASSET_BYTES,
  shortHash,
  uploadAsset
} from '../lib/assets.js'

// Assets manages the shared read-only store. Every app sandbox mounts the same
// directory at /assets, so this page is the only writer; uploads reuse the
// session cookie and CSRF token from the shell.

const assets = ref([])
const totalBytes = ref(0)
const loading = ref(false)
const loaded = ref(false)
const loadError = ref('')

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    const { assets: next, totalBytes: total } = await listAssets()
    assets.value = next
    totalBytes.value = total
    loaded.value = true
  } catch (err) {
    loadError.value = err.message
    if (!loaded.value) {
      assets.value = []
      totalBytes.value = 0
    }
  } finally {
    loading.value = false
  }
}

function note(message) {
  notice.value = message
}

onMounted(load)

// --- upload -----------------------------------------------------------------

const pickedFile = ref(null)
const target = ref('')
const targetTouched = ref(false)
const targetAttempted = ref(false)
const dragging = ref(false)
const uploading = ref(false)
const progress = ref(0)
const uploadError = ref('')
const notice = ref('')
const fileInput = ref(null)

const targetIssue = computed(() => {
  if (!pickedFile.value && !targetTouched.value) return ''
  return assetPathError(target.value)
})
const showTargetIssue = computed(() => Boolean(targetIssue.value) && (targetAttempted.value || targetTouched.value))
const canUpload = computed(() => Boolean(pickedFile.value) && !assetPathError(target.value) && !uploading.value)

const pickedLabel = computed(() => {
  if (!pickedFile.value) return 'No file chosen'
  return `${pickedFile.value.name} · ${formatBytes(pickedFile.value.size)}`
})

function chooseFile(file) {
  if (!file) return
  pickedFile.value = file
  uploadError.value = ''
  notice.value = ''
  if (!targetTouched.value) target.value = defaultTarget(file.name)
  if (file.size > MAX_ASSET_BYTES) {
    uploadError.value = `“${file.name}” is ${formatBytes(file.size)}; the store accepts at most ${formatBytes(MAX_ASSET_BYTES)} per file.`
  }
}

function onPick(event) {
  chooseFile(event.target.files?.[0])
  // Allow re-picking the same file after a failed upload.
  if (event.target) event.target.value = ''
}

function onDrop(event) {
  dragging.value = false
  chooseFile(event.dataTransfer?.files?.[0])
}

function startDrag(event) {
  event.preventDefault()
  dragging.value = true
}

function onTargetInput() {
  targetTouched.value = true
  uploadError.value = ''
}

function resetDraft() {
  pickedFile.value = null
  target.value = ''
  targetTouched.value = false
  targetAttempted.value = false
  progress.value = 0
}

async function submitUpload() {
  uploadError.value = ''
  notice.value = ''
  targetAttempted.value = true
  if (!canUpload.value) return
  if (pickedFile.value.size > MAX_ASSET_BYTES) {
    uploadError.value = `The store accepts at most ${formatBytes(MAX_ASSET_BYTES)} per file.`
    return
  }
  uploading.value = true
  progress.value = 0
  try {
    const entry = await uploadAsset(pickedFile.value, target.value.trim(), {
      onProgress(value) {
        progress.value = value
      }
    })
    progress.value = 1
    notice.value = `Saved /assets/${entry.id} (${formatBytes(entry.size_bytes)}).`
    resetDraft()
    await load()
  } catch (err) {
    uploadError.value = err.message
  } finally {
    uploading.value = false
  }
}

// --- delete -----------------------------------------------------------------

const deleteTarget = ref(null)
const deleteBusy = ref(false)
const deleteError = ref('')

function askDelete(entry) {
  deleteError.value = ''
  notice.value = ''
  deleteTarget.value = entry
}

async function confirmDelete() {
  if (!deleteTarget.value) return
  const entry = deleteTarget.value
  deleteBusy.value = true
  deleteError.value = ''
  try {
    await deleteAsset(entry.id)
    deleteTarget.value = null
    note(`Deleted /assets/${entry.id}. Apps referencing it will fail to read it.`)
    await load()
  } catch (err) {
    deleteError.value = err.message
  } finally {
    deleteBusy.value = false
  }
}

const empty = computed(() => loaded.value && !loading.value && assets.value.length === 0)

</script>

<template>
  <div class="space-y-4">
    <header class="flex flex-wrap items-end justify-between gap-3">
      <div>
        <p class="text-xs font-semibold uppercase tracking-[0.16em] text-base-content/60">Media</p>
        <h1 class="text-2xl font-semibold tracking-tight">Assets</h1>
        <p class="mt-1 max-w-3xl text-sm text-base-content/70">
          Sounds, images and data that apps read from the shared store. Every app sandbox sees this
          same directory read-only at <code class="font-mono text-xs">/assets</code>; only this page writes to it.
        </p>
      </div>
      <button type="button" class="btn btn-sm btn-ghost" :disabled="loading" @click="load">
        <span v-if="loading" class="loading loading-spinner loading-xs" aria-hidden="true"></span>
        Refresh
      </button>
    </header>

    <div v-if="notice" role="status" class="alert alert-success py-2">
      <span class="text-sm">{{ notice }}</span>
      <button type="button" class="btn btn-ghost btn-xs" aria-label="Dismiss message" @click="notice = ''">Dismiss</button>
    </div>

    <section class="card border border-base-300 bg-base-100">
      <div class="card-body gap-4 p-5">
        <div>
          <h2 class="card-title text-base">Upload an asset</h2>
          <p class="mt-1 text-sm text-base-content/70">
            The selected file is sent as the raw request body, up to {{ formatBytes(MAX_ASSET_BYTES) }}.
            Subdirectories in the destination path are created; an existing file is replaced atomically.
          </p>
        </div>

        <div v-if="uploadError" role="alert" class="alert alert-error py-2 text-sm">
          <span>{{ uploadError }}</span>
        </div>

        <div
          class="rounded-xl border-2 border-dashed p-4 transition-colors"
          :class="dragging ? 'border-primary bg-base-200' : 'border-base-300'"
          @dragenter="startDrag"
          @dragover="startDrag"
          @dragleave.prevent="dragging = false"
          @drop.prevent="onDrop"
        >
          <input
            ref="fileInput"
            type="file"
            class="file-input file-input-bordered w-full"
            aria-label="Choose a file to upload"
            :disabled="uploading"
            @change="onPick"
          />
          <p class="mt-2 text-xs text-base-content/60">…or drop a file onto this area.</p>
          <p class="mt-1 break-all text-xs text-base-content/70">{{ pickedLabel }}</p>
        </div>

        <label class="block">
          <span class="mb-1 block text-sm font-medium">Destination path</span>
          <input
            v-model="target"
            type="text"
            class="input input-bordered w-full font-mono text-sm"
            :class="{ 'input-error': showTargetIssue }"
            placeholder="audio/replies/hello.wav"
            autocomplete="off"
            spellcheck="false"
            :disabled="uploading"
            :aria-invalid="showTargetIssue ? 'true' : undefined"
            aria-describedby="asset-target-hint"
            @input="onTargetInput"
          />
          <p id="asset-target-hint" class="mt-1 text-xs" :class="showTargetIssue ? 'text-error' : 'text-base-content/60'">
            {{ showTargetIssue ? targetIssue : 'Relative to the store, using / separators. Defaults to the file name.' }}
          </p>
        </label>

        <div class="flex flex-wrap items-center gap-3">
          <button
            type="button"
            class="btn btn-primary btn-sm"
            :disabled="!canUpload"
            :aria-busy="uploading ? 'true' : 'false'"
            @click="submitUpload"
          >
            <span v-if="uploading" class="loading loading-spinner loading-xs" aria-hidden="true"></span>
            {{ uploading ? 'Uploading…' : 'Upload' }}
          </button>
          <span v-if="uploading" class="text-xs text-base-content/70" role="status">Sending the file to the speaker…</span>
          <button v-else-if="pickedFile" type="button" class="btn btn-ghost btn-sm" @click="resetDraft">Clear</button>
        </div>

        <progress
          v-if="uploading"
          class="progress progress-primary w-full"
          :value="Math.round(progress * 100)"
          max="100"
          aria-label="Upload progress"
        ></progress>
      </div>
    </section>

    <section class="card border border-base-300 bg-base-100">
      <div class="card-body gap-3 p-5">
        <div class="flex flex-wrap items-baseline justify-between gap-2">
          <h2 class="card-title text-base">Stored assets</h2>
          <p class="text-xs text-base-content/60">
            {{ assets.length }} {{ assets.length === 1 ? 'file' : 'files' }} · {{ formatBytes(totalBytes) }}
          </p>
        </div>

        <div v-if="loadError" role="alert" class="alert alert-error py-2 text-sm">
          <span>
            <strong class="font-semibold">Asset list unavailable.</strong>
            {{ loadError }}
          </span>
          <button type="button" class="btn btn-sm btn-outline" :disabled="loading" @click="load">Try again</button>
        </div>

        <div v-if="loading && !loaded" class="space-y-2" aria-hidden="true">
          <div class="skeleton h-8 w-full"></div>
          <div class="skeleton h-8 w-full"></div>
          <div class="skeleton h-8 w-2/3"></div>
        </div>

        <div v-else-if="empty" class="rounded-xl border border-dashed border-base-300 p-6 text-center">
          <p class="text-sm font-medium">The store is empty</p>
          <p class="mt-1 text-sm text-base-content/70">
            Upload a WAV reply, an image or a data file above. It becomes available to apps at
            <code class="font-mono text-xs">/assets/&lt;path&gt;</code> immediately.
          </p>
        </div>

        <div v-else class="overflow-x-auto">
          <table class="table table-sm">
            <thead>
              <tr>
                <th scope="col">Path</th>
                <th scope="col">Type</th>
                <th scope="col" class="text-right">Size</th>
                <th scope="col">SHA-256</th>
                <th scope="col"><span class="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="entry in assets" :key="entry.id">
                <td class="max-w-[22rem]">
                  <span class="block break-all font-mono text-xs">{{ entry.path }}</span>
                </td>
                <td class="whitespace-nowrap text-xs">{{ entry.media_type }}</td>
                <td class="whitespace-nowrap text-right text-xs">{{ formatBytes(entry.size_bytes) }}</td>
                <td class="whitespace-nowrap font-mono text-xs" :title="entry.sha256">{{ shortHash(entry.sha256) }}</td>
                <td class="text-right">
                  <button
                    type="button"
                    class="btn btn-error btn-xs btn-outline"
                    :aria-label="`Delete ${entry.path}`"
                    @click="askDelete(entry)"
                  >Delete</button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </section>

    <ModalDialog v-if="deleteTarget" title="Delete asset" @close="deleteTarget = null">
      <p class="text-sm">
        Delete <code class="break-all font-mono text-xs">{{ deleteTarget.path }}</code> from the shared store?
        Apps that read it will fail until another file takes its place. This cannot be undone.
      </p>
      <div v-if="deleteError" role="alert" class="alert alert-error mt-3 py-2 text-sm">
        <span>{{ deleteError }}</span>
      </div>
      <template #footer>
        <button type="button" class="btn btn-ghost" :disabled="deleteBusy" @click="deleteTarget = null">Cancel</button>
        <button
          type="button"
          class="btn btn-error"
          :disabled="deleteBusy"
          :aria-busy="deleteBusy ? 'true' : 'false'"
          @click="confirmDelete"
        >
          <span v-if="deleteBusy" class="loading loading-spinner loading-xs" aria-hidden="true"></span>
          Delete
        </button>
      </template>
    </ModalDialog>
  </div>
</template>