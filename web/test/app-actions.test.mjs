import test from 'node:test'
import assert from 'node:assert/strict'
import {
  coldPlayLaunch,
  hasLiveRun,
  hasReadyRun,
  isColdPlay,
  isReadyRun
} from '../src/lib/appActions.js'

// The app page must not send a declared action to a stopped app: the daemon
// answers FAILED_PRECONDITION and never queues it. Only the shipped radio's
// Play is a launch with its saved station; other apps' actions stay disabled.

const play = { actionId: 'play' }
const radio = {
  appId: 'org.capsulad.radio',
  launchArguments: [{ name: 'station', required: false }]
}
const settings = [{ key: 'station', type: 'INTEGER', defaultValueJson: '1' }]
const stored = [{ key: 'station', valueJson: '2', hasValue: true }]
const stopped = []
const ready = [{ state: 'APP_READY' }]
const starting = [{ state: 'APP_STARTING' }]

test('run readiness accepts the proto state shapes', () => {
  assert.equal(isReadyRun({ state: 'APP_READY' }), true)
  assert.equal(isReadyRun({ state: 'APP_RUN_STATE_READY' }), true)
  assert.equal(isReadyRun({ state: 'APP_STARTING' }), false)
  assert.equal(isReadyRun({}), false)
  assert.equal(hasReadyRun(ready), true)
  assert.equal(hasReadyRun(starting), false)
  assert.equal(hasLiveRun(starting), true)
  assert.equal(hasLiveRun(stopped), false)
})

test('the radio stopped Play launches with the saved station', () => {
  assert.deepEqual(coldPlayLaunch(play, radio, stopped, settings, stored), {
    arguments: [{ name: 'station', value: '2' }]
  })
})

test('the setting default seeds a launch before a snapshot arrives', () => {
  assert.deepEqual(coldPlayLaunch(play, radio, stopped, settings, []), {
    arguments: [{ name: 'station', value: '1' }]
  })
})

test('a stored value wins over the schema default', () => {
  assert.deepEqual(
    coldPlayLaunch(play, radio, stopped, settings, [{ key: 'station', valueJson: '"Tone B"', hasValue: true }]),
    { arguments: [{ name: 'station', value: 'Tone B' }] }
  )
})

test('only the shipped radio contract is a cold Play', () => {
  const other = {
    appId: 'example.player',
    launchArguments: [{ name: 'station' }]
  }
  assert.equal(isColdPlay(play, other, stopped), false)
  assert.equal(coldPlayLaunch(play, other, stopped, settings, stored), null)
  assert.equal(isColdPlay({ actionId: 'play' }, { appId: 'org.capsulad.radio' }, stopped), false)
  assert.equal(isColdPlay({ actionId: 'pause' }, radio, stopped), false)
  assert.equal(coldPlayLaunch({ actionId: 'pause' }, radio, stopped, settings, stored), null)
})

test('a missing or empty saved station blocks the launch with an explanation', () => {
  assert.deepEqual(coldPlayLaunch(play, radio, stopped, [], []), {
    error: 'The saved station setting is unavailable; check the application settings.'
  })
  assert.deepEqual(
    coldPlayLaunch(play, radio, stopped, settings, [{ key: 'station', valueJson: '""', hasValue: true }]),
    { error: 'The saved station selection is unavailable; choose a station in the application settings.' }
  )
})

test('a live run is never the cold Play path', () => {
  assert.equal(isColdPlay(play, radio, ready), false)
  assert.equal(coldPlayLaunch(play, radio, ready, settings, stored), null)
  // A starting or stopping run is live but cannot accept actions; launching
  // again would race that run, so the caller must wait for it.
  assert.equal(isColdPlay(play, radio, starting), false)
  assert.equal(coldPlayLaunch(play, radio, starting, settings, stored), null)
})
