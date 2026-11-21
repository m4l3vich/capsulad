package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"capsulad/sdk/capsulad"

	"capsulad/apps/cadence/internal/enroll"
	"capsulad/apps/cadence/internal/redact"
	"capsulad/apps/cadence/internal/settings"
)

type configCall struct {
	rpc     string
	request map[string]any
	err     error
}

type fakeConfigCaller struct {
	mu       sync.Mutex
	revision uint64
	stale    int
	calls    []configCall
}

func (f *fakeConfigCaller) CallJSON(_ context.Context, _, _ string, rpc string, request, response any, _ time.Duration) error {
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	call := configCall{rpc: rpc, request: req}
	f.calls = append(f.calls, call)
	if rpc != "Update" {
		return errors.New("unexpected rpc " + rpc)
	}
	if f.stale > 0 {
		f.stale--
		call.err = errors.New("ABORTED: app settings revision is stale (current 1)")
		f.calls[len(f.calls)-1] = call
		return call.err
	}
	f.revision++
	if response != nil {
		answer, _ := json.Marshal(map[string]any{
			"revision": strconv.FormatUint(f.revision, 10),
			"values":   []any{},
		})
		return json.Unmarshal(answer, response)
	}
	return nil
}

func (f *fakeConfigCaller) snapshot() []configCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]configCall, len(f.calls))
	copy(out, f.calls)
	return out
}

type fakeLink struct {
	mu        sync.Mutex
	boards    []string
	wakes     int
	setBoards int
}

func (l *fakeLink) SetBoard(board string) {
	l.mu.Lock()
	l.boards = append(l.boards, board)
	l.setBoards++
	l.mu.Unlock()
}

func (l *fakeLink) Wake() {
	l.mu.Lock()
	l.wakes++
	l.mu.Unlock()
}

func (l *fakeLink) state() (setBoards, wakes int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.setBoards, l.wakes
}

func testRuntime(caller configCaller) (*runtime, *bytes.Buffer) {
	var buf bytes.Buffer
	r := &runtime{
		caller:   caller,
		env:      capsulad.Env{AppID: "org.capsulad.cadence"},
		log:      log.New(&buf, "", 0),
		redactor: &redact.Redactor{},
		link:     &fakeLink{},
		runCtx:   context.Background(),
		validateCA: func([]byte) error {
			return nil
		},
	}
	r.current.Store(&settings.Config{HoldTail: time.Second, FollowUp: true})
	return r, &buf
}

func testEnrollment(r *runtime, cfg settings.Config, board string, revision uint64) *enrollment {
	ctx, cancel := context.WithCancel(context.Background())
	e := &enrollment{
		key: enrollmentKey(cfg, board), revision: revision, board: board,
		cfg: cfg, ctx: ctx, cancel: cancel,
	}
	r.mu.Lock()
	r.enroll = e
	r.mu.Unlock()
	r.current.Store(&cfg)
	return e
}

func TestCommitEnrollmentIsOneCompareAndSwap(t *testing.T) {
	caller := &fakeConfigCaller{revision: 5}
	r, logs := testRuntime(caller)
	if err := r.commitEnrollment(context.Background(), "tok-secret", 5); err != nil {
		t.Fatal(err)
	}
	calls := caller.snapshot()
	if len(calls) != 1 || calls[0].rpc != "Update" {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].request["expectedRevision"] != "5" {
		t.Fatalf("revision = %v", calls[0].request["expectedRevision"])
	}
	changes, _ := calls[0].request["changes"].([]any)
	if len(changes) != 2 {
		t.Fatalf("changes = %+v", changes)
	}
	tokenChange := changes[0].(map[string]any)
	if tokenChange["key"] != settings.KeyDeviceToken || tokenChange["valueJson"] != strconv.Quote("tok-secret") {
		t.Fatalf("token change = %+v", tokenChange)
	}
	codeChange := changes[1].(map[string]any)
	if codeChange["key"] != settings.KeyEnrollCode || codeChange["clear"] != true {
		t.Fatalf("code change = %+v", codeChange)
	}
	if strings.Contains(logs.String(), "tok-secret") || strings.Contains(logs.String(), "K7M4QX") {
		t.Fatalf("a credential reached the log: %q", logs.String())
	}
}

func TestFinishEnrollmentInstallsAfterCommit(t *testing.T) {
	caller := &fakeConfigCaller{revision: 7}
	r, logs := testRuntime(caller)
	cfg := settings.Config{BaseURL: "https://cadence.example.com", EnrollCode: "K7M4QX", Board: "vk-capsula-mini"}
	e := testEnrollment(r, cfg, "vk-capsula-mini", 7)
	r.finishEnrollment(e, enrollCredentials())

	got := r.config()
	if got.Token != "tok-enrolled" || got.EnrollCode != "" || !got.Configured {
		t.Fatalf("config = %+v", got)
	}
	setBoards, wakes := r.link.(*fakeLink).state()
	if setBoards != 1 || wakes != 1 {
		t.Fatalf("link = %d/%d, want one board set and wake", setBoards, wakes)
	}
	if !strings.Contains(logs.String(), "enroll_stored") || !strings.Contains(logs.String(), "enroll_ok board=vk-capsula-mini") {
		t.Fatalf("log = %q", logs.String())
	}
	if r.enroll != nil {
		t.Fatal("finished exchange still owns the runtime")
	}
}

func TestFinishEnrollmentDiscardsASupersededResult(t *testing.T) {
	caller := &fakeConfigCaller{revision: 8}
	r, logs := testRuntime(caller)
	cfg := settings.Config{BaseURL: "https://server-a.example", EnrollCode: "CODE-A", Board: "sim"}
	e := testEnrollment(r, cfg, "sim", 3)
	// The administrator changed the server and code while A was in flight.
	r.current.Store(&settings.Config{BaseURL: "https://server-b.example", EnrollCode: "CODE-B", Board: "sim"})
	r.finishEnrollment(e, enrollCredentials())

	if len(caller.snapshot()) != 0 {
		t.Fatalf("superseded exchange wrote settings: %+v", caller.snapshot())
	}
	if got := r.config(); got.BaseURL != "https://server-b.example" || got.EnrollCode != "CODE-B" || got.Token != "" {
		t.Fatalf("config replaced: %+v", got)
	}
	if setBoards, wakes := r.link.(*fakeLink).state(); setBoards != 0 || wakes != 0 {
		t.Fatalf("superseded exchange touched the link: %d/%d", setBoards, wakes)
	}
	if strings.Contains(logs.String(), "enroll_ok") || strings.Contains(logs.String(), "enroll_stored") {
		t.Fatalf("superseded exchange claimed success: %q", logs.String())
	}
}

func TestFinishEnrollmentDoesNotInstallWhenCommitIsStale(t *testing.T) {
	caller := &fakeConfigCaller{revision: 9, stale: 1}
	r, logs := testRuntime(caller)
	cfg := settings.Config{BaseURL: "https://server-a.example", EnrollCode: "CODE-A", Board: "sim"}
	e := testEnrollment(r, cfg, "sim", 4)
	r.finishEnrollment(e, enrollCredentials())

	calls := caller.snapshot()
	if len(calls) != 1 || calls[0].request["expectedRevision"] != "4" {
		t.Fatalf("calls = %+v", calls)
	}
	if got := r.config(); got.Token != "" || got.EnrollCode != "CODE-A" {
		t.Fatalf("stale token installed: %+v", got)
	}
	if setBoards, wakes := r.link.(*fakeLink).state(); setBoards != 0 || wakes != 0 {
		t.Fatalf("stale commit touched the link: %d/%d", setBoards, wakes)
	}
	if !strings.Contains(logs.String(), "enroll_superseded") {
		t.Fatalf("log = %q", logs.String())
	}
}

func TestPlanEnrollmentCancelsChangedInputsAndKeepsSameInputs(t *testing.T) {
	r, _ := testRuntime(&fakeConfigCaller{})
	cfg := settings.Config{BaseURL: "https://a", EnrollCode: "CODE-A", Board: "sim"}
	first, cancelOld := r.planEnrollment(cfg, 2)
	if first == nil || cancelOld != nil {
		t.Fatalf("first plan = %+v, %v", first, cancelOld)
	}
	start, cancelOld := r.planEnrollment(cfg, 3)
	if start != nil || cancelOld != nil {
		t.Fatalf("same-key plan = %+v, %v", start, cancelOld)
	}
	r.mu.Lock()
	if r.enroll == nil || r.enroll.revision != 3 {
		t.Fatalf("same-key revision = %+v", r.enroll)
	}
	r.mu.Unlock()

	changed := cfg
	changed.EnrollCode = "CODE-B"
	start2, cancelOld := r.planEnrollment(changed, 4)
	if start2 == nil || cancelOld == nil {
		t.Fatalf("changed plan = %+v, %v", start2, cancelOld)
	}
	cancelOld()
	select {
	case <-first.ctx.Done():
	default:
		t.Fatal("the old exchange context was not cancelled")
	}
}

func TestInvalidCAKeepsThePreviousConfiguration(t *testing.T) {
	caller := &fakeConfigCaller{}
	r, logs := testRuntime(caller)
	r.validateCA = func([]byte) error { return errors.New("ca_pem rejected: bad certificate") }
	old := settings.Config{BaseURL: "https://old.example", Token: "old-token", Configured: true}
	r.current.Store(&old)

	ca, _ := json.Marshal("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")
	r.applySettings(&capsulad.SettingsChanged{Revision: 1, Values: []capsulad.SettingValue{
		{Key: "server_url", ValueJSON: `"https://new.example"`},
		{Key: "device_token", ValueJSON: `"new-token"`},
		{Key: "ca_pem", ValueJSON: string(ca)},
	}})

	if got := r.config(); got.BaseURL != old.BaseURL || got.Token != old.Token {
		t.Fatalf("invalid CA replaced the config: %+v", got)
	}
	if setBoards, wakes := r.link.(*fakeLink).state(); setBoards != 0 || wakes != 0 {
		t.Fatalf("invalid CA touched the link: %d/%d", setBoards, wakes)
	}
	if !strings.Contains(logs.String(), "not a usable trust root") {
		t.Fatalf("log = %q", logs.String())
	}
}

func TestRevisionOfAcceptsBothEncodings(t *testing.T) {
	for name, raw := range map[string]any{
		"string": "12",
		"number": float64(12),
		"json":   json.Number("12"),
	} {
		if got, ok := revisionOf(raw); !ok || got != 12 {
			t.Fatalf("%s: %d %v", name, got, ok)
		}
	}
	if _, ok := revisionOf(nil); ok {
		t.Fatal("nil revision accepted")
	}
}

func TestMergeHeldToken(t *testing.T) {
	held := heldToken{token: "tok", code: "CODE", base: "https://s"}
	for name, tc := range map[string]struct {
		cfg       settings.Config
		wantToken string
		wantCode  string
		wantHeld  bool
	}{
		"the snapshot still has the spent code": {
			cfg:       settings.Config{BaseURL: "https://s", EnrollCode: "CODE"},
			wantToken: "tok", wantCode: "", wantHeld: true,
		},
		"a stored token wins": {
			cfg:       settings.Config{BaseURL: "https://s", Token: "stored", EnrollCode: "CODE"},
			wantToken: "stored", wantCode: "CODE",
		},
		"a new code clears the held token": {
			cfg:      settings.Config{BaseURL: "https://s", EnrollCode: "NEW"},
			wantCode: "NEW",
		},
		"a new server clears the held token": {
			cfg:      settings.Config{BaseURL: "https://other", EnrollCode: "CODE"},
			wantCode: "CODE",
		},
	} {
		t.Run(name, func(t *testing.T) {
			next, kept := mergeHeldToken(tc.cfg, held)
			if next.Token != tc.wantToken || next.EnrollCode != tc.wantCode {
				t.Fatalf("cfg = %+v, want token %q code %q", next, tc.wantToken, tc.wantCode)
			}
			if (kept.token != "") != tc.wantHeld {
				t.Fatalf("held = %+v, want held=%v", kept, tc.wantHeld)
			}
			if tc.wantToken != "" && !next.Configured {
				t.Fatal("a token did not make the configuration usable")
			}
		})
	}
}

func enrollCredentials() enroll.Credentials {
	return enroll.Credentials{DeviceID: "dev-enrolled", Token: "tok-enrolled"}
}

func TestPublishIsRevisionAware(t *testing.T) {
	r, _ := testRuntime(&fakeConfigCaller{})
	r.mu.Lock()
	r.publishedRev = 5
	r.mu.Unlock()
	older := settings.Config{BaseURL: "https://old.example", Token: "old"}
	if r.publishLocked(older, 4) {
		t.Fatal("an older configuration was published")
	}
	if setBoards, wakes := r.link.(*fakeLink).state(); setBoards != 0 || wakes != 0 {
		t.Fatalf("older publication touched the link: %d/%d", setBoards, wakes)
	}
	if got := r.config(); got.BaseURL != "" || got.Token != "" {
		t.Fatalf("older publication replaced current: %+v", got)
	}
	newer := settings.Config{BaseURL: "https://new.example", Token: "new"}
	if !r.publishLocked(newer, 5) {
		t.Fatal("same-revision publication was refused")
	}
	if got := r.config(); got.BaseURL != newer.BaseURL || got.Token != newer.Token {
		t.Fatalf("newer publication not installed: %+v", got)
	}
	if setBoards, wakes := r.link.(*fakeLink).state(); setBoards != 1 || wakes != 1 {
		t.Fatalf("newer publication link = %d/%d", setBoards, wakes)
	}
}

func TestFinishEnrollmentDoesNotOverwriteANewerPublication(t *testing.T) {
	caller := &fakeConfigCaller{revision: 11}
	r, _ := testRuntime(caller)
	cfg := settings.Config{BaseURL: "https://same.example", EnrollCode: "CODE-A", Board: "sim"}
	e := testEnrollment(r, cfg, "sim", 2)
	r.mu.Lock()
	r.publishedRev = 10
	r.mu.Unlock()
	newer := cfg
	newer.Token = "newer-token"
	newer.Configured = true
	r.mu.Lock()
	r.current.Store(&newer)
	r.mu.Unlock()

	r.finishEnrollment(e, enrollCredentials())
	if got := r.config(); got.Token != "newer-token" {
		t.Fatalf("older enrollment overwrote newer config: %+v", got)
	}
	if setBoards, wakes := r.link.(*fakeLink).state(); setBoards != 0 || wakes != 0 {
		t.Fatalf("older enrollment woke the link: %d/%d", setBoards, wakes)
	}
}

func TestSupersededEnrollmentDoesNotChangeTheSharedRedactor(t *testing.T) {
	caller := &fakeConfigCaller{revision: 12}
	r, _ := testRuntime(caller)
	cfg := settings.Config{BaseURL: "https://same.example", EnrollCode: "CODE-A", Board: "sim"}
	e := testEnrollment(r, cfg, "sim", 2)
	newer := cfg
	newer.Token = "newer-token"
	newer.Configured = true
	r.mu.Lock()
	r.publishedRev = 10
	r.current.Store(&newer)
	r.redactor.Set("newer-token", "CODE-A")
	r.mu.Unlock()

	r.finishEnrollment(e, enrollCredentials())
	if got := r.redactor.Text("newer-token"); got != "[redacted]" {
		t.Fatalf("newer token is no longer masked: %q", got)
	}
	if got := r.redactor.Text("tok-enrolled"); got != "tok-enrolled" {
		t.Fatalf("superseded token was installed in the shared redactor: %q", got)
	}
}
