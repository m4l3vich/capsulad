//go:build darwin

package turn

// Importing the cgo SDK makes the darwin test binary use the external linker,
// which is what emits LC_UUID. Go 1.22's internal linker omits it, and dyld on
// current macOS refuses to run the resulting test executable. No SDK call is
// made from these tests; the import exists only for the link step.
import _ "capsulad/sdk/capsulad"
