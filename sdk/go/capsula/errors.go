package capsula

import (
	"errors"
	"fmt"
)

// ErrClosed is returned by calls made after the client's bus has ended.
var ErrClosed = errors.New("capsula: client is closed")

// ErrLibraryUnavailable reports that the shared native engine could not be
// found or loaded. Env.Library and CAPSULA_LIBRARY should name the exact file
// installed by the launcher (for example /capsula/lib/libcapsula.so.1).
var ErrLibraryUnavailable = errors.New("capsula: native library unavailable")

// LibraryError describes a failed shared-library load.
type LibraryError struct {
	Path string
	Err  error
}

func (e *LibraryError) Error() string {
	path := e.Path
	if path == "" {
		path = platformLibraryName()
	}
	return fmt.Sprintf("capsula: cannot load native library %s: %v (set CAPSULA_LIBRARY to the "+
		"absolute libcapsula path installed by the launcher, for example "+
		"/capsula/lib/libcapsula.so.1)", path, e.Err)
}

func (e *LibraryError) Unwrap() error { return e.Err }
func (e *LibraryError) Is(target error) bool {
	return target == ErrLibraryUnavailable
}

// ABIError reports an incompatible native engine ABI. The SDK requires exactly
// major 1 and a minor at least 0.
type ABIError struct {
	RequiredMajor int
	RequiredMinor int
	ActualMajor   int
	ActualMinor   int
}

func (e *ABIError) Error() string {
	return fmt.Sprintf("capsula: native library ABI %d.%d is incompatible; this SDK requires ABI %d.%d "+
		"(rebuild or reinstall the shared runtime)", e.ActualMajor, e.ActualMinor,
		e.RequiredMajor, e.RequiredMinor)
}

// StatusError is a framework/app call failure. Code is a canonical gRPC status
// code for call failures, or a native ABI status value for transport/loader
// failures; Op names the failed operation and Message carries the framework or
// native detail.
type StatusError struct {
	Op      string
	Code    int32
	Message string
}

func (e *StatusError) Error() string {
	op := e.Op
	if op == "" {
		op = "call"
	}
	if e.Message == "" {
		return fmt.Sprintf("capsula: %s failed (%d)", op, e.Code)
	}
	return fmt.Sprintf("capsula: %s failed (%d): %s", op, e.Code, e.Message)
}
