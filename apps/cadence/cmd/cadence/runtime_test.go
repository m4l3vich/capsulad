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
	mu            sync.Mutex
	revision      uint64
	stale         int
	revisionOnStale uint64
	calls         []configCall
	getValues     []any
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
	if rpc == "Get" {
		if response == nil {
			return nil
		}
		answer, _ := json.Marshal(map[string]any{
			"revision": strconv.FormatUint(f.revision, 10),
			"values":   f.getValues,
		})
		return json.Unmarshal(answer, response)
	}
	if rpc != "Update" {
		return errors.New("unexpected rpc " + rpc)
	}
	if f.stale > 0 {
		f.stale--
		if f.revisionOnStale != 0 {
			f.revision = f.revisionOnStale
		}
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

// enrollmentStatusRaceCaller simulates a last_test_status write applied
// immediately before the enrollment's compare-and-swap. The first Update is
// stale and advances the revision as that status write would; Get reports the
// fresh revision, and the retry must store the already-obtained token.
type enrollmentStatusRaceCaller struct {
	mu        sync.Mutex
	revision  uint64
	updates   int
	succeeded int
	calls     []configCall
	getValues []any
}

func (f *enrollmentStatusRaceCaller) CallJSON(_ context.Context, _, _ string, rpc string, request, response any, _ time.Duration) error {
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
	f.calls = append(f.calls, configCall{rpc: rpc, request: req})
	switch rpc {
	case "Get":
		if response == nil {
			return nil
		}
		answer, _ := json.Marshal(map[string]any{
			"revision": strconv.FormatUint(f.revision, 10),
			"values":   f.getValues,
		})
		return json.Unmarshal(answer, response)
	case "Update":
		f.updates++
		expected, _ := req["expectedRevision"].(string)
		if f.updates == 1 || expected != strconv.FormatUint(f.revision, 10) {
			if f.updates == 1 {
				// The concurrent status write advanced the revision.
				f.revision++
			}
			err := errors.New("ABORTED: app settings revision is stale (current " +
				strconv.FormatUint(f.revision, 10) + ")")
			f.calls[len(f.calls)-1].err = err
			return err
		}
		f.revision++
		f.succeeded++
		if response == nil {
			return nil
		}
		answer, _ := json.Marshal(map[string]any{
			"revision": strconv.FormatUint(f.revision, 10),
			"values":   []any{},
		})
		return json.Unmarshal(answer, response)
	default:
		return errors.New("unexpected rpc " + rpc)
	}
}

func (f *enrollmentStatusRaceCaller) snapshot() []configCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]configCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// snapshotValues builds the Get response values for the enrollment inputs.
func snapshotValues(server, board, caPEM string, enrollCode, deviceToken bool, tokenFile string) []any {
	return []any{
		map[string]any{"key": settings.KeyServerURL, "valueJson": strconv.Quote(server), "hasValue": true},
		map[string]any{"key": settings.KeyBoard, "valueJson": strconv.Quote(board), "hasValue": true},
		map[string]any{"key": settings.KeyCAPEM, "valueJson": strconv.Quote(caPEM), "hasValue": caPEM != ""},
		map[string]any{"key": settings.KeyEnrollCode, "valueJson": "", "hasValue": enrollCode},
		map[string]any{"key": settings.KeyDeviceToken, "valueJson": "", "hasValue": deviceToken},
		map[string]any{"key": settings.KeyDeviceTokenFile, "valueJson": strconv.Quote(tokenFile), "hasValue": tokenFile != ""},
	}
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
		caller:       caller,
		env:          capsulad.Env{AppID: "org.capsulad.cadence"},
		log:          log.New(&buf, "", 0),
		redactor:     &redact.Redactor{},
		link:         &fakeLink{},
		runCtx:       context.Background(),
		settingsWait: 500 * time.Millisecond,
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
	cfg := settings.Config{BaseURL: "https://s.example", EnrollCode: "K7M4QX", Board: "sim"}
	e := testEnrollment(r, cfg, "sim", 5)
	newRevision, err := r.commitEnrollment(context.Background(), e, "tok-secret", 5)
	if err != nil {
		t.Fatal(err)
	}
	if newRevision != 6 {
		t.Fatalf("revision = %d, want 6", newRevision)
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
	caller := &fakeConfigCaller{revision: 9, stale: 5}
	r, logs := testRuntime(caller)
	cfg := settings.Config{BaseURL: "https://server-a.example", EnrollCode: "CODE-A", Board: "sim"}
	caller.getValues = snapshotValues(cfg.BaseURL, cfg.Board, "", true, false, "")
	e := testEnrollment(r, cfg, "sim", 4)
	r.mu.Lock()
	r.publishedRev = 9
	r.mu.Unlock()
	r.revision.Store(9)
	r.finishEnrollment(e, enrollCredentials())

	calls := caller.snapshot()
	if len(calls) != 7 {
		t.Fatalf("calls = %+v, want four stale updates and three revision reads", calls)
	}
	if calls[0].rpc != "Update" || calls[0].request["expectedRevision"] != "4" {
		t.Fatalf("first call = %+v", calls[0])
	}
	if calls[1].rpc != "Get" {
		t.Fatalf("second call = %+v, want the fresh revision", calls[1])
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

func TestWriteTestStatusStoresTheNonSecretOutcome(t *testing.T) {
	caller := &fakeConfigCaller{revision: 4}
	r, _ := testRuntime(caller)
	r.revision.Store(4)

	r.writeTestStatus(nil)

	calls := caller.snapshot()
	if len(calls) != 1 || calls[0].rpc != "Update" {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].request["expectedRevision"] != "4" {
		t.Fatalf("revision = %v", calls[0].request["expectedRevision"])
	}
	changes, _ := calls[0].request["changes"].([]any)
	if len(changes) != 1 {
		t.Fatalf("changes = %+v", changes)
	}
	change := changes[0].(map[string]any)
	if change["key"] != settings.KeyLastTestStatus {
		t.Fatalf("key = %v", change["key"])
	}
	var status string
	if err := json.Unmarshal([]byte(change["valueJson"].(string)), &status); err != nil {
		t.Fatal(err)
	}
	if status != "test_ok" {
		t.Fatalf("status = %q", status)
	}
	if got := r.revision.Load(); got != 5 {
		t.Fatalf("revision = %d, want the update's revision", got)
	}
}

func TestWriteTestStatusRedactsAndCompactsFailures(t *testing.T) {
	caller := &fakeConfigCaller{revision: 9}
	r, _ := testRuntime(caller)
	r.revision.Store(9)
	r.redactor.Set("tok-secret", "CODE-123")

	r.writeTestStatus(errors.New("device tok-secret rejected for CODE-123\ncheck the server"))

	calls := caller.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	changes, _ := calls[0].request["changes"].([]any)
	change := changes[0].(map[string]any)
	var status string
	if err := json.Unmarshal([]byte(change["valueJson"].(string)), &status); err != nil {
		t.Fatal(err)
	}
	want := "test_failed: device [redacted] rejected for [redacted] check the server"
	if status != want {
		t.Fatalf("status = %q, want %q", status, want)
	}
	if strings.Contains(status, "tok-secret") || strings.Contains(status, "CODE-123") {
		t.Fatalf("a credential reached last_test_status: %q", status)
	}
}

func TestWriteTestStatusRetriesOnceAfterAStaleRevision(t *testing.T) {
	caller := &fakeConfigCaller{revision: 3, stale: 1, revisionOnStale: 4}
	r, _ := testRuntime(caller)
	r.revision.Store(3)

	r.writeTestStatus(nil)

	calls := caller.snapshot()
	if len(calls) != 3 {
		t.Fatalf("calls = %+v, want a stale Update, Get and a fresh-revision retry", calls)
	}
	if calls[0].rpc != "Update" || calls[0].request["expectedRevision"] != "3" {
		t.Fatalf("first call = %+v", calls[0])
	}
	if calls[1].rpc != "Get" {
		t.Fatalf("second call = %+v, want the authoritative revision", calls[1])
	}
	if calls[2].rpc != "Update" || calls[2].request["expectedRevision"] != "4" {
		t.Fatalf("retry call = %+v, want expectedRevision 4", calls[2])
	}
	if !strings.Contains(calls[0].err.Error(), "stale") {
		t.Fatalf("first call did not fail stale: %+v", calls[0])
	}
	if got := r.revision.Load(); got != 5 {
		t.Fatalf("revision = %d, want the retry's revision 5", got)
	}
}

// TestEnrollmentRetriesAfterAStatusWrite proves a concurrent last_test_status
// write cannot lose the already-obtained device token or leave the enrollment
// code pending: the stale commit is retried against the fresh revision.
func TestEnrollmentRetriesAfterAStatusWrite(t *testing.T) {
	caller := &enrollmentStatusRaceCaller{revision: 4}
	r, logs := testRuntime(caller)
	cfg := settings.Config{
		BaseURL:    "https://cadence.example.com",
		EnrollCode: "K7M4QX",
		Board:      "sim",
		HoldTail:   time.Second,
		FollowUp:   true,
	}
	caller.getValues = snapshotValues(cfg.BaseURL, cfg.Board, "", true, false, "")
	e := testEnrollment(r, cfg, "sim", 4)
	r.mu.Lock()
	r.publishedRev = 5
	r.mu.Unlock()
	r.revision.Store(5)

	r.finishEnrollment(e, enrollCredentials())

	got := r.config()
	if got.Token != "tok-enrolled" || got.EnrollCode != "" || !got.Configured {
		t.Fatalf("config = %+v", got)
	}
	if r.enroll != nil {
		t.Fatal("finished enrollment still owns the runtime")
	}
	if r.publishedRev != 6 {
		t.Fatalf("published revision = %d, want the committed revision 6", r.publishedRev)
	}
	if got := r.revision.Load(); got != 6 {
		t.Fatalf("revision = %d, want 6", got)
	}
	if !strings.Contains(logs.String(), "enroll_ok board=sim") {
		t.Fatalf("log = %q", logs.String())
	}

	quote := func(value string) string {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	// The status snapshot produced before the commit still carries the code
	// and no token. It must not revert the stored token.
	r.applySettings(&capsulad.SettingsChanged{Revision: 5, Values: []capsulad.SettingValue{
		{Key: settings.KeyServerURL, ValueJSON: quote(cfg.BaseURL)},
		{Key: settings.KeyEnrollCode, ValueJSON: quote(cfg.EnrollCode)},
		{Key: settings.KeyBoard, ValueJSON: quote(cfg.Board)},
		{Key: settings.KeyHoldReleaseTail, ValueJSON: "1000"},
		{Key: settings.KeyFollowUp, ValueJSON: "true"},
		{Key: settings.KeyLastTestStatus, ValueJSON: quote("test_ok")},
	}})
	got = r.config()
	if got.Token != "tok-enrolled" || got.EnrollCode != "" {
		t.Fatalf("a stale status snapshot reverted the credential: %+v", got)
	}
	if calls := caller.snapshot(); len(calls) != 3 {
		t.Fatalf("calls = %+v, want stale Update, Get and a successful retry", calls)
	} else {
		if calls[0].rpc != "Update" || calls[0].request["expectedRevision"] != "4" {
			t.Fatalf("first call = %+v", calls[0])
		}
		if calls[1].rpc != "Get" {
			t.Fatalf("second call = %+v, want the fresh revision", calls[1])
		}
		if calls[2].rpc != "Update" || calls[2].request["expectedRevision"] != "5" {
			t.Fatalf("retry call = %+v", calls[2])
		}
	}
	if caller.succeeded != 1 {
		t.Fatalf("successful updates = %d, want the retried token write", caller.succeeded)
	}
}

// A status-only snapshot must not republish or wake the link, and it must
// advance an in-flight enrollment's compare-and-swap revision so a connection
// test cannot make the one-time code stale.
func TestStatusOnlySnapshotDoesNotInvalidateEnrollment(t *testing.T) {
	r, _ := testRuntime(&fakeConfigCaller{})
	cfg := settings.Config{
		BaseURL:    "https://cadence.example.com",
		EnrollCode: "K7M4QX",
		Board:      "auto",
		HoldTail:   time.Second,
		FollowUp:   true,
	}
	e := testEnrollment(r, cfg, settings.BoardID(cfg.Board, r.model), 2)
	r.mu.Lock()
	r.publishedRev = 2
	r.mu.Unlock()

	quote := func(value string) string {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	r.applySettings(&capsulad.SettingsChanged{Revision: 6, Values: []capsulad.SettingValue{
		{Key: settings.KeyServerURL, ValueJSON: quote(cfg.BaseURL)},
		{Key: settings.KeyEnrollCode, ValueJSON: quote(cfg.EnrollCode)},
		{Key: settings.KeyBoard, ValueJSON: quote(cfg.Board)},
		{Key: settings.KeyHoldReleaseTail, ValueJSON: "1000"},
		{Key: settings.KeyFollowUp, ValueJSON: "true"},
		{Key: settings.KeyLastTestStatus, ValueJSON: quote("test_ok")},
	}})

	if setBoards, wakes := r.link.(*fakeLink).state(); setBoards != 0 || wakes != 0 {
		t.Fatalf("a status-only snapshot republished the link: %d/%d", setBoards, wakes)
	}
	if e.revision != 6 {
		t.Fatalf("enrollment revision = %d, want 6", e.revision)
	}
	if got := r.publishedRev; got != 6 {
		t.Fatalf("published revision = %d, want 6", got)
	}
	if r.enroll != e {
		t.Fatal("the enrollment was cancelled")
	}
}

// TestEnrollmentRetryAbortsOnAuthoritativeInputChange makes the local snapshot
// lag while the authoritative Get snapshot carries an admin edit. The retry
// must abort before writing, so the admin's inputs and the one-time code are
// not clobbered.
func TestEnrollmentRetryAbortsOnAuthoritativeInputChange(t *testing.T) {
	const oldCA = "-----BEGIN CERTIFICATE-----\nold\n-----END CERTIFICATE-----\n"
	const newCA = "-----BEGIN CERTIFICATE-----\nnew\n-----END CERTIFICATE-----\n"
	for name, tc := range map[string]struct {
		capem  []byte
		values []any
	}{
		"server": {
			values: snapshotValues("https://changed.example", "sim", "", true, false, ""),
		},
		"board": {
			values: snapshotValues("https://cadence.example.com", "vk-capsula-mini", "", true, false, ""),
		},
		"ca": {
			capem:  []byte(oldCA),
			values: snapshotValues("https://cadence.example.com", "sim", newCA, true, false, ""),
		},
		"code presence": {
			values: snapshotValues("https://cadence.example.com", "sim", "", false, false, ""),
		},
		"token presence": {
			values: snapshotValues("https://cadence.example.com", "sim", "", true, true, ""),
		},
	} {
		t.Run(name, func(t *testing.T) {
			caller := &enrollmentStatusRaceCaller{revision: 4, getValues: tc.values}
			r, logs := testRuntime(caller)
			cfg := settings.Config{
				BaseURL:    "https://cadence.example.com",
				EnrollCode: "K7M4QX",
				Board:      "sim",
				HoldTail:   time.Second,
				FollowUp:   true,
				CAPEM:      tc.capem,
			}
			e := testEnrollment(r, cfg, "sim", 4)
			r.mu.Lock()
			r.publishedRev = 5
			r.mu.Unlock()
			r.revision.Store(5)

			r.finishEnrollment(e, enrollCredentials())

			if got := r.config(); got.Token != "" || got.EnrollCode != "K7M4QX" {
				t.Fatalf("a changed authoritative input was clobbered: %+v", got)
			}
			if setBoards, wakes := r.link.(*fakeLink).state(); setBoards != 0 || wakes != 0 {
				t.Fatalf("aborted retry touched the link: %d/%d", setBoards, wakes)
			}
			if caller.updates != 1 {
				t.Fatalf("updates = %d, want only the stale attempt", caller.updates)
			}
			if caller.succeeded != 0 {
				t.Fatalf("successful updates = %d, want no device_token write", caller.succeeded)
			}
			calls := caller.snapshot()
			if len(calls) != 2 || calls[0].rpc != "Update" || calls[1].rpc != "Get" {
				t.Fatalf("calls = %+v, want one stale Update and the authoritative Get", calls)
			}
			if !strings.Contains(logs.String(), "enroll_superseded") {
				t.Fatalf("log = %q", logs.String())
			}
		})
	}
}

// TestNoRuntimeChangeResavingCodeStartsEnrollment covers the early-return
// branch: an unchanged config must still run planEnrollment so re-saving the
// same code retries a failed exchange, while a same-key live attempt is a
// no-op.
func TestNoRuntimeChangeResavingCodeStartsEnrollment(t *testing.T) {
	r, _ := testRuntime(&fakeConfigCaller{})
	cfg := settings.Config{
		BaseURL:    "https://s.example",
		EnrollCode: "CODE-A",
		Board:      "sim",
		HoldTail:   time.Second,
		FollowUp:   true,
	}
	r.current.Store(&cfg)
	r.mu.Lock()
	r.publishedRev = 1
	r.mu.Unlock()
	r.revision.Store(1)

	var started []*enrollment
	r.startEnrollment = func(e *enrollment) { started = append(started, e) }
	quote := func(value string) string {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	values := []capsulad.SettingValue{
		{Key: settings.KeyServerURL, ValueJSON: quote(cfg.BaseURL)},
		{Key: settings.KeyEnrollCode, ValueJSON: quote(cfg.EnrollCode)},
		{Key: settings.KeyBoard, ValueJSON: quote(cfg.Board)},
		{Key: settings.KeyHoldReleaseTail, ValueJSON: "1000"},
		{Key: settings.KeyFollowUp, ValueJSON: "true"},
	}

	r.applySettings(&capsulad.SettingsChanged{Revision: 2, Values: values})
	if len(started) != 1 {
		t.Fatalf("started = %d enrollments, want one", len(started))
	}
	if r.enroll == nil || r.enroll != started[0] {
		t.Fatal("the planned enrollment is not live")
	}
	if started[0].revision != 2 {
		t.Fatalf("enrollment revision = %d, want 2", started[0].revision)
	}
	t.Cleanup(started[0].cancel)

	r.applySettings(&capsulad.SettingsChanged{Revision: 3, Values: values})
	if len(started) != 1 {
		t.Fatalf("a live same-key attempt was restarted: %d", len(started))
	}
	if r.enroll == nil || r.enroll.revision != 3 {
		t.Fatalf("live enrollment revision = %+v, want 3", r.enroll)
	}
}

// TestEnrollmentRetryWaitsForDelayedMaskedCode proves a replaced code whose
// pushed event is delayed cannot be cleared by the retry: the retry waits for
// the authoritative revision, then the local masked input check aborts it.
func TestEnrollmentRetryWaitsForDelayedMaskedCode(t *testing.T) {
	cfg := settings.Config{
		BaseURL:    "https://cadence.example.com",
		EnrollCode: "OLD-CODE",
		Board:      "sim",
		HoldTail:   time.Second,
		FollowUp:   true,
	}
	for name, tc := range map[string]struct {
		deliver bool
		wait    time.Duration
	}{
		"delayed event": {deliver: true, wait: 500 * time.Millisecond},
		"event timeout": {deliver: false, wait: 30 * time.Millisecond},
	} {
		t.Run(name, func(t *testing.T) {
			caller := &enrollmentStatusRaceCaller{
				revision:  4,
				getValues: snapshotValues(cfg.BaseURL, cfg.Board, "", true, false, ""),
			}
			r, logs := testRuntime(caller)
			r.settingsWait = tc.wait
			e := testEnrollment(r, cfg, "sim", 4)
			r.mu.Lock()
			r.publishedRev = 4
			r.mu.Unlock()
			r.revision.Store(4)

			done := make(chan struct{})
			if tc.deliver {
				go func() {
					defer close(done)
					time.Sleep(50 * time.Millisecond)
					updated := cfg
					updated.EnrollCode = "NEW-CODE"
					r.mu.Lock()
					r.current.Store(&updated)
					r.publishedRev = 5
					r.mu.Unlock()
					r.revision.Store(5)
				}()
			} else {
				close(done)
			}

			r.finishEnrollment(e, enrollCredentials())
			<-done

			got := r.config()
			if got.Token != "" {
				t.Fatalf("the credential was stored: %+v", got)
			}
			if tc.deliver {
				if got.EnrollCode != "NEW-CODE" {
					t.Fatalf("the delayed code was not observed: %+v", got)
				}
			} else if got.EnrollCode != "OLD-CODE" {
				t.Fatalf("the code was changed on timeout: %+v", got)
			}
			if caller.updates != 1 || caller.succeeded != 0 {
				t.Fatalf("updates = %d succeeded = %d; want only the stale attempt",
					caller.updates, caller.succeeded)
			}
			if !strings.Contains(logs.String(), "enroll_superseded") {
				t.Fatalf("log = %q", logs.String())
			}
		})
	}
}