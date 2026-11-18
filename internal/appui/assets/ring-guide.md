# Ring

The Ring is a built-in system app that supplies the LED ring's baseline effect.
It runs only while it is the selected ring provider; unlike the Clock, it owns
no timer state. It has no package to install, remove or update.

## Controls

The ring has no physical controls of its own. Its effect, palette, brightness,
speed and active window are configured in the Web UI, and other apps can own a
ring presentation or capture through the normal framework services.

- A volume, mute or alert indication temporarily overrides the baseline.
- An application's ring presentation or capture overrides the baseline for as
  long as it holds ownership; the selected effect resumes when it releases.
- Outside an enabled active window the Ring presents an explicit blank frame.
  Volume, mute, alerts and application captures still use the ring.

## Launch and actions

Open **Apps → Ring → Details**. The Ring declares no actions and no launch
arguments. It starts when it is the selected ring provider and stays stopped
when another app or **none** is selected.

The effect changes as soon as its setting is saved. The Ring does not need a
restart for effect, palette, brightness, speed or schedule changes.

## Settings

| Setting | Meaning |
|---|---|
| Ring effect | Breathe, spinner, rainbow, pulse, comet, sparkle or one static colour. |
| Palette | `linear-gradient(<angle>deg, #RRGGBB <pct>%, ...)` or a solid `#RRGGBB`. |
| Brightness | Baseline brightness from 0 to 1. |
| Speed | Effect speed from 0.1 to 4. |
| Active window | Light the ring only between the start and end times. |
| Active from / until | `HH:MM` in the device timezone. The window may cross midnight; equal start and end mean all day. |

## Stop, teardown and recovery

- Stopping the Ring while it is the selected provider is shown as **Restart**:
  the selected provider is restarted, not turned off.
- Selecting another ring app or **none** is the durable off switch. An explicit
  **none** survives a daemon restart and is a deliberate blank ring.
- Turning **Active window** off restores the effect at all hours. Disabling it
  at night is not the same as deselecting the Ring.
- If the daemon restarts, the saved selection and settings are restored and the
  selected effect is presented again; the ring does not need a manual launch
  when it is the selected provider.

## Troubleshooting

- **The ring is dark at night:** check *Active window* and its start/end times.
  Outside that window the blank frame is intentional.
- **The ring is off all day:** confirm the Ring (or another app) is selected in
  **Settings → Ring**; **none** is an explicit blank baseline.
- **The palette is rejected:** use the documented gradient grammar, for example
  `linear-gradient(90deg, #00DCB4 0%, #5082FF 100%)`, or one solid `#RRGGBB`.
- **Volume or alerts show briefly:** those are framework indications and
  deliberately take priority over the baseline.
- **An app's animation left the ring stuck:** stop that app from its detail page;
  releasing its presentation restores the selected baseline.
- **Changes do not appear:** confirm the settings save succeeded; the Web UI
  reports a rejected value instead of silently keeping it.