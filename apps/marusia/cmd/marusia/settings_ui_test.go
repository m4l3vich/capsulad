package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"capsulad/apps/marusia/internal/settings"
	"capsulad/sdk/capsulad"
)

// fakeSettingsCaller is the app-configuration service seam. It can return one
// stale Update (as a concurrent status write would) and records the RPCs.
type fakeSettingsCaller struct {
	mu         sync.Mutex
	revision   uint64
	updates    int
	gets       int
	calls      []string
	expected   []string
	staleFirst bool
	onUpdate   func()
	onGet      func()
}

func (f *fakeSettingsCaller) CallJSON(_ context.Context, _, _, rpc string, request, response any, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, rpc)
	switch rpc {
	case "Update":
		f.updates++
		if req, ok := request.(map[string]any); ok {
			if expected, ok := req["expectedRevision"].(string); ok {
				f.expected = append(f.expected, expected)
			}
		}
		if f.staleFirst && f.updates == 1 {
			if f.onUpdate != nil {
				f.onUpdate()
			}
			return errors.New("ABORTED: app settings revision is stale (current 8)")
		}
		f.revision++
		if response == nil {
			return nil
		}
		answer, _ := json.Marshal(map[string]any{
			"revision": strconv.FormatUint(f.revision, 10),
			"values":   []any{},
		})
		return json.Unmarshal(answer, response)
	case "Get":
		f.gets++
		if f.onGet != nil {
			f.onGet()
		}
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

// TestApplySettingsAutoExchangesOnlyANewToken covers the submission contract:
// a token present in the initial snapshot is existing state, a repeated value
// is not a new submission, and a changed non-empty value is handed to the one
// automatic exchange path.
func TestApplySettingsAutoExchangesOnlyANewToken(t *testing.T) {
	var exchanged []string
	a := &app{
		fallback: settings.Fallback{Serial: "SN-test", DeviceID: "device-test"},
		values:   settings.Default(),
		queue:    make(chan activationJob, 1),
	}
	a.autoExchange = func(token string) { exchanged = append(exchanged, token) }

	a.applySettings(settingsWithToken("TOKEN-ONE"))
	if len(exchanged) != 0 {
		t.Fatalf("the initial snapshot started an exchange: %v", exchanged)
	}
	a.applySettings(settingsWithToken("TOKEN-ONE"))
	if len(exchanged) != 0 {
		t.Fatalf("an unchanged token started an exchange: %v", exchanged)
	}
	a.applySettings(settingsWithToken(""))
	if len(exchanged) != 0 {
		t.Fatalf("clearing the token started an exchange: %v", exchanged)
	}
	a.applySettings(settingsWithToken("TOKEN-TWO"))
	if len(exchanged) != 1 || exchanged[0] != "TOKEN-TWO" {
		t.Fatalf("a new token must start exactly one exchange, got %v", exchanged)
	}
}

func settingsWithToken(token string) *capsulad.SettingsChanged {
	value := `""`
	if token != "" {
		value = `"` + token + `"`
	}
	return &capsulad.SettingsChanged{Revision: 2, Values: []capsulad.SettingValue{
		{Key: settings.KeyVKAccessToken, ValueJSON: value},
	}}
}

// TestSessionStoreRevisionRejectsASupersededToken covers the ordering guard:
// the revision and pending token are captured under one lock, so an exchange
// that spent an older token must not store its session or clear a newer one.
func TestSessionStoreRevisionRejectsASupersededToken(t *testing.T) {
	a := &app{values: settings.Default(), revision: 7}
	a.values.VKAccessToken = "NEWER-TOKEN"

	if _, err := a.sessionStoreRevision("OLDER-TOKEN"); !errors.Is(err, errReauthSuperseded) {
		t.Fatalf("superseded exchange error = %v, want errReauthSuperseded", err)
	}
	if a.values.VKAccessToken != "NEWER-TOKEN" {
		t.Fatalf("the newer token was changed: %q", a.values.VKAccessToken)
	}
	if _, err := a.sessionStoreRevision("  NEWER-TOKEN  "); err != nil {
		t.Fatalf("the live token was rejected: %v", err)
	}
	revision, err := a.sessionStoreRevision("NEWER-TOKEN")
	if err != nil || revision != 7 {
		t.Fatalf("revision = %d, err = %v; want 7, nil", revision, err)
	}
}

func storeSessionApp(caller *fakeSettingsCaller, token string) *app {
	a := &app{
		ctx:            context.Background(),
		env:            capsulad.Env{AppID: "org.capsulad.marusia"},
		values:         settings.Default(),
		revision:       7,
		settingsCaller: caller,
	}
	a.values.VKAccessToken = token
	return a
}

func TestStoreSessionRetriesAfterAStatusOnlyWrite(t *testing.T) {
	caller := &fakeSettingsCaller{revision: 8, staleFirst: true}
	a := storeSessionApp(caller, "TOKEN")
	a.settingsWait = 200 * time.Millisecond
	// The pushed status event reaches the local snapshot before the retry.
	caller.onGet = func() {
		a.mu.Lock()
		a.revision = 8
		a.mu.Unlock()
	}

	err := a.storeSession(context.Background(),
		vkSession{ID: "sid", Secret: "secret", AccountID: "acct"},
		"ok: session accepted from the VK access token", "TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if a.values.VKAccessToken != "" {
		t.Fatalf("the spent token was not cleared: %q", a.values.VKAccessToken)
	}
	if a.revision != 9 {
		t.Fatalf("revision = %d, want the retry's revision 9", a.revision)
	}
	if caller.updates != 2 || caller.gets != 1 {
		t.Fatalf("updates = %d gets = %d; want one stale attempt, a Get and a retry",
			caller.updates, caller.gets)
	}
	if len(caller.expected) != 2 || caller.expected[0] != "7" || caller.expected[1] != "8" {
		t.Fatalf("expected revisions = %v, want [7 8]", caller.expected)
	}
	if a.lastStatusWrite != "ok: session accepted from the VK access token" {
		t.Fatalf("lastStatusWrite = %q", a.lastStatusWrite)
	}
}

func TestStoreSessionSupersededOnAStaleToken(t *testing.T) {
	caller := &fakeSettingsCaller{revision: 7, staleFirst: true}
	a := storeSessionApp(caller, "TOKEN")
	caller.onUpdate = func() {
		a.mu.Lock()
		a.values.VKAccessToken = "NEWER"
		a.mu.Unlock()
	}

	err := a.storeSession(context.Background(),
		vkSession{ID: "sid", Secret: "secret", AccountID: "acct"},
		"ok: session accepted from the VK access token", "TOKEN")
	if !errors.Is(err, errReauthSuperseded) {
		t.Fatalf("error = %v, want errReauthSuperseded", err)
	}
	if caller.updates != 1 || caller.gets != 1 {
		t.Fatalf("updates = %d gets = %d; a superseded exchange must not retry",
			caller.updates, caller.gets)
	}
	if a.values.VKAccessToken != "NEWER" {
		t.Fatalf("the newer token was changed: %q", a.values.VKAccessToken)
	}
	if a.revision != 7 {
		t.Fatalf("revision = %d, want 7", a.revision)
	}
}

func TestStoreSessionSupersededBeforeTheTokenRetry(t *testing.T) {
	for name, tc := range map[string]struct {
		onGet func(*app)
		wait  time.Duration
	}{
		"token arrives during Get": {
			onGet: func(a *app) {
				a.mu.Lock()
				a.values.VKAccessToken = "NEWER"
				a.revision = 8
				a.mu.Unlock()
			},
		},
		"event timeout": {
			wait: 30 * time.Millisecond,
		},
	} {
		t.Run(name, func(t *testing.T) {
			caller := &fakeSettingsCaller{revision: 8, staleFirst: true}
			a := storeSessionApp(caller, "TOKEN")
			if tc.wait != 0 {
				a.settingsWait = tc.wait
			}
			if tc.onGet != nil {
				caller.onGet = func() { tc.onGet(a) }
			}

			err := a.storeSession(context.Background(),
				vkSession{ID: "sid", Secret: "secret", AccountID: "acct"},
				"ok: session accepted from the VK access token", "TOKEN")
			if !errors.Is(err, errReauthSuperseded) {
				t.Fatalf("error = %v, want errReauthSuperseded", err)
			}
			if caller.updates != 1 || caller.gets != 1 {
				t.Fatalf("updates = %d gets = %d; want one stale attempt and one Get",
					caller.updates, caller.gets)
			}
			wantToken := "TOKEN"
			wantRevision := uint64(7)
			if tc.onGet != nil {
				wantToken = "NEWER"
				wantRevision = 8
			}
			if a.values.VKAccessToken != wantToken {
				t.Fatalf("token = %q, want %q", a.values.VKAccessToken, wantToken)
			}
			if a.revision != wantRevision {
				t.Fatalf("revision = %d, want %d (no retry CAS)", a.revision, wantRevision)
			}
		})
	}
}

func TestRecordAuthStatusRetriesWithTheFreshRevision(t *testing.T) {
	caller := &fakeSettingsCaller{revision: 8, staleFirst: true}
	a := storeSessionApp(caller, "")
	a.values.LastAuthStatus = "ok: old status"

	a.recordAuthStatus(true, "session accepted from the VK access token")

	if caller.updates != 2 || caller.gets != 1 {
		t.Fatalf("updates = %d gets = %d; want a stale attempt, a Get and a retry",
			caller.updates, caller.gets)
	}
	if len(caller.expected) != 2 || caller.expected[0] != "7" || caller.expected[1] != "8" {
		t.Fatalf("expected revisions = %v, want [7 8]", caller.expected)
	}
	if a.revision != 9 {
		t.Fatalf("revision = %d, want 9", a.revision)
	}
	if a.lastStatusWrite != "ok: session accepted from the VK access token" {
		t.Fatalf("lastStatusWrite = %q", a.lastStatusWrite)
	}
}

func TestRecordAuthStatusSkipsAnUnchangedStatus(t *testing.T) {
	caller := &fakeSettingsCaller{revision: 7}
	a := storeSessionApp(caller, "")
	a.values.LastAuthStatus = "ok: session attach succeeded"

	a.recordAuthStatus(true, "session attach succeeded")
	if len(caller.calls) != 0 {
		t.Fatalf("an unchanged stored status wrote settings: %v", caller.calls)
	}

	a.values.LastAuthStatus = ""
	a.lastStatusWrite = "ok: session attach succeeded"
	a.recordAuthStatus(true, "session attach succeeded")
	if len(caller.calls) != 0 {
		t.Fatalf("an unchanged pending status wrote settings: %v", caller.calls)
	}

	a.recordAuthStatus(true, "session accepted from the VK access token")
	if caller.updates != 1 {
		t.Fatalf("updates = %d, want one write for a changed status", caller.updates)
	}
}

// TestAuthStatusTextIsShortAndCredentialFree fixes the form the settings page
// reads: a stable prefix, one line, bounded length and no credential material.
func TestAuthStatusTextIsShortAndCredentialFree(t *testing.T) {
	if got := authStatusText(true, "  session\n  accepted  "); got != "ok: session accepted" {
		t.Fatalf("success status = %q", got)
	}
	if got := authStatusText(false, ""); got != "failed: authorization failed" {
		t.Fatalf("empty failure status = %q", got)
	}
	long := strings.Repeat("x", 400)
	got := authStatusText(false, long)
	if !strings.HasPrefix(got, "failed: ") || !strings.HasSuffix(got, "…") {
		t.Fatalf("long status = %q", got)
	}
	if len([]rune(strings.TrimPrefix(got, "failed: "))) != 241 {
		t.Fatalf("long status was not capped: %d runes", len([]rune(got)))
	}
	const token = "vk-token-secret"
	if scrubbed := scrubToken("request "+token+" rejected", token); strings.Contains(scrubbed, token) {
		t.Fatalf("scrubToken leaked the token: %q", scrubbed)
	}
	redirect := "https://oauth.example/cb#access_token=" + token + "&user_id=1"
	if scrubbed := scrubSubmittedToken("request "+token+" rejected", redirect); strings.Contains(scrubbed, token) {
		t.Fatalf("scrubSubmittedToken leaked the parsed token: %q", scrubbed)
	}
}

// TestSettingsDocumentContract guards the packaging promises the app's own
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
		`key = "last_auth_status"`,
		`key = "vk_access_token"`,
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
		"vk_access_token",
		"last_auth_status",
		"onState",
		"<details",
		"addEventListener('keydown'",
		"event.key === 'Enter'",
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
