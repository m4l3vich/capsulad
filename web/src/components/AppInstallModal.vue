<script setup>
import ModalDialog from '../components/ModalDialog.vue'

const emit = defineEmits(['close'])
</script>

<template>
  <ModalDialog max-width="max-w-xl" title="Install an app" @close="emit('close')">
    <div class="border rounded border-base-300 p-3 flex justify-evenly">
      <!-- TODO: show free disk space -->
      <div class="flex flex-col justify-between">
        <div>
          <dt class="text-xs uppercase tracking-wide text-base-content/60">Disk space free</dt>
          <dd class="data-value">{{ 'TODO' }}</dd>
        </div>
        <div>
          <dt class="text-xs uppercase tracking-wide text-base-content/60">Disk space used</dt>
          <dd class="data-value">{{ 'TODO' }} ({{ 'TODO' }}%)</dd>
        </div>
      </div>

      <!-- TODO: <75% green/success; 75%-90% orange/warning; 90+ red/error -->
      <div class="radial-progress" style="--value:70;" aria-valuenow="70" role="progressbar">
        70%<br/>
        <span class="text-xs uppercase tracking-wide text-base-content/60">used</span>
      </div>
    </div>

    <p class="text-sm mt-2">
      Choose a .tar.gz app package file or enter a URL to install the app.
    </p>

    <!-- TODO: draggable zone and an url input + actual app installation flow -->
  </ModalDialog>
</template>
