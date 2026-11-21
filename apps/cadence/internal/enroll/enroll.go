// Package enroll exchanges a one-time code made in the Cadence dashboard for
// this device's bearer token. The request is the upstream device enrollment
// contract: POST <server>/v2/enroll with {"code", "fingerprint":{"board"}}.
// The answer is the only time the server returns the token.
package enroll

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"capsulad/sdk/capsulad"
)

// Doer is one HTTP exchange. Production is *capsulad.HTTPClient, so the app
// uses the shared SDK's verified TLS and never bundles net/http.
type Doer interface {
	Do(ctx context.Context, req capsulad.HTTPRequest) (*capsulad.HTTPResponse, error)
}

// Credentials are what a used code yields.
type Credentials struct {
	DeviceID string
	Token    string
}

// Errors distinguish a refused code from a server or transport problem, so the
// caller can tell the operator what to do next.
var (
	// ErrRefused is the server's 403: the code is unknown, expired, already
	// used, or was created for another board.
	ErrRefused = errors.New("enroll: code refused")
	// ErrMalformed is a 200 answer that is not the documented object.
	ErrMalformed = errors.New("enroll: malformed server answer")
)

const (
	requestTimeout = 20 * time.Second
	maxAnswerBytes = 64 << 10
)

// Exchange redeems code for this device's token. baseURL is the validated
// server base (an optional path prefix is kept), board is the upstream board id
// the dashboard code was created for, and code is the one-time dashboard code.
// Neither the code nor the token is included in an error string.
func Exchange(ctx context.Context, doer Doer, baseURL, board, code string) (Credentials, error) {
	code = strings.TrimSpace(code)
	board = strings.TrimSpace(board)
	if doer == nil {
		return Credentials{}, errors.New("enroll: no HTTP client")
	}
	if code == "" {
		return Credentials{}, errors.New("enroll: empty enrollment code")
	}
	if board == "" {
		return Credentials{}, errors.New("enroll: empty board")
	}
	request := struct {
		Code        string `json:"code"`
		Fingerprint struct {
			Board string `json:"board"`
		} `json:"fingerprint"`
	}{Code: code}
	request.Fingerprint.Board = board
	body, err := json.Marshal(request)
	if err != nil {
		return Credentials{}, errors.New("enroll: encode request")
	}
	response, err := doer.Do(ctx, capsulad.HTTPRequest{
		Method:  "POST",
		URL:     strings.TrimRight(baseURL, "/") + "/v2/enroll",
		Header:  map[string]string{"Content-Type": "application/json", "Accept": "application/json"},
		Body:    body,
		Timeout: requestTimeout,
	})
	if err != nil {
		// The transport error can quote the URL; it carries no code, but the
		// caller logs its own fixed message rather than this one.
		return Credentials{}, errors.New("enroll: request failed")
	}
	switch response.StatusCode {
	case 200:
	case 403:
		return Credentials{}, ErrRefused
	default:
		return Credentials{}, fmt.Errorf("enroll: server answered %d", response.StatusCode)
	}
	if len(response.Body) == 0 || len(response.Body) > maxAnswerBytes {
		return Credentials{}, ErrMalformed
	}
	var answer struct {
		DeviceID string `json:"deviceId"`
		Token    string `json:"token"`
	}
	if err := json.Unmarshal(response.Body, &answer); err != nil ||
		strings.TrimSpace(answer.DeviceID) == "" || strings.TrimSpace(answer.Token) == "" {
		return Credentials{}, ErrMalformed
	}
	return Credentials{DeviceID: answer.DeviceID, Token: answer.Token}, nil
}
