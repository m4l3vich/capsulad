<script setup>
import { computed, onBeforeUnmount, useTemplateRef } from 'vue'
import { useI18n } from 'vue-i18n'
import { AVAILABLE_LOCALES, setLocale } from '../lib/i18n.js'

// Mirrors ThemeSwitch's dropdown; the summary carries the current locale's
// two-letter code instead of an icon so the active language is visible without
// opening the menu.
const { locale } = useI18n()

const currentLocale = computed(() =>
  AVAILABLE_LOCALES.find(entry => entry.id === locale.value) || AVAILABLE_LOCALES[0]
)

const details = useTemplateRef('dropdown')

function globalOnClick(e) {
  if (!details.value.contains(e.target)) {
    details.value.removeAttribute('open')
  }
}

function choose(id) {
  setLocale(id)
  details.value.removeAttribute('open')
}

document.addEventListener('click', globalOnClick)
onBeforeUnmount(() => {
  document.removeEventListener('click', globalOnClick)
})
</script>

<template>
  <details ref="dropdown" class="dropdown dropdown-end">
    <summary class="btn btn-sm btn-outline btn-square" :aria-label="$t('language.label')">
      {{ currentLocale.short }}
    </summary>
    <ul class="menu dropdown-content bg-base-100 rounded-box z-1 w-52 p-2 shadow-sm">
      <li class="text-xs p-2 text-base-content/50">{{ $t('language.label') }}</li>
      <li v-for="entry in AVAILABLE_LOCALES" :key="entry.id">
        <a @click="choose(entry.id)">
          {{ $t(entry.labelKey) }}
        </a>
      </li>
    </ul>
  </details>
</template>
