# MicroPython Devtools (`dev.micropython`)

## Using this app

This is a **development-only** MicroPython probe for the script host and
framework calls. It reads device and audio state, exercises app-settings
compare-and-swap, fires an action, watches resources for a bounded heartbeat and
answers a raw-protobuf AppBus echo call. It is not a product app.

- **Launch:** open **Apps → MicroPython Devtools → Details**. The optional `step`
  argument is a free-form label recorded in the invocation log; it does not
  change the probe.
- **Controls:** the *Log framework state* action runs the probe. At startup the
  app reads `WatchResources` for three quiet seconds and logs each resource
  event. The `dev.devtools.v1.ScriptEcho` interface accepts one string field and
  replies with `micropython:<text>`.
- **Teardown:** *Stop* on the detail page ends the run; the script host has no
  background mode. Starting it again is a new run with a fresh probe.
- **Troubleshooting:**
  - **Launch fails:** the device needs the MicroPython host
    (`build/lib/capsulad-micropython`, built by `make interpreter-micropython`).
    A package can be installed without it, but the run cannot start.
  - **No log lines after startup:** open *Logs* in the detail page. The host logs
    `ready` and the startup probe before the heartbeat.
  - **The heartbeat says unavailable:** a timed resource read failed. The app
    logs the reason and continues; it does not stop the run.
  - **The echo call fails:** only the `Echo` method is implemented, and the
    request must carry one string field; other methods end with
    `UNIMPLEMENTED` and malformed payloads with `INVALID_ARGUMENT`.
  - **Settings do not change:** the `runs` counter is written with the current
    revision. A concurrent settings edit can make the compare-and-swap fail; the
    failure is logged and does not stop the app.

The package is an example of the script-host contract; its interface and log
wording are development aids, not a stable device API.