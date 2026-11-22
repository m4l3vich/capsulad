//go:build darwin && cgo

// Package linkext exists only to make darwin test binaries link externally.
//
// Go 1.22's internal linker emits a test binary without LC_UUID on current
// macOS, which dyld refuses to start. Importing a cgo package (this one) makes
// the toolchain use the external linker, matching the root Makefile's
// -linkmode=external for darwin. It is imported only from _test files.
package linkext

import "C"
