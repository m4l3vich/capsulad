Standard media contract served by the Internet Radio app (capsulad.contracts.media.v1.MediaPlayerService).

The contract is defined by the framework; this copy of its descriptor is published
so callers can discover and describe the interface without bundling it. The app
exposes exactly one player, id `radio`, whose state follows the station session:
`PLAYBACK_BUFFERING` while the tuner is dialling, `PLAYBACK_PLAYING` while PCM is
flowing, `PLAYBACK_PAUSED` while paused and `PLAYBACK_STOPPED` when idle or after a
failure (the reason is logged and shown on the segment display).

`SkipNext` and `SkipPrevious` select the next or previous station and keep playing;
`Play` tunes the selected station; `Pause` pauses and makes the tuner stop fetching. A `Pause`
that arrives while a natural end is draining the queued tail is treated as a stop (the speaker
stream is already half-closed, so it cannot be paused): the reply is `accepted=true,
detail="stream ended"` and the player becomes `PLAYBACK_STOPPED`.
`GetStatus` and `WatchPlayers` report `appName`, `sourceName` (station name),
`mediaName`, `title`/`artist` when the tuner supplies them, `supportedActions` and a
monotonic `revision`.
