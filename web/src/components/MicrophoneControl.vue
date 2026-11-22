<script setup>
import { computed, ref, watch } from 'vue'
import { liveState, useSettings } from '../lib/live.js'

const { data, error, update } = useSettings()
const submitting = ref(false)
const enabled = ref(true)

const muted = computed(() => !!data.value?.values?.microphoneMuted)
const available = computed(() => !!data.value && liveState.value === 'open')

// Positive control: ON means the microphone is enabled, matching the badge.
watch(() => data.value?.values?.microphoneMuted, (value) => {
  if (!submitting.value) enabled.value = !value
}, { immediate: true })

async function applyEnabled(next) {
  if (!available.value || submitting.value) return
  enabled.value = next
  submitting.value = true
  const ok = await update('microphone', { muted: !next })
  submitting.value = false
  // A rejected/dropped update must not leave a visually toggled control; the
  // failed call already re-read the authoritative snapshot.
  if (!ok) enabled.value = !Boolean(data.value?.values?.microphoneMuted)
}
</script>

<template>
  <div class="flex flex-col gap-1.5">
    <label class="flex items-center gap-2 text-sm">
      <input
        type="checkbox"
        class="toggle toggle-primary toggle-sm"
        role="switch"
        :checked="enabled"
        :disabled="!available || submitting"
        :aria-label="$t('settings.microphoneToggleAria')"
        @change="applyEnabled($event.target.checked)"
      />
      <span>{{ enabled ? $t('settings.microphoneOn') : $t('settings.microphoneOff') }}</span>
    </label>
    <p v-if="!data" class="text-xs text-base-content/60">{{ $t('settings.waitingSettings') }}</p>
    <p v-else-if="liveState !== 'open'" class="text-xs text-warning">{{ $t('settings.disconnectedPaused') }}</p>
    <p v-else-if="error" class="text-xs text-error">{{ error }}</p>
  </div>
</template>
