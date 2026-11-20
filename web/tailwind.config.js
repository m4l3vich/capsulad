import daisyui from 'daisyui'
import { themes } from './theme.config.js'

// One appliance console with two palettes: the original light neutral surfaces
// and a dark counterpart for a dim room. Both are local CSS; no external font
// or image requests, because the daemon serves a restrictive CSP.
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
    themes
  }
}