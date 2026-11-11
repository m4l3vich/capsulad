# App IPC example packaging — draft 0.5

An installed app offering a custom Protobuf service packages:

- a manifest (`ring-controller.toml` illustrates one),
- source `.proto` files and a compiled `FileDescriptorSet` (`.protoset`),
- Markdown method documentation describing arguments, side effects, errors, and examples.

**No per-app Unix socket exists.** The framework launches all apps; each app connects to `unix:///run/capsulad/hal.sock` and holds one long-lived `AppBusService.Connect` stream. The framework sends requests for declared custom methods over that stream. A caller discovers the service in `AppInterfaceService`, then uses `AppBusService.Call` targeting the destination `app_id`, protobuf service, and method. A local SDK can hide the message-envelope handling and make this look like a normal typed method call.

`protoc` can compile the standard media descriptor with:

```sh
protoc --proto_path=proto --include_imports --include_source_info \
  --descriptor_set_out=schemas/media.protoset \
  capsula/contracts/media/v1/media.proto
```

For a custom ring controller contract, compile its own `.proto` similarly. These contracts are examples, not mandates for all ring controllers. The schemas in this package were validated with a local `protoc` 3.13 compiler (using `--experimental_allow_proto3_optional`).

See `examples/ring-controller.proto`, `examples/docs/ring-api.md`, and `docs/MEDIA_CONTRACT.md` for method semantics.
