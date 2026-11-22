<script setup>
import '../assets/material-symbols-rounded.css'
defineProps({ size: String })
</script>

<template>
  <span class="material-symbols-rounded" :style="{ fontSize: size }"><slot/></span>
</template>

<style lang="css">
.material-symbols-rounded {
  font-variation-settings: 'FILL' 1, 'wght' 400, 'GRAD' 0;
  user-select: none;
}
</style>