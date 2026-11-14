package bluetooth

import (
	"context"
	"testing"
)

// The simulator playlist gives transport commands a deterministic, public
// observable (MediaState.Title) without adding a last-command echo field.
func TestMemoryPlaylistNextPrevious(t *testing.T) {
	m := NewMemory()
	m.SimulateConnect(Device{Address: "AA:BB", Name: "Phone"}, MediaState{Present: true, Playing: true})
	m.SetPlaylist([]string{"one", "two", "three"})
	ctx := context.Background()

	title := func() string {
		t.Helper()
		state, err := m.Media(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return state.Title
	}
	if got := title(); got != "one" {
		t.Fatalf("initial title = %q", got)
	}
	if err := m.Command(ctx, CommandNext); err != nil {
		t.Fatal(err)
	}
	if got := title(); got != "two" {
		t.Fatalf("next title = %q", got)
	}
	if err := m.Command(ctx, CommandPrevious); err != nil {
		t.Fatal(err)
	}
	if got := title(); got != "one" {
		t.Fatalf("previous title = %q", got)
	}
	if err := m.Command(ctx, CommandPrevious); err != nil {
		t.Fatal(err)
	}
	if got := title(); got != "three" {
		t.Fatalf("wrapped previous title = %q", got)
	}
	// Toggle changes playback state only; it must not move the track.
	if err := m.Command(ctx, CommandToggle); err != nil {
		t.Fatal(err)
	}
	if got := title(); got != "three" {
		t.Fatalf("toggle changed title to %q", got)
	}
	// Without a playlist the transport is still an accepted no-op, preserving
	// the historical simulator behavior.
	m.SetMedia(MediaState{Present: true, Playing: true, Title: "single"})
	if err := m.Command(ctx, CommandNext); err != nil {
		t.Fatal(err)
	}
	if got := title(); got != "single" {
		t.Fatalf("no-playlist next changed title to %q", got)
	}
}
