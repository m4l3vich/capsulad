// Classic, render-blocking boot script served from `public/` so the theme is
// decided before the first paint and without an inline script (the daemon's
// CSP only allows `script-src 'self'`). It runs before the SPA module and
// before `style.css` is parsed, so a dark-mode browser never flashes the light
// palette. `src/lib/theme.js` owns every later change; the storage key and
// attribute names are the contract between the two.
//
// Deliberately ES5 and dependency-free: it is copied verbatim, not bundled.
;(function () {
  'use strict'
  var KEY = 'capsulad.theme'
  var LIGHT = 'capsulad'
  var DARK = 'capsulad-dark'
  var mode = 'system'
  try {
    var stored = window.localStorage.getItem(KEY)
    if (stored === 'light' || stored === 'dark' || stored === 'system') mode = stored
  } catch (_) {
    // Storage can be unavailable (private mode, blocked cookies). System is
    // the documented default and needs no persistence.
  }
  var dark = mode === 'dark'
  if (mode === 'system') {
    try {
      dark = !!(window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches)
    } catch (_) {
      dark = false
    }
  }
  var root = document.documentElement
  root.setAttribute('data-theme', dark ? DARK : LIGHT)
  root.setAttribute('data-theme-mode', mode)
  var themeColor = document.querySelector('meta[name="theme-color"]')
  if (themeColor) themeColor.setAttribute('content', dark ? '#0b1220' : '#f8fafc')
  var colorScheme = document.querySelector('meta[name="color-scheme"]')
  if (colorScheme) colorScheme.setAttribute('content', dark ? 'dark' : 'light')
})()