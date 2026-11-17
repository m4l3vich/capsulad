package tuner

import (
	"context"
	"testing"
	"time"
)

func testManager() *Manager {
	return &Manager{ctx: context.Background(), playerApp: DefaultPlayerApp}
}

func TestStopCancelsCurrentSession(t *testing.T) {
	m := testManager()
	s := newSession(m, TuneRequest{
		SessionID:   "session-1",
		URL:         "http://example.test/live.mp3",
		PlayerAppID: DefaultPlayerApp,
	})
	m.cur = s

	result := m.Stop(StopRequest{SessionID: "session-1"})
	if !result.Accepted {
		t.Fatalf("stop not accepted: %+v", result)
	}
	select {
	case <-s.ctx.Done():
	default:
		t.Fatal("session context was not cancelled")
	}

	second := m.Stop(StopRequest{SessionID: "session-1"})
	if second.Accepted {
		t.Fatalf("second stop accepted: %+v", second)
	}
}

func TestStopIgnoresForeignSession(t *testing.T) {
	m := testManager()
	s := newSession(m, TuneRequest{SessionID: "session-1", URL: "http://example.test/a"})
	m.cur = s

	result := m.Stop(StopRequest{SessionID: "session-2"})
	if result.Accepted {
		t.Fatalf("foreign stop accepted: %+v", result)
	}
	if s.ctx.Err() != nil {
		t.Fatal("foreign stop cancelled the live session")
	}
	if !m.isCurrent(s) {
		t.Fatal("foreign stop removed the live session")
	}
}

func TestSupersedingSessionIsNotCurrent(t *testing.T) {
	m := testManager()
	first := newSession(m, TuneRequest{SessionID: "session-1", URL: "http://example.test/a"})
	m.cur = first
	second := newSession(m, TuneRequest{SessionID: "session-2", URL: "http://example.test/b"})
	m.cur = second

	if m.isCurrent(first) {
		t.Fatal("superseded session reported current")
	}
	if !m.isCurrent(second) {
		t.Fatal("new session not current")
	}
	m.clearCurrent(first)
	if !m.isCurrent(second) {
		t.Fatal("clearCurrent removed the wrong session")
	}
}

func TestValidateRejectsUnsafeInput(t *testing.T) {
	m := testManager()
	base := TuneRequest{
		SessionID:   "s",
		URL:         "https://example.test/live.mp3",
		PlayerAppID: DefaultPlayerApp,
	}
	if detail := m.validate(base); detail != "" {
		t.Fatalf("valid request rejected: %s", detail)
	}
	cases := []struct {
		name string
		req  TuneRequest
	}{
		{"empty session", TuneRequest{URL: base.URL, PlayerAppID: base.PlayerAppID}},
		{"control in session", TuneRequest{SessionID: "s\nx", URL: base.URL, PlayerAppID: base.PlayerAppID}},
		{"control in station", TuneRequest{SessionID: "s", URL: base.URL, StationName: "a\rb", PlayerAppID: base.PlayerAppID}},
		{"long station", TuneRequest{SessionID: "s", URL: base.URL, StationName: string(make([]byte, 300)), PlayerAppID: base.PlayerAppID}},
		{"non-http url", TuneRequest{SessionID: "s", URL: "ftp://example.test/x", PlayerAppID: base.PlayerAppID}},
		{"foreign player", TuneRequest{SessionID: "s", URL: base.URL, PlayerAppID: "org.example.other"}},
	}
	for _, c := range cases {
		if detail := m.validate(c.req); detail == "" {
			t.Fatalf("%s accepted", c.name)
		}
	}
}

func TestManagerCloseCancelsSession(t *testing.T) {
	m := testManager()
	s := newSession(m, TuneRequest{SessionID: "session-1", URL: "http://example.test/a"})
	close(s.done) // pretend the run goroutine already exited
	m.cur = s
	done := make(chan struct{})
	go func() {
		m.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close blocked")
	}
	if s.ctx.Err() == nil {
		t.Fatal("Close did not cancel the session")
	}
	if m.cur != nil {
		t.Fatal("Close left a session installed")
	}
}
