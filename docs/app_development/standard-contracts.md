# Standard contracts

Three public contracts are shared vocabulary between apps:

| Contract | Service | Exposed by |
|---|---|---|
| Clock | `capsulad.contracts.clock.v1.ClockService` | built-in app `capsulad.clock` |
| Ring | `capsulad.contracts.ring.v1.RingService` | built-in app `capsulad.ring` |
| Media | `capsulad.contracts.media.v1.MediaPlayerService` | any app that declares it; the daemon's Web UI also lists the connected Bluetooth source, which is framework-owned and has no app id |

They are ordinary app interfaces. There is no private path, no special token and no second
socket: a caller targets the app ID through `AppBusService.Call`, with the same deadlines,
cancellation and encoding rules as any other routed call. The daemon's Web UI is just another
caller.

Calling a standard contract:

```text
target_app_id = "capsulad.clock"
grpc_service  = "capsulad.contracts.clock.v1.ClockService"
rpc_name      = "GetState"
request       = serialized GetStateRequest (empty message)
```

```go
// Go: raw protobuf is always available, even without a descriptor.
// Call is (ctx, targetAppID, service, rpc, requestBytes, timeout); the last
// argument is a time.Duration budget, 0 for no deadline beyond the context.
reply, err := client.Call(ctx, "capsulad.clock",
    "capsulad.contracts.clock.v1.ClockService", "GetState", nil, 5*time.Second)
if err != nil {
    log.Fatal(err)
}
_ = reply // unmarshal into your generated GetStateResponse
```

```js
// QuickJS: the contract descriptors are part of the framework descriptor set,
// so the JSON path works without registering anything.
var state = capsulad.routed("capsulad.clock",
  "capsulad.contracts.clock.v1.ClockService", "GetState", {}, 5000);
capsulad.log("local time " + state.localTimeRfc3339);
```

The three contract services are present in the framework's embedded descriptor set, so JSON
calls work out of the box in every binding. Raw protobuf calls work regardless.

Discover them at runtime through `AppInterfaceService.ListInterfaces` and
`DescribeInterface`; `available` tells you whether the target's run is ready.

## Clock

`capsulad.contracts.clock.v1.ClockService`, target app `capsulad.clock`.

Product rules:

- A stopwatch starts at zero when idle; starting while it runs is a no-op.
- A timer alerts until it is explicitly dismissed; the alert is not a transient notification.
- Alarms are recurring schedules stored in the app setting `alarms`; an alarm alert rings until
  it is explicitly dismissed.
- Unrelated setting, timezone or device changes never reset a running stopwatch or a pending
  timer.

| RPC | Request | Response | Behaviour |
|---|---|---|---|
| `GetState` | `GetStateRequest{}` | `ClockState` | Current time, stopwatch, timers, alarms, active alerts. |
| `WatchState` | `WatchStateRequest{}` | stream `ClockState` | Full snapshot first, then one snapshot per observable change. |
| `StopwatchStart` | `StopwatchCommandRequest{}` | `StopwatchState` | Starts at zero when idle; no-op while running. |
| `StopwatchPause` | `StopwatchCommandRequest{}` | `StopwatchState` | Preserves accumulated elapsed time. |
| `StopwatchResume` | `StopwatchCommandRequest{}` | `StopwatchState` | Continues from the paused elapsed time. |
| `StopwatchReset` | `StopwatchCommandRequest{}` | `StopwatchState` | Returns to idle at zero. |
| `StartTimer` | `StartTimerRequest{duration_ms, label}` | `Timer` | Starts a timer that alerts until dismissed. |
| `CancelTimer` | `CancelTimerRequest{timer_id}` | `Timer` | Cancels a running timer. A completed timer is removed by dismissing its alert, not by cancelling. |
| `ListTimers` | `ListTimersRequest{}` | `ListTimersResponse{timer[]}` | All live and completed timers known to the app. |
| `ListAlarms` | `ListAlarmsRequest{}` | `ListAlarmsResponse{alarm[]}` | Configured alarms with next/last trigger. |
| `AddAlarm` | `AddAlarmRequest` | `Alarm` | Adds one normalized dcron expression. |
| `DeleteAlarm` | `DeleteAlarmRequest{alarm_id, expected_revision}` | `DeleteAlarmResponse{deleted}` | Removes one configured alarm. |
| `SetAlarmEnabled` | `SetAlarmEnabledRequest{alarm_id, enabled, expected_revision}` | `Alarm` | Disables or re-enables without losing it. |
| `DismissAlert` | `DismissAlertRequest{alert_id}` | `DismissAlertResponse{dismissed}` | Stops one alert; an empty `alert_id` dismisses all. |

### Messages

`StopwatchState` = `{status, elapsed_ms, started_unix_ms}`; `status` is
`STOPWATCH_STATUS_IDLE`, `STOPWATCH_STATUS_RUNNING` or `STOPWATCH_STATUS_PAUSED`.

`Timer` = `{timer_id, label, duration_ms, remaining_ms, status, started_unix_ms,
finished_unix_ms}`; `status` is `TIMER_STATUS_RUNNING`, `TIMER_STATUS_COMPLETED` or
`TIMER_STATUS_CANCELLED`.

`Alarm`:

| Field | Type | Meaning |
|---|---|---|
| `alarm_id` | string | Stable entry identity: `expr [@zone] [!once]`. |
| `schedule` | string | Normalized five-field expression. |
| `next_trigger_rfc3339` | string | Next fire time. |
| `next_trigger_unix_ms` | uint64 | Same, in Unix milliseconds. |
| `last_triggered_unix_ms` | uint64 | Last fire time. |
| `ringing` | bool | True while this alarm owns an active alert. |
| `alert_id` | string | The active alert, empty when not ringing. |
| `effective_timezone` | string | Zone actually used (explicit or device). |
| `enabled` | bool | False for a disabled alarm. |
| `one_shot` | bool | Fires once, then persists as disabled. |
| `timezone` | string | Explicit IANA zone, empty for the device zone. |
| `label` | string | Display label from the `alarm_labels` map. |

`ActiveAlert` = `{alert_id, kind, source_id, label, started_unix_ms}`; `kind` is
`ALERT_KIND_TIMER` or `ALERT_KIND_ALARM`.

`ClockState` = `{local_time_rfc3339, timezone, stopwatch, timers[], alarms[], active_alerts[],
revision, alarms_setting}`. `revision` increases whenever any observable field changes;
snapshots are ordered, and a consumer should converge on the latest. `alarms_setting` is the
canonical `alarms` string exactly as persisted.

`AddAlarmRequest`:

| Field | Type | Meaning |
|---|---|---|
| `schedule` | string | One five-field dcron expression. |
| `expected_revision` | uint64 | App-settings revision for compare-and-swap; `0` skips the check. |
| `timezone` | string | Optional IANA zone, empty follows the device. |
| `one_shot` | bool | Fire at the next match, then mark disabled. |
| `disabled` | bool | Stay configured but do not fire. |
| `label` | string | Display label; empty keeps an existing label or uses `"Alarm"`. |

Adding an existing normalized identity is idempotent. Errors: a stale revision is `ABORTED`, a
malformed schedule or modifier is `INVALID_ARGUMENT`, storage failure is `INTERNAL`.

### Alarm grammar

The only alarm storage is the app setting `alarms`, a string of dcron entries separated by a
comma after the completed fifth field and any suffixes, canonically followed by one space.

```text
alarms = [ entry { ", " entry } ]
entry  = minute SP hour SP day-of-month SP month SP day-of-week
         [ SP "@" IANA-zone ] [ SP "!once" ] [ SP "!disabled" ]
field  = item { "," item }
item   = "*" | "*" "/" step | number | number "-" number | number "-" number "/" step
```

- Ranges: minute 0–59, hour 0–23, day-of-month 1–31, month 1–12, day-of-week 0–7 (0 and 7 are
  both Sunday).
- Modifiers appear after the fifth field in this order: `@Asia/Tokyo`, `!once`, `!disabled`.
  Suffixes never contain commas.
- A comma followed by whitespace is always an entry separator, never a field list. Do not split
  on every comma.
- When both day-of-month and day-of-week are restricted, classic cron OR semantics apply; a
  full-domain field such as `*/1` counts as unrestricted.
- An empty string means no alarms. A trailing separator or an empty entry is rejected.

```text
0 7 * * 1                            Mondays at 07:00
30 6 * * 1-5 @Asia/Tokyo             weekdays at 06:30 in Tokyo
0,30 7 * * *                         07:00 and 07:30 daily
30 6 * * * @Asia/Tokyo !once, 0 7 * * * !disabled
                                     a one-shot Tokyo alarm and a disabled 07:00 alarm
```

The setting `alarm_labels` is a JSON object mapping each stable identity to a display label
(default `{}`). It never creates, deletes or enables an alarm.

### Consuming the clock

```go
// Ask for the next full snapshot; WatchState re-sends one after every change.
// CallJSON is (ctx, targetAppID, service, rpc, request, response, timeout).
var state map[string]any
if err := client.CallJSON(ctx, "capsulad.clock",
    "capsulad.contracts.clock.v1.ClockService", "GetState", nil, &state,
    5*time.Second); err != nil {
    log.Fatal(err)
}
```

A consumer should treat `ClockState` as a snapshot: render from the newest one and ignore
fields it does not understand. Do not poll faster than `WatchState` sends; snapshots coalesce.

## Ring

`capsulad.contracts.ring.v1.RingService`, target app `capsulad.ring`.

The ring still renders its baseline through normal presentation/capture routing. These RPCs
read and change its declared settings; they never touch routing ownership, so changing settings
does not reset the baseline presentation.

| RPC | Request | Response | Behaviour |
|---|---|---|---|
| `GetState` | `GetStateRequest{}` | `RingState` | Effective rendered values and ownership. |
| `WatchState` | `WatchStateRequest{}` | stream `RingState` | Snapshot first, then one per change. |
| `GetSettings` | `GetSettingsRequest{}` | `RingSettingsSnapshot` | Declared settings and revision. |
| `UpdateSettings` | `UpdateSettingsRequest` | `RingSettingsSnapshot` | Partial compare-and-swap update. |
| `WatchSettings` | `WatchSettingsRequest{}` | stream `RingSettingsSnapshot` | Snapshot first, then converges on the newest revision. |

`RingSettings`:

| Field | Type | Rule |
|---|---|---|
| `effect` | string | `breathe`, `spinner`, `rainbow`, `pulse`, `comet`, `sparkle` or `static`. `static` is a solid colour and does not animate. |
| `palette` | string | Canonical gradient string, or a bare hex colour for a solid palette. |
| `brightness` | float | `[0,1]`. |
| `speed` | float | `[0.1,4]`. |
| `schedule_enabled` | bool | Enables the active window. |
| `schedule_start`, `schedule_end` | string | `HH:MM` in the device timezone. The window may cross midnight; equal bounds mean all day. |

`RingSettingsSnapshot` = `{revision, values:RingSettings}`.
`UpdateSettingsRequest` = `{expected_revision, values:RingSettings, update_fields[]}`. Only the
listed paths change; a stale revision is `ABORTED`; validation matches generic app settings.

`RingState`:

| Field | Type | Meaning |
|---|---|---|
| `effect`, `palette`, `brightness`, `speed` | matching types | Effective rendered values, not a pending edit. |
| `presentation_id` | string | Presentation owned by the ring baseline; empty when stopped. |
| `active` | bool | False while the configured window excludes local time; the app then presents an explicit blank baseline and other captures still preempt it. |
| `settings_revision` | uint64 | Revision the rendering reflects. |
| `schedule_enabled`, `schedule_start`, `schedule_end` | matching types | Schedule as configured. |
| `pixel_count` | uint32 | Pixel count from the advertised ring capability. |

### Palette grammar

```text
linear-gradient([<angle>deg,] <stop> [, <stop> ...])
#RGB | #RRGGBB
```

- A bare hex colour is a one-stop gradient (a solid palette) and is what `static` uses. A
  gradient is also accepted for `static` and renders its first stop.
- The angle is optional, `0`–`360` degrees, and defaults to 90.
- A stop is `#RGB` or `#RRGGBB` with an optional percentage. At least one and at most eight
  stops are accepted.
- Positions must be nondecreasing over 0–100 %; omitted positions are distributed evenly between
  their specified neighbours, with the first defaulting to 0 % and the last to 100 %.
- No CSS variables, colour functions, repeating gradients, URLs or other CSS functions.

`rainbow` passes the palette's colours sampled at 0, 1/3 and 2/3 to the built-in
`system.ring.rainbow` preset and rotates them, so a single-colour palette yields a grayscale
rotation rather than invented hues.

### Consuming the ring

```js
var state = capsulad.routed("capsulad.ring", "capsulad.contracts.ring.v1.RingService",
                           "GetState", {}, 5000);
if (!state.active) {
  // the ring is outside its active window; do not assume it is lit
}
```

Writes from other apps are allowed and validated exactly like the settings UI: this is a
device-level service, not another app's private configuration.

## Media

`capsulad.contracts.media.v1.MediaPlayerService`. This is an app-provided contract: the provider
implements it and declares it, and consumers call it through the provider's app ID. No
application is bundled with the daemon and no app id is reserved for media: a speaker with a
connected phone has no media app at all.

The connected Bluetooth source is still visible and controllable without one. The framework's
own `BluetoothMediaService` (see [services.md](services.md#bluetoothmediaservice)) is the data
and command path: the Web UI "Now playing" panel lists the source as a framework-owned provider
(one player `bluetooth` while a source is connected, none otherwise) and its
PLAY/PAUSE/SKIP_NEXT/SKIP_PREVIOUS buttons call that service, while the physical circle key and
swipes use it directly whenever no standard player is playing or paused. Framework and
application providers coexist; consumers discover every running application provider through
`AppInterfaceService` and choose among the players they expose.

| RPC | Request | Response | Behaviour |
|---|---|---|---|
| `ListPlayers` | `ListPlayersRequest{}` | `ListPlayersResponse{players[]}` | All players the provider exposes. |
| `WatchPlayers` | `WatchPlayersRequest{}` | stream `PlayerEvent` | First event is a full snapshot, then incremental changes. |
| `GetStatus` | `GetStatusRequest{player_id}` | `PlayerStatus` | One player's state. |
| `Play` | `PlayerRequest{player_id}` | `PlayerCommandResult` | Ask to play. |
| `Pause` | `PlayerRequest{player_id}` | `PlayerCommandResult` | Ask to pause. |
| `SkipNext` | `PlayerRequest{player_id}` | `PlayerCommandResult` | Ask for the next item. |
| `SkipPrevious` | `PlayerRequest{player_id}` | `PlayerCommandResult` | Ask for the previous item. |

`PlayerStatus`:

| Field | Type | Meaning |
|---|---|---|
| `player_id` | string | Unique within the provider; stable while the player lives. |
| `playback_state` | enum | `PLAYBACK_STOPPED`, `PLAYBACK_PLAYING`, `PLAYBACK_PAUSED`, `PLAYBACK_BUFFERING`. |
| `supported_actions` | enum[] | `MEDIA_PLAY`, `MEDIA_PAUSE`, `MEDIA_SKIP_NEXT`, `MEDIA_SKIP_PREVIOUS`. |
| `app_name` | string | Display-friendly source application, for example `AirPlay`. |
| `source_name` | string | Display-friendly source, for example `Apple Music — John's iPhone`. |
| `media_name` | optional string | Display-friendly media indication; no title inference required. |
| `title`, `artist`, `album` | optional string | Structured metadata for consumers that format their own display. |
| `duration_ms`, `position_ms` | optional uint64 | Playback position. |
| `revision` | uint64 | Monotonic within this player's lifetime. |

`PlayerEvent` is a oneof `snapshot{players[]}`, `updated{PlayerStatus}` or
`removed{player_id}`. `PlayerCommandResult` = `{accepted, detail}`: the command is acknowledged
synchronously, while actual playback changes arrive through `WatchPlayers` or `GetStatus`.

### Providing the contract

Declare the interface in the manifest and add the provide hint so administrators can find it:

```toml
provides = ["media_player"]

[[interfaces]]
service = "capsulad.contracts.media.v1.MediaPlayerService"
version = "v1"
descriptor = "descriptors/media.protoset"   # optional: copy of the contract descriptor
```

Then register a handler for each method on the app bus. A player list is per provider: one app
may expose several independently controllable players. The stream ends when the app disconnects
or stops; a consumer must re-subscribe after a provider restart.

`capsulad-cli media list` lists running apps that publish the contract:

```text
org.example.player (My player) run=run_…
```

### Consuming the contract

```go
var players struct {
    Players []struct {
        PlayerID      string `json:"playerId"`
        AppName       string `json:"appName"`
        PlaybackState string `json:"playbackState"`
    } `json:"players"`
}
if err := client.CallJSON(ctx, "org.example.player",
    "capsulad.contracts.media.v1.MediaPlayerService", "ListPlayers", nil, &players,
    5*time.Second); err != nil {
    log.Fatal(err)
}
```

The contract does not control framework ducking. Playback gain and conversation ducking are
owned by the audio service; see [audio.md](audio.md).

## Declaring and consuming summary

| Goal | What to do |
|---|---|
| Publish a standard contract | Add `[[interfaces]]` with the contract service name, add the matching `provides` hint, implement every RPC on the app bus. |
| Call a standard contract | Routed call to the provider's app ID and the contract service name. |
| Discover providers | `AppInterfaceService.ListInterfaces{running_only: true}` and filter by `grpc_service`. |
| Read documentation | `AppInterfaceService.DescribeInterface{app_id, grpc_service}`. |
| Check compatibility | A provider that does not implement a method must end that call with `UNIMPLEMENTED`; consumers must treat it as "not supported", not as an error to retry. |