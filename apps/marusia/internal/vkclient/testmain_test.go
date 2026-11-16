package vkclient

import (
	"os"
	"testing"
)

// TestMain makes the staged library available to every test in this package
// when the gated integration run is requested. Request ids are generated with
// the native primitive before any transport is opened, and the SDK's loader
// caches its first path; setting the launcher variable here keeps that first
// load pointed at the test artifact.
func TestMain(m *testing.M) {
	if library := os.Getenv("CAPSULAD_TEST_LIBRARY"); library != "" {
		_ = os.Setenv("CAPSULAD_LIBRARY", library)
	}
	os.Exit(m.Run())
}
