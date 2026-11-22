<script setup>
import { computed, onScopeDispose, ref, watch } from 'vue'
import { liveState, useSettings } from '../lib/live.js'
import { pct } from '../lib/format.js'

const { data, error, pending, update, viewEpoch } = useSettings()

const local = ref(0)
const dragging = ref(false)
const applying = ref(false)
let timer = null
let inflight = 0

const available = computed(() => !!data.value && liveState.value === 'open')
const remote = computed(() => pct(data.value?.values?.masterVolume))

// Follow hardware and other-client changes immediately, including an
// authoritative view whose numeric volume happens to equal the previous one.
// Only dragging or our own in-flight request pauses the follow.
watch([viewEpoch, remote], ([, value]) => {
  if (dragging.value || applying.value) return
  local.value = value
}, { immediate: true })

function queue() {
  if (timer) clearTimeout(timer)
  timer = setTimeout(push, 120)
}

async function push() {
  timer = null
  if (!available.value) return
  const value = Math.max(0, Math.min(100, Math.round(Number(local.value) || 0)))
  const epochBefore = viewEpoch.value
  local.value = value
  inflight += 1
  applying.value = true
  const ok = await update('volume', { masterVolume: value / 100 })
  inflight -= 1
  if (inflight > 0) return
  applying.value = false
  // Reconcile when an authoritative view arrived during the request (even if
  // its numeric value equals the pre-request value), or after rejection.
  // Otherwise keep the accepted value until its own push arrives.
  if (!ok || viewEpoch.value !== epochBefore) local.value = remote.value
}

function endDrag() {
  dragging.value = false
  queue()
}

onScopeDispose(() => {
  // Flush a pending slider step instead of dropping the last position.
  if (timer) {
    clearTimeout(timer)
    timer = null
    push()
  }
})
</script>

<template>
  <div class="flex flex-col gap-1.5">
    <div class="flex gap-2">
      <input
        v-model.number="local"
        type="range"
        class="range range-primary range-sm"
        min="0"
        max="100"
        step="1"
        :aria-label="$t('settings.masterOutputVolume')"
        :disabled="!available || applying"
        @pointerdown="dragging = true"
        @pointerup="endDrag"
        @pointercancel="endDrag"
        @change="endDrag"
        @input="queue"
      />
      <span class="text-sm font-semibold tabular-nums">{{ local }}%</span>
    </div>
    <p v-if="!data" class="text-xs text-base-content/60">{{ $t('settings.waitingSettings') }}</p>
    <p v-else-if="liveState !== 'open'" class="text-xs text-warning">{{ $t('settings.disconnectedPaused') }}</p>
    <p v-else-if="error" class="text-xs text-error">{{ error }}</p>
  </div>
</template>
