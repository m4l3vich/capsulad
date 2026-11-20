// The two appliance themes. Shared by the SPA build and the settings-document
// kit build so a settings frame resolves exactly the same palette as the page
// that hosts it. daisyUI emits the first entry as `:root` and every entry in
// its `[data-theme=…]` form; the SPA boot script and the frame bridge set that
// attribute before first paint.
//
// Colours stay within the daisyUI variable set: no component styles, no
// external fonts, no images. The light theme is the original console palette;
// the dark theme keeps base-content at roughly 13:1 against base-100 and every
// primary/success/warning/error pairing above 4.5:1 for its content colour.

export const LIGHT_THEME = 'capsulad'
export const DARK_THEME = 'capsulad-dark'

const rounded = {
  '--rounded-box': '0.75rem',
  '--rounded-btn': '0.5rem',
  '--rounded-badge': '9999px',
  '--animation-btn': '0.15s',
  '--animation-input': '0.15s',
  '--btn-focus-scale': '0.98',
  '--border-btn': '1px',
  '--tab-border': '1px',
  '--tab-radius': '0.5rem'
}

export const themes = [
  {
    [LIGHT_THEME]: {
      primary: '#1d4ed8',
      'primary-content': '#eff6ff',
      secondary: '#475569',
      'secondary-content': '#f8fafc',
      accent: '#0284c7',
      'accent-content': '#f0f9ff',
      neutral: '#1f2937',
      'neutral-content': '#f8fafc',
      'base-100': '#f8fafc',
      'base-200': '#f1f5f9',
      'base-300': '#e2e8f0',
      'base-content': '#0f172a',
      info: '#0369a1',
      'info-content': '#f0f9ff',
      success: '#15803d',
      'success-content': '#f0fdf4',
      warning: '#b45309',
      'warning-content': '#fffbeb',
      error: '#b91c1c',
      'error-content': '#fef2f2',
      ...rounded
    }
  },
  {
    [DARK_THEME]: {
      primary: '#60a5fa',
      'primary-content': '#082f49',
      secondary: '#94a3b8',
      'secondary-content': '#0f172a',
      accent: '#38bdf8',
      'accent-content': '#082f49',
      neutral: '#020617',
      'neutral-content': '#e2e8f0',
      'base-100': '#0f172a',
      'base-200': '#0b1220',
      'base-300': '#26334a',
      'base-content': '#e2e8f0',
      info: '#38bdf8',
      'info-content': '#082f49',
      success: '#4ade80',
      'success-content': '#052e16',
      warning: '#fbbf24',
      'warning-content': '#422006',
      error: '#f87171',
      'error-content': '#450a0a',
      ...rounded
    }
  }
]