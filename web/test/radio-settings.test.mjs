import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import vm from 'node:vm'

// Queued-save regression for the Internet Radio settings document.
//
// The real bridge (internal/appui/assets/bridge.js) is injected into every
// settings frame and the radio page script runs against it. The host serializes
// patches, answers each with a result carrying the requestId patch() returned,
// and only then posts the authoritative state. The page must acknowledge a
// draft from that correlated pair, never from an uncorrelated state that
// happens to equal the textarea (a repeated initial value), and never from an
// earlier save's success.

const bridgeSource = fs.readFileSync(
  new URL('../../internal/appui/assets/bridge.js', import.meta.url),
  'utf8'
)
const documentHTML = fs.readFileSync(
  new URL('../../apps/radio/ui/settings.html', import.meta.url),
  'utf8'
)
const inlineScripts = [...documentHTML.matchAll(/<script(?:\s[^>]*)?>([\s\S]*?)<\/script>/g)]
assert.ok(inlineScripts.length > 0, 'the packaged settings document has no script')
const pageSource = inlineScripts[inlineScripts.length - 1][1]

const CHANNEL = '0123456789abcdef0123456789abcdef'
const PREFIX = 'capsulad.settings-ui/1:'
const STATIONS = 'stations'

function makeElement(tag) {
  const listeners = {}
  const element = {
    tagName: String(tag).toUpperCase(),
    children: [],
    parentNode: null,
    className: '',
    style: {},
    attributes: {},
    textContent: '',
    value: '',
    checked: false,
    setAttribute(name, value) {
      this.attributes[name] = String(value)
    },
    getAttribute(name) {
      return this.attributes[name]
    },
    appendChild(child) {
      child.parentNode = element
      element.children.push(child)
      return child
    },
    replaceChildren(...children) {
      element.children.length = 0
      for (const child of children) element.appendChild(child)
    },
    addEventListener(type, fn) {
      ;(listeners[type] ||= []).push(fn)
    },
    dispatch(type, extra) {
      for (const fn of (listeners[type] || []).slice()) {
        fn({ type, target: element, preventDefault() {}, ...extra })
      }
    }
  }
  Object.defineProperty(element, 'classList', {
    get() {
      const set = () => new Set(String(element.className || '').split(/\s+/).filter(Boolean))
      return {
        add(...values) {
          const current = set()
          values.forEach((value) => current.add(value))
          element.className = [...current].join(' ')
        },
        remove(...values) {
          const current = set()
          values.forEach((value) => current.delete(value))
          element.className = [...current].join(' ')
        },
        contains(value) {
          return set().has(value)
        }
      }
    }
  })
  return element
}

function mount() {
  const ids = ['picker', 'stations', 'autoplay', 'always-display', 'save', 'status', 'state-pill', 'selection-note']
  const elements = new Map(ids.map((id) => [id, makeElement(id === 'stations' ? 'textarea' : 'div')]))
  const document = {
    documentElement: { scrollHeight: 240 },
    body: { scrollHeight: 240 },
    getElementById(id) {
      return elements.get(id) || null
    },
    createElement(tag) {
      return makeElement(tag)
    }
  }
  const posted = []
  const windowListeners = {}
  const parent = {
    postMessage(message) {
      posted.push(message)
    }
  }
  const sandbox = {
    parent,
    document,
    console,
    addEventListener(type, fn) {
      ;(windowListeners[type] ||= []).push(fn)
    }
  }
  const context = vm.createContext(sandbox)
  vm.runInContext('globalThis.window = globalThis;', context)
  vm.runInContext(bridgeSource, context)
  vm.runInContext(pageSource, context)

  function postFromHost(data) {
    for (const fn of (windowListeners.message || []).slice()) fn({ source: parent, data })
  }

  function values({ stations, station = 1, autoplay = false, always_display: alwaysDisplay = false }) {
    return [
      { key: STATIONS, valueJson: JSON.stringify(stations), hasValue: true },
      { key: 'station', valueJson: String(station), hasValue: true },
      { key: 'autoplay', valueJson: autoplay ? 'true' : 'false', hasValue: true },
      { key: 'always_display', valueJson: alwaysDisplay ? 'true' : 'false', hasValue: true }
    ]
  }

  function init(initial) {
    postFromHost({
      type: PREFIX + 'init',
      channel: CHANNEL,
      appId: 'org.capsulad.radio',
      apiVersion: 1,
      schema: { settings: [{ key: STATIONS }, { key: 'station' }, { key: 'autoplay' }, { key: 'always_display' }] },
      revision: initial.revision || 1,
      values: values(initial)
    })
  }

  // An authoritative snapshot arriving from the host or another writer.
  function state(initial) {
    postFromHost({
      type: PREFIX + 'state',
      channel: CHANNEL,
      appId: 'org.capsulad.radio',
      revision: initial.revision || 1,
      schema: { settings: [{ key: STATIONS }, { key: 'station' }, { key: 'autoplay' }, { key: 'always_display' }] },
      values: values(initial)
    })
  }

  function result(fields) {
    postFromHost({
      type: PREFIX + 'result',
      channel: CHANNEL,
      appId: 'org.capsulad.radio',
      requestId: fields.requestId,
      ok: !!fields.ok,
      revision: fields.revision || 0,
      message: fields.message || '',
      error: fields.error || ''
    })
  }

  // The host pumps one queued patch at a time: the correlated result, then
  // the authoritative snapshot for the same commit.
  function ack(requestId, committed, revision) {
    result({ requestId, ok: true, revision, message: 'Saved.' })
    state({ ...committed, revision })
  }

  function reject(requestId, committed, revision, error) {
    result({ requestId, ok: false, revision, error })
    state({ ...committed, revision })
  }

  function patches() {
    return posted
      .filter((message) => message.type === PREFIX + 'patch')
      // The bridge runs in another vm realm, so its arrays do not share this
      // realm's prototypes; normalize before deep-comparing.
      .map((message) => ({
        requestId: message.requestId,
        changes: JSON.parse(JSON.stringify(message.changes))
      }))
  }

  return {
    elements,
    init,
    state,
    ack,
    reject,
    patches,
    stations: elements.get('stations'),
    alwaysDisplay: elements.get('always-display'),
    status: elements.get('status'),
    save: elements.get('save'),
    type(text) {
      elements.get('stations').value = text
      elements.get('stations').dispatch('input')
    },
    clickSave() {
      elements.get('save').dispatch('click')
    },
    toggleAlwaysDisplay(checked) {
      const element = elements.get('always-display')
      element.checked = checked
      element.dispatch('change')
    }
  }
}

const SAVED0 = '# persisted\nOld | https://old.test/old.mp3'
const A_TEXT = '# draft A\nA | https://a.test/a.mp3'
const B_TEXT = '# draft B\nA | https://a.test/a.mp3\nB | https://b.test/b.mp3'
const D_TEXT = '# external\nD | https://d.test/d.mp3'

function stationsPatch(text) {
  return [{ key: STATIONS, valueJson: JSON.stringify(text) }]
}

test('a stale success cannot clear a newer queued draft', () => {
  const ui = mount()
  ui.init({ stations: SAVED0 })
  assert.equal(ui.stations.value, SAVED0)

  ui.type(A_TEXT)
  ui.clickSave()
  ui.type(B_TEXT)
  ui.clickSave()

  const sent = ui.patches()
  assert.deepEqual(sent.map((patch) => patch.requestId), [1, 2])
  assert.deepEqual(sent[0].changes, stationsPatch(A_TEXT))
  assert.deepEqual(sent[1].changes, stationsPatch(B_TEXT))

  // Save A commits: its result and its snapshot arrive while draft B is
  // visible and B is already queued.
  ui.ack(1, { stations: A_TEXT }, 2)
  assert.equal(ui.stations.value, B_TEXT, 'draft B must survive save A committing')

  // Save B is rejected: the draft and the error survive its correlated result
  // and its identical-value snapshot.
  ui.reject(2, { stations: A_TEXT }, 2, 'B rejected')
  assert.equal(ui.stations.value, B_TEXT, 'a rejected save must not discard its draft')
  assert.equal(ui.status.textContent, 'B rejected')

  // An unrelated writer must not overwrite the dirty draft either.
  ui.state({ stations: D_TEXT, revision: 3 })
  assert.equal(ui.stations.value, B_TEXT)

  // Resubmitting the draft and letting it commit acknowledges it; from then on
  // external snapshots are authoritative again.
  ui.clickSave()
  const resent = ui.patches().at(-1)
  assert.deepEqual(resent.changes, stationsPatch(B_TEXT))
  ui.ack(resent.requestId, { stations: B_TEXT }, 4)
  assert.equal(ui.stations.value, B_TEXT)

  ui.state({ stations: D_TEXT, revision: 5 })
  assert.equal(ui.stations.value, D_TEXT, 'a clean document adopts the authoritative snapshot')
})

test('a repeated initial value is not acknowledged by an uncorrelated state', () => {
  const ui = mount()
  // The persisted value is B; the user first queues an edit away from it, then
  // deliberately restores B and queues that save.
  ui.init({ stations: B_TEXT })

  ui.type(A_TEXT)
  ui.clickSave()
  ui.type(B_TEXT)
  ui.clickSave()
  assert.deepEqual(
    ui.patches().map((patch) => patch.requestId),
    [1, 2]
  )

  // Another writer echoes the initial B before save A commits. State equality
  // alone would treat this as B's acknowledgement; the correlated requestId
  // must not.
  ui.state({ stations: B_TEXT, revision: 2 })
  assert.equal(ui.stations.value, B_TEXT)

  // Save A commits: B is still queued, so its draft must not be replaced.
  ui.ack(1, { stations: A_TEXT }, 3)
  assert.equal(ui.stations.value, B_TEXT, 'save A must not overwrite the queued B draft')

  // Save B fails: the draft stays and the failure is reported.
  ui.reject(2, { stations: A_TEXT }, 3, 'B rejected')
  assert.equal(ui.stations.value, B_TEXT)
  assert.equal(ui.status.textContent, 'B rejected')

  ui.state({ stations: A_TEXT, revision: 4 })
  assert.equal(ui.stations.value, B_TEXT, 'dirty draft survives an external state')

  // A fresh save of the restored draft commits and acknowledges it.
  ui.clickSave()
  const retry = ui.patches().at(-1)
  ui.ack(retry.requestId, { stations: B_TEXT }, 5)
  assert.equal(ui.stations.value, B_TEXT)

  ui.state({ stations: A_TEXT, revision: 6 })
  assert.equal(ui.stations.value, A_TEXT)
})

test('the settings document exposes the canonical daisyUI controls', () => {
  assert.match(documentHTML, /id="stations"[^>]*class="textarea textarea-bordered[^"]*"/)
  assert.match(documentHTML, /id="autoplay"[^>]*class="checkbox"/)
  assert.match(documentHTML, /id="always-display"[^>]*class="checkbox"/)
  assert.match(documentHTML, /class="btn btn-primary"/)
  assert.match(documentHTML, /class="alert" id="status"/)
  assert.match(documentHTML, /id="status"[^>]*hidden/)
  assert.match(documentHTML, /status\.hidden = !text/)
  assert.match(documentHTML, /Always display station number/)
})

test('always display starts unchecked and follows the persisted boolean', () => {
  const ui = mount()
  ui.init({ stations: SAVED0 })
  assert.equal(ui.alwaysDisplay.checked, false)
  ui.state({ stations: SAVED0, always_display: true, revision: 2 })
  assert.equal(ui.alwaysDisplay.checked, true)
  ui.state({ stations: SAVED0, always_display: false, revision: 3 })
  assert.equal(ui.alwaysDisplay.checked, false)
})

test('always display persists independently of an unsaved station draft', () => {
  const ui = mount()
  ui.init({ stations: SAVED0 })
  ui.type(A_TEXT)
  assert.equal(ui.stations.value, A_TEXT)

  ui.toggleAlwaysDisplay(true)
  const toggle = ui.patches().at(-1)
  assert.deepEqual(toggle.changes, [{ key: 'always_display', valueJson: 'true' }])

  // The toggle's correlated result and the authoritative snapshot must not
  // submit, discard or overwrite the station-list draft.
  ui.ack(toggle.requestId, { stations: SAVED0, always_display: true }, 2)
  assert.equal(ui.stations.value, A_TEXT)
  assert.equal(ui.alwaysDisplay.checked, true)

  // Turning it back off is another independent setting write; the draft and
  // the other controls are untouched.
  ui.toggleAlwaysDisplay(false)
  const off = ui.patches().at(-1)
  assert.deepEqual(off.changes, [{ key: 'always_display', valueJson: 'false' }])
  ui.ack(off.requestId, { stations: SAVED0, always_display: false }, 3)
  assert.equal(ui.stations.value, A_TEXT)
  assert.equal(ui.alwaysDisplay.checked, false)
  assert.equal(ui.patches().length, 2)
})

test('a station save in flight does not swallow a new display preference', () => {
  const ui = mount()
  ui.init({ stations: SAVED0 })
  ui.type(A_TEXT)
  ui.clickSave()
  const save = ui.patches().at(-1)

  ui.toggleAlwaysDisplay(true)
  const toggle = ui.patches().at(-1)
  assert.deepEqual(toggle.changes, [{ key: 'always_display', valueJson: 'true' }])

  // Save A commits first, with the old display value. Its correlated result
  // clears the draft acknowledgement, and its snapshot is authoritative for
  // the station list; the display patch is still queued.
  ui.ack(save.requestId, { stations: A_TEXT, always_display: false }, 2)
  assert.equal(ui.stations.value, A_TEXT)

  // The display patch then commits: its value is applied without rewriting the
  // textarea draft.
  ui.ack(toggle.requestId, { stations: A_TEXT, always_display: true }, 3)
  assert.equal(ui.stations.value, A_TEXT)
  assert.equal(ui.alwaysDisplay.checked, true)
  assert.deepEqual(
    ui.patches().map((patch) => patch.changes),
    [stationsPatch(A_TEXT), [{ key: 'always_display', valueJson: 'true' }]]
  )
})
