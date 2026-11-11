# Protobuf validation — draft 0.5

All three `.proto` source files were **successfully compiled** with `libprotoc 3.13.0`, using `--experimental_allow_proto3_optional` because this installed compiler predates standard support for `optional` in `proto3`.

The build produced `proto/capsula-framework-v0.5.protoset`, a combined serialized `FileDescriptorSet` (including source information). That file contains:

- `capsula/framework/v1/framework.proto`: 15 services, 177 messages, 16 top-level enums;
- `capsula/contracts/media/v1/media.proto`: 1 service, 9 messages;
- `ring-controller.proto`: 1 example service, 6 messages.

Example reproduction with a recent `protoc` (recent versions don't need the experimental flag):

```bash
protoc -I proto -I _devdocs/examples \
  --include_imports --include_source_info \
  --descriptor_set_out=proto/capsula-framework-v0.5.protoset \
  proto/capsula/framework/v1/framework.proto \
  proto/capsula/contracts/media/v1/media.proto \
  _devdocs/examples/ring-controller.proto
```

This validates protobuf **syntax and message/service descriptors**. It does **not** validate hardware backends, compile generated Go or Rust SDKs, prove that the reverse-RPC transport works, or prove operational security of mTLS/provenance/sandboxing.
