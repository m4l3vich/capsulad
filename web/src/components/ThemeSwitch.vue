<script setup>
import { THEME_MODES, setThemeMode, theme } from '../lib/theme.js'
</script>

<template>
  <div class="join shrink-0" role="group" aria-label="Color theme">
    <button
      v-for="option in THEME_MODES"
      :key="option.id"
      type="button"
      class="btn btn-xs join-item"
      :class="theme.mode === option.id ? 'btn-primary btn-active' : 'btn-ghost'"
      :aria-pressed="theme.mode === option.id"
      :title="`${option.label} appearance`"
      @click="setThemeMode(option.id)"
    >{{ option.label }}</button>
  </div>
</template>