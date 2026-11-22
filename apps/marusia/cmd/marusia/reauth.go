package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"time"

	"capsulad/apps/marusia/internal/settings"
	"capsulad/apps/marusia/internal/vkclient"
	"capsulad/sdk/capsulad"
)

// Reauthorization deliberately lives beside the turn loop: the VK access token
// is a one-shot input that becomes a durable session, and every failure names
// the class without echoing the token or a response body. Networking uses the
// shared SDK's HTTP client, never a second stack.

const (
	// configService is the framework's app-configuration service; an app may
	// update its own settings through it.
	configService = "capsulad.framework.v1.ApplicationConfigurationService"
	// vkAPIOrigin is VK's public API, the same host the setup script uses to
	// resolve the user id behind an access token.
	vkAPIOrigin = "https://api.vk.com"
	// vkExchangeTimeout bounds one VK or vendor HTTP exchange.
	vkExchangeTimeout = 20 * time.Second
	// vkResponseLimit keeps a hostile or broken endpoint from streaming an
	// unbounded JSON body into this small helper.
	vkResponseLimit = 1 << 20
)

type vkSession struct {
	ID        string
	Secret    string
	AccountID string
}

// errReauthSuperseded means a newer VK access token was submitted while this
// exchange was running. The newer token stays pending; this exchange must not
// clear or overwrite it.
var errReauthSuperseded = errors.New("marusia: a newer VK access token is pending")

// reauthWithToken exchanges a VK access token (or the redirect URL carrying
// one) for a session, validates the session against the configured endpoint,
// and only then stores it and clears the one-shot token. It returns the
// operator-facing detail and, on failure, the error that caused it; the
// non-secret outcome is also written to last_auth_status. A failure leaves the
// token in place so the operator can retry without pasting it again.
func (a *app) reauthWithToken(config settings.Config, raw string) (string, error) {
	ctx, cancel := context.WithTimeout(a.ctx, 75*time.Second)
	defer cancel()

	if strings.TrimSpace(config.DeviceID) == "" {
		// Registration and the following session probe must present the same
		// identity; the process-local id is otherwise generated separately.
		config.DeviceID = vkclient.NewUUID()
	}
	if strings.HasPrefix(strings.ToLower(config.BaseURL), "http://") {
		log.Printf("marusia: WARN the VK access token is sent to %s without TLS; "+
			"a passive observer can read it", config.BaseURL)
	}

	session, err := provisionVK(ctx, a.env, config.BaseURL, raw, config.DeviceID)
	if err != nil {
		detail := scrubSubmittedToken(err.Error(), raw)
		log.Printf("marusia: reauth failed: %s", detail)
		a.recordAuthStatus(false, detail)
		return detail, err
	}

	protocol, err := vkclient.New(vkclient.Config{
		BaseURL:       config.BaseURL,
		XTokenID:      config.XTokenID,
		Serial:        config.Serial,
		DeviceID:      config.DeviceID,
		Session:       vkclient.Session{SessionID: session.ID, SessionSecret: session.Secret},
		ClientVersion: config.ClientVersion,
	})
	if err != nil {
		detail := err.Error()
		log.Printf("marusia: reauth failed: %s", detail)
		a.recordAuthStatus(false, detail)
		return detail, err
	}
	defer protocol.Close()

	if err := protocol.Authorize(ctx); err != nil {
		detail := describeReauth(err)
		log.Printf("marusia: reauth failed: %s", detail)
		a.recordAuthStatus(false, detail)
		return detail, err
	}

	success := "session accepted from the VK access token"
	if err := a.storeSession(ctx, session, authStatusText(true, success), raw); err != nil {
		if errors.Is(err, errReauthSuperseded) {
			detail := "a newer VK access token is pending; this exchange was not stored"
			log.Printf("marusia: reauth: %s", detail)
			return detail, err
		}
		detail := "the session was accepted but could not be saved; try again"
		log.Printf("marusia: reauth: %s (%v)", detail, err)
		a.recordAuthStatus(false, detail)
		return detail, err
	}

	logReauthSuccess("the VK access token", vkclient.Session{SessionID: session.ID, AccountID: session.AccountID})
	return success, nil
}

// storeSession writes the session, clears the one-shot token and records the
// non-secret outcome in one compare-and-swap. The revision and the live
// pending token are captured under one lock: a newer token submitted while the
// exchange ran is left pending, and a status-only concurrent write is retried
// against the fresh revision so a valid session is still saved.
func (a *app) storeSession(ctx context.Context, session vkSession, status, exchangedToken string) error {
	if a.env.AppID == "" {
		return errors.New("no framework application identity")
	}
	revision, err := a.sessionStoreRevision(exchangedToken)
	if err != nil {
		return err
	}
	idJSON, _ := json.Marshal(session.ID)
	secretJSON, _ := json.Marshal(session.Secret)
	statusJSON, _ := json.Marshal(status)
	changes := []any{
		map[string]any{"key": settings.KeySessionID, "valueJson": string(idJSON)},
		map[string]any{"key": settings.KeySessionSecret, "valueJson": string(secretJSON)},
		map[string]any{"key": settings.KeyLastAuthStatus, "valueJson": string(statusJSON)},
		map[string]any{"key": settings.KeyVKAccessToken, "clear": true},
	}
	var lastErr error
	for attempt := range 3 {
		if attempt > 0 {
			fresh, err := a.settingsRevision(ctx)
			if err != nil {
				return err
			}
			revision = fresh
			// A newer token whose pushed event is delayed must be observed
			// before this attempt can clear it. Wait for the local snapshot to
			// reach the authoritative revision, then re-read the token.
			if !a.waitForRevision(ctx, revision) {
				return errReauthSuperseded
			}
			a.mu.Lock()
			live := strings.TrimSpace(a.values.VKAccessToken)
			a.mu.Unlock()
			if live != strings.TrimSpace(exchangedToken) {
				return errReauthSuperseded
			}
		}
		request := map[string]any{
			"appId":            a.env.AppID,
			"expectedRevision": strconv.FormatUint(revision, 10),
			"changes":          changes,
		}
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		var response map[string]any
		callErr := a.callSettings(callCtx, "Update", request, &response, 10*time.Second)
		cancel()
		if callErr == nil {
			a.mu.Lock()
			if strings.TrimSpace(a.values.VKAccessToken) == strings.TrimSpace(exchangedToken) {
				a.values.VKAccessToken = ""
			}
			if rev, ok := revisionOf(response["revision"]); ok && rev > a.revision {
				a.revision = rev
			}
			a.lastStatusWrite = strings.TrimSpace(status)
			a.mu.Unlock()
			return nil
		}
		lastErr = callErr
		if ctx.Err() != nil || !staleRevision(callErr) {
			return callErr
		}
	}
	return lastErr
}

// sessionStoreRevision reads the revision and verifies, under the same lock,
// that the live pending token is still the one this exchange spent.
func (a *app) sessionStoreRevision(exchangedToken string) (uint64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.values.VKAccessToken) != strings.TrimSpace(exchangedToken) {
		return 0, errReauthSuperseded
	}
	if a.revision == 0 {
		return 0, errors.New("no settings revision")
	}
	return a.revision, nil
}

// settingsRevision reads the authoritative revision after a stale session
// write so the store can be retried without re-spending the token.
func (a *app) settingsRevision(ctx context.Context) (uint64, error) {
	if a.env.AppID == "" {
		return 0, errors.New("no framework application identity")
	}
	request := map[string]any{"appId": a.env.AppID}
	var response map[string]any
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := a.callSettings(callCtx, "Get", request, &response, 10*time.Second); err != nil {
		return 0, err
	}
	revision, ok := revisionOf(response["revision"])
	if !ok || revision == 0 {
		return 0, errors.New("settings snapshot has no revision")
	}
	return revision, nil
}

// waitForRevision waits, bounded, for the pushed settings event at revision to
// be applied locally. a.revision and a.values are updated under the same lock,
// so once the revision is reached the masked token is the observed one.
func (a *app) waitForRevision(ctx context.Context, revision uint64) bool {
	if revision == 0 {
		return false
	}
	wait := a.settingsWait
	if wait <= 0 {
		wait = 2 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		a.mu.Lock()
		applied := a.revision
		a.mu.Unlock()
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

// callSettings uses the test seam when present and the framework client
// otherwise.
func (a *app) callSettings(ctx context.Context, rpc string, request, response any, timeout time.Duration) error {
	if a.settingsCaller != nil {
		return a.settingsCaller.CallJSON(ctx, "", configService, rpc, request, response, timeout)
	}
	return a.client.CallJSON(ctx, "", configService, rpc, request, response, timeout)
}

// recordAuthStatus writes the non-secret outcome of an exchange or attach. A
// status is a convenience for the app's settings page, not a credential; a
// concurrent settings edit is retried, and a persistent failure is logged
// rather than overwriting a newer revision. An unchanged status is a no-op so
// a successful turn attach does not bump the settings revision on every turn.
func (a *app) recordAuthStatus(ok bool, detail string) {
	if a.env.AppID == "" {
		return
	}
	text := authStatusText(ok, detail)
	a.mu.Lock()
	stored := strings.TrimSpace(a.values.LastAuthStatus)
	pending := strings.TrimSpace(a.lastStatusWrite)
	a.mu.Unlock()
	if text == stored || (pending != "" && text == pending) {
		return
	}
	valueJSON, err := json.Marshal(text)
	if err != nil {
		return
	}
	base := a.ctx
	if base == nil {
		base = context.Background()
	}
	a.mu.Lock()
	revision := a.revision
	a.mu.Unlock()
	for range 2 {
		if revision == 0 {
			return
		}
		callErr := a.putAuthStatus(base, strconv.FormatUint(revision, 10), valueJSON)
		if callErr == nil {
			a.mu.Lock()
			a.lastStatusWrite = text
			a.mu.Unlock()
			return
		}
		if base.Err() != nil || !staleRevision(callErr) {
			log.Printf("marusia: could not record the auth status: %v", callErr)
			return
		}
		fresh, err := a.settingsRevision(base)
		if err != nil {
			log.Printf("marusia: could not read the fresh settings revision: %v", err)
			return
		}
		revision = fresh
	}
	log.Printf("marusia: could not record the auth status; a newer settings revision won")
}

func (a *app) putAuthStatus(base context.Context, expectedRevision string, valueJSON []byte) error {
	request := map[string]any{
		"appId":            a.env.AppID,
		"expectedRevision": expectedRevision,
		"changes": []any{
			map[string]any{"key": settings.KeyLastAuthStatus, "valueJson": string(valueJSON)},
		},
	}
	callCtx, cancel := context.WithTimeout(base, 10*time.Second)
	defer cancel()
	var response map[string]any
	if err := a.callSettings(callCtx, "Update", request, &response, 10*time.Second); err != nil {
		return err
	}
	a.mu.Lock()
	if revision, ok := revisionOf(response["revision"]); ok && revision > a.revision {
		a.revision = revision
	}
	a.mu.Unlock()
	return nil
}

// authStatusText normalizes one outcome into the short, credential-free text
// the settings page reads: "ok: …" or "failed: …".
func authStatusText(ok bool, detail string) string {
	text := strings.Join(strings.Fields(detail), " ")
	if text == "" {
		if ok {
			text = "session accepted"
		} else {
			text = "authorization failed"
		}
	}
	if runes := []rune(text); len(runes) > 240 {
		text = string(runes[:240]) + "…"
	}
	if ok {
		return "ok: " + text
	}
	return "failed: " + text
}

// scrubToken removes a submitted one-shot value from an error message.
// Transport errors can quote a request URL, and the token is the credential
// in it.
func scrubToken(text, token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return text
	}
	return strings.ReplaceAll(text, token, "[redacted]")
}

// scrubSubmittedToken removes both the raw submission and the access token
// parsed out of a redirect URL, so a message that quotes either form is safe.
func scrubSubmittedToken(text, raw string) string {
	text = scrubToken(text, raw)
	if token, _, err := parseVKToken(raw); err == nil {
		text = scrubToken(text, token)
	}
	return text
}

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

// provisionVK mirrors the setup script's registration calls. Only the session
// is returned: a session is what the attachment needs, and no manufacturing
// xtoken is invented for an existing device.
func provisionVK(ctx context.Context, env capsulad.Env, baseURL, raw, deviceID string) (vkSession, error) {
	token, userID, err := parseVKToken(raw)
	if err != nil {
		return vkSession{}, err
	}
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return vkSession{}, errors.New("marusia: reauth needs a service endpoint")
	}
	client, err := capsulad.OpenHTTP(env, capsulad.HTTPOptions{
		Roots:           "system",
		MaxBody:         vkResponseLimit,
		FollowRedirects: 0,
		ConnectTimeout:  15 * time.Second,
		ReadTimeout:     vkExchangeTimeout,
		TotalTimeout:    vkExchangeTimeout,
	})
	if err != nil {
		return vkSession{}, fmt.Errorf("marusia: HTTP client: %w", err)
	}
	defer client.Close()

	if userID == "" {
		userID, err = lookupVKUser(ctx, client, token)
		if err != nil {
			return vkSession{}, err
		}
	}
	device := strings.TrimSpace(deviceID)
	if device == "" {
		device = vkclient.NewUUID()
	}
	params := url.Values{
		"vk_access_token":   {token},
		"vk_user_id":        {userID},
		"device_id":         {device},
		"device_ver":        {"1.91.5"},
		"client_request_id": {vkclient.NewUUID()},
	}
	var registration struct {
		Result struct {
			Token string `json:"token"`
		} `json:"result"`
	}
	if err := vkPost(ctx, client, base+"/registration/by_vk", params, &registration); err != nil {
		return vkSession{}, fmt.Errorf("marusia: registration/by_vk: %w", err)
	}
	if registration.Result.Token == "" {
		return vkSession{}, errors.New("marusia: registration/by_vk returned no registration token")
	}
	sessionParams := url.Values{
		"reg_token":         {registration.Result.Token},
		"with_secret":       {"1"},
		"with_account_info": {"1"},
		"accept_tos":        {"1"},
		"accept_adv_mail":   {"DEFAULT"},
		"device_id":         {device},
		"device_ver":        {"1.91.5"},
		"client_request_id": {vkclient.NewUUID()},
	}
	var body struct {
		Result struct {
			SessionID     string `json:"session_id"`
			SessionSecret string `json:"session_secret"`
			AccountID     any    `json:"account_id"`
		} `json:"result"`
	}
	if err := vkPost(ctx, client, base+"/registration/get_session", sessionParams, &body); err != nil {
		return vkSession{}, fmt.Errorf("marusia: registration/get_session: %w", err)
	}
	if body.Result.SessionID == "" || body.Result.SessionSecret == "" {
		return vkSession{}, errors.New("marusia: registration/get_session returned an incomplete session")
	}
	return vkSession{
		ID:        body.Result.SessionID,
		Secret:    body.Result.SessionSecret,
		AccountID: fmt.Sprint(body.Result.AccountID),
	}, nil
}

func lookupVKUser(ctx context.Context, client *capsulad.HTTPClient, token string) (string, error) {
	endpoint := vkAPIOrigin + "/method/users.get?" + url.Values{
		"access_token": {token},
		"v":            {"5.199"},
	}.Encode()
	response, err := client.Do(ctx, capsulad.HTTPRequest{
		Method: "GET", URL: endpoint,
		Header: map[string]string{"Accept": "application/json"},
		Timeout: vkExchangeTimeout,
	})
	if err != nil {
		return "", errors.New("marusia: users.get could not reach VK")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("marusia: users.get returned HTTP %d", response.StatusCode)
	}
	var body struct {
		Response []struct {
			ID int64 `json:"id"`
		} `json:"response"`
		Error *struct {
			Code int `json:"error_code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body, &body); err != nil {
		return "", errors.New("marusia: users.get returned invalid JSON")
	}
	if body.Error != nil {
		return "", fmt.Errorf("marusia: users.get failed with VK error %d", body.Error.Code)
	}
	if len(body.Response) == 0 || body.Response[0].ID == 0 {
		return "", errors.New("marusia: users.get returned no user id")
	}
	return strconv.FormatInt(body.Response[0].ID, 10), nil
}

func vkPost(ctx context.Context, client *capsulad.HTTPClient, endpoint string, params url.Values, target any) error {
	response, err := client.Do(ctx, capsulad.HTTPRequest{
		Method: "POST", URL: endpoint + "?" + params.Encode(),
		Header: map[string]string{"Accept": "application/json"},
		Timeout: vkExchangeTimeout,
	})
	if err != nil {
		return errors.New("network request failed")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	if err := json.Unmarshal(response.Body, target); err != nil {
		return errors.New("invalid JSON response")
	}
	return nil
}

// parseVKToken accepts a bare access token or the redirect URL the VK OAuth
// flow produced (query or fragment). It never includes the value in an error.
func parseVKToken(raw string) (token, userID string, err error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return "", "", errors.New("marusia: VK access token or redirect URL is required")
	}
	if !strings.HasPrefix(strings.ToLower(text), "http://") && !strings.HasPrefix(strings.ToLower(text), "https://") {
		if strings.Contains(text, "access_token=") {
			values, parseErr := url.ParseQuery(strings.TrimLeft(text, "?#&"))
			if parseErr == nil {
				token = strings.TrimSpace(values.Get("access_token"))
				userID = strings.TrimSpace(values.Get("user_id"))
				if token != "" {
					return token, userID, nil
				}
			}
		}
		return text, "", nil
	}
	parsed, parseErr := url.Parse(text)
	if parseErr != nil {
		return "", "", errors.New("marusia: the redirect URL is not valid")
	}
	values := parsed.Query()
	for key, entries := range parseFragment(parsed.Fragment) {
		if _, ok := values[key]; !ok {
			values[key] = entries
		}
	}
	token = strings.TrimSpace(values.Get("access_token"))
	userID = strings.TrimSpace(values.Get("user_id"))
	if token == "" {
		return "", "", errors.New("marusia: the redirect URL has no access_token")
	}
	return token, userID, nil
}

func parseFragment(fragment string) url.Values {
	if fragment == "" {
		return nil
	}
	values, err := url.ParseQuery(fragment)
	if err != nil {
		return nil
	}
	return values
}
