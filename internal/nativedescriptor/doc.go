// Package nativedescriptor guards the protobuf descriptor the native runtime
// embeds.
//
// The Go SDK's CallJSON converts JSON at the edge of the wire through the
// native library, which decodes replies against
// sdk/native/descriptors/capsula-framework.protoset. The dynamic codec drops a
// field the embedded file does not describe, so a proto change reaches apps
// only after that file is regenerated:
//
//	protoc -I proto --include_imports \
//	  --descriptor_set_out=sdk/native/descriptors/capsula-framework.protoset \
//	  capsula/framework/v1/framework.proto capsula/contracts/clock/v1/clock.proto \
//	  capsula/contracts/media/v1/media.proto capsula/contracts/ring/v1/ring.proto
//
// Regenerating the file is not enough on its own: it is compiled into
// libcapsula with include_bytes!, so the native runtime must be rebuilt and
// redeployed before apps see a new field. A deployed library built from an
// older copy keeps dropping the field even though the daemon reports it.
// The test here compares the committed file with the generated Go descriptor,
// so file drift fails the build; it cannot see the deployed library's age.
package nativedescriptor
