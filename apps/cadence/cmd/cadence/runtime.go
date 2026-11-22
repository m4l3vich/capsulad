package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"capsulad/sdk/capsulad"

	"capsulad/apps/cadence/internal/enroll"
	"capsulad/apps/cadence/internal/link"
	"capsulad/apps/cadence/internal/redact"
	"capsulad/apps/cadence/internal/settings"
)

const (
	// configService is the framework's app-configuration service. The app may
	// update its own settings through it, which is how an enrollment token
	// becomes durable without a second storage API.
	configService = "capsulad.framework.v1.ApplicationConfigurationService"

	// settingsCallTimeout bounds one compare-and-swap call.
	settingsCallTimeout = 5 * time.Second
	// enrollmentTimeout bounds one code exchange.
	enrollmentTimeout = 30 * time.Second
)

// linkControl is the link surface a configuration change needs. *link.Client
// is the production implementation; tests substitute it.
type linkControl interface {
	SetBoard(board string)
	Wake()
}

// configCaller is the one framework call the runtime makes. *capsulad.Client
// is the production implementation; a test substitutes it to prove the
// compare-and-swap sequence.
type configCaller interface {
	CallJSON(ctx context.Context, targetAppID, service, rpc string, request, response any, timeout time.Duration) error
}

// runtime owns the live configuration and the state that crosses settings
// snapshots and the enrollment exchange. It is the one place that turns a
// snapshot into a link configuration.
type runtime struct {
	caller   configCaller
	env      capsulad.Env
	log      *log.Logger
	redactor *redact.Redactor
	link     linkControl
	model    string
	assets   string
	runCtx   context.Context
	// settingsWait bounds how long a stale enrollment retry waits for the
	// pushed settings event to reach the authoritative revision.
	settingsWait time.Duration
	// startEnrollment launches one planned exchange. It is a test seam; the
	// production path starts a goroutine.
	startEnrollment func(*enrollment)
	// validateCA is the SDK's trust-root parser, bound to env. It is a seam so
	// the rejection path can be tested without loading the native library.
	validateCA func([]byte) error

	current  atomic.Pointer[settings.Config]
	revision atomic.Uint64

	// mu guards the publication revision, the held enrollment token and the
	// live exchange. A settings event, the enrollment goroutine and the commit
	// can overlap, so current config and link publication happen under it.
	mu           sync.Mutex
	publishedRev uint64
	held         heldToken
	enroll       *enrollment
}

// heldToken is a token the exchange just obtained, kept until a stored
// snapshot carries it. If the token write is delayed or fails, a settings echo
// with the same code must not disconnect a device that already enrolled.
type heldToken struct {
	token string
	code  string
	base  string
}

// enrollment is one in-flight exchange. Its context is cancelled when the
// server, board, code or CA changes, so a superseded answer can never replace
// the live configuration.
type enrollment struct {
	key      string
	revision uint64
	board    string
	cfg      settings.Config
	ctx      context.Context
	cancel   context.CancelFunc
	// committing is set across the one CAS. The settings event that carries
	// the token this CAS wrote can arrive before the RPC returns; that event
	// must not cancel the exchange that produced it.
	committing bool
}

func newRuntime(ctx context.Context, client *capsulad.Client, env capsulad.Env, logger *log.Logger,
	redactor *redact.Redactor, linkClient *link.Client, model, assets string) *runtime {
	r := &runtime{
		caller: client, env: env, log: logger, redactor: redactor,
		link: linkClient, model: model, assets: assets, runCtx: ctx,
		settingsWait: 2 * time.Second,
		validateCA:   func(pem []byte) error { return validateTrustRoots(env, pem) },
	}
	r.current.Store(&settings.Config{HoldTail: time.Second, FollowUp: true})
	return r
}

func (r *runtime) config() settings.Config { return *r.current.Load() }

// validateTrustRoots asks the shared SDK to parse the PEM as trust roots. It
// opens no socket; OpenHTTP only builds the TLS root store. A label saying
// CERTIFICATE is not enough: the certificate bytes must be usable.
func validateTrustRoots(env capsulad.Env, pemBytes []byte) error {
	if len(pemBytes) == 0 {
		return nil
	}
	client, err := capsulad.OpenHTTP(env, capsulad.HTTPOptions{
		Roots: "pem", CAPEM: pemBytes, MaxBody: 1 << 10,
	})
	if err != nil {
		return err
	}
	return client.Close()
}

// mergeHeldToken keeps a freshly committed token when a settings snapshot
// still carries the spent code, because the token event may not have landed
// yet. A stored token wins; a different code or server clears the held value.
func mergeHeldToken(cfg settings.Config, held heldToken) (settings.Config, heldToken) {
	setConfigured := func(c settings.Config) settings.Config {
		c.Configured = c.BaseURL != "" && c.Token != ""
		return c
	}
	switch {
	case cfg.Token != "":
		return setConfigured(cfg), heldToken{}
	case held.token != "" && cfg.EnrollCode == held.code && cfg.BaseURL == held.base:
		cfg.Token = held.token
		// The code is single-use and already spent; do not exchange it again.
		cfg.EnrollCode = ""
		return setConfigured(cfg), held
	case cfg.EnrollCode != held.code || cfg.BaseURL != held.base:
		return setConfigured(cfg), heldToken{}
	default:
		return setConfigured(cfg), held
	}
}

func enrollmentKey(cfg settings.Config, board string) string {
	sum := sha256.Sum256(cfg.CAPEM)
	return cfg.BaseURL + "\x00" + board + "\x00" + cfg.EnrollCode + "\x00" + hex.EncodeToString(sum[:])
}

// sameRuntimeConfig reports whether two parsed snapshots describe the same
// link, enrollment and turn configuration. Status-only updates parse to the
// same Config; they must not republish (and wake) the link or invalidate an
// in-flight enrollment's compare-and-swap revision.
func sameRuntimeConfig(a, b settings.Config) bool {
	return a.BaseURL == b.BaseURL &&
		a.Token == b.Token &&
		a.EnrollCode == b.EnrollCode &&
		a.Board == b.Board &&
		a.HoldTail == b.HoldTail &&
		a.FollowUp == b.FollowUp &&
		a.Configured == b.Configured &&
		bytes.Equal(a.CAPEM, b.CAPEM)
}

// applySettings validates one app-private settings snapshot and installs it.
// A malformed value keeps the previous configuration and logs why; the token
// and the enrollment code are never logged.
func (r *runtime) applySettings(sc *capsulad.SettingsChanged) {
	if sc == nil {
		return
	}
	// A settings snapshot is a whole state, not a delta. An older revision
	// must never overwrite a newer one, especially the snapshot this app's own
	// enrollment write produces.
	if sc.Revision != 0 {
		for {
			last := r.revision.Load()
			if sc.Revision < last {
				r.log.Printf("settings revision=%d ignored; current revision is %d", sc.Revision, last)
				return
			}
			if r.revision.CompareAndSwap(last, sc.Revision) {
				break
			}
		}
	}
	values := make(settings.Values, len(sc.Values))
	for _, v := range sc.Values {
		values[v.Key] = v.ValueJSON
	}
	next, err := settings.Parse(values, os.ReadFile, func(rel string) ([]byte, error) {
		return settings.ReadAssetFile(r.assets, rel)
	})
	if err != nil {
		r.log.Printf("settings rejected: %v (keeping the previous configuration)", err)
		return
	}
	// Validate the trust roots before publishing a configuration. A failed
	// parse keeps the working link and its previous CA rather than dropping it
	// and failing every reconnect.
	if len(next.CAPEM) > 0 && r.validateCA != nil {
		if err := r.validateCA(next.CAPEM); err != nil {
			r.log.Printf("settings rejected: ca_pem is not a usable trust root (%v); keeping the previous configuration", err)
			return
		}
	}

	r.mu.Lock()
	next, r.held = mergeHeldToken(next, r.held)
	if r.publishedRev != 0 && sameRuntimeConfig(*r.current.Load(), next) {
		if sc.Revision != 0 && sc.Revision > r.publishedRev {
			r.publishedRev = sc.Revision
		}
		r.mu.Unlock()
		// The config is unchanged, but a re-saved enrollment code must still
		// retry a failed exchange. planEnrollment is a no-op for a same-key
		// live attempt and starts a fresh one when none is running.
		start, cancelOld := r.planEnrollment(next, sc.Revision)
		if cancelOld != nil {
			cancelOld()
		}
		if start != nil {
			r.launchEnrollment(start)
		}
		r.log.Printf("settings applied: no runtime change")
		return
	}
	published := r.publishLocked(next, sc.Revision)
	r.mu.Unlock()
	if !published {
		r.log.Printf("settings revision=%d ignored; a newer configuration is published", sc.Revision)
		return
	}
	for _, warning := range next.Warnings {
		r.log.Printf("%s", warning)
	}
	if next.Configured {
		r.log.Printf("settings applied server=%s board=%s", redact.URL(next.BaseURL), settings.BoardID(next.Board, r.model))
	} else if next.EnrollCode != "" {
		r.log.Printf("settings applied: enrollment code set; exchanging it for a device token")
	} else {
		r.log.Printf("settings applied: not configured; set server_url and device_token, an enrollment code, or a token file")
	}

	start, cancelOld := r.planEnrollment(next, sc.Revision)
	if cancelOld != nil {
		cancelOld()
	}
	if start != nil {
		r.launchEnrollment(start)
	}
}

// launchEnrollment starts one planned exchange. Tests substitute startEnrollment
// to observe the planned attempt without contacting a server.
func (r *runtime) launchEnrollment(e *enrollment) {
	if e == nil {
		return
	}
	if r.startEnrollment != nil {
		r.startEnrollment(e)
		return
	}
	go r.runEnrollment(e)
}

// planEnrollment keeps one exchange per server/board/code/CA. A settings
// change with different inputs cancels the old exchange; an unchanged key
// only advances the revision the commit must match.
func (r *runtime) planEnrollment(cfg settings.Config, revision uint64) (start *enrollment, cancelOld context.CancelFunc) {
	board := settings.BoardID(cfg.Board, r.model)
	r.mu.Lock()
	defer r.mu.Unlock()
	if cfg.Token != "" || cfg.EnrollCode == "" || cfg.BaseURL == "" || r.runCtx == nil || r.runCtx.Err() != nil {
		if r.enroll != nil {
			if r.enroll.committing && cfg.Token != "" && cfg.EnrollCode == "" {
				// This is the echo of our own token CAS, not a new edit.
				return nil, nil
			}
			cancelOld = r.enroll.cancel
			r.enroll = nil
		}
		return nil, cancelOld
	}
	key := enrollmentKey(cfg, board)
	if r.enroll != nil {
		if r.enroll.key == key {
			r.enroll.revision = revision
			return nil, nil
		}
		cancelOld = r.enroll.cancel
		r.enroll = nil
	}
	ctx, cancel := context.WithCancel(r.runCtx)
	e := &enrollment{key: key, revision: revision, board: board, cfg: cfg, ctx: ctx, cancel: cancel}
	r.enroll = e
	return e, cancelOld
}

// runEnrollment spends one dashboard code. Its result is installed only after
// a single compare-and-swap commits both the token and the matching code clear
// at the revision that started the exchange.
func (r *runtime) runEnrollment(e *enrollment) {
	defer e.cancel()
	ctx, cancel := context.WithTimeout(e.ctx, enrollmentTimeout)
	defer cancel()
	httpClient, err := capsulad.OpenHTTP(r.env, capsulad.HTTPOptions{
		Roots: "system", CAPEM: e.cfg.CAPEM,
		UserAgent:      "capsulad-cadence/" + appVersion,
		MaxBody:        64 << 10,
		ConnectTimeout: 10 * time.Second,
		ReadTimeout:    15 * time.Second,
		TotalTimeout:   enrollmentTimeout,
	})
	if err != nil {
		if e.ctx.Err() == nil {
			r.log.Printf("enroll_failed: HTTP client: %v", err)
		}
		r.endAttempt(e)
		return
	}
	defer func() { _ = httpClient.Close() }()

	creds, err := enroll.Exchange(ctx, httpClient, e.cfg.BaseURL, e.board, e.cfg.EnrollCode)
	if err != nil {
		if e.ctx.Err() != nil {
			return
		}
		switch {
		case errors.Is(err, enroll.ErrRefused):
			hint := "create a new dashboard code for " + e.board
			if e.board != settings.BoardSim {
				hint += ", or set board=sim when the dashboard's default Simulator was picked"
			}
			r.log.Printf("enroll_refused board=%s: the code is unknown, expired, already used, or was made for another board; %s", e.board, hint)
		case errors.Is(err, enroll.ErrMalformed):
			r.log.Printf("enroll_failed: the server's enrollment answer was not the documented object")
		default:
			r.log.Printf("enroll_failed: the server could not be reached; check server_url and the CA trust setting; the code stays set")
		}
		r.endAttempt(e)
		return
	}
	r.finishEnrollment(e, creds)
}

// finishEnrollment checks the exchange still owns the live inputs, then commits
// the token and the spent-code clear. A stale revision caused by a status-only
// write is retried against the fresh revision; a changed enrollment input
// aborts, so one server's token is never written into another's configuration.
func (r *runtime) finishEnrollment(e *enrollment, creds enroll.Credentials) {
	r.mu.Lock()
	if r.enroll != e || e.ctx.Err() != nil {
		r.mu.Unlock()
		return
	}
	current := *r.current.Load()
	if enrollmentKey(current, e.board) != e.key {
		r.enroll = nil
		r.mu.Unlock()
		return
	}
	revision := e.revision
	e.committing = true
	r.mu.Unlock()

	nextRevision, err := r.commitEnrollment(e.ctx, e, creds.Token, revision)
	r.mu.Lock()
	e.committing = false
	r.mu.Unlock()
	if err != nil {
		if e.ctx.Err() == nil {
			if errors.Is(err, errEnrollmentSuperseded) || staleRevision(err) {
				r.log.Printf("enroll_superseded: settings changed during enrollment; the token was not stored and the spent code needs a new one")
			} else {
				r.log.Printf("enroll_warning: the token could not be stored (%v); it is not used", err)
			}
		}
		r.endAttempt(e)
		return
	}

	// The CAS already committed both fields. Log the outcome even if the
	// settings event carrying the new token arrives before this goroutine
	// finishes: that event is evidence of success, not a superseding edit.
	// The shared redactor is changed only by publishLocked, so an older
	// completion cannot unmask a newer published token or code.
	r.log.Printf("enroll_ok board=%s", e.board)
	r.log.Printf("enroll_stored: the device token is saved and the enrollment code is spent")

	r.mu.Lock()
	if r.enroll != e {
		r.mu.Unlock()
		return
	}
	current = *r.current.Load()
	if enrollmentKey(current, e.board) != e.key || current.Token != e.cfg.Token {
		r.enroll = nil
		r.mu.Unlock()
		return
	}
	next := current
	next.Token = creds.Token
	next.EnrollCode = ""
	next.Configured = true
	r.held = heldToken{token: creds.Token, code: e.cfg.EnrollCode, base: e.cfg.BaseURL}
	r.storeRevision(nextRevision)
	r.publishLocked(next, nextRevision)
	r.enroll = nil
	r.mu.Unlock()
}

// publishLocked makes cfg the live configuration and link target under one
// lock. A publication older than the last one is refused, so an enrollment
// finishing after a newer settings callback cannot overwrite it.
func (r *runtime) publishLocked(cfg settings.Config, revision uint64) bool {
	if revision != 0 {
		if revision < r.publishedRev {
			return false
		}
		r.publishedRev = revision
	}
	r.current.Store(&cfg)
	held := r.held.token
	r.redactor.Set(cfg.Token, cfg.EnrollCode, held)
	r.link.SetBoard(settings.BoardID(cfg.Board, r.model))
	// Re-dial so a changed server, credential, CA or board takes effect now.
	r.link.Wake()
	return true
}

// endAttempt clears a finished exchange so a later settings save can retry.
func (r *runtime) endAttempt(e *enrollment) {
	r.mu.Lock()
	if r.enroll == e {
		e.cancel()
		r.enroll = nil
	}
	r.mu.Unlock()
}

// errEnrollmentSuperseded means the settings inputs changed while the
// enrollment retry was running. The obtained token is not stored into a
// different configuration.
var errEnrollmentSuperseded = errors.New("enrollment superseded by a newer settings change")

// commitEnrollment is the single compare-and-swap that stores the token and
// clears the code that produced it. The revision is the one from the settings
// snapshot whose inputs the exchange used; a concurrent settings write
// (including last_test_status) makes that update stale, so the already-obtained
// token is retried against the fresh revision rather than re-spending the code.
func (r *runtime) commitEnrollment(ctx context.Context, e *enrollment, token string, revision uint64) (uint64, error) {
	if r.env.AppID == "" {
		return 0, errors.New("no framework application identity")
	}
	if revision == 0 {
		return 0, errors.New("no settings revision")
	}
	tokenJSON, err := json.Marshal(token)
	if err != nil {
		return 0, err
	}
	changes := []any{
		map[string]any{"key": settings.KeyDeviceToken, "valueJson": string(tokenJSON)},
		map[string]any{"key": settings.KeyEnrollCode, "clear": true},
	}
	var lastErr error
	for attempt := range 4 {
		if attempt > 0 {
			snapshot, err := r.settingsRevision(ctx)
			if err != nil {
				return 0, err
			}
			revision = snapshot.revision
			// Never CAS a masked input the process has not observed. Wait for
			// the pushed settings event to reach this revision; a replaced
			// enroll_code is only visible in the local app-private snapshot.
			if !r.waitForAppliedRevision(ctx, revision) {
				return 0, errEnrollmentSuperseded
			}
			r.mu.Lock()
			local := *r.current.Load()
			ours := r.enroll == e && e.ctx.Err() == nil &&
				local.BaseURL == e.cfg.BaseURL &&
				local.Board == e.cfg.Board &&
				local.EnrollCode == e.cfg.EnrollCode &&
				local.Token == e.cfg.Token &&
				bytes.Equal(local.CAPEM, e.cfg.CAPEM)
			r.mu.Unlock()
			// The authoritative snapshot, not just the lagging local copy,
			// decides whether this enrollment still describes the stored
			// inputs. A concurrent admin edit must not be clobbered by the
			// retry. The local check catches a masked change whose app-private
			// event has now arrived.
			if !ours || !r.enrollmentInputsMatch(e, snapshot) {
				return 0, errEnrollmentSuperseded
			}
		}
		newRevision, err := r.update(ctx, revision, changes)
		if err == nil {
			return newRevision, nil
		}
		lastErr = err
		if ctx.Err() != nil || !staleRevision(err) {
			return 0, err
		}
	}
	return 0, lastErr
}

// waitForAppliedRevision waits, bounded, for the pushed settings event at
// revision to be applied locally. publishedRev only advances after the snapshot
// has been parsed and installed (or accepted as a no-runtime-change update).
func (r *runtime) waitForAppliedRevision(ctx context.Context, revision uint64) bool {
	if revision == 0 {
		return false
	}
	wait := r.settingsWait
	if wait <= 0 {
		wait = 2 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		r.mu.Lock()
		applied := r.publishedRev
		r.mu.Unlock()
		if applied >= revision {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		pause := 25 * time.Millisecond
		if remaining < pause {
			pause = remaining
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

type snapshotValue struct {
	valueJSON string
	hasValue  bool
}

// settingsSnapshot is the authoritative app-settings read. The Get contract
// redacts masked values, so enroll_code and device_token are represented by
// presence; the non-masked inputs are exact.
type settingsSnapshot struct {
	revision uint64
	values   map[string]snapshotValue
}

func (s settingsSnapshot) text(key string) string {
	entry, ok := s.values[key]
	if !ok || entry.valueJSON == "" {
		return ""
	}
	var text string
	if err := json.Unmarshal([]byte(entry.valueJSON), &text); err != nil {
		return ""
	}
	return text
}

func (s settingsSnapshot) has(key string) bool {
	entry, ok := s.values[key]
	return ok && entry.hasValue
}

// settingsRevision reads the authoritative app-settings revision and values so
// a stale enrollment commit can be retried only when its inputs are unchanged.
func (r *runtime) settingsRevision(ctx context.Context) (settingsSnapshot, error) {
	if r.env.AppID == "" {
		return settingsSnapshot{}, errors.New("no framework application identity")
	}
	request := map[string]any{"appId": r.env.AppID}
	var response map[string]any
	callCtx, cancel := context.WithTimeout(ctx, settingsCallTimeout)
	defer cancel()
	if err := r.caller.CallJSON(callCtx, "", configService, "Get", request, &response, settingsCallTimeout); err != nil {
		return settingsSnapshot{}, err
	}
	revision, ok := revisionOf(response["revision"])
	if !ok || revision == 0 {
		return settingsSnapshot{}, errors.New("settings snapshot has no revision")
	}
	snapshot := settingsSnapshot{revision: revision, values: map[string]snapshotValue{}}
	if values, ok := response["values"].([]any); ok {
		for _, raw := range values {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			key, _ := entry["key"].(string)
			if key == "" {
				continue
			}
			valueJSON, _ := entry["valueJson"].(string)
			hasValue, _ := entry["hasValue"].(bool)
			snapshot.values[key] = snapshotValue{valueJSON: valueJSON, hasValue: hasValue}
		}
	}
	return snapshot, nil
}

// enrollmentInputsMatch compares the authoritative snapshot to the configuration
// that started the exchange. Masked code/token values are compared by presence
// because Get redacts them; server, board and CA are compared by value.
func (r *runtime) enrollmentInputsMatch(e *enrollment, snapshot settingsSnapshot) bool {
	if normalizedServerURL(snapshot.text(settings.KeyServerURL)) != e.cfg.BaseURL {
		return false
	}
	board := strings.TrimSpace(snapshot.text(settings.KeyBoard))
	if board == "" {
		board = settings.BoardAuto
	}
	if board != e.cfg.Board {
		return false
	}
	if !r.sameCA(e.cfg.CAPEM, snapshot.text(settings.KeyCAPEM)) {
		return false
	}
	if snapshot.has(settings.KeyEnrollCode) != (e.cfg.EnrollCode != "") {
		return false
	}
	tokenPresent := snapshot.has(settings.KeyDeviceToken) ||
		strings.TrimSpace(snapshot.text(settings.KeyDeviceTokenFile)) != ""
	return tokenPresent == (e.cfg.Token != "")
}

func normalizedServerURL(raw string) string {
	text := strings.TrimSpace(raw)
	if text == "" {
		return ""
	}
	u, err := url.Parse(text)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "\x00invalid"
	}
	return u.Scheme + "://" + u.Host + strings.TrimRight(u.Path, "/")
}

func (r *runtime) sameCA(current []byte, raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return len(current) == 0
	}
	if strings.Contains(raw, "-----BEGIN") {
		return bytes.Equal([]byte(raw), current)
	}
	rel, err := settings.NormalizeAssetPath(raw)
	if err != nil {
		return false
	}
	pemBytes, err := settings.ReadAssetFile(r.assets, rel)
	if err != nil {
		return false
	}
	return bytes.Equal(pemBytes, current)
}

// update performs one compare-and-swap update and returns the new revision.
func (r *runtime) update(ctx context.Context, revision uint64, changes []any) (uint64, error) {
	request := map[string]any{
		"appId":            r.env.AppID,
		"expectedRevision": strconv.FormatUint(revision, 10),
		"changes":          changes,
	}
	var response map[string]any
	callCtx, cancel := context.WithTimeout(ctx, settingsCallTimeout)
	defer cancel()
	if err := r.caller.CallJSON(callCtx, "", configService, "Update", request, &response, settingsCallTimeout); err != nil {
		return 0, err
	}
	rev, ok := revisionOf(response["revision"])
	if !ok {
		return 0, errors.New("settings update has no revision")
	}
	return rev, nil
}

// revisionOf reads a 64-bit revision encoded as canonical protobuf JSON (a
// string) or as a number.
func revisionOf(raw any) (uint64, bool) {
	switch value := raw.(type) {
	case string:
		parsed, err := strconv.ParseUint(value, 10, 64)
		return parsed, err == nil
	case float64:
		if value < 0 {
			return 0, false
		}
		return uint64(value), true
	case json.Number:
		parsed, err := strconv.ParseUint(string(value), 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func staleRevision(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "stale") || strings.Contains(text, "aborted")
}

// testConnection checks DNS, TCP and TLS to the configured server without a
// device token. It is the pre-enrollment step: it proves the URL and the CA
// trust roots work before a one-time code is spent. The test itself opens no
// device link; the non-secret outcome is also written to last_test_status for
// the app's own settings page.
func (r *runtime) testConnection(ctx context.Context) error {
	err := r.probeConnection(ctx)
	r.writeTestStatus(err)
	return err
}

func (r *runtime) probeConnection(ctx context.Context) error {
	cfg := r.config()
	if cfg.BaseURL == "" {
		err := errors.New("no Cadence server URL configured")
		r.log.Printf("test_failed: %v", err)
		return err
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Hostname() == "" {
		err = errors.New("the Cadence server URL is not valid")
		r.log.Printf("test_failed server=%s: %v", redact.URL(cfg.BaseURL), err)
		return err
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		err = errors.New("the Cadence server URL has an invalid port")
		r.log.Printf("test_failed server=%s: %v", redact.URL(cfg.BaseURL), err)
		return err
	}
	testCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	stream, err := capsulad.OpenTCP(testCtx, r.env, u.Hostname(), uint16(portNumber), capsulad.TCPOptions{
		TLS:        u.Scheme == "https",
		Roots:      "system",
		CAPEM:      cfg.CAPEM,
		ServerName: u.Hostname(),
	})
	if err != nil {
		r.log.Printf("test_failed server=%s: %v", redact.URL(cfg.BaseURL), err)
		return err
	}
	_ = stream.Close()
	r.log.Printf("test_ok server=%s", redact.URL(cfg.BaseURL))
	return nil
}

// writeTestStatus persists the last connection-test outcome. It is a separate
// non-secret setting so the settings page can show the result without reading
// the app log; a stale revision is retried, and a persistent failure is logged
// rather than overwriting a newer revision. The stored text never carries a
// token or an enrollment code.
func (r *runtime) writeTestStatus(probeErr error) {
	if r.env.AppID == "" {
		return
	}
	status := "test_ok"
	if probeErr != nil {
		reason := strings.Join(strings.Fields(r.redactor.Error(probeErr)), " ")
		if reason == "" {
			reason = "connection failed"
		}
		if runes := []rune(reason); len(runes) > 240 {
			reason = string(runes[:240]) + "…"
		}
		status = "test_failed: " + reason
	}
	valueJSON, err := json.Marshal(status)
	if err != nil {
		return
	}
	base := r.runCtx
	if base == nil {
		base = context.Background()
	}
	revision := r.revision.Load()
	for range 2 {
		if revision == 0 {
			return
		}
		newRevision, updateErr := r.putTestStatus(base, revision, valueJSON)
		if updateErr == nil {
			r.storeRevision(newRevision)
			return
		}
		if base.Err() != nil || !staleRevision(updateErr) {
			r.log.Printf("test_status: the result could not be stored: %v", r.redactor.Error(updateErr))
			return
		}
		snapshot, err := r.settingsRevision(base)
		if err != nil {
			r.log.Printf("test_status: could not read the fresh settings revision: %v", r.redactor.Error(err))
			return
		}
		revision = snapshot.revision
	}
	r.log.Printf("test_status: the result could not be stored; a newer settings revision won")
}

func (r *runtime) putTestStatus(base context.Context, revision uint64, valueJSON []byte) (uint64, error) {
	ctx, cancel := context.WithTimeout(base, settingsCallTimeout)
	defer cancel()
	return r.update(ctx, revision, []any{
		map[string]any{"key": settings.KeyLastTestStatus, "valueJson": string(valueJSON)},
	})
}

func (r *runtime) storeRevision(revision uint64) {
	if revision > r.revision.Load() {
		r.revision.Store(revision)
	}
}
