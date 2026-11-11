# Standard media-player contract — draft 0.5

**Schema:** `proto/capsula/contracts/media/v1/media.proto`  
**Service:** `capsula.contracts.media.v1.MediaPlayerService`  
**Transport:** framework-brokered Protobuf messages over `AppBusService.Call()` on the single Unix socket `unix:///run/capsulad/hal.sock`. **No media-app server socket.**

This is an optional contract for media applications. It provides media playback state, human-readable metadata and common controls to voice assistants, LED ring controllers, the Web UI and any other apps. Apps can also expose unrelated custom interfaces.

| Method | Meaning |
|---|---|
| `ListPlayers({})` | Snapshot of players belonging to that app |
| `WatchPlayers({})` | Stream initial snapshot then updates/removals |
| `GetStatus({playerId})` | Current state of one player |
| `Play/Pause({playerId})` | Ask one player to play/resume or pause |
| `SkipNext/SkipPrevious({playerId})` | Ask for adjacent track |

`PlayerStatus` fields include the app name (e.g. **AirPlay**), source name (e.g. **Apple Music — John's iPhone**), optional media name (e.g. **Scarlet Fire — Otis McDonald**), structured track fields if known, playback state, and current supported controls. Clients should check `supported_actions`. Unsupported commands return `FAILED_PRECONDITION`; an absent player yields `NOT_FOUND`. `WatchPlayers` begins with a snapshot and then publishes changes, so clients do not need polling. Media playback metadata is optional and should never be invented by providers.

## Routed invocation example

1. List app interfaces using `AppInterfaceService.ListInterfaces`, filtering for the media service; identify its `app_id`.
2. Read `DescribeInterface` to obtain a compiled protobuf descriptor and human-oriented method documentation.
3. Open an `AppBusService.Call` bidi stream. First packet: `RoutedCallStart{target_app_id, grpc_service:"capsula.contracts.media.v1.MediaPlayerService", rpc_name:"WatchPlayers"}`. Second packet: serialized `WatchPlayersRequest`. Then half-close the request side.
4. Receive a stream of serialized `PlayerEvent` protobuf response messages until the caller cancels or the app disconnects. The framework forwards these from the app's long-lived `AppBusService.Connect` connection.
5. To pause, start another `Call` stream targeting `Pause`, send serialized `PlayerRequest{player_id:"main"}`, half-close, and decode the reply as `PlayerCommandResult`.

The framework does not interpret media payloads, pause the media app, or modify its source metadata. Its **audio mixer** independently performs background ducking during an active assistant conversation.

For a tool-using agent, descriptor bytes determine the correct protobuf message types; the app's Markdown method documentation describes semantics and examples. An SDK or local tool is still needed to encode protobuf and operate the routed call stream. The framework does not turn arbitrary custom methods into plain-text HTTP endpoints.
