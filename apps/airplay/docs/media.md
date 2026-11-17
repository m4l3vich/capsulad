# `capsulad.contracts.media.v1.MediaPlayerService` in the AirPlay receiver

`org.capsulad.airplay` provides the standard media contract for the live AirPlay
session. This is the same contract the Internet Radio front end provides and the
same one the daemon's panel gestures and the Web UI's "Now playing" panel
consume; there is no private path.

## Player

Exactly one player exists, and only while a sender is connected:

| Field | Value |
|---|---|
| `player_id` | `airplay` |
| `app_name` | `AirPlay` |
| `source_name` | the sender's device name when it sent one, else the model parsed from its RTSP `User-Agent`, else the peer address |
| `playback_state` | `PLAYBACK_BUFFERING` after the control connection opens, `PLAYBACK_PLAYING` once `RECORD`/`SETRATEANCHORTIME` starts playout, `PLAYBACK_PAUSED` on a rate-0 command |
| `supported_actions` | `MEDIA_PLAY`, `MEDIA_PAUSE`, `MEDIA_SKIP_NEXT`, `MEDIA_SKIP_PREVIOUS` |
| `title`, `artist`, `album` | from the sender's DMAP metadata, when it sends any |
| `media_name` | `"<title> — <artist>"` when both are present, otherwise whichever exists |
| `position_ms`, `duration_ms` | from the sender's `progress: start/current/end` report; `duration_ms` is `end - start` |
| `revision` | increases on every observable change |

`ListPlayers` returns zero players while no sender is connected, and `GetStatus`
for `airplay` ends with `NOT_FOUND` in that state. `WatchPlayers` sends one
snapshot first and then incremental `updated` events; the receiver never sends
`removed` because the player identity is stable for the app's lifetime — a
disconnected sender appears as `PLAYBACK_STOPPED` and is dropped from
`ListPlayers` because the board is idle, so consumers must re-read the snapshot
rather than cache a player forever.

## Commands and what they actually do

| RPC | Effect |
|---|---|
| `Play` | posts the sender's own "play" remote command (`modernMediaRemoteCommand` 0) on the event channel |
| `Pause` | posts "pause" (1) |
| `SkipNext` | posts "next item" (4) |
| `SkipPrevious` | posts "previous item" (5) |

The reply is an acknowledgement, never proof of playback: the sender's actual
state change arrives later through `WatchPlayers`, exactly as the contract
requires. With no sender, or with a sender that never established an event
channel, the command is refused with `accepted=false` and a reason.

## Why volume is not a player action

AirPlay carries the sender's volume in decibels. A supervised app cannot write
the device master volume (that surface is reserved for a local administrator),
so the receiver applies the sender volume as a gain on the PCM it writes. The
framework mixer's master volume and soft limiter still apply on top, which means
the sender can only attenuate its own stream, never raise the device above the
level a person set on it.
