// Package proto carries the serialized interface descriptor the bridge
// registers so JSON callers can reach HomeAssistantService, plus the .proto
// source it is generated from.
//
// The descriptor is embedded rather than read from the package directory,
// because a native app runs from a pivoted read-only root where only the
// executable's own package is guaranteed to exist. build.sh packages the same
// file at proto/homeassistant.protoset for the manifest's descriptor field.
package proto

import _ "embed"

// DescriptorSet is the serialized google.protobuf.FileDescriptorSet for
// org.capsulad.homeassistant.v1.
//
//go:embed homeassistant.protoset
var DescriptorSet []byte
