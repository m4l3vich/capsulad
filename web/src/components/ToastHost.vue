<script setup>
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { toasts, dismissToast } from '../lib/toast.js'
import Icon from './Icon.vue'

// Kind drives the daisyUI alert tone, the announced politeness and the glyph.
// Errors and warnings interrupt; successes and plain info wait their turn.
const presentation = {
  success: { alert: 'alert-success', role: 'status', icon: 'check_circle' },
  info: { alert: 'alert-info', role: 'status', icon: 'info' },
  warning: { alert: 'alert-warning', role: 'alert', icon: 'warning' },
  error: { alert: 'alert-error', role: 'alert', icon: 'error' }
}

function look(toast) {
  return presentation[toast.kind] || presentation.info
}

// A modal <dialog> occupies the browser's top layer, which no z-index can paint
// over, so a merely fixed stack gets dimmed by the dialog backdrop. A manual
// popover is the only other way into that layer. "manual" keeps Escape and
// light-dismiss out of the stack's path (a dialog keeps its own Escape) and
// showPopover() never moves focus.
const stack = ref(null)
// Longer than the leave transition below, so the last toast finishes animating
// before the top-layer entry is removed.
const LEAVE_MS = 200

let hideTimer = null
let shown = false
// Cleared if the Popover API turns out to be unusable here; the element then
// falls back to the plain fixed stack.
let supported = true

function hasModalDialog() {
  return document.querySelector('dialog[open]') !== null
}

function openStack(raise) {
  const element = stack.value
  if (!element || !supported) return
  if (typeof element.showPopover !== 'function') return
  try {
    if (shown) {
      if (!raise) return
      // Re-entering the top layer is the only way above a dialog that opened
      // after the previous toast. Both calls run in this task, so the browser
      // paints no frame in between and nothing flickers.
      element.hidePopover()
    }
    element.showPopover()
    shown = true
  } catch (_) {
    // Popover support is present but this element cannot use it (detached, or
    // a partial implementation). Keep the toasts visible as a fixed stack.
    supported = false
    shown = false
    element.removeAttribute('popover')
  }
}

function hideStack() {
  if (!shown) return
  shown = false
  try {
    stack.value?.hidePopover()
  } catch (_) {
    supported = false
    stack.value?.removeAttribute('popover')
  }
}

function cancelHide() {
  if (hideTimer === null) return
  clearTimeout(hideTimer)
  hideTimer = null
}

function scheduleHide() {
  if (hideTimer !== null) return
  hideTimer = setTimeout(() => {
    hideTimer = null
    hideStack()
  }, LEAVE_MS)
}

// toasts is a reactive array, so this fires on additions, replacements
// (dedupe/id slots) and removals alike.
function sync() {
  if (toasts.length === 0) {
    scheduleHide()
    return
  }
  cancelHide()
  // Any change while a dialog is open may have left the stack underneath it.
  openStack(hasModalDialog())
}

// A toast can already be showing when a modal dialog opens; the stack is then
// still beneath the fresh dialog because nothing mutated the toast list. A
// capture-phase toggle sees every top-layer change, including the stack's own
// popover toggles, which are ignored by target. Re-raising on a modal toggle
// re-enters the layer without a further toggle event, so this cannot loop.
function onTopLayerToggle(event) {
  if (event.target === stack.value) return
  if (!toasts.length || !hasModalDialog()) return
  openStack(true)
}

watch(toasts, sync)
onMounted(() => {
  document.addEventListener('toggle', onTopLayerToggle, true)
  if (toasts.length > 0) sync()
})

onBeforeUnmount(() => {
  document.removeEventListener('toggle', onTopLayerToggle, true)
  cancelHide()
  hideStack()
})
</script>

<template>
  <div
    ref="stack"
    popover="manual"
    class="pointer-events-none fixed inset-x-0 bottom-0 top-auto z-[1000] m-0 w-full overflow-visible border-0 bg-transparent px-3 pb-[max(0.75rem,env(safe-area-inset-bottom))] pt-0 text-inherit sm:inset-x-auto sm:bottom-4 sm:right-4 sm:w-96 sm:max-w-[calc(100vw-2rem)] sm:px-0 sm:pb-4"
  >
    <TransitionGroup
      tag="div"
      name="toast"
      class="flex flex-col gap-2"
      role="region"
      aria-label="Notifications"
      aria-live="polite"
      aria-atomic="false"
    >
      <div
        v-for="toast in toasts"
        :key="toast.id"
        class="alert pointer-events-auto flex items-start gap-2 p-3 text-left shadow-lg sm:items-center"
        :class="look(toast).alert"
        :role="look(toast).role"
      >
        <Icon size="20px" class="mt-px shrink-0" aria-hidden="true">{{ look(toast).icon }}</Icon>
        <p class="min-w-0 flex-1 break-words text-sm leading-snug text-left">{{ toast.message }}</p>
        <button
          type="button"
          class="btn btn-ghost btn-xs btn-square -mr-1.5 -mt-0.5 shrink-0"
          aria-label="Dismiss notification"
          @click="dismissToast(toast.id)"
        >
          <svg aria-hidden="true" viewBox="0 0 20 20" class="h-3.5 w-3.5" fill="none" stroke="currentColor" stroke-width="2">
            <path d="M5 5l10 10M15 5L5 15" stroke-linecap="round" />
          </svg>
        </button>
      </div>
    </TransitionGroup>
  </div>
</template>

<style scoped>
.toast-enter-active,
.toast-leave-active {
  transition: opacity 160ms ease, transform 160ms ease;
}

.toast-enter-from,
.toast-leave-to {
  opacity: 0;
  transform: translateY(0.5rem);
}

.toast-move {
  transition: transform 160ms ease;
}

@media (prefers-reduced-motion: reduce) {
  .toast-enter-active,
  .toast-leave-active,
  .toast-move {
    transition: none;
  }
}
</style>
