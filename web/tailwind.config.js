import daisyui from 'daisyui'

// One appliance console theme: light neutral surfaces, a deep blue primary and
// enough contrast for a kitchen/desk display. No external font or image
// requests, because the daemon serves a restrictive CSP.
export default {
  content: ['./index.html', './src/**/*.{vue,js}'],
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
    themes: [
      {
        capsulad: {
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
      }
    ]
  }
}
