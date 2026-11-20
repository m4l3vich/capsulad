# `capsulad.contracts.media.v1.MediaPlayerService` in the AirPlay receiver

`org.capsulad.airplay` provides the standard media contract for the live AirPlay
session. This is the same contract the Internet Radio front end provides and the
same one the daemon's panel gestures and the Web UI's "Now playing" panel
consume; there is no private path.

## Player

Exactly one player exists, and only while a sender owns the media session. A
completed HAP pair-setup or pair-verify keys a connection but does not create a
player; an accepted `ANNOUNCE` or a `SETUP` that stages a real audio stream does.
Event-only and type-130 control-only `SETUP` forms do not.

| Field | Value |
|---|---|
| `player_id` | `airplay` |
| `app_name` | `AirPlay` |
| `source_name` | the sender's device name when it sent one, else the model parsed from its RTSP `User-Agent`, else the peer address |
| `playback_state` | `PLAYBACK_BUFFERING` after an accepted `ANNOUNCE` or eligible `SETUP` starts the media session, `PLAYBACK_PLAYING` once `RECORD` or a valid `SETRATEANCHORTI` starts playout (buffered audio becomes playing only after decoded PCM reaches the sink), `PLAYBACK_PAUSED` on a rate-0 command |
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

Volume is device state, not a per-player property: the speaker has one persisted
master, shared by the local controls, the Web UI, Bluetooth and AirPlay. The
media contract therefore has no volume verb; a `Play`/`Pause` acknowledgement
would not be the right place to mutate a device-wide setting.

AirPlay carries the sender's slider as decibels, not as an amplitude. The
promoted, HAP-keyed active media owner (pause included) may send
`SET_PARAMETER volume:` in either the text or binary-plist form; the receiver
maps it with the reference's transfer, `-30..0 dB -> 0..1` master (the
`-144 dB` mute value and values at or below `-30 dB` are 0), and writes the one
master through `AudioService.SetMasterVolume`. Positive, non-finite and
out-of-contract values are ignored; they never raise the speaker. A candidate
that has only probed or paired, an event-only or type-130 control setup, a
plaintext legacy RAOP sender and a retired session are acknowledged with `200`
but change nothing, matching the reference's failed principal check. Connecting
or promoting a sender never writes the master.

There is no second gain stage over this app's PCM. `/info`'s `initialVolume`,
the event channel's `updateInfo`, `GET_PARAMETER volume:` and the `dvlc`
notification all derive from the same master reading. A receiver-side change
(local control, Web UI, Bluetooth) is mirrored after `updateInfo` as one
authoritative `dvlc` at the actual master unit; the receiver does not send
artificial intermediate slider values. A sender's own SET records the applied
level so it is never echoed back; a delayed echo is correlated only with an
exact value actually staged on that live channel, never with a broad time
window. If a superseded dvlc lands after a newer genuine SET, the next pass
repairs the sender presentation to the authoritative master without changing
the master. While that token remains armed, a corrective mirror is deferred;
the echo or the bounded expiry releases the next regular pass. If the framework
cannot read the master, the reported value is `-144 dB` (mute) rather than
`0 dB`/full scale, and no notification is posted; the actual master is never
written by that fallback.
