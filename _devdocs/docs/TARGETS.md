# Initial hardware targets — draft 0.5

The initial hardware scope is **Capsula Mini only**:

| API `TargetPlatform` | Device image |
|---|---|
| `CAPSULA_MINI_ROCKCHIP_STOCK` | Rockchip, jailbroken stock firmware |
| `CAPSULA_MINI_AMLOGIC_STOCK` | Amlogic, jailbroken stock firmware |
| `CAPSULA_MINI_AMLOGIC_ALPINE` | Amlogic, Alpine Linux firmware |

Backend implementation and capability probing must be done **separately** for all three. In particular, do not assume matching touch raw-coordinates, DSP, audio driver format, display controller, process isolation support, or interpreter/ABI compatibility. Return `BackendAvailability` and `ResourceInfo` capability data, with an explicit `BACKEND_UNVERIFIED` state when evidence is missing.

Big Capsula and Neo support is deliberately **deferred**. The public API should remain flexible enough for later adapters, but no speculative hardware behavior is promised or implemented now.
