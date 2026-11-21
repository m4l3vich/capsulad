package enroll_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"capsulad/sdk/capsulad"

	"capsulad/apps/cadence/internal/enroll"
)

type fakeDoer struct {
	req  capsulad.HTTPRequest
	resp *capsulad.HTTPResponse
	err  error
}

func (d *fakeDoer) Do(_ context.Context, req capsulad.HTTPRequest) (*capsulad.HTTPResponse, error) {
	d.req = req
	return d.resp, d.err
}

func TestExchangePostsTheUpstreamEnrollmentRequest(t *testing.T) {
	doer := &fakeDoer{resp: &capsulad.HTTPResponse{
		StatusCode: 200,
		Body:       []byte(`{"deviceId":"dev-1","token":"tok-1"}`),
	}}
	got, err := enroll.Exchange(context.Background(), doer, "https://cadence.example.com/proxy/", "vk-capsula-mini", " K7M 4QX ")
	if err != nil {
		t.Fatal(err)
	}
	if got.DeviceID != "dev-1" || got.Token != "tok-1" {
		t.Fatalf("credentials = %+v", got)
	}
	if doer.req.Method != "POST" || doer.req.URL != "https://cadence.example.com/proxy/v2/enroll" {
		t.Fatalf("request = %s %s", doer.req.Method, doer.req.URL)
	}
	if doer.req.Header["Content-Type"] != "application/json" {
		t.Fatalf("headers = %v", doer.req.Header)
	}
	var body struct {
		Code        string `json:"code"`
		Fingerprint struct {
			Board string `json:"board"`
		} `json:"fingerprint"`
	}
	if err := json.Unmarshal(doer.req.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "K7M 4QX" || body.Fingerprint.Board != "vk-capsula-mini" {
		t.Fatalf("body = %+v", body)
	}
	if doer.req.Timeout == 0 {
		t.Fatal("request has no deadline")
	}
}

func TestExchangeRefusedAndMalformed(t *testing.T) {
	refused := &fakeDoer{resp: &capsulad.HTTPResponse{StatusCode: 403, Body: []byte(`{"error":"enroll_refused"}`)}}
	if _, err := enroll.Exchange(context.Background(), refused, "https://s", "sim", "ABCDEFGH"); !errors.Is(err, enroll.ErrRefused) {
		t.Fatalf("403 = %v, want ErrRefused", err)
	}
	for name, body := range map[string]string{
		"empty":          ``,
		"not json":       `{`,
		"missing id":     `{"token":"tok"}`,
		"missing token":  `{"deviceId":"dev"}`,
		"empty token":    `{"deviceId":"dev","token":" "}`,
		"wrong shape":    `[]`,
		"over the limit": `{"deviceId":"dev","token":"tok","padding":"` + strings.Repeat("x", maxAnswerBytes) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			doer := &fakeDoer{resp: &capsulad.HTTPResponse{StatusCode: 200, Body: []byte(body)}}
			if _, err := enroll.Exchange(context.Background(), doer, "https://s", "sim", "ABCDEFGH"); !errors.Is(err, enroll.ErrMalformed) {
				t.Fatalf("200 %s = %v, want ErrMalformed", name, err)
			}
		})
	}
}

func TestExchangeServerAndTransportErrors(t *testing.T) {
	failed := &fakeDoer{resp: &capsulad.HTTPResponse{StatusCode: 500}}
	if _, err := enroll.Exchange(context.Background(), failed, "https://s", "sim", "ABCDEFGH"); err == nil ||
		!strings.Contains(err.Error(), "500") {
		t.Fatalf("500 = %v", err)
	}
	transport := &fakeDoer{err: errors.New("dial tcp: secret-url")}
	if _, err := enroll.Exchange(context.Background(), transport, "https://s", "sim", "ABCDEFGH"); err == nil ||
		strings.Contains(err.Error(), "secret-url") {
		t.Fatalf("transport = %v", err)
	}
	if _, err := enroll.Exchange(context.Background(), nil, "https://s", "sim", "ABCDEFGH"); err == nil {
		t.Fatal("nil client accepted")
	}
	if _, err := enroll.Exchange(context.Background(), &fakeDoer{}, "https://s", "sim", " "); err == nil {
		t.Fatal("empty code accepted")
	}
}

const maxAnswerBytes = 64 << 10
