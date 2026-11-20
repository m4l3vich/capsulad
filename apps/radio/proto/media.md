Standard media contract served by the Internet Radio app
(`capsulad.contracts.media.v1.MediaPlayerService`).

The app exposes exactly one player, id `radio`, with a stable identity while
the app runs. The contract is served only while a run is alive: the radio is not
a resident provider, so after an idle exit there is no player to discover or
command and a launch is the recovery.

* `PLAYBACK_BUFFERING` while the station stream is being opened or reconnected;
* `PLAYBACK_PLAYING` while decoded audio is being queued to the speaker;
* `PLAYBACK_PAUSED` while paused. The run keeps this state, including the
  segment pause sign, for one minute; `Play` before then cancels the expiry and
  re-dials the station. If the minute passes, the app reports its terminal
  state and exits.
* `PLAYBACK_STOPPED` while a playback Stop holds for a resume or a Dismiss is
  closing the run. A playback Stop closes the stream like Pause, holds the
  pause sign for one minute, accepts `Play` to re-dial the selected station,
  and exits when the hold ends. Dismiss and an administrative process stop are
  immediate. A terminal failure reports `PLAYBACK_STOPPED` too: its reason is
  logged and, while the segment display is owned, shown as four dashes. With
  `always_display` off that ownership ends 10 seconds after the last tuning
  attempt; with it on the dashes last until the user stops the radio, picks
  another station, or one idle minute passes. The run exits when that window
  closes.

`Play` opens the selected station, `Pause` closes the stream, `SkipNext` and
`SkipPrevious` change the selected station (wrapping) and keep playback live
when the radio was playing, buffering, paused or in a playback Stop hold.
Commands are acknowledged synchronously with `{accepted, detail}`; actual state changes arrive through
`WatchPlayers` or `GetStatus`.

`GetStatus`, `Play`, `Pause`, `SkipNext` and `SkipPrevious` require a non-empty
`playerId` (`INVALID_ARGUMENT` when missing, `NOT_FOUND` for an unknown id).
`GetStatus` and `WatchPlayers` report `appName` = "Internet radio",
`sourceName` (selected station name), `mediaName` (ICY title when present, else
the station name), `title`/`artist` from ICY metadata, `supportedActions` and a
monotonic `revision`.
