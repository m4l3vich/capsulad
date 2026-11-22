package rockchip

import (
	"testing"

	"capsulad/internal/backend"
	"capsulad/internal/backend/profile"
)

// The profile is the only authority on which board revision the backend
// reports; the probe must not invent one.
func TestConfigForCarriesBoardRevision(t *testing.T) {
	cfg := ConfigFor(profile.RK3308Rev4Stock)
	if cfg.BoardRevision != "Capsula-mini-4" {
		t.Fatalf("board revision = %q, want Capsula-mini-4", cfg.BoardRevision)
	}
	b, err := New(Config{RootPrefix: t.TempDir(), BoardRevision: "custom-rev"})
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Info().BoardRevision; got != "custom-rev" {
		t.Fatalf("DeviceInfo.BoardRevision = %q, want custom-rev", got)
	}
}

// A caller that leaves the revision unset gets the supported default, never an
// empty headline.
func TestNewDefaultsBoardRevision(t *testing.T) {
	b, err := New(Config{RootPrefix: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Info().BoardRevision; got != "Capsula-mini-4" {
		t.Fatalf("DeviceInfo.BoardRevision = %q, want the rockchip default", got)
	}
	if b.Platform() != backend.PlatformRockchipStock {
		t.Fatalf("platform = %s", b.Platform())
	}
}
