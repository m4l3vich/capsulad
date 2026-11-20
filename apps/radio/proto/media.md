Standard media contract served by the Internet Radio app
(`capsulad.contracts.media.v1.MediaPlayerService`).

The app exposes exactly one player, id `radio`, with a stable identity while
the app runs:

* `PLAYBACK_BUFFERING` while the station stream is being opened or reconnected;
* `PLAYBACK_PLAYING` while decoded audio is being queued to the speaker;
* `PLAYBACK_PAUSED` while paused;
* `PLAYBACK_STOPPED` when idle or after a terminal failure. The failure reason
  is logged and, while the segment display is owned, shown as four dashes; with
  `always_display` off that ownership ends 10 seconds after the last tuning
  attempt. A stopped entry remains visible to `ListPlayers`/`GetStatus` so its
  selected station stays discoverable and controllable; the Web UI's Now
  Playing panel only lists actively playing entries.

`Play` opens the selected station, `Pause` closes the stream, `SkipNext` and
`SkipPrevious` change the selected station (wrapping) and keep playback live
when the radio was playing, buffering or paused. Commands are acknowledged
synchronously with `{accepted, detail}`; actual state changes arrive through
`WatchPlayers` or `GetStatus`.

`GetStatus`, `Play`, `Pause`, `SkipNext` and `SkipPrevious` require a non-empty
`playerId` (`INVALID_ARGUMENT` when missing, `NOT_FOUND` for an unknown id).
`GetStatus` and `WatchPlayers` report `appName` = "Internet radio",
`sourceName` (selected station name), `mediaName` (ICY title when present, else
the station name), `title`/`artist` from ICY metadata, `supportedActions` and a
monotonic `revision`.