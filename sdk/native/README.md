# Native runtime descriptor

`src/dynamic.rs` embeds `descriptors/capsula-framework.protoset` with
`include_bytes!`. `CallJSON` and the other dynamic JSON codecs use that snapshot;
new protocol fields absent from it are unknown to the native runtime and can be
dropped while decoding replies.

Regenerate the framework descriptor from the canonical protocol tree after
schema changes with `make -C sdk/native descriptor`, which runs:

```sh
protoc -I proto --include_imports \
  --descriptor_set_out=sdk/native/descriptors/capsula-framework.protoset \
  capsula/framework/v1/framework.proto \
  capsula/contracts/clock/v1/clock.proto \
  capsula/contracts/media/v1/media.proto \
  capsula/contracts/ring/v1/ring.proto
```

The `internal/nativedescriptor` Go test compares this committed descriptor's
message and field names with the generated Go framework descriptor. It catches
file drift, but it cannot inspect the library already installed on a device:
the native runtime must also be rebuilt and redeployed after regeneration for
apps to see new fields. Until then a deployed library can still lag and drop
fields such as `DeviceInfo.hardware_serial`; apps can use `CAPSULA_DEVICE_SERIAL`
as a descriptor-free path to the host serial.