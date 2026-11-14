<script setup>
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useSettings } from '../lib/live.js'

// Friendly timezone picker: the default view shows only the speaker's current
// setting, a phone/computer suggestion and UTC. A bounded city list appears
// only after the user searches, and the device zone changes only through an
// explicit Apply. The browser's own zone is never selected or submitted
// automatically.

const { data, error, update } = useSettings()

const FALLBACK_ZONES = [
  'UTC',
  'Europe/London',
  'Europe/Berlin',
  'Europe/Moscow',
  'Europe/Istanbul',
  'Africa/Cairo',
  'Africa/Johannesburg',
  'Asia/Dubai',
  'Asia/Kolkata',
  'Asia/Shanghai',
  'Asia/Tokyo',
  'Asia/Seoul',
  'Australia/Sydney',
  'Pacific/Auckland',
  'America/Sao_Paulo',
  'America/New_York',
  'America/Chicago',
  'America/Denver',
  'America/Los_Angeles'
]

const RESULT_LIMIT = 20

function listZones() {
  try {
    const zones = Intl.supportedValuesOf?.('timeZone')
    if (Array.isArray(zones) && zones.length) return zones
  } catch (_) { /* older engines: fall back to the curated shortlist */ }
  return FALLBACK_ZONES
}

function decorate(zone) {
  const parts = String(zone).split('/')
  const city = (parts[parts.length - 1] || zone).replaceAll('_', ' ')
  const region = parts.length > 1 ? parts.slice(0, -1).join(' · ').replaceAll('_', ' ') : ''
  return { zone, city, region }
}

const zones = listZones().map(decorate)
const zoneIndex = new Map(zones.map((entry) => [entry.zone, entry]))

const browserZone = (() => {
  try { return Intl.DateTimeFormat().resolvedOptions().timeZone || '' } catch (_) { return '' }
})()

const now = ref(new Date())
let ticker = null

const stored = computed(() => String(data.value?.values?.timezone || ''))
const query = ref('')
const selected = ref(stored.value)
const dirty = ref(false)
const saving = ref(false)
const actionError = ref('')
const note = ref('')

// A push from the device reflects the authoritative zone unless the user is
// holding an unapplied selection of their own.
watch(stored, (value) => {
  if (!dirty.value && value !== selected.value) selected.value = value
})

function friendly(zone) {
  if (!zone) return null
  return zoneIndex.get(zone) || decorate(zone)
}

const currentEntry = computed(() => friendly(stored.value))
const selectedEntry = computed(() => friendly(selected.value))

const hasQuery = computed(() => query.value.trim().length > 0)

function matches(entry, needle) {
  return `${entry.city} ${entry.region} ${entry.zone}`.toLowerCase().includes(needle)
}

const filtered = computed(() => {
  const needle = query.value.trim().toLowerCase()
  if (!needle) return []
  return zones.filter((entry) => matches(entry, needle)).slice(0, RESULT_LIMIT)
})

const matchingTotal = computed(() => {
  const needle = query.value.trim().toLowerCase()
  if (!needle) return 0
  return zones.filter((entry) => matches(entry, needle)).length
})

const suggestion = computed(() => (
  browserZone && browserZone !== stored.value && browserZone !== selected.value ? browserZone : ''
))

const suggestionEntry = computed(() => friendly(suggestion.value))

function timeIn(zone) {
  if (!zone) return '—'
  try {
    return new Intl.DateTimeFormat(undefined, {
      timeZone: zone, weekday: 'short', hour: '2-digit', minute: '2-digit'
    }).format(now.value)
  } catch (_) {
    return '—'
  }
}

function offsetIn(zone) {
  if (!zone) return '—'
  for (const style of ['longOffset', 'shortOffset']) {
    try {
      const parts = new Intl.DateTimeFormat('en-US', {
        timeZone: zone, timeZoneName: style
      }).formatToParts(now.value)
      const name = parts.find((part) => part.type === 'timeZoneName')?.value || ''
      if (name) return name.replace('GMT', 'UTC')
    } catch (_) { /* try the next style */ }
  }
  return '—'
}

function pick(zone) {
  selected.value = zone
  dirty.value = zone !== stored.value
  actionError.value = ''
  note.value = ''
}

function useSuggestion() {
  if (!suggestion.value) return
  pick(suggestion.value)
  query.value = ''
  note.value = 'Phone or computer time zone selected. Press Apply to use it on the speaker.'
}

async function applyZone() {
  if (saving.value) return
  saving.value = true
  actionError.value = ''
  note.value = ''
  const zone = selected.value
  try {
    const ok = await update('timezone', { timezone: zone })
    if (ok) {
      dirty.value = false
      const entry = friendly(zone)
      note.value = entry
        ? `Speaker time zone set to ${entry.city}${entry.region ? ` (${entry.region})` : ''}.`
        : 'The speaker now follows its default time zone.'
    } else {
      actionError.value = error.value || 'The speaker rejected that time zone.'
      selected.value = stored.value
      dirty.value = false
    }
  } finally {
    saving.value = false
  }
}

onMounted(() => {
  ticker = setInterval(() => { now.value = new Date() }, 30000)
})

onUnmounted(() => {
  if (ticker) clearInterval(ticker)
  ticker = null
})
</script>

<template>
  <div class="space-y-3">
    <div class="space-y-2 rounded-box bg-base-200 px-3 py-3">
      <p class="text-xs uppercase tracking-wide opacity-60">Speaker time zone</p>
      <template v-if="currentEntry">
        <p class="text-base font-medium">
          {{ currentEntry.city }}
          <span v-if="currentEntry.region" class="text-sm font-normal opacity-60">{{ currentEntry.region }}</span>
        </p>
        <p class="text-sm opacity-70">
          Now: {{ timeIn(stored) }} <span class="opacity-60">·</span> {{ offsetIn(stored) }}
        </p>
      </template>
      <template v-else>
        <p class="text-base font-medium">Speaker default time zone</p>
        <p class="text-sm opacity-70">
          The speaker uses its own time zone. Choose one below to override it.
        </p>
      </template>

      <details class="text-xs opacity-70">
        <summary class="cursor-pointer">Technical details</summary>
        <p v-if="stored" class="mt-1">
          Zone identifier: <span class="font-mono">{{ stored }}</span>.
        </p>
        <p class="mt-1">With no zone chosen, the speaker follows its firmware default.</p>
      </details>
    </div>

    <div v-if="suggestion" class="alert alert-info" role="status">
      <span>
        Your phone or computer uses
        <strong>{{ suggestionEntry?.city || suggestion }}</strong>.
      </span>
      <button type="button" class="btn btn-sm btn-ghost" @click="useSuggestion">
        Use this phone or computer’s time zone
      </button>
    </div>

    <div class="flex flex-col gap-2 sm:flex-row">
      <label class="input input-bordered flex flex-1 items-center gap-2">
        <span class="text-xs uppercase opacity-60">Search</span>
        <input
          v-model="query"
          type="search"
          class="grow"
          placeholder="Search for your city"
          aria-label="Search for your city or region"
          autocomplete="off"
        />
      </label>
      <button
        type="button"
        class="btn btn-primary sm:w-44"
        :disabled="saving || !dirty"
        :aria-busy="saving ? 'true' : 'false'"
        @click="applyZone"
      >
        <span v-if="saving" class="loading loading-spinner loading-xs" aria-hidden="true"></span>
        {{ saving ? 'Applying…' : 'Apply time zone' }}
      </button>
    </div>

    <div v-if="dirty" class="rounded-box border border-base-300 px-3 py-2 text-sm" role="status">
      <p>
        Will use
        <strong>{{ selected ? (selectedEntry?.city || selected) : 'Speaker default time zone' }}</strong>
        <span v-if="selected && selectedEntry?.region" class="opacity-60">{{ selectedEntry.region }}</span>
        <span v-if="selected" class="opacity-60"> · {{ offsetIn(selected) }} · {{ timeIn(selected) }}</span>
      </p>
      <p class="mt-1 text-xs opacity-60">Press Apply time zone to change the speaker.</p>
    </div>

    <p v-if="!hasQuery" class="text-xs opacity-60">
      Type a city or region like “Berlin” or “Asia” to see matching time zones.
    </p>

    <div v-if="!hasQuery" class="rounded-box border border-base-300">
      <p class="px-3 pt-2 text-xs uppercase tracking-wide opacity-60">Suggestions</p>
      <button
        type="button"
        :aria-pressed="selected === 'UTC' ? 'true' : 'false'"
        class="flex w-full items-center justify-between gap-3 border-t border-base-300 px-3 py-2 text-left hover:bg-base-200"
        :class="selected === 'UTC' ? 'bg-base-300' : ''"
        @click="pick('UTC')"
      >
        <span>
          <span class="text-sm font-medium">UTC</span>
          <span class="ml-2 text-xs opacity-60">Coordinated Universal Time</span>
        </span>
        <span class="shrink-0 text-right text-xs">
          <span class="font-mono">{{ offsetIn('UTC') }}</span>
          <span class="ml-2 opacity-60">{{ timeIn('UTC') }}</span>
        </span>
      </button>
      <button
        v-if="stored"
        type="button"
        :aria-pressed="selected === '' ? 'true' : 'false'"
        class="flex w-full items-center justify-between gap-3 border-t border-base-300 px-3 py-2 text-left hover:bg-base-200"
        :class="selected === '' ? 'bg-base-300' : ''"
        @click="pick('')"
      >
        <span>
          <span class="text-sm font-medium">Speaker default time zone</span>
          <span class="ml-2 text-xs opacity-60">stop overriding it</span>
        </span>
      </button>
    </div>

    <div v-if="hasQuery" class="max-h-64 overflow-y-auto rounded-box border border-base-300">
      <button
        v-for="entry in filtered"
        :key="entry.zone"
        type="button"
        :aria-pressed="selected === entry.zone ? 'true' : 'false'"
        class="flex w-full items-center justify-between gap-3 border-b border-base-300 px-3 py-2 text-left last:border-b-0 hover:bg-base-200"
        :class="selected === entry.zone ? 'bg-base-300' : ''"
        @click="pick(entry.zone)"
      >
        <span class="min-w-0">
          <span class="text-sm font-medium">{{ entry.city }}</span>
          <span v-if="entry.region" class="ml-2 text-xs opacity-60">{{ entry.region }}</span>
        </span>
        <span class="shrink-0 text-right text-xs">
          <span class="font-mono">{{ offsetIn(entry.zone) }}</span>
          <span class="ml-2 opacity-60">{{ timeIn(entry.zone) }}</span>
        </span>
      </button>
      <p v-if="!filtered.length" class="px-3 py-4 text-sm opacity-70">
        No time zones match “{{ query }}”. Try a city like Berlin or a region like Asia.
      </p>
      <p v-else-if="matchingTotal > filtered.length" class="border-t border-base-300 px-3 py-2 text-xs opacity-60">
        Showing {{ filtered.length }} of {{ matchingTotal }} matches — keep typing to narrow the list.
      </p>
    </div>

    <div v-if="actionError" class="alert alert-error" role="alert">{{ actionError }}</div>
    <p v-else-if="note" class="text-sm text-success" role="status">{{ note }}</p>
    <p class="text-xs opacity-60">
      Changing the zone restarts clock-driven apps; running timers, the stopwatch and alarms keep their state.
    </p>
  </div>
</template>
