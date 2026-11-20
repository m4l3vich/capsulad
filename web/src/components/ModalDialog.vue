<script setup>
import { onMounted, onUnmounted, ref } from 'vue'

const props = defineProps({
  title: { type: String, default: 'Dialog' },
  // Width class for the panel; owners pass a narrower token for short content.
  maxWidth: { type: String, default: 'max-w-4xl' },
  // Optional veto for dismiss attempts (X, Escape, backdrop). Returning false
  // keeps the dialog open so the owner can ask about unsaved work.
  beforeClose: { type: Function, default: null }
})

const emit = defineEmits(['close'])
const dialog = ref(null)
const titleId = `capsulad-dialog-${Math.random().toString(36).slice(2, 10)}`

onMounted(() => {
  dialog.value?.showModal()
})

onUnmounted(() => {
  if (dialog.value?.open) dialog.value.close()
})

function close() {
  if (props.beforeClose && props.beforeClose() === false) return
  if (dialog.value?.open) dialog.value.close()
  emit('close')
}
</script>

<template>
  <dialog ref="dialog" class="modal" :aria-labelledby="titleId" @cancel.prevent="close">
    <div
      class="modal-box flex max-h-[85vh] w-[calc(100vw-1.5rem)] flex-col overflow-hidden p-0"
      :class="maxWidth"
    >
      <div class="flex shrink-0 flex-wrap items-start justify-between gap-x-3 gap-y-2 border-b border-base-300 p-4 pb-3">
        <h2 :id="titleId" class="min-w-0 flex-1 break-words text-lg font-semibold leading-tight">{{ title }}</h2>
        <div class="flex shrink-0 items-center gap-1">
          <slot name="actions" />
          <button type="button" class="btn btn-sm btn-ghost btn-square -mr-1 -mt-1" aria-label="Close dialog" @click="close">
            <svg aria-hidden="true" viewBox="0 0 20 20" class="h-4 w-4" fill="none" stroke="currentColor" stroke-width="2">
              <path d="M5 5l10 10M15 5L5 15" stroke-linecap="round" />
            </svg>
          </button>
        </div>
      </div>
      <div class="min-h-0 flex-1 overflow-y-auto p-4">
        <slot />
      </div>
      <div v-if="$slots.footer" class="modal-action m-0 shrink-0 border-t border-base-300 p-4">
        <slot name="footer" />
      </div>
    </div>
    <div class="modal-backdrop" @click="close"></div>
  </dialog>
</template>
