<script setup>
import 'material-symbols'
defineProps({ size: Number })
</script>

<template>
  <span class="material-symbols-outlined" :style="{ fontSize: size }"><slot/></span>
</template>