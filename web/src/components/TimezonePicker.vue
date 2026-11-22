<script setup>
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import Icon from './Icon.vue'
import ModalDialog from './ModalDialog.vue'
import { useSettings } from '../lib/live.js'
import { notify } from '../lib/toast.js'
import {
  CITY_ENTRIES,
  COUNTRY_PATHS,
  MAP_HEIGHT,
  MAP_WIDTH,
  entryFor,
  nearestCity,
  projectPoint,
  zonePathFor
} from '../lib/timezones.js'

// Time zone card. The card always shows the full IANA identifier the device
// stores (or states that it is following its firmware default) plus its local
// time and UTC offset. Editing happens in a modal map picker: clicking the map
// asks for the nearest zone.tab city, highlights that zone's real boundary and
// pins the city; the search list does the same explicitly. Nothing is written
// until Apply is pressed, and the browser's own zone is never assumed.

const { data, error, update } = useSettings()

const RESULT_LIMIT = 40

// Pin geometry is expressed in viewBox units, derived from the map width so a
// 18000-unit map still gets a few screen pixels of marker.
const PIN_INNER = Math.round(MAP_WIDTH / 230)
const PIN_OUTER = Math.round(MAP_WIDTH / 110)
const PIN_HALO = Math.round(PIN_OUTER * 1.4)

const now = ref(new Date())
const openPicker = ref(false)
const draft = ref('')
const query = ref('')
const saving = ref(false)

let ticker = null

const stored = computed(() => String(data.value?.values?.timezone || ''))
const storedEntry = computed(() => (stored.value ? entryFor(stored.value) : null))
const draftEntry = computed(() => (draft.value ? entryFor(draft.value) : null))
const needle = computed(() => query.value.trim().toLowerCase())

// The selected zone's boundary path, drawn with fill-rule="evenodd" so holes
// (lakes, enclaves) stay unfilled inside it.
const selectedPath = computed(() => zonePathFor(draft.value))

const pin = computed(() => {
  const entry = draftEntry.value
  if (!entry || entry.lat === null || entry.lon === null) return null
  return projectPoint(entry.lat, entry.lon)
})

// A push from the device is authoritative for the card, but it must not
// replace a selection the user is holding in the open dialog. Only an
// untouched draft follows the device.
watch(stored, (value, previous) => {
  if (!openPicker.value || draft.value === previous) draft.value = value
})

const listEntries = computed(() => {
  const value = needle.value
  if (!value) return CITY_ENTRIES
  return CITY_ENTRIES.filter((entry) => (
    `${entry.zone} ${entry.city} ${entry.country}`.toLowerCase().includes(value)
  ))
})

const visibleEntries = computed(() => listEntries.value.slice(0, RESULT_LIMIT))

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

function resetDialogState() {
  draft.value = stored.value
  query.value = ''
}

function startEditing() {
  now.value = new Date()
  resetDialogState()
  openPicker.value = true
}

function closeDialog() {
  openPicker.value = false
  resetDialogState()
}

// Dismissal is blocked while the patch is in flight; the write settles and the
// dialog closes itself on success.
function beforeClose() {
  return !saving.value
}

function choose(zone) {
  draft.value = zone
  document.activeElement.blur()
}

/**
 * Map click: convert the pointer from screen space to viewBox space (honouring
 * preserveAspectRatio letterboxing), then to lon/lat, then pick the nearest
 * zone.tab city. No per-city hit targets are rendered.
 */
function pickOnMap(event) {
  const svg = event.currentTarget
  const rect = svg.getBoundingClientRect()
  if (!rect.width || !rect.height) return
  const scale = Math.min(rect.width / MAP_WIDTH, rect.height / MAP_HEIGHT)
  const left = rect.left + (rect.width - MAP_WIDTH * scale) / 2
  const top = rect.top + (rect.height - MAP_HEIGHT * scale) / 2
  const x = (event.clientX - left) / scale
  const y = (event.clientY - top) / scale
  if (x < 0 || y < 0 || x > MAP_WIDTH || y > MAP_HEIGHT) return
  const lon = (x / MAP_WIDTH) * 360 - 180
  const lat = 90 - (y / MAP_HEIGHT) * 180
  const nearest = nearestCity(lon, lat)
  if (nearest) choose(nearest.zone)
}

async function applyZone() {
  if (saving.value) return
  const zone = draft.value
  if (!zone) {
    notify.warning('Choose a time zone before applying.')
    return
  }
  saving.value = true
  try {
    const ok = await update('timezone', { timezone: zone })
    if (ok) {
      notify.success(`Device time zone set to ${zone}.`)
      openPicker.value = false
      resetDialogState()
    } else {
      notify.error(error.value || 'The device rejected that time zone. Try another.')
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
        class="btn btn-sm btn-outline -mr-1 -mt-1"
        aria-label="Edit time zone"
        @click="startEditing"
      >
        Change
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

    <ModalDialog
      v-if="openPicker"
      title="Choose time zone"
      :before-close="beforeClose"
      @close="closeDialog"
    >
      <div class="space-y-3">
        <div class="dropdown w-full">
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

          <ul v-if="visibleEntries.length" class="dropdown-content menu bg-base-100 rounded-box z-1 p-2 shadow-sm w-full max-h-56 overflow-y-auto flex-nowrap">
            <li v-for="entry in visibleEntries" :key="entry.zone" class="border-b border-base-300 last:border-b-0 w-full">
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
                    {{ entry.city }}<template v-if="entry.country"> · {{ entry.country }}</template>
                  </span>
                </span>
                <span class="shrink-0 text-right text-xs text-base-content/60">
                  <span class="block font-mono">{{ offsetIn(entry.zone) }}</span>
                  <span class="block">{{ clockIn(entry.zone) }}</span>
                </span>
              </button>
            </li>
          </ul>
        </div>

        <div class="overflow-hidden rounded-box border border-base-300">
          <div class="bg-base-100">
            <svg
              :viewBox="`0 0 ${MAP_WIDTH} ${MAP_HEIGHT}`"
              class="block max-h-[44vh] w-full cursor-crosshair"
              preserveAspectRatio="xMidYMid meet"
              role="img"
              aria-label="World map of time zones. Click near your location to select the nearest city, or choose a city from the list below."
              @click="pickOnMap"
            >
              <rect :width="MAP_WIDTH" :height="MAP_HEIGHT" class="fill-base-100" />
              <path
                :d="COUNTRY_PATHS"
                fill-rule="evenodd"
                class="fill-base-content/10 stroke-base-content/30"
                stroke-width="0.7"
                vector-effect="non-scaling-stroke"
              />
              <path
                v-if="selectedPath"
                :d="selectedPath"
                fill-rule="evenodd"
                class="fill-primary/30 stroke-primary"
                stroke-width="1.2"
                vector-effect="non-scaling-stroke"
                pointer-events="none"
              />
              <g
                v-if="pin"
                :transform="`translate(${pin.x} ${pin.y})`"
                pointer-events="none"
                aria-hidden="true"
              >
                <circle :r="PIN_HALO" class="fill-base-100/80" />
                <circle :r="PIN_OUTER" class="fill-primary/25" />
                <circle
                  :r="PIN_INNER"
                  class="fill-primary stroke-base-100"
                  stroke-width="1.5"
                  vector-effect="non-scaling-stroke"
                />
              </g>
            </svg>
          </div>
          <p class="border-t border-base-300 px-2 py-1 text-[11px] leading-snug text-base-content/50">
            Time zone boundaries © timezone-boundary-builder contributors (ODbL 1.0) · Base map Natural Earth (public domain)
          </p>
        </div>
      </div>

      <template #footer>
        <p class="flex min-h-[1.75rem] flex-wrap flex-grow items-center gap-x-1 text-sm text-base-content/70">
          <template v-if="draft">
            <span class="font-mono text-base-content">{{ draft }}</span>
            <span aria-hidden="true">·</span>
            <span>{{ offsetIn(draft) }}</span>
            <span aria-hidden="true">·</span>
            <span>{{ clockIn(draft) }}</span>
          </template>
          <span v-else>Click the map near your location, or search for a city.</span>
        </p>

        <button type="button" class="btn btn-ghost" :disabled="saving" @click="closeDialog">Cancel</button>
        <button
          type="button"
          class="btn btn-primary"
          :disabled="saving || !draft || draft === stored"
          :aria-busy="saving ? 'true' : 'false'"
          @click="applyZone"
        >
          Apply
        </button>
      </template>
    </ModalDialog>
  </div>
</template>
