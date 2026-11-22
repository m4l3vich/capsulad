// Application action helpers.
//
// The framework only delivers a declared action to a ready run: triggering one
// for a stopped or starting app is FAILED_PRECONDITION and actions are never
// queued. The shipped Internet Radio package is the one app whose stopped Play
// is a public launch: its `play` action is the standard media Play and its
// `station` launch argument selects the saved station. No other app gets this
// fallback, and the separate Launch form draft is never an input.

import { safeJson, status } from './format.js'
import { t } from './i18n.js'

/** The explicit shipped contract for a stopped Play that is a launch. */
export const coldPlayContract = Object.freeze({
  appId: 'org.capsulad.radio',
  actionId: 'play',
  argument: 'station',
  setting: 'station'
})

/** True when one live run is ready to accept declared actions. */
export function isReadyRun(run) {
  const state = String(run?.state || '')
  return state === 'APP_READY' || state === 'APP_RUN_STATE_READY' || status(state) === 'ready'
}

export function hasReadyRun(runs) {
  return (runs || []).some(isReadyRun)
}

/** True while any run exists, including one that is still starting/stopping. */
export function hasLiveRun(runs) {
  return (runs || []).length > 0
}

/**
 * True when the action is the shipped radio's Play and the app has no live run.
 * The radio declares the `station` launch argument; an app that does not is
 * never treated as this contract, even if it also declares a `play` action.
 */
export function isColdPlay(action, app, runs) {
  if (!action || !app) return false
  if (app.appId !== coldPlayContract.appId || action.actionId !== coldPlayContract.actionId) return false
  if (hasLiveRun(runs)) return false
  return (app.launchArguments || []).some((arg) => arg.name === coldPlayContract.argument)
}

/**
 * Plan the public launch that stands in for the radio's Play on a stopped app.
 * The argument is seeded only from the saved setting of the same name (or the
 * setting's schema default); a value typed into the separate Launch form is
 * never consulted.
 *
 * Returns null when this is not the shipped radio cold-Play contract, `{error}`
 * when the saved selection cannot be resolved, and `{arguments}` otherwise.
 */
export function coldPlayLaunch(action, app, runs, schemaSettings, values) {
  if (!isColdPlay(action, app, runs)) return null
  const def = (schemaSettings || []).find((entry) => entry.key === coldPlayContract.setting)
  if (!def) {
    return { error: t('overview.stationSettingUnavailable') }
  }
  const state = (values || []).find((entry) => entry.key === coldPlayContract.setting)
  const value = String(settingValue(def, state) ?? '').trim()
  if (!value) {
    return { error: t('overview.stationSelectionUnavailable') }
  }
  return { arguments: [{ name: coldPlayContract.argument, value }] }
}

function settingValue(def, state) {
  if (def.type?.includes('BOOLEAN')) {
    if (!state?.hasValue) return safeJson(def.defaultValueJson) === 'true'
    return safeJson(state.valueJson) === 'true'
  }
  if (!state?.hasValue) return def.defaultValueJson ? safeJson(def.defaultValueJson) : ''
  return safeJson(state.valueJson)
}
