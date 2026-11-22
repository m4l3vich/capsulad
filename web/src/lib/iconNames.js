// Icon names rendered through the Material Symbols Rounded ligature font.
//
// The font shipped in src/assets is a subset generated from the pinned
// material-symbols package. Every name used by <Icon> must exist in that
// package: scripts/build-icon-font.py fails on an unknown name and
// scripts/check-icon-font.mjs keeps the checked-in subset and these lists in
// sync. Add a name here only when the 0.48.3 font actually provides it.

// Kind drives the daisyUI alert tone, the announced politeness and the glyph.
export const TOAST_ICONS = Object.freeze({
  success: 'check_circle',
  info: 'info',
  warning: 'warning',
  error: 'error'
})

// Wi-Fi signal strength, strongest to weakest. The unknown case also covers a
// missing or zero dBm reading.
export const WIFI_SIGNAL_ICONS = Object.freeze({
  unknown: 'signal_wifi_0_bar',
  strong: 'signal_wifi_4_bar',
  fair: 'network_wifi_3_bar',
  weak: 'network_wifi_2_bar',
  veryWeak: 'network_wifi_1_bar'
})

// Icons for the settings section navigation and search filter.
export const SETTINGS_SECTION_ICONS = Object.freeze({
  voiceAssistant: 'robot_2',
  wakeword: 'record_voice_over',
  // The pinned 0.48.3 font has no `fluorescent` glyph (Material added it in a
  // later release). `light_mode` is the nearest display/brightness symbol and
  // renders as a solid glyph at FILL 1.
  displayRing: 'light_mode',
  microphone: 'mic',
  bluetooth: 'bluetooth',
  wifi: 'wifi',
  device: 'schedule'
})

export const THEME_ICONS = Object.freeze({
  light: 'light_mode',
  dark: 'dark_mode',
  system: 'devices'
})
