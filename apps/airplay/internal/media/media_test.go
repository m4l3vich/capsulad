package media

import (
	"encoding/binary"
	"math"
	"reflect"
	"sync"
	"testing"
)

func TestGainFromDB(t *testing.T) {
	cases := []struct {
		dB   float64
		want float64
	}{
		{0, 1},
		{-6.0206, 0.5},
		{-20, 0.1},
		{-144, 0},
		{-80, 0},
		{3, 1},
		{math.NaN(), 0},
	}
	for _, tc := range cases {
		got := GainFromDB(tc.dB)
		if math.Abs(got-tc.want) > 1e-4 {
			t.Fatalf("GainFromDB(%v) = %v, want %v", tc.dB, got, tc.want)
		}
	}
}

func TestScalePCM16(t *testing.T) {
	pcm := make([]byte, 8)
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint16(pcm[i*2:], 1000)
	}
	ScalePCM16(pcm, 0.5)
	for i := 0; i < 4; i++ {
		if got := int16(binary.LittleEndian.Uint16(pcm[i*2:])); got != 500 {
			t.Fatalf("sample %d = %d, want 500", i, got)
		}
	}
	// Clipping and silence.
	binary.LittleEndian.PutUint16(pcm[0:2], 32000)
	ScalePCM16(pcm, 2)
	if got := int16(binary.LittleEndian.Uint16(pcm[0:2])); got != 32767 {
		t.Fatalf("clipped sample = %d, want 32767", got)
	}
	binary.LittleEndian.PutUint16(pcm[0:2], 1234)
	ScalePCM16(pcm, 0)
	for i := range pcm {
		if pcm[i] != 0 {
			t.Fatalf("silence left byte %d = %d", i, pcm[i])
		}
	}
}

func TestSnapshotShape(t *testing.T) {
	status := Status{
		PlaybackState: StatePlaying,
		SourceName:    "iPhone",
		MediaName:     "T — A",
		Title:         "T",
		Artist:        "A",
		Album:         "Al",
		DurationMS:    180000,
		PositionMS:    42,
		Revision:      7,
	}
	want := map[string]any{
		"playerId":         PlayerID,
		"playbackState":    string(StatePlaying),
		"supportedActions": []any{ActionPlay, ActionPause, ActionSkipNext, ActionSkipPrev},
		"appName":          AppName,
		"sourceName":       "iPhone",
		"revision":         "7",
		"mediaName":        "T — A",
		"title":            "T",
		"artist":           "A",
		"album":            "Al",
		"durationMs":       "180000",
		"positionMs":       "42",
	}
	if got := status.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %#v, want %#v", got, want)
	}

	// Empty optional strings and a zero duration are omitted; 64-bit integers
	// stay protobuf-JSON strings.
	bare := Status{PlaybackState: StateStopped, SourceName: "no sender"}.Snapshot()
	for _, key := range []string{"mediaName", "title", "artist", "album", "durationMs"} {
		if _, ok := bare[key]; ok {
			t.Fatalf("optional key %q must be omitted: %#v", key, bare)
		}
	}
	wantBare := map[string]any{
		"playerId":         PlayerID,
		"playbackState":    string(StateStopped),
		"supportedActions": []any{ActionPlay, ActionPause, ActionSkipNext, ActionSkipPrev},
		"appName":          AppName,
		"sourceName":       "no sender",
		"revision":         "0",
		"positionMs":       "0",
	}
	if !reflect.DeepEqual(bare, wantBare) {
		t.Fatalf("bare snapshot = %#v, want %#v", bare, wantBare)
	}
}

func TestBoardLifecycle(t *testing.T) {
	board := NewBoard()
	if board.Live() || board.CurrentSession() != 0 {
		t.Fatalf("a new board is live: live=%v session=%d", board.Live(), board.CurrentSession())
	}
	generation := board.BeginSession("Test iPhone")
	if generation == 0 {
		t.Fatal("BeginSession returned the zero generation")
	}
	if !board.Live() || board.CurrentSession() != generation {
		t.Fatalf("board not live after BeginSession: live=%v session=%d", board.Live(), board.CurrentSession())
	}
	snapshot := board.Snapshot()
	if snapshot["sourceName"] != "Test iPhone" || snapshot["playbackState"] != string(StateBuffering) {
		t.Fatalf("snapshot = %v", snapshot)
	}
	board.SetMetadata(generation, "Title", "Artist", "Album", 120000, 5000)
	snapshot = board.Snapshot()
	if snapshot["title"] != "Title" || snapshot["mediaName"] != "Title — Artist" {
		t.Fatalf("metadata snapshot = %v", snapshot)
	}
	if snapshot["durationMs"] != "120000" || snapshot["positionMs"] != "5000" {
		t.Fatalf("uint64 fields must be protobuf-JSON strings: %v", snapshot)
	}
	first, events, unsubscribe := board.Subscribe()
	if _, ok := first["snapshot"]; !ok {
		t.Fatalf("first event = %v, want a snapshot", first)
	}
	board.SetPlayback(generation, StatePlaying)
	select {
	case event := <-events:
		if _, ok := event["updated"]; !ok {
			t.Fatalf("event = %v", event)
		}
	default:
		t.Fatal("no incremental event was published")
	}
	unsubscribe()
	board.SessionEnded(generation)
	if board.Live() {
		t.Fatal("board still live after SessionEnded")
	}
	if got := NamesFromSource("AirPlay/600.1 (MacBook Pro; macOS 15.1)", "", "10.0.0.2"); got != "MacBook Pro" {
		t.Fatalf("name from user agent = %q", got)
	}
	if got := NamesFromSource("", "Living Room", "10.0.0.2"); got != "Living Room" {
		t.Fatalf("name from device = %q", got)
	}
}

func TestStaleGenerationIsNoOp(t *testing.T) {
	board := NewBoard()
	generation := board.BeginSession("Test iPhone")
	before := board.Status()

	// A write from any other generation, the zero value included, is dropped
	// without touching the state or the revision.
	board.SetPlayback(generation+1, StatePlaying)
	board.SetMetadata(generation+1, "Title", "Artist", "Album", 1000, 500)
	board.SetProgress(generation+1, 999)
	board.SetProgress(0, 999)
	board.SetSourceName(generation+1, "Intruder")
	if got := board.Status(); got != before {
		t.Fatalf("a stale generation changed the board: %+v, want %+v", got, before)
	}
	if got := board.StaleWrites(); got != 5 {
		t.Fatalf("stale writes = %d, want 5", got)
	}

	// The owning generation still writes, and only it bumps the revision.
	board.SetSourceName(generation, "Renamed iPhone")
	board.SetPlayback(generation, StatePlaying)
	got := board.Status()
	if got.SourceName != "Renamed iPhone" || got.PlaybackState != StatePlaying || got.Revision != before.Revision+2 {
		t.Fatalf("owner write lost: %+v", got)
	}
	if got := board.StaleWrites(); got != 5 {
		t.Fatalf("a live write was counted as stale: %d", got)
	}
}

func TestSecondSessionSupersedesFirst(t *testing.T) {
	board := NewBoard()
	first := board.BeginSession("First iPhone")
	board.SetMetadata(first, "First Title", "First Artist", "First Album", 1000, 100)
	board.SetPlayback(first, StatePlaying)

	second := board.BeginSession("Second iPhone")
	if second == first {
		t.Fatalf("the second session reused generation %d", first)
	}
	if !board.Live() || board.CurrentSession() != second {
		t.Fatalf("live=%v session=%d, want the second session", board.Live(), board.CurrentSession())
	}
	if got := board.Status(); got.SourceName != "Second iPhone" || got.Title != "" || got.MediaName != "" ||
		got.DurationMS != 0 || got.PositionMS != 0 || got.PlaybackState != StateBuffering {
		t.Fatalf("the new session did not reset the player: %+v", got)
	}

	// The superseded session's playLoop wakes up late: every write is dropped,
	// and its teardown cannot stop the session that replaced it.
	superseded := board.Status()
	board.SetPlayback(first, StatePaused)
	board.SetMetadata(first, "Stale Title", "Stale Artist", "", 0, 0)
	board.SetProgress(first, 4242)
	board.SessionEnded(first)
	if got := board.Status(); got != superseded {
		t.Fatalf("the superseded session stomped the board: %+v, want %+v", got, superseded)
	}
	if !board.Live() || board.CurrentSession() != second {
		t.Fatalf("the superseded SessionEnded stopped the live session: live=%v session=%d", board.Live(), board.CurrentSession())
	}
	if got := board.StaleWrites(); got != 4 {
		t.Fatalf("stale writes = %d, want 4", got)
	}

	// The newest session wins.
	board.SetPlayback(second, StatePlaying)
	board.SetMetadata(second, "Second Title", "Second Artist", "Second Album", 2000, 200)
	got := board.Status()
	if got.PlaybackState != StatePlaying || got.Title != "Second Title" || got.MediaName != "Second Title — Second Artist" ||
		got.DurationMS != 2000 || got.PositionMS != 200 || got.Revision != superseded.Revision+2 {
		t.Fatalf("the newest session did not win: %+v", got)
	}

	board.SessionEnded(second)
	if board.Live() || board.CurrentSession() != 0 || board.Status().PlaybackState != StateStopped {
		t.Fatalf("board not idle after the live session ended: %+v", board.Status())
	}
	board.SetPlayback(second, StatePlaying)
	if board.Live() || board.Status().PlaybackState != StateStopped {
		t.Fatalf("a write after the session ended was applied: %+v", board.Status())
	}
	if got := board.StaleWrites(); got != 5 {
		t.Fatalf("stale writes = %d, want 5", got)
	}
}

// TestSubscribeUnsubscribeRace is the C2 regression test: unsubscribe closes
// the subscriber's channel while ten thousand publications are in flight, so a
// send racing that close would panic (or surface as send-after-close under
// -race).
func TestSubscribeUnsubscribeRace(t *testing.T) {
	const publications = 10000
	board := NewBoard()
	generation := board.BeginSession("Race iPhone")

	for round := 0; round < 8; round++ {
		_, events, unsubscribe := board.Subscribe()

		var writers sync.WaitGroup
		writers.Add(1)
		go func() {
			defer writers.Done()
			for i := range publications {
				board.SetProgress(generation, uint64(i+1))
			}
		}()

		// A live consumer drains while the writer is publishing, so the
		// channel is read, written and closed concurrently.
		var readers sync.WaitGroup
		readers.Add(1)
		drained := make(chan struct{})
		go func() {
			defer readers.Done()
			for range events {
			}
			close(drained)
		}()

		// Unsubscribing mid-burst is the race the old design lost. Calling it
		// twice proves the close happens exactly once (a second close would
		// panic), and the writer above keeps publishing afterwards to prove
		// nothing is ever sent on the closed channel.
		unsubscribe()
		unsubscribe()

		writers.Wait()
		<-drained
		readers.Wait()

		if _, ok := <-events; ok {
			t.Fatal("an event was delivered after unsubscribe")
		}
	}

	if got := board.StaleWrites(); got != 0 {
		t.Fatalf("stale writes = %d, want 0", got)
	}
}
