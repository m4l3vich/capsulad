Internet radio player interface: a companion tuner pushes decoded station PCM here.

Wire encoding is raw protobuf; no descriptor registration is required to call or
serve these methods. The authoritative schema is proto/radio.proto in this package.

## Push

Push one decoded chunk of the active station session, or its final message.

Request: `PushRequest{sequence, session_id, pcm, sample_rate_hz, channels,
sample_format (1=PCM_S16LE, 2=PCM_F32LE), first_frame, discontinuity,
dropped_frames, title, artist, finished, failed, detail}`.
Reply: `PushResult{accepted, detail}`.

One routed unary call per chunk, and the caller must wait for the reply before
sending the next chunk: the player writes the chunk into the framework speaker
stream inside the call, so the reply is the backpressure signal. `accepted=false`
means "drop this session now"; `detail` is `stale session`, `unauthorized caller`,
`push authorization unavailable`, `paused`, `stopped`, `no playback stream` or
`over capacity`.

`session_id` is required and must be the exact echo of `TuneResult.session_id`;
a message without it, or with another session, is refused as stale and changes no
state. The player also authorizes the caller: only the run of the tuner app it
tuned (the daemon-verified caller run id resolved through
`AppInterfaceService.ListInterfaces`) may push, so a foreign app cannot inject
audio, metadata or a terminal failure. When that lookup itself fails, a push from
the serving run ends the session (`push authorization unavailable`) because the
chunk is lost either way, while an unknown or foreign caller is only refused and
cannot end playback. Recommended chunk size 80-500 ms
(200 ms default), `pcm` at most 256 KiB, sample rate 8000-192000 Hz, 1-2 channels.
End a session with `finished=true` (pcm optional) or `failed=true` plus `detail`.
