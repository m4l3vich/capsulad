# Soundboard (`org.capsulad.soundboard`)

A QuickJS soundboard for the Capsula Mini. The unmarked centre key plays the
active WAV from the shared `/assets` store; a left or right swipe across the
panel switches sounds and briefly shows the active name on the four-digit
segment display. Each sound has its own ring colour: a fixed hex value or a
fresh random colour on every activation.

| | |
|---|---|
| App ID | `org.capsulad.soundboard` |
| Runtime | `quickjs` |
| Needs | speaker, partial touch capture (centre and built-in swipes); an optional four-digit segment display and RGB ring |
| Host API | `capsulad.readAssetFile` (the rebuilt QuickJS host) |
| Package | `manifest.toml`, `main.js`, this guide, `ui/settings.html` |

## Sounds

Sounds are user assets, not package files. Upload WAVs under the shared asset
store (the Web UI **Assets** page writes `<data-dir>/assets`):

```text
Assets page -> upload beep.wav with the asset id audio/beep.wav
```

Any standard RIFF/WAVE file with 16-bit PCM or 32-bit IEEE float samples is
accepted; mono and multichannel files are resampled by the framework mixer. The
app caps one sound at 8 MiB. A press opens the speaker stream and starts the
colour window, then the input loop converts and queues the file in frame-aligned
64 KiB chunks (a 12 KiB first chunk starts audio promptly), so the ring timer
and a second press stay responsive while a large WAV streams. A missing,
malformed or oversized file is logged and shows dashes; it does not end the
run.

## Settings

The custom settings UI edits one JSON text setting, `sounds`. Each entry is:

```json
{"name": "BEEP", "asset": "/assets/beep.wav", "color": "random"}
```

| Property | Rule |
|---|---|
| `name` | 1–4 uppercase seven-segment characters after trimming. Allowed: `0-9 A B C D E F G H I J L N O P Q R S T U Y -`. Duplicate names are allowed. |
| `asset` | `/assets/` followed by slash-separated components and a `.wav` extension. No empty, `.` or `..` component, no backslash. |
| `color` | `random` (the default) or a browser HEX value `#rrggbb`. The UI offers the native colour picker plus eight predefined colours. |

The editor validates both the structured JSON and its individual fields; the
app validates them again when it receives the setting because the framework
enforces only the JSON type. Invalid entries are skipped with a log line.
Settings are read when the app starts; restart it after editing the list. Up to
16 sounds are kept.

## Controls and ownership

| Action | Result |
|---|---|
| Centre press | Play the active sound; the ring lights its colour as the speaker stream opens and holds it for the sound duration (minimum 700 ms, maximum 10 s). |
| Swipe left | Previous sound; show its name for about 1.4 s. |
| Swipe right | Next sound; show its name for about 1.4 s. |

The app holds a `TOUCH_PARTIAL_GESTURES` capture, so the centre and swipes
belong to it while the volume and microphone keys keep their normal framework
behaviour. The segment and ring captures are acquired only for the brief
name/colour window and released afterwards, so the configured clock and ring
baseline return immediately. Stopping the run cancels playback, the input
subscription, the captures and both presentations.

A short centre tap can also be the first contact of a built-in gesture; the
framework resolves that gesture on the following edge, exactly as documented in
the speaker behaviour contract.

## Build, install, verify

```sh
make capsulad-cli
sh apps/soundboard/build.sh
capsulad-cli app install build/apps/org.capsulad.soundboard-0.1.0.tar.gz
capsulad-cli app start org.capsulad.soundboard
```

The target needs the QuickJS host rebuilt with `capsulad.readAssetFile`. A host
without it logs an explicit unsupported line and stops before opening a capture.
The app also needs `Call.tryRecv`, which has been part of the host since Simon
Says.

```sh
node --test apps/soundboard/test/*.test.mjs
```

The contract suite loads the shipped `main.js` into a fake QuickJS host and
checks settings validation, swipe selection, centre playback (including the
exact WAV data chunk and chunk alignment), random/fixed colours, name glyphs,
the lazy segment/ring ownership windows, missing assets and terminal cleanup.
Host evidence is not a physical LED, display or acoustic measurement.
