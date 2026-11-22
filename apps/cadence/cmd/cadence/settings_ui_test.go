package main

import (
	"os"
	"strings"
	"testing"
)

// TestSettingsDocumentContract guards the packaging promises the Cadence
// settings page relies on: the manifest declares the document and the status
// key, and the document is a self-contained bridge consumer below the limit.
func TestSettingsDocumentContract(t *testing.T) {
	manifest, err := os.ReadFile("../../manifest.toml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(manifest)
	for _, want := range []string{
		"[settings_ui]",
		`html = "ui/settings.html"`,
		`key = "last_test_status"`,
		`id = "test_connection"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("manifest is missing %q", want)
		}
	}

	document, err := os.ReadFile("../../ui/settings.html")
	if err != nil {
		t.Fatal(err)
	}
	if len(document) > 512<<10 {
		t.Fatalf("settings document is %d bytes; the limit is %d", len(document), 512<<10)
	}
	html := string(document)
	for _, want := range []string{
		"CapsuladUI",
		`type="password"`,
		"last_test_status",
		"device_token",
		"server_url",
		"hold_release_tail_ms",
		"follow_up",
		"onState",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("settings document is missing %q", want)
		}
	}
	for _, forbidden := range []string{"<form", `type="submit"`, "addEventListener('submit'"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("settings document depends on form submission: %q", forbidden)
		}
	}
}
