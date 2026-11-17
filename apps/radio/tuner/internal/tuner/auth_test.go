package tuner

import (
	"context"
	"errors"
	"testing"
)

func authManager(interfaces []appInterface, err error) *Manager {
	m := testManager()
	m.listAppInterfaces = func(context.Context) ([]appInterface, error) {
		return interfaces, err
	}
	return m
}

func TestAuthorizeCallerAcceptsConfiguredPlayerRun(t *testing.T) {
	m := authManager([]appInterface{
		{AppID: "org.example.other", RunID: "run_9", Service: "x", Available: true},
		{AppID: DefaultPlayerApp, RunID: "run_2", Service: "org.capsulad.radio.v1.RadioPlayerService", Available: true},
	}, nil)
	if err := m.authorizeCaller(context.Background(), "run_2"); err != nil {
		t.Fatalf("authorized caller refused: %v", err)
	}
}

func TestAuthorizeCallerRefusesEveryoneElse(t *testing.T) {
	interfaces := []appInterface{
		{AppID: DefaultPlayerApp, RunID: "run_2", Available: true},
		{AppID: "org.example.evil", RunID: "run_3", Available: true},
		{AppID: DefaultPlayerApp, RunID: "run_4", Available: false},
	}
	cases := []struct {
		name   string
		caller string
	}{
		{"empty identity", ""},
		{"pid fallback", "pid:1234"},
		{"different app", "run_3"},
		{"not ready", "run_4"},
		{"unknown run", "run_99"},
	}
	for _, c := range cases {
		m := authManager(interfaces, nil)
		if err := m.authorizeCaller(context.Background(), c.caller); err == nil {
			t.Fatalf("%s: caller %q accepted", c.name, c.caller)
		}
	}
}

func TestAuthorizeCallerReportsLookupFailure(t *testing.T) {
	m := authManager(nil, errors.New("bus down"))
	if err := m.authorizeCaller(context.Background(), "run_2"); err == nil {
		t.Fatal("lookup failure accepted")
	}
}

func TestAuthorizeCallerUsesConfiguredPlayerApp(t *testing.T) {
	m := authManager([]appInterface{
		{AppID: "org.example.custom", RunID: "run_7", Available: true},
	}, nil)
	m.playerApp = "org.example.custom"
	if err := m.authorizeCaller(context.Background(), "run_7"); err != nil {
		t.Fatalf("configured player refused: %v", err)
	}
}
