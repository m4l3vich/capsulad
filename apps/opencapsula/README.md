# OpenCapsula

[OpenCapsula](https://github.com/yawaflua/OpenCapsule) is a first-party
`voice_assistant` client for an OpenCapsule server. A framework activation opens
the ring, captures the request with an energy endpointer and streams 16 kHz mono
PCM to the server's `/v1/stream` websocket. The reply plays through the framework
playback endpoint; the server can also ask the speaker to read the volume, control
a Bluetooth source or end the conversation.

The app is supervised like any other package. It is not a system app and does
not replace the daemon or its bundled providers.

## Operator setup

1. Install and select the package as the speaker's voice assistant:
   ```sh
   capsulad-cli app install org.capsulad.opencapsula-0.1.0.tar.gz
   capsulad-cli app start org.capsulad.opencapsula
   ```
   The Web UI's application page and the assistant selection use the same
   `voice_assistant` role.
2. Open the OpenCapsule server's panel and copy the device stream URL and the
   device token. The default server URL assumes the server runs on the speaker
   host at port 8080; a typical LAN deployment is
   `ws://192.168.1.10:8080/v1/stream`. Use `wss://` when the server terminates
   TLS with a certificate the speaker's system trust store accepts.
3. Set **OpenCapsule server URL** and **Device token** in the app settings. Both
   are validated on the next settings snapshot; an invalid URL or a field of the
   wrong JSON type is rejected and the last good value stays in effect. A new URL
   or token is used by the next connection, not by a connection already up.
4. Optionally set the language, location and IANA time zone, and choose whether
   follow-up questions and earcons are enabled. Leave **Device ID** empty to use
   the framework's stable device id.

The package has no assets and no writable data directory. The token is stored by
the framework as a masked setting and is delivered in clear only to this app; it
is sent only in the websocket `Authorization: Bearer` header.

## What a turn does

- Any framework activation (`wakeword`, touch or API) begins a conversation.
- The ring presentation is opened before capture, because the framework requires
  a visible indication before it delivers microphone audio. While listening the
  whole ring follows the input level; thinking is a blue chase, speaking is
  green and level-reactive, errors are red and a lost link is amber.
- The microphone is opened with an explicit 16 kHz mono `PCM_S16LE` preference.
  The app validates the started format and refuses a capture that is not 16 kHz
  mono PCM16, because that is what the server protocol accepts.
- An energy VAD keeps 300 ms of pre-roll. Speech onset sends `turn_start`, then
  the pre-roll, then live PCM. A sustained silence ends the utterance and sends
  `turn_end`. Server-side VAD (`listen_end`) stops streaming and sends
  `turn_end` if the client has not sent it yet.
- Reply audio is played at the `ready` output format, which must be LINEAR16 mono
  at 16 kHz or 24 kHz. The writer paces chunks at real time and drains the
  framework playback tail after `turn_complete` before a follow-up.
- With follow-up enabled, the app listens for one more question within
  `follow_up_timeout_ms` without another wake word. No speech, a failed turn, a
  `conversation.end` command or follow-up disabled returns everything to idle.
- A new activation while thinking or speaking aborts playback, sends
  `turn_cancel`, and starts a new conversation.
- The `dismiss` action aborts listening and playback and releases the microphone,
  ring and conversation.

## Announced capabilities

Only these actions appear in the device hello and every one has a handler:

| Action | Behavior |
|---|---|
| `volume.get` | Reads the framework master volume and reports it as `level` 0..100 plus `muted: false` (the framework has no output mute). |
| `media.status` | Reports the connected Bluetooth AVRCP source, or `present: false` when none is connected. |
| `media.control` | Sends `play`, `pause`, `toggle`, `next` or `previous` to the Bluetooth source. `stop` is refused because the SDK's AVRCP bridge has no stop command. |
| `conversation.end` | Lets the current reply finish, then goes idle instead of following up. |

Master-volume writes and Bluetooth discoverability are administrator-only
framework operations. This supervised app does not announce them; volume is
readable but not writable here, and Bluetooth pairing/discoverability stays in
the Web UI. Bluetooth connect, disconnect and device listing are likewise
administrator-only, so they are not announced or executed.

Every announced command is answered with `command_result` on a bounded
goroutine, so a slow Bluetooth or settings call never stalls the turn or the
websocket reader.

## Connection behavior

The app dials through `capsulad.OpenTCP` (verified TLS for `wss`, system roots)
and the shared `capsulad/sdk/websocket` client. The production binary does not
import `net/http`, `crypto/tls` or a gRPC stack. A failed attempt backs off from
1 s to 30 s; a successful `ready` resets the delay. A framework activation wakes
a backoff sleep once, but an immediately failing dial still backs off before the
next attempt, so a broken server cannot busy-loop the speaker. A websocket ping
every 20 s with a 10 s pong deadline drops a half-open link.

## Known limits

- The OpenCapsule server must speak protocol version 1 on the configured path.
  Remote turns, announcements, alarms, plugins, server wake word and the
  `wake_stream` extension are not implemented; the hello advertises no optional
  features.
- Reply audio must be LINEAR16 mono at 16 kHz or 24 kHz. Other `ready` output
  formats are rejected with a clear log line and the link reconnects with
  backoff. The app does not resample.
- The microphone must deliver 16 kHz mono PCM16. A backend that reports another
  format ends the turn instead of sending a stream the server would misread.
- Master volume is readable (`volume.get`) but not writable by a supervised
  assistant: the daemon permits `SetMasterVolume` only for a local administrator
  (and the installed AirPlay receiver). Bluetooth pairing and discoverability
  are administrator-only as well and stay in the Web UI, so neither
  `volume.set`/`volume.adjust` nor `bluetooth.pair` is announced.
- `media.control` and `media.status` cover only the connected Bluetooth AVRCP
  source. There is no local file, HTTP stream or AirPlay media implementation in
  this app.
- The follow-up window is a single extra listen after a reply; there is no
  multi-turn local conversation state beyond what the server keeps for the
  connection.

## Attribution

The wire protocol names and message shapes are compatible with
[OpenCapsule](https://github.com/yawaflua/OpenCapsule), licensed under the Apache
License 2.0. This app is an independent implementation; the reference was read
for protocol compatibility, and no source file was copied. See the repository's
`LICENSE` and the upstream `AUTHORS` file for the reference project's notices.
