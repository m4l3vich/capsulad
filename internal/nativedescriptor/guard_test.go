package nativedescriptor

import (
	"os"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
)

// embeddedDescriptor is the file sdk/native/src/dynamic.rs embeds with
// include_bytes!. The native runtime decodes every JSON reply against it.
const embeddedDescriptor = "../../sdk/native/descriptors/capsulad-framework.protoset"

const frameworkProto = "capsulad/framework/v1/framework.proto"

// readEmbeddedFile returns the framework file as committed to sdk/native.
func readEmbeddedFile(t *testing.T) *descriptorpb.FileDescriptorProto {
	t.Helper()
	raw, err := os.ReadFile(embeddedDescriptor)
	if err != nil {
		t.Fatalf("read embedded descriptor: %v", err)
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &set); err != nil {
		t.Fatalf("parse %s: %v", embeddedDescriptor, err)
	}
	// protodesc.NewFiles validates the whole set, including imports; a file
	// that is present but unlinked would otherwise pass unnoticed.
	if _, err := protodesc.NewFiles(&set); err != nil {
		t.Fatalf("embedded descriptor set is invalid: %v", err)
	}
	for _, file := range set.File {
		if file.GetName() == frameworkProto {
			return file
		}
	}
	t.Fatalf("%s does not describe %s", embeddedDescriptor, frameworkProto)
	return nil
}

// missingFields lists every message field the generated Go descriptor declares
// but the embedded file does not, as "Message.field". Nested messages are
// compared by their relative path so a nested-only drift is visible too.
func missingFields(generated, embedded *descriptorpb.FileDescriptorProto) []string {
	have := map[string]map[string]bool{}
	var index func(prefix string, messages []*descriptorpb.DescriptorProto)
	index = func(prefix string, messages []*descriptorpb.DescriptorProto) {
		for _, message := range messages {
			name := prefix + message.GetName()
			fields := map[string]bool{}
			for _, field := range message.GetField() {
				fields[field.GetName()] = true
			}
			have[name] = fields
			index(name+".", message.GetNestedType())
		}
	}
	index("", embedded.GetMessageType())

	var missing []string
	var walk func(prefix string, messages []*descriptorpb.DescriptorProto)
	walk = func(prefix string, messages []*descriptorpb.DescriptorProto) {
		for _, message := range messages {
			name := prefix + message.GetName()
			fields, ok := have[name]
			if !ok {
				missing = append(missing, name)
			} else {
				for _, field := range message.GetField() {
					if !fields[field.GetName()] {
						missing = append(missing, name+"."+field.GetName())
					}
				}
			}
			walk(name+".", message.GetNestedType())
		}
	}
	walk("", generated.GetMessageType())
	return missing
}

// TestEmbeddedFrameworkDescriptorCoversGeneratedFields fails when the embedded
// script is stale. It compares the generated Go descriptor with the committed
// file; it cannot detect that the deployed libcapsulad still embeds an older
// copy, which is why sdk/native/README.md requires a rebuild after
// regenerating.
func TestEmbeddedFrameworkDescriptorCoversGeneratedFields(t *testing.T) {
	generated := protodesc.ToFileDescriptorProto(frameworkv1.File_capsulad_framework_v1_framework_proto)
	embedded := readEmbeddedFile(t)

	if missing := missingFields(generated, embedded); len(missing) > 0 {
		t.Fatalf("%s is stale: %d field(s) missing, regenerate it and rebuild the native runtime: %v",
			embeddedDescriptor, len(missing), missing)
	}

	// The serial is the field this guard was added for: the daemon reports
	// DeviceInfo.hardware_serial and a stale descriptor silently drops it, so
	// assert it by name as well as through the generic comparison.
	if !hasField(embedded, "DeviceInfo", "hardware_serial") {
		t.Fatalf("%s does not describe DeviceInfo.hardware_serial", embeddedDescriptor)
	}
}

// TestDescriptorGuardDetectsMissingField proves the direction of the guard: a
// field removed from the generated descriptor is reported. It runs against an
// in-memory copy, so no committed file is reverted to demonstrate the failure.
func TestDescriptorGuardDetectsMissingField(t *testing.T) {
	generated := protodesc.ToFileDescriptorProto(frameworkv1.File_capsulad_framework_v1_framework_proto)
	embedded := readEmbeddedFile(t)

	stale := proto.Clone(embedded).(*descriptorpb.FileDescriptorProto)
	removed := false
	for _, message := range stale.GetMessageType() {
		if message.GetName() != "DeviceInfo" {
			continue
		}
		kept := message.GetField()[:0]
		for _, field := range message.GetField() {
			if field.GetName() == "hardware_serial" {
				removed = true
				continue
			}
			kept = append(kept, field)
		}
		message.Field = kept
	}
	if !removed {
		t.Fatal("DeviceInfo.hardware_serial is absent from the embedded descriptor already")
	}
	missing := missingFields(generated, stale)
	if len(missing) != 1 || missing[0] != "DeviceInfo.hardware_serial" {
		t.Fatalf("missing = %v, want [DeviceInfo.hardware_serial]", missing)
	}
}

func hasField(file *descriptorpb.FileDescriptorProto, message, field string) bool {
	for _, candidate := range file.GetMessageType() {
		if candidate.GetName() != message {
			continue
		}
		for _, f := range candidate.GetField() {
			if f.GetName() == field {
				return true
			}
		}
	}
	return false
}
