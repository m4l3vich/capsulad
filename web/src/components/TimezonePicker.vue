<script setup>
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import Icon from './Icon.vue'
import ModalDialog from './ModalDialog.vue'
import { useSettings } from '../lib/live.js'
import {
  LAND_PATHS,
  MAP_HEIGHT,
  MAP_WIDTH,
  MARKER_ENTRIES,
  PROJECTED_MARKERS,
  entryFor,
  zoneCatalogue
} from '../lib/timezones.js'

// Time zone card. The card always shows the full IANA identifier the device
// stores (or states that it is following its firmware default) plus its local
// time and UTC offset. Editing happens in a modal map picker: markers and the
// search list only change the pending selection, and the device is written
// only when Apply is pressed. The browser's own zone is never offered,
// selected or submitted automatically.

const { data, error, update } = useSettings()

const CATALOGUE = zoneCatalogue()
const RESULT_LIMIT = 40

const now = ref(new Date())
const openPicker = ref(false)
const draft = ref('')
const query = ref('')
const active = ref('')
const saving = ref(false)
const dialogError = ref('')
const statusNote = ref('')

let ticker = null
let noteTimer = null

const stored = computed(() => String(data.value?.values?.timezone || ''))
const storedEntry = computed(() => (stored.value ? entryFor(stored.value) : null))
const draftEntry = computed(() => (draft.value ? entryFor(draft.value) : null))
const needle = computed(() => query.value.trim().toLowerCase())

// A push from the device is authoritative for the card, but it must not
// replace a selection the user is holding in the open dialog. Only an
// untouched draft follows the device.
watch(stored, (value, previous) => {
  if (!openPicker.value || draft.value === previous) draft.value = value
})

const listEntries = computed(() => {
  const value = needle.value
  if (!value) return MARKER_ENTRIES
  return CATALOGUE.filter((entry) => (
    `${entry.zone} ${entry.city} ${entry.region}`.toLowerCase().includes(value)
  ))
})

const visibleEntries = computed(() => listEntries.value.slice(0, RESULT_LIMIT))
const resultTotal = computed(() => listEntries.value.length)

// Formatters are built once per zone/kind; the dialog re-renders the visible
// list every tick and on every keystroke, so constructing them per call is
// avoidable work.
const formatters = new Map()

function formatterFor(key, zone, options) {
  let formatter = formatters.get(key)
  if (!formatter) {
    formatter = new Intl.DateTimeFormat(undefined, { timeZone: zone, ...options })
    formatters.set(key, formatter)
  }
  return formatter
}

function clockIn(zone) {
  if (!zone) return '—'
  try {
    return formatterFor(`clock|${zone}`, zone, { hour: '2-digit', minute: '2-digit' }).format(now.value)
  } catch (_) {
    return '—'
  }
}

function dayClockIn(zone) {
  if (!zone) return '—'
  try {
    return formatterFor(`day|${zone}`, zone, {
      weekday: 'short', hour: '2-digit', minute: '2-digit'
    }).format(now.value)
  } catch (_) {
    return '—'
  }
}

function offsetIn(zone) {
  if (!zone) return '—'
  for (const style of ['longOffset', 'shortOffset']) {
    try {
      const formatter = formatterFor(`offset|${style}|${zone}`, zone, {
        timeZoneName: style
      })
      const parts = formatter.formatToParts(now.value)
      const name = parts.find((part) => part.type === 'timeZoneName')?.value || ''
      if (name) return name.replace('GMT', 'UTC')
    } catch (_) {
      // Try the next style, then give up on the offset only.
    }
  }
  return '—'
}

function startEditing() {
  now.value = new Date()
  draft.value = stored.value
  query.value = ''
  active.value = ''
  dialogError.value = ''
  statusNote.value = ''
  openPicker.value = true
}

function closeDialog() {
  openPicker.value = false
  draft.value = stored.value
  query.value = ''
  active.value = ''
  dialogError.value = ''
}

// Dismissal is blocked while the patch is in flight; the write settles and the
// dialog closes itself on success.
function beforeClose() {
  return !saving.value
}

function choose(zone) {
  draft.value = zone
  active.value = zone
  dialogError.value = ''
}

function clearActive(zone) {
  if (active.value === zone) active.value = ''
}

async function applyZone() {
  if (saving.value) return
  if (!draft.value) {
    dialogError.value = 'Choose a time zone before applying.'
    return
  }
  const zone = draft.value
  saving.value = true
  dialogError.value = ''
  try {
    const ok = await update('timezone', { timezone: zone })
    if (ok) {
      statusNote.value = `Device time zone set to ${zone}.`
      openPicker.value = false
      draft.value = stored.value
      query.value = ''
      active.value = ''
      if (noteTimer) clearTimeout(noteTimer)
      noteTimer = setTimeout(() => {
        statusNote.value = ''
        noteTimer = null
      }, 6000)
    } else {
      dialogError.value = error.value || 'The device rejected that time zone. Try another.'
    }
  } finally {
    saving.value = false
  }
}

onMounted(() => {
  ticker = setInterval(() => { now.value = new Date() }, 15000)
})

onUnmounted(() => {
  if (ticker) clearInterval(ticker)
  ticker = null
  if (noteTimer) clearTimeout(noteTimer)
  noteTimer = null
})
</script>

<template>
  <div class="rounded-box bg-base-200/60 p-3 sm:p-4">
    <div class="flex items-start justify-between gap-3">
      <div class="min-w-0">
        <h3 class="text-lg font-semibold leading-tight">Timezone</h3>
        <p v-if="stored" class="mt-1 break-all font-mono text-sm font-medium text-base-content">{{ stored }}</p>
        <p v-else class="mt-1 text-sm text-base-content/70">The device uses its firmware default time zone.</p>
      </div>
      <button
        type="button"
        class="btn btn-sm btn-ghost btn-square -mr-1 -mt-1"
        aria-label="Edit time zone"
        @click="startEditing"
      >
        <Icon size="18px">edit</Icon>
      </button>
    </div>

    <p v-if="stored" class="mt-2 text-sm text-base-content/70">
      Local time <span class="font-medium text-base-content">{{ dayClockIn(stored) }}</span>
      <span class="mx-1 text-base-content/40" aria-hidden="true">·</span>
      <span class="font-mono">{{ offsetIn(stored) }}</span>
      <span v-if="storedEntry?.city && storedEntry.city !== stored" class="text-base-content/60">
        <span class="mx-1 text-base-content/40" aria-hidden="true">·</span>{{ storedEntry.city }}
      </span>
    </p>
    <p v-else class="mt-2 text-sm text-base-content/60">
      Nothing is stored on the device, so it follows its firmware default. Choose a time zone to override it.
    </p>

    <p v-if="statusNote" class="mt-2 text-sm text-success" role="status">{{ statusNote }}</p>

    <ModalDialog
      v-if="openPicker"
      title="Choose time zone"
      :before-close="beforeClose"
      @close="closeDialog"
    >
      <div class="space-y-3">
        <label class="input input-bordered flex items-center gap-2">
          <Icon size="18px" class="text-base-content/60">search</Icon>
          <input
            v-model="query"
            type="search"
            class="grow"
            placeholder="Search a city or time zone"
            aria-label="Search a city or time zone"
            autocomplete="off"
            spellcheck="false"
          />
        </label>

        <div class="overflow-hidden rounded-box border border-base-300">
          <div class="bg-base-300/30 p-1 sm:p-2">
            <svg
              :viewBox="`0 0 ${MAP_WIDTH} ${MAP_HEIGHT}`"
              class="block w-full"
              preserveAspectRatio="xMidYMid meet"
              role="group"
              aria-label="World map with time zone markers"
              @mouseleave="active = ''"
            >
              <rect
                :width="MAP_WIDTH"
                :height="MAP_HEIGHT"
                class="fill-base-100"
              />
              <path
                v-for="(shape, index) in LAND_PATHS"
                :key="index"
                :d="shape"
                class="fill-base-content/15 stroke-base-content/25"
                stroke-width="1"
                vector-effect="non-scaling-stroke"
              />
              <g
                v-for="marker in PROJECTED_MARKERS"
                :key="marker.zone"
                class="cursor-pointer outline-none"
                role="button"
                tabindex="0"
                :aria-label="marker.zone"
                :aria-pressed="draft === marker.zone ? 'true' : 'false'"
                @click="choose(marker.zone)"
                @keydown.enter.prevent="choose(marker.zone)"
                @keydown.space.prevent="choose(marker.zone)"
                @mouseenter="active = marker.zone"
                @mouseleave="clearActive(marker.zone)"
                @focus="active = marker.zone"
                @blur="active = ''"
              >
                <title>{{ marker.zone }}</title>
                <circle :cx="marker.x" :cy="marker.y" r="6" fill="transparent" />
                <circle
                  v-if="draft === marker.zone || active === marker.zone"
                  :cx="marker.x"
                  :cy="marker.y"
                  :r="draft === marker.zone ? 4.4 : 3.4"
                  :class="draft === marker.zone ? 'fill-primary/35' : 'fill-base-content/25'"
                />
                <circle
                  :cx="marker.x"
                  :cy="marker.y"
                  :r="draft === marker.zone ? 2.3 : 1.6"
                  :class="draft === marker.zone ? 'fill-primary' : 'fill-base-content/50'"
                />
              </g>
            </svg>
          </div>
          <p class="flex min-h-[1.75rem] flex-wrap items-center gap-x-1 border-t border-base-300 px-2 py-1 text-xs text-base-content/70">
            <template v-if="active">
              <span class="font-mono text-base-content">{{ active }}</span>
              <span aria-hidden="true">·</span>
              <span>{{ offsetIn(active) }}</span>
              <span aria-hidden="true">·</span>
              <span>{{ clockIn(active) }}</span>
            </template>
            <span v-else>Tap a marker or search for a city.</span>
          </p>
        </div>

        <div class="overflow-hidden rounded-box border border-base-300">
          <p class="border-b border-base-300 bg-base-200/60 px-3 py-1.5 text-xs font-medium text-base-content/60">
            {{ needle ? 'Matches' : 'Cities' }}
          </p>
          <ul v-if="visibleEntries.length" class="max-h-56 overflow-y-auto">
            <li v-for="entry in visibleEntries" :key="entry.zone" class="border-b border-base-300 last:border-b-0">
              <button
                type="button"
                class="flex w-full items-center justify-between gap-3 px-3 py-2 text-left hover:bg-base-200"
                :class="draft === entry.zone ? 'bg-base-300' : ''"
                :aria-pressed="draft === entry.zone ? 'true' : 'false'"
                @click="choose(entry.zone)"
              >
                <span class="min-w-0">
                  <span class="block truncate font-mono text-sm">{{ entry.zone }}</span>
                  <span class="block truncate text-xs text-base-content/60">
                    {{ entry.city }}<template v-if="entry.region"> · {{ entry.region }}</template>
                  </span>
                </span>
                <span class="shrink-0 text-right text-xs text-base-content/60">
                  <span class="block font-mono">{{ offsetIn(entry.zone) }}</span>
                  <span class="block">{{ clockIn(entry.zone) }}</span>
                </span>
              </button>
            </li>
          </ul>
          <p v-else class="px-3 py-4 text-sm text-base-content/60">
            No time zones match “{{ query }}”. Try a city like Berlin or a region like Asia.
          </p>
          <p
            v-if="resultTotal > visibleEntries.length"
            class="border-t border-base-300 px-3 py-1.5 text-xs text-base-content/60"
          >
            Showing {{ visibleEntries.length }} of {{ resultTotal }} matches — keep typing to narrow the list.
          </p>
        </div>

        <div class="rounded-box bg-base-200/60 px-3 py-2">
          <template v-if="draftEntry">
            <p class="break-all font-mono text-sm font-medium">{{ draftEntry.zone }}</p>
            <p class="text-xs text-base-content/60">
              {{ draftEntry.city }}<template v-if="draftEntry.region"> · {{ draftEntry.region }}</template>
              <span class="mx-1" aria-hidden="true">·</span>{{ offsetIn(draft) }}
              <span class="mx-1" aria-hidden="true">·</span>{{ dayClockIn(draft) }}
            </p>
          </template>
          <p v-else class="text-sm text-base-content/60">Choose a time zone to preview it.</p>
          <p v-if="dialogError" class="mt-1 text-sm text-error" role="alert">{{ dialogError }}</p>
        </div>
      </div>

      <template #footer>
        <button type="button" class="btn btn-ghost" :disabled="saving" @click="closeDialog">Cancel</button>
        <button
          type="button"
          class="btn btn-primary"
          :disabled="saving || !draft || draft === stored"
          :aria-busy="saving ? 'true' : 'false'"
          @click="applyZone"
        >
          <span v-if="saving" class="loading loading-spinner loading-xs" aria-hidden="true"></span>
          {{ saving ? 'Applying…' : 'Apply' }}
        </button>
      </template>
    </ModalDialog>
  </div>
</template>
