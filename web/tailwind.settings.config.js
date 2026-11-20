import daisyui from 'daisyui'
import { fileURLToPath } from 'node:url'
import { themes } from './theme.config.js'

const appDocs = fileURLToPath(new URL('../internal/appui/assets/*.html', import.meta.url))
const packagedDocs = fileURLToPath(new URL('../apps/*/ui/settings.html', import.meta.url))

// Classes an application settings document may use. There is no Node build on
// the speaker, so this list — not the content scan — is the stable contract for
// packaged apps: every entry is emitted even when no checked-in document uses
// it. Keep it in step with docs/app_development/settings-actions.md.
const componentClasses = [
  // Layout and surfaces
  'card', 'card-body', 'card-title', 'card-actions', 'card-compact',
  'divider', 'divider-horizontal',
  // Actions
  'btn', 'btn-primary', 'btn-secondary', 'btn-accent', 'btn-ghost', 'btn-outline',
  'btn-error', 'btn-success', 'btn-active', 'btn-disabled', 'btn-xs', 'btn-sm',
  'btn-lg', 'btn-block', 'btn-circle', 'btn-square', 'btn-link',
  'loading', 'loading-spinner', 'loading-xs', 'loading-sm',
  // Form controls
  'form-control', 'label', 'label-text', 'label-text-alt',
  'input', 'input-bordered', 'input-sm', 'input-primary', 'input-error', 'input-ghost',
  'select', 'select-bordered', 'select-sm', 'select-primary', 'select-error',
  'textarea', 'textarea-bordered', 'textarea-sm', 'textarea-primary', 'textarea-error',
  'checkbox', 'checkbox-primary', 'checkbox-sm', 'checkbox-xs',
  'toggle', 'toggle-primary', 'toggle-sm',
  'range', 'range-primary', 'range-sm', 'range-xs',
  'radio', 'radio-primary', 'radio-sm',
  // Feedback
  'alert', 'alert-info', 'alert-success', 'alert-warning', 'alert-error',
  'badge', 'badge-primary', 'badge-secondary', 'badge-accent', 'badge-ghost',
  'badge-outline', 'badge-success', 'badge-warning', 'badge-error', 'badge-sm', 'badge-lg',
  'link', 'link-primary',
  // Data
  'table', 'table-sm', 'table-zebra', 'table-pin-rows',
  'join', 'join-item'
]

const utilityClasses = [
  'flex', 'inline-flex', 'grid', 'hidden', 'block', 'inline-block',
  'flex-col', 'flex-row', 'flex-wrap', 'flex-nowrap', 'flex-1', 'grow', 'shrink-0', 'min-w-0',
  'items-center', 'items-start', 'items-end', 'items-baseline', 'items-stretch',
  'justify-between', 'justify-start', 'justify-end', 'justify-center', 'self-start',
  'gap-1', 'gap-1.5', 'gap-2', 'gap-2.5', 'gap-3', 'gap-3.5', 'gap-4', 'gap-5', 'gap-6',
  'grid-cols-1', 'grid-cols-2', 'grid-cols-3', 'grid-cols-4',
  'sm:grid-cols-2', 'sm:flex-row', 'sm:w-auto', 'md:grid-cols-2',
  'w-full', 'w-auto', 'w-16', 'w-24', 'w-28', 'w-32', 'w-36', 'h-auto', 'h-8', 'h-9',
  'h-10', 'h-11', 'h-12', 'min-h-9', 'min-w-36', 'max-w-full', 'max-w-prose', 'max-w-2xl',
  'p-2', 'p-2.5', 'p-3', 'p-4', 'px-2', 'px-3', 'py-1', 'py-1.5', 'py-2',
  'mt-1', 'mt-2', 'mt-3', 'mt-4', 'mt-5', 'mb-1', 'mb-2', 'mb-3', 'ml-auto', 'mr-auto',
  'space-y-1', 'space-y-2', 'space-y-3',
  'text-xs', 'text-sm', 'text-base', 'text-lg', 'text-xl',
  'font-medium', 'font-semibold', 'font-bold', 'font-mono',
  'uppercase', 'tracking-wide', 'normal-case', 'text-left', 'text-center', 'text-right',
  'break-words', 'break-all', 'truncate', 'whitespace-nowrap', 'tabular-nums',
  'leading-snug', 'leading-relaxed',
  'text-base-content', 'text-base-content/60', 'text-base-content/70',
  'text-primary', 'text-primary-content', 'text-error', 'text-success', 'text-warning', 'text-info',
  'bg-base-100', 'bg-base-200', 'bg-base-300', 'bg-primary', 'bg-transparent',
  'border', 'border-0', 'border-2', 'border-base-300', 'border-error', 'border-success',
  'rounded', 'rounded-box', 'rounded-lg', 'rounded-full', 'divide-y', 'divide-base-300',
  'overflow-x-auto', 'overflow-y-auto', 'cursor-pointer', 'select-none',
  'sr-only', 'relative', 'absolute', 'sticky', 'top-0', 'z-10',
  'opacity-50', 'opacity-60', 'opacity-70'
]

export default {
  content: [appDocs, packagedDocs],
  theme: {
    extend: {
      fontFamily: {
        mono: ['ui-monospace', 'SFMono-Regular', 'Menlo', 'Consolas', 'monospace']
      }
    }
  },
  plugins: [daisyui],
  daisyui: {
    logs: false,
    themes
  },
  safelist: [...componentClasses, ...utilityClasses]
}