<script setup>
import { computed, onBeforeUnmount, useTemplateRef } from 'vue'
import { THEME_MODES, setThemeMode, theme } from '../lib/theme.js'
import Icon from './Icon.vue'

const currentThemeIcon = computed(() =>
  THEME_MODES.find(e => e.id === theme.mode).icon
)

// to get rid of this manual "click outside to close"
// we need to update to daisyUI 5. that's for later
const details = useTemplateRef('dropdown')

function globalOnClick(e) {
  if (!details.value.contains(e.target)) {
    details.value.removeAttribute('open')
  }
}

document.addEventListener('click', globalOnClick)
onBeforeUnmount(() => {
  document.removeEventListener('click', globalOnClick)
})
</script>

<template>
  <details ref="dropdown" class="dropdown dropdown-end">
    <summary class="btn btn-sm btn-outline btn-square">
      <Icon size="20px">{{ currentThemeIcon }}</Icon>
    </summary>
    <ul class="menu dropdown-content bg-base-100 rounded-box z-1 w-52 p-2 shadow-sm">
      <li class="text-xs p-2 text-base-content/50">{{ $t('theme.label') }}</li>
      <li v-for="theme in THEME_MODES" :key="theme.id">
        <a @click="setThemeMode(theme.id)">
          <Icon size="20px">{{ theme.icon }}</Icon>
          {{ $t(`theme.${theme.id}`) }}
        </a>
      </li>
    </ul>
  </details>
</template>
