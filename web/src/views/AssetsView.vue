<script setup>
import { computed, onMounted, ref, useTemplateRef } from 'vue'
import ModalDialog from '../components/ModalDialog.vue'
import { notify } from '../lib/toast.js'
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
import Icon from '../components/Icon.vue'

// Assets manages the shared read-only store. Every app sandbox mounts the same
// directory at /assets, so this page is the only writer; uploads reuse the
// session cookie and CSRF token from the shell.

const ROOT = '/assets/'
const folder = ref(ROOT)
const assets = ref([])
const totalBytes = ref(0)
const loading = ref(false)
const loaded = ref(false)
const loadError = ref('')

const createFolderName = ref('')
const createFolderBusy = ref(false)
const createFolderModal = ref(false)

const visibleAssets = computed(() => {
  const result = []

  for (let asset of assets.value) {
    asset = { ...asset }
    if (!asset.path.startsWith(folder.value)) continue

    asset.path = asset.path.replace(folder.value, '')

    if (asset.path.includes('/')) {
      const subfolderName = asset.path.split('/')[0]
      if (result.some(e => e.path === subfolderName && e.media_type === 'folder')) continue

      result.push({
        id: subfolderName,
        path: subfolderName,
        media_type: 'folder'
      })
      continue
    }


    result.push(asset)
  }

  return result.toSorted((a, b) => {
    if (a.media_type === b.media_type === 'folder') return a.path.localeCompare(b.path)
    if (a.media_type === 'folder') return -1
    if (b.media_type === 'folder') return 1
    return a.path.localeCompare(b.path)
  })
})

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    const { assets: next, totalBytes: total } = await listAssets()
    assets.value = next
    totalBytes.value = total
    loaded.value = true
  } catch (err) {
    // The first load has no list to fall back on, so it stays inline; later
    // failures keep the last list and only announce the miss.
    if (!loaded.value) {
      loadError.value = err.message
      assets.value = []
      totalBytes.value = 0
    } else {
      notify.warning(`Refresh failed: ${err.message}`)
    }
  } finally {
    loading.value = false
  }
}

onMounted(load)

// --- upload -----------------------------------------------------------------
const dragging = ref(false)
const uploading = ref(false)
const progress = ref(0)
const pickedFile = ref(null)
const fileInput = useTemplateRef('fileInput')

async function uploadFile(file) {
  if (!file) return
  if (file.size > MAX_ASSET_BYTES) {
    notify.error(`“${file.name}” is ${formatBytes(file.size)}; the store accepts at most ${formatBytes(MAX_ASSET_BYTES)} per file.`)
  }

  pickedFile.value = file
  uploading.value = true
  progress.value = 0
  try {
    const entry = await uploadAsset(file, folder.value.substring(ROOT.length) + file.name, {
      onProgress(value) {
        progress.value = value
      }
    })
    progress.value = 1
    notify.success(`Saved /assets/${entry.id} (${formatBytes(entry.size_bytes)}).`)
    await load()
  } catch (err) {
    notify.error(err.message)
  } finally {
    pickedFile.value = null
    uploading.value = false
  }
}

function onPick(event) {
  uploadFile(event.target.files?.[0])
  // Allow re-picking the same file after a failed upload.
  if (event.target) event.target.value = ''
}

function onDrop(event) {
  dragging.value = false
  uploadFile(event.dataTransfer?.files?.[0])
}

function startDrag(event) {
  event.preventDefault()
  dragging.value = true
}

// --- delete -----------------------------------------------------------------

const deleteTarget = ref(null)
const deleteBusy = ref(false)

function askDelete(entry) {
  deleteTarget.value = entry
}

async function confirmDelete() {
  if (!deleteTarget.value) return
  const entry = deleteTarget.value
  deleteBusy.value = true
  try {
    await deleteAsset(entry.id)
    deleteTarget.value = null
    notify.success(`Deleted /assets/${entry.id}. Apps referencing it will fail to read it.`)
    await load()
  } catch (err) {
    notify.error(err.message)
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
        <h1 class="text-2xl font-semibold tracking-tight">Files</h1>
        <p class="mt-1 max-w-3xl text-sm text-base-content/70">
          This is a shared store for files that apps can use. Apps can access it (read-only) at <code class="font-mono text-xs">/assets/</code>
        </p>
        <p class="mt-1 max-w-3xl text-sm text-base-content/70">
          You can drop a file into the folder view below to upload it there.
        </p>
      </div>
    </header>

    <input
      ref="fileInput"
      type="file"
      class="hidden"
      @change="onPick"
    />

    <section
      class="card border border-base-300 bg-base-100 relative"
      :class="dragging ? 'border-primary bg-base-200' : 'border-base-300'"
      @dragenter="startDrag"
      @dragover="startDrag"
      @dragleave.prevent="dragging = false"
      @drop.prevent="onDrop"
    >
      <div v-if="uploading" class="card-body absolute pointer-events-none bg-base-200/75 w-full h-full z-10 flex flex-col justify-center items-center">
        <Icon size="48px" class="text-primary">cloud_upload</Icon>
        <span class="text-primary font-bold">Uploading "{{ pickedFile.name }}" ({{ formatBytes(pickedFile.size) }})</span>
        <div class="flex gap-4 items-center">
          <progress
            class="progress progress-primary w-[300px]"
            :value="Math.round(progress * 100)"
            max="100"
            aria-label="Upload progress"
          />
          <span class="text-sm font-mono" v-text="(progress * 100).toFixed(2) + '%'"/>
        </div>
      </div>

      <div v-if="dragging" class="card-body absolute pointer-events-none bg-base-200/75 w-full h-full z-10 flex flex-col justify-center items-center">
        <Icon size="48px" class="text-primary">cloud_upload</Icon>
        <span class="text-primary font-bold">Drop to upload this file into {{ folder }}</span>
      </div>

      <div class="card-body gap-3 p-5">
        <div class="flex flex-wrap items-baseline gap-4">
          <h2 class="card-title text-base flex-grow" v-text="folder"/>
          <span class="text-xs text-base-content/60">
            <!-- TODO: display free bytes on disk -->
            {{ formatBytes(totalBytes) }} used · {{ 'TODO' }} free
          </span>

          <!-- TODO: ability to create folder -->
          <button class="btn btn-sm btn-primary btn-outline" @click="createFolderModal = true">
            Create a folder
          </button>

          <button class="btn btn-sm btn-primary" @click="() => fileInput.click()">
            Upload
          </button>
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
              <tr v-if="folder !== ROOT">
                <td class="max-w-[22rem]">
                  <a
                    class="block link link-primary break-all font-mono text-xs"
                    @click="() => folder = folder.replace(/[^\/]+\/$/, '')"
                  >..</a>
                </td>
                <td></td>
                <td></td>
                <td></td>
                <td></td>
              </tr>
              <tr v-for="entry in visibleAssets" :key="entry.id">
                <td class="max-w-[22rem]">
                  <a
                    v-if="entry.media_type === 'folder'"
                    class="block link link-primary break-all font-mono text-xs"
                    @click="() => folder += entry.path + '/'"
                  >{{ entry.path }}</a>
                  <span class="block break-all font-mono text-xs" v-else>{{ entry.path }}</span>
                </td>
                <td class="whitespace-nowrap text-xs">{{ entry.media_type }}</td>
                <td class="whitespace-nowrap text-right text-xs">
                  <template v-if="entry.size_bytes">{{ formatBytes(entry.size_bytes) }}</template>
                  <template v-else>—</template>
                </td>
                <td class="whitespace-nowrap font-mono text-xs" :title="entry.sha256">{{ shortHash(entry.sha256) }}</td>
                <td>
                  <div class="flex justify-end items-center gap-2">
                    <!-- TODO: tooltips for the icon buttons. actual daisyUI tooltips, not just title attribute -->
                    <!-- TODO: actually copy the path -->
                    <button
                      v-if="entry.media_type !== 'folder'"
                      type="button"
                      class="btn btn-xs btn-ghost btn-square"
                      aria-label="Copy file path"
                    >
                      <Icon size="16px" class="text-primary">file_copy</Icon>
                    </button>
                    <!-- TODO: ability to download a file -->
                    <button
                      v-if="entry.media_type !== 'folder'"
                      type="button"
                      class="btn btn-primary btn-xs btn-ghost btn-ghost"
                      :aria-label="`Download ${entry.path}`"
                    >
                      <Icon size="20px" class="text-primary">download</Icon>
                    </button>
                    <button
                      v-if="entry.media_type !== 'folder'"
                      type="button"
                      class="btn btn-error btn-xs btn-outline"
                      :aria-label="`Delete ${entry.path}`"
                      @click="askDelete(entry)"
                    >
                      <Icon size="16px">delete</Icon>
                      Delete
                    </button>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </section>

    <ModalDialog v-if="createFolderModal" title="Create a new folder" @close="createFolderModal = false" max-width="max-h-sm">
      <div class="text-sm">
        Enter a name for the folder that will be created in <code class="rounded bg-base-200 px-1 py-0.5 font-mono text-primary">{{ folder }}</code>:
      </div>
      <label class="input input-sm input-bordered flex w-full items-center gap-2 w-full mt-2">
        <Icon size="16px" class="text-primary">folder_open</Icon>
        <input
          v-model="createFolderName"
          type="search"
          class="grow text-sm"
          placeholder="New Folder (1)"
          autocomplete="off"
        />
      </label>
      <template #footer>
        <button type="button" class="btn btn-ghost" :disabled="createFolderBusy" @click="createFolderModal = false">Cancel</button>
        <button
          type="button"
          class="btn btn-primary"
          :disabled="createFolderBusy"
          :aria-busy="createFolderBusy ? 'true' : 'false'"
          @click="confirmDelete"
        >
          <span v-if="createFolderBusy" class="loading loading-spinner loading-xs" aria-hidden="true"></span>
          Create
        </button>
      </template>
    </ModalDialog>

    <ModalDialog v-if="deleteTarget" title="Delete asset" @close="deleteTarget = null" max-width="max-h-sm">
      <p class="text-sm">
        Delete <code class="rounded bg-base-200 px-1 py-0.5 font-mono text-primary">/assets/{{ deleteTarget.id }}</code> from the shared store? This cannot be undone.
      </p>
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