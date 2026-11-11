# Example Ring Controller RPC API

**Package:** `org.example.ring.v1` · **Service:** `RingControllerService` · **Version:** 1.0.0

**Purpose:** Let another local app or an AI agent configure this ring controller's visual style. This interface only changes the controller's own desired frames. It does **not** capture the physical ring, change routing, or affect other apps. Its presentation appears whenever the user selects the controller as default or the controller's own presentation is otherwise routed to the ring.

## Methods

`ListModes({})`: returns modes currently supported by this implementation. Example response:

```json
{"modes":[{"id":"ambient","name":"Ambient","description":"Slow breathing light"},{"id":"visualizer","name":"Visualizer","description":"React to the available media audio feed"}]}
```

`GetState({})`: returns the currently selected mode. Example response: `{"currentModeId":"ambient","presentationActive":true}`. The active flag means the app's logical presentation is alive, not that it is visible on the device.

`SetMode({"modeId":"visualizer"})`: switches the ring controller's own mode. Example response: `{"currentModeId":"visualizer","presentationActive":true}`. The effect is persistent for this process until changed; whether it survives restart is implementation-specific. Unknown modes return `INVALID_ARGUMENT`. The RPC can succeed even if another app temporarily captures the ring; the mode becomes visible again when the default controller is selected.

**Usage:** Ask the framework's `AppInterfaceService.ListInterfaces` for `org.example.ring.v1.RingControllerService`, then `DescribeInterface` to retrieve this documentation and descriptor set. Invoke the documented method through `AppBusService.Call` on the framework Unix socket; the framework delivers the request to the running ring-controller app over its `AppBusService.Connect` stream. Avoid assuming this custom service exists on other LED ring controllers.
