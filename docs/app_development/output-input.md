# Output and input

Apps publish output through **presentations** and receive physical input through **captures**.
Both are runtime objects scoped to one app run: there are no declared output ports, stable
output IDs or per-app sockets. The method index is in [services.md](services.md); this page is
the behavioral contract.

| Resource | Kind | Routing | Who may present |
|---|---|---|---|
| `display.segment.front` | segment display | default app plus captures | any app |
| `light.ring.main` | LED ring | default app plus captures | any app |
| `touch.panel.main` | five-key panel | captures only | no presentations |
| `role.voice_assistant` | assistant role | captures only | no presentations |

Discover what the device actually offers with `DeviceService.ListResources`. A resource that is
absent has no backend; do not assume a display, ring or panel exists.

## Presentation stream

`PresentationService.Open` is bidirectional. The first request **must** be `OpenPresentation`;
the daemon answers with `PresentationReady` and then `PresentationVisibility` events. Frames
replace the presentation's content in place; they do not create new resources.

```text
→ PresentationRequest{open:{resource_id}}
← PresentationEvent{ready:{presentation_id, resource_id, has_valid_frame}}
→ PresentationRequest{frame:{…}}           (repeatable)
← PresentationEvent{ready:{…, has_valid_frame:true}}   first valid content
← PresentationEvent{visibility:{visible, reason}}      selection changes
→ PresentationRequest{close:{}}            normal completion
```

| Request / event | Fields | Rules |
|---|---|---|
| `OpenPresentation` | `resource_id` | Required. Only a segment or ring resource accepts it. |
| `PresentationReady` | `presentation_id`, `resource_id`, `has_valid_frame` | Sent immediately after open, before any content. `presentation_id` is runtime-only. |
| `PresentationFrame` | one of `segment`, `ring`, `ring_effect`, `segment_face`, `blank` | Content must match the resource kind. |
| `ClosePresentation` | — | Releases the presentation and every capture that references it. |
| `PresentationVisibility` | `visible`, `reason` | `reason` is diagnostic: `routing: <cause>`, `closed`, `already selected`. |

### Frame payloads

| Payload | Fields | Validation |
|---|---|---|
| `SegmentFrame` | `cells[{mask, decimal_point}]`, `colon` | `mask` must fit 16 bits. Each cell is one digit, left to right; bit `i` selects segment `i`, and `0x7f` lights the whole digit. The Mini has four digits; extra cells are ignored and missing cells are dark. |
| `RingFrame` | `pixels[{red, green, blue}]` | Each channel is clamped to 0–255. The Mini has 12 pixels; extra pixels are dropped and missing pixels stay dark. |
| `RingEffect` | `effect_id`, `arguments[{name, value}]` | The ID must be a built-in ring effect returned by `ListPresets`; arguments are validated against its typed parameters. |
| `SegmentFace` | `face_id`, `arguments` | The ID must be a built-in segment face returned by `ListPresets`. |
| `BlankPresentation` | — | Explicit blank. It counts as valid content. |

`PresetValue` is a oneof of `text`, `integer`, `number`, `boolean` or `color`. Unknown effects,
faces or parameters are `INVALID_ARGUMENT` and end the stream.

### Readiness and lifetime

- A presentation becomes **eligible** for routing when it receives its first valid content,
  including an explicit blank. Before that it exists but is never selected.
- `has_valid_frame` is `false` in the first `ready` event and `true` after the first content.
- Content changes inside one presentation keep the same `presentation_id` and the same captures.
- An app should keep one baseline presentation per resource. Extra eligible presentations from
  the same app are not selected separately and are reported as a contract warning.
- Closing the stream, sending `ClosePresentation`, or the process exiting destroys the
  presentation, releases its captures and removes it from routing.
- Open one stream per presentation. A second `open` on an established stream is
  `INVALID_ARGUMENT`.

### Complete stream example

Four digits showing `12:34` on a configured default display app:

```json
→ {"open":{"resourceId":"display.segment.front"}}
← {"ready":{"presentationId":"prs_9f2c1a7d4b5e6f708192a3b4c5","resourceId":"display.segment.front","hasValidFrame":false}}
→ {"frame":{"segment":{"cells":[{"mask":6,"decimalPoint":false},{"mask":91,"decimalPoint":false},{"mask":79,"decimalPoint":false},{"mask":102,"decimalPoint":false}],"colon":true}}}
← {"ready":{"presentationId":"prs_9f2c1a7d4b5e6f708192a3b4c5","resourceId":"display.segment.front","hasValidFrame":true}}
← {"visibility":{"visible":true,"reason":"routing: default changed"}}
→ {"frame":{"blank":{}}}
→ {"close":{}}
```

A ring frame and a built-in effect on the same presentation lifetime:

```json
→ {"frame":{"ring":{"pixels":[{"red":255,"green":0,"blue":0},{"red":0,"green":128,"blue":255}]}}}
→ {"frame":{"ringEffect":{"effectId":"system.ring.spinner","arguments":[{"name":"color","value":{"color":{"red":0,"green":255,"blue":128}}},{"name":"speed","value":{"number":1.5}}]}}}
```

## Captures and routing priority

A capture is a **temporary routing selection**, not a permission. `RoutingService.Capture` is a
server stream whose first message is the request; the daemon then streams events until the
stream is cancelled, the capture is released, or its presentation/run disappears.

| Request field | Meaning |
|---|---|
| `resource_id` | Required. Segment, ring, touch or assistant role. |
| `presentation_id` | Required for segment/ring and must be owned by the caller's run. Must be empty for touch and assistant role. |
| `touch_mode` | Required and non-`TOUCH_CAPTURE_MODE_UNSPECIFIED` for touch; must be `TOUCH_CAPTURE_MODE_UNSPECIFIED` (or absent) for every other resource. |

```json
→ {"resourceId":"display.segment.front","presentationId":"prs_9f2c1a7d4b5e6f708192a3b4c5"}
← {"granted":{"capture":{"captureId":"cap_4c1d…","resourceId":"display.segment.front","appId":"org.example.weather","runId":"run_…","presentationId":"prs_9f2c1a7d4b5e6f708192a3b4c5","acquiredMonotonicNs":"18432000000","selected":true,"ready":true}}}
← {"changed":{"selected":true,"reason":"granted"}}
← {"changed":{"selected":false,"reason":"capture released"}}
```

`CaptureInfo` fields: `capture_id`, `resource_id`, `app_id`, `run_id`, `presentation_id`,
`acquired_monotonic_ns`, `selected`, `ready`, `touch_mode`. `ready` is true only when the
referenced presentation has valid content.

Selection rules for display and ring, in priority order:

1. The **newest acquired eligible live capture** wins. A capture whose presentation has no valid
   content is skipped; an older ready capture stays selected.
2. With no eligible capture, the configured default app's baseline presentation wins. Only the
   default app's earliest eligible presentation is used as its baseline.
3. With no default, a ready framework fallback presentation owned by `capsula.system` wins (for
   example a volume overlay).
4. Otherwise nothing is selected (`selected_app_id` empty) and the resource is dark.

For touch and the assistant role the **newest capture** wins; with no capture, the framework
keeps the panel (`selected_capture_id` empty).

`RoutingState` reports `resource_id`, `configured_default_app_id`, `selected_presentation_id`,
`selected_app_id`, `captures` in acquisition order and a `revision`. It has no reason field;
`CaptureSelectionChanged.reason` is the transition cause and is diagnostic only. Observed
reasons include `granted`, `capture acquired`, `capture released`, `presentation closed`,
`app run ended` and `default changed`.

Release rules:

- `ReleaseCapture{capture_id}` releases only a capture owned by the caller. `released=false`
  means it was not yours or was already gone.
- Release order is not necessarily LIFO. Releasing a newer capture lets the previous eligible
  capture resume, and the daemon recomputes selection.
- Closing a presentation, ending its stream, a crashed run or an app stop releases dependent
  captures automatically.
- `ForceRelease` is administrator-only.
- A capture stream ending does **not** by itself mean the app is unselected forever; watch
  `changed` events or query `GetRouting`.

## Touch capture modes

| Mode | App receives | Framework keeps |
|---|---|---|
| `TOUCH_PARTIAL_GESTURES` | `play_pause` and `center` key events, completed swipes | volume up/down, microphone mute; circle inference is full-capture only |
| `TOUCH_FULL_RAW` | every key and inferred gesture; raw reports when the backend advertises coordinates | no volume, mute, media or assistant action |

The daemon refuses the mode a backend cannot serve:

- full capture when `full_panel_available` is false;
- partial capture when `partial_gestures_available` is false.

The Mini panel advertises `full_panel_available=true` and
`partial_gestures_available=true` with `coordinates_available=false`. Raw coordinates are never
fabricated: `coordinates_available=false` with a complete key stream is the normal Mini case.

## The five panel keys

| Key | Physical position | Framework action while it owns the key |
|---|---|---|
| `mic_toggle` | north | toggles microphone mute |
| `volume_up` | east | steps volume up and ramps while held |
| `play_pause` | south, the marked circle | talk key: tap toggles media, hold summons the assistant |
| `volume_down` | west | steps volume down and ramps while held |
| `center` | unmarked centre | dismisses an alert or assistant turn; drives a framework interaction such as the stopwatch |

Both capture modes receive key events as `KeyInput{key, pressed}`. Names are stable; never map
them from a physical key code yourself.

### Inferred gestures

The Mini has no coordinate panel. Swipes and, in full capture only, circles are recognized
from ordered multi-key contacts and delivered as `GestureInput{gesture_id, phase}` with phase `end`:

| Gesture | Recognition |
|---|---|
| `swipe_left` / `swipe_right` | Centre evidence: a `volume_up` (right) or `volume_down` (left) pressed **while the centre is still held** (no press-gap allowance, 1500 ms total budget) or within **700 ms after the centre is released**, or the opposite → centre → direction chain. Also a **full-panel crossing** with no centre between the edges: `volume_down` then `volume_up` (right) or `volume_up` then `volume_down` (left) with a press-to-press gap of at most **400 ms**. |
| `swipe_up` / `swipe_down` | Chain only on the north–south axis: `S,C,N` up / `N,C,S` down. A centre followed by `play_pause` or `mic_toggle` alone is not a swipe. |
| `circle_cw` / `circle_ccw` | One closed traversal of all four perimeter keys in a consistent rotational direction, each contact beginning while the previous is still down. **Full capture only.** |
| `tap`, `double_tap`, `hold`, `primary` | Generated by the logical primary control (the simulator's `primary` key and hardware that exposes one). The Mini's marked circle is `play_pause` and its unmarked centre is `center`, which arrive as keys. |

The panel is a `gpio-keys` node advertising only `EV_SYN|EV_KEY`, so there are no coordinates.
Raw `/dev/input/event0` captures on the Mini show a full swipe emits exactly one press + SYN +
release + SYN, and a swipe anchored at the physical centre can report the centre key. That centre
key is the gesture evidence a single contact adds; a whole-panel crossing that reports only its two
end-stop edges is recognized from their order and timing.

Nothing is deferred. Every key press and release acts on its own edge: a bare volume press steps
immediately, a release stops the ramp as before, and the contact that completes a recognized
gesture is the only one suppressed. A chain's leading opposite edge has therefore already produced
its own volume step or media toggle when the direction edge resolves the gesture. The same holds
for a full-panel crossing: its leading edge has already stepped when the second edge resolves it,
so the framework **reverts that one step** through the canonical master-volume path while delivering
the gesture; a slow opposite pair (> 400 ms) stays two steps and a lone tap stays one step with no
revert. An abandoned
sequence produces no gesture and suppresses nothing. Recognition is a bounded shadow of at most
five contacts; a press more than 250 ms after the previous one (400 ms for the opposite volume pair of a
full-panel crossing), or a sixth contact, abandons it and starts fresh — except an edge pressed while
the centre is still held, which stays a candidate up to the 1500 ms total budget. The other windows
are 1500 ms for a whole chain, 700 ms for the hold threshold and 400 ms for the full-panel crossing.
Circles are resolved only inside a full capture, so a partial capture receives its five contacts as
individual keys. A recognised horizontal swipe is sent to current media through the standard media
contract over the app bus: routed `MediaPlayerService.SkipNext`/`SkipPrevious` for an app provider,
or `BluetoothMediaService.Command` for the framework-owned Bluetooth source. There is no private
daemon→hardware transport path.

Raw reports are delivered only during a full capture, and only when the backend advertises
coordinates. `RawTouchReport` carries `contacts[{contact_id, x_normalized, y_normalized,
pressure, touching}]`, an optional opaque `backend_sample` with `backend_sample_format`, and
`coordinates_valid`. On the Mini no report is produced.

## Input stream

```text
InputService.WatchInput{resource_id:"touch.panel.main", capture_id:"cap_…"}
```

`capture_id` must be the caller's **currently selected** touch capture. A stale, foreign or
unselected capture is `FAILED_PRECONDITION`: `capture_id must be the caller's currently selected
touch capture`. There is no bypass subscription that exposes system volume or mute gestures.

Events carry `resource_id`, `monotonic_time_ns` and exactly one of:

```json
{"resourceId":"touch.panel.main","monotonicTimeNs":"9123456789","key":{"key":"center","pressed":true}}
{"resourceId":"touch.panel.main","monotonicTimeNs":"9125000000","gesture":{"gestureId":"swipe_left","phase":"end"}}
{"resourceId":"touch.panel.main","monotonicTimeNs":"9126000000","raw":{"contacts":[{"contactId":0,"xNormalized":0.42,"yNormalized":0.18,"pressure":0.6,"touching":true}],"coordinatesValid":true}}
```

The subscription ends when the capture stops being selected, is released, or its run ends.
Re-subscribe after acquiring a new capture.

## Dismissal rules an app must respect

While the framework owns the relevant physical control, it resolves framework state before any
app or media action:

1. A sounding framework alert (clock timer or alarm) takes the press. The alert is dismissed and
   the press, its release, hold and repeat are consumed, so one press cannot both silence the
   alert and summon the assistant.
2. Otherwise, an assistant turn in progress is dismissed and the press is consumed.
3. Otherwise, a running framework interaction (the stopwatch) owns the button and receives its
   tap, double tap or hold.
4. Otherwise the press becomes an assistant activation, media action or volume step.

`volume_up`, `volume_down` and `mic_toggle` never dismiss an alert; they keep their safety
behavior.

Consequences for an app holding a capture:

- Once your capture is selected, the framework stops acting on the controls your mode covers.
  With a partial capture, `play_pause` and `center` belong to you, so the framework can no longer
  dismiss its own alert or assistant turn with them. With a full capture, no system action runs
  at all.
- A gesture suppresses only the contact that completes it. Because nothing is deferred, the
  leading edge of an opposite → centre → direction chain has already fired its own key action
  before you receive the gesture event; do not expect a gesture to retract a key you already got.
- A press the framework consumed before your capture is selected never reaches you, and its
  release is swallowed; do not wait for a matching pair.
- If your UI changes what the panel means, provide an explicit way to dismiss or exit, and
  release the capture when you no longer need it. A capture is not an indefinite lock: the user's
  controls return when you release, close the presentation, or your run ends.
- Never synthesize or replay input events, and never assume raw coordinates exist.

## Limits

| Limit | Current behavior |
|---|---|
| Several presentations per app | Allowed, but only the earliest eligible one is a baseline; the daemon logs a contract warning. |
| Unready capture | Acquired, listed with `ready=false`, never selected until content arrives. |
| Capture of a foreign presentation | `INVALID_ARGUMENT`; presentations and captures are run-scoped. |
| Touch capture without a mode | `INVALID_ARGUMENT`. |
| Full capture on a key-only backend | Supported (every key and inferred gesture); raw reports are absent. |
| Raw coordinates on the Mini | Not available and never invented. |
| Input bypass | Not available; `WatchInput` requires the selected capture. |
| Force release | Administrator only. |
| Retroactive gesture | A capture that becomes selected mid-sequence cancels the pending sequence; no gesture is delivered for it, and keys already resolved on their own edges stay resolved (nothing was deferred). |