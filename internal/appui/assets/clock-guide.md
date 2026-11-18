# Clock

The Clock is a built-in system app. It owns the segment display's time,
stopwatch, timers and alarms, so it runs for the whole time the daemon is up
even when another app supplies the visible display. It has no package to
install, remove or update.

## Controls

- **Circle/play-pause key:** while a stopwatch is running or paused, a tap
  pauses or resumes it. While the stopwatch owns the key, that tap does not
  toggle media and does not summon the assistant.
- **Reset the stopwatch:** hold the circle key, or double-tap it, depending on
  the *Stopwatch reset gesture* setting. A non-matching hold or double-tap does
  nothing.
- **Dismiss a sounding alert:** press the circle key, the neutral centre or the
  primary panel. The same press cannot also start an assistant turn or toggle
  media. The alert does not expire on its own; it repeats until dismissed.
- **Timer and alarm precedence:** the nearest running timer takes the display
  and an orange ring countdown. A running stopwatch holds an orange whole-ring
  pulse, but the timer countdown takes precedence.
- **Volume and microphone keys:** these keep their normal device functions even
  while an alert or stopwatch is active.

## Launch and actions

Open **Apps → Clock → Details**. The Clock's launch form declares no
arguments; the useful controls are its actions:

| Action | Effect |
|---|---|
| Start stopwatch | Starts a stopwatch from zero. |
| Pause stopwatch | Pauses a running stopwatch. |
| Resume stopwatch | Resumes a paused stopwatch. |
| Reset stopwatch | Clears the stopwatch and returns the display to the clock baseline. |
| Start timer | Starts a countdown using the *Timer duration* setting. |
| Cancel timer | Cancels the running timer and releases the countdown. |
| Dismiss alert | Dismisses a sounding timer or alarm remotely. |

There is no separate "stop clock" action that turns the clock off. The Clock
always runs; choose another display provider or **none** in **Settings → Display**
to change what the segment display shows.

## Settings

| Setting | Meaning |
|---|---|
| Blink colon | Blink the colon between hours and minutes. |
| Timezone | IANA zone for the clock and alarm schedules. Empty follows the device timezone. |
| Display | Clock, stopwatch or timer when no timer or stopwatch is active. |
| Timer duration (seconds) | Duration used by *Start timer*; 1 to 86400. |
| Stopwatch reset gesture | Hold or double-tap the circle key to reset. |
| Alarms | Comma-separated five-field schedules in the device timezone. Optional `@IANA-zone`, `!once` and `!disabled` modifiers. |
| Alarm labels | Display labels keyed by the canonical alarm identity. |

An alarm whose schedule matches a sounding alert is not enqueued twice. Editing
an alarm or disabling it dismisses its sounding alert.

## Stop, teardown and recovery

- Stopping the built-in Clock is a restart when it is the selected display
  provider; it is not a durable off switch.
- Deselecting the Clock as the display provider, or selecting **none**, changes
  the baseline without stopping the clock's timers. A deliberate choice of
  **none** survives a daemon restart.
- Cancel a timer and reset a stopwatch when handing the device over; both
  release the ring and return the display to the configured baseline.

## Troubleshooting

- **Digits are blank:** confirm a display provider is selected in **Settings →
  Display**. Choosing **none** is an explicit blank frame, not a fault.
- **Timer or alarm did not sound:** check the timezone, the five schedule fields
  and any `!disabled` modifier. Alert audio also needs a non-zero device volume.
- **The circle key toggles music instead of the stopwatch:** no stopwatch is
  running or paused, or the configured reset gesture did not match. Start a
  stopwatch from the actions first.
- **The alert keeps ringing:** timer and alarm alerts repeat until dismissed;
  this is deliberate. Dismiss it from the panel or with the *Dismiss alert*
  action.
- **The display shows another app:** another selected provider or an active
  application presentation currently owns the display; the Clock baseline
  returns when that ownership ends.