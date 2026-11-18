# Hardware Devtools (`dev.hardware`)

## Using this app

This is a **development-only** package for checking the speaker's hardware and
framework surfaces through the public app API. It is not a product app and its
output is not a stable user interface.

- **Launch:** open **Apps → Hardware Devtools → Details**. The optional `step`
  argument selects one step (`info`, `display`, `ring`, `input`, `audio`,
  `settings`, `echo`) or `selftest`; an empty value runs the full self-test.
- **Controls:** the detail page also exposes each step as an action. Every step
  is serialized through one bounded queue, so wait for the current step to end
  before starting another.
- **Teardown:** steps own and release their presentations, captures, audio
  handles and streams before returning. *Stop* ends the run; a subsequent
  *Launch* starts a new one. There is no background mode.
- **Troubleshooting:**
  - **A step reports `unsupported`:** the backend or simulator lacks that
    surface (for example no coordinate panel or no ring). This is a first-class
    result, not an app failure.
  - **A step reports `error`:** check the detail page logs for the step name,
    status and message. Step resources are still released on failure.
  - **Launch fails:** the package needs the native runtime metadata and a
    compatible `libcapsulad`; use `make native` before packaging.
  - **The queue says full:** four steps are already pending. Let them finish or
    stop the app.

The package publishes `dev.devtools.v1.DevToolsService` for its echo step. The
contract and build logic are in this directory; they are examples for app
authors, not device API guarantees.