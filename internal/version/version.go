// Package version carries build identity for capsulad and its SDK.
package version

// Version is the framework implementation version. It is deliberately
// independent from the draft wire/spec version (0.5): this code implements a
// subset of the draft and must not be presented as a stable v1.
const Version = "0.1.0-dev"

// APIDraft is the architecture/protobuf draft this build targets.
const APIDraft = "0.5"

// SDKVersion is the host API version negotiated with application packages.
const SDKVersion = "0.5"
