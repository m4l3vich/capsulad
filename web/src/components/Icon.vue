<script setup>
import 'material-symbols'
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