package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
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
		validateCA: func(pem []byte) error { return validateTrustRoots(env, pem) },
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
		go r.runEnrollment(start)
	}
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
// the token and the spent-code clear in one CAS. A stale revision means the
// settings changed while the request was in flight: the answer is dropped
// rather than writing one server's token into another server's configuration.
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

	err := r.commitEnrollment(e.ctx, creds.Token, revision)
	r.mu.Lock()
	e.committing = false
	r.mu.Unlock()
	if err != nil {
		if e.ctx.Err() == nil {
			if staleRevision(err) {
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
	if enrollmentKey(current, e.board) != e.key {
		r.enroll = nil
		r.mu.Unlock()
		return
	}
	next := current
	next.Token = creds.Token
	next.EnrollCode = ""
	next.Configured = true
	r.held = heldToken{token: creds.Token, code: e.cfg.EnrollCode, base: e.cfg.BaseURL}
	r.publishLocked(next, e.revision)
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

// commitEnrollment is the single compare-and-swap that stores the token and
// clears the code that produced it. The revision is the one from the settings
// snapshot whose inputs the exchange used; any intervening change makes the
// update stale and it is discarded.
func (r *runtime) commitEnrollment(ctx context.Context, token string, revision uint64) error {
	if r.env.AppID == "" {
		return errors.New("no framework application identity")
	}
	if revision == 0 {
		return errors.New("no settings revision")
	}
	tokenJSON, err := json.Marshal(token)
	if err != nil {
		return err
	}
	_, err = r.update(ctx, revision, []any{
		map[string]any{"key": settings.KeyDeviceToken, "valueJson": string(tokenJSON)},
		map[string]any{"key": settings.KeyEnrollCode, "clear": true},
	})
	return err
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
