package daemon

import (
	"context"
	"testing"
	"time"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/uichanges"
	"capsulad/internal/websecurity"
)

// A wakeword catalog mutation must invalidate the Web UI topic even when the
// caller is not the browser (CLI/admin gRPC). The hub carries the signal; the
// browser watcher rebuilds the full view from the authoritative store.
func TestDeleteWakewordSamplePublishesUIChange(t *testing.T) {
	hub := uichanges.New()
	d := &Daemon{cfg: Config{DataDir: t.TempDir()}, uiChanges: hub}
	signals, cancel := hub.Subscribe()
	defer cancel()

	svc := &assistantService{d: d}
	if _, err := svc.DeleteWakewordSample(context.Background(), &frameworkv1.DeleteWakewordSampleRequest{
		SampleId: "does-not-exist",
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	select {
	case topic := <-signals:
		if topic != uichanges.Wakeword {
			t.Fatalf("topic = %v, want Wakeword", topic)
		}
	case <-time.After(time.Second):
		t.Fatal("wakeword deletion did not invalidate the UI topic")
	}
}

func TestCredentialWatcherConvergesLiveSessions(t *testing.T) {
	dir := t.TempDir()
	logs, err := logbus.New(ids.NewClock(), 20, "")
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	daemonStore := websecurity.Open(dir, websecurity.DefaultOptions())
	if err := daemonStore.Load(); err != nil {
		t.Fatal(err)
	}
	if err := daemonStore.SetPassword("first external password"); err != nil {
		t.Fatal(err)
	}
	session, _, err := daemonStore.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	hub := uichanges.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &Daemon{ctx: ctx, logs: logs, websec: daemonStore, uiChanges: hub}
	d.watchWebCredentials()
	signals, unsubscribe := hub.Subscribe()
	defer unsubscribe()

	cli := websecurity.Open(dir, websecurity.DefaultOptions())
	if err := cli.Load(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cli.CreateAPIKey("external-key"); err != nil {
		t.Fatal(err)
	}
	select {
	case topic := <-signals:
		if topic != uichanges.Access {
			t.Fatalf("topic = %v, want Access", topic)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("external API-key change did not invalidate Access")
	}
	if !daemonStore.ValidateSession(session) {
		t.Fatal("API-key-only change revoked a session")
	}

	if err := cli.SetPassword("second external password"); err != nil {
		t.Fatal(err)
	}
	select {
	case topic := <-signals:
		if topic != uichanges.Access {
			t.Fatalf("topic = %v, want Access", topic)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("external password change did not invalidate Access")
	}
	if daemonStore.ValidateSession(session) {
		t.Fatal("session survived an external password change")
	}
	if !daemonStore.VerifyPassword("second external password") {
		t.Fatal("reloaded password does not verify")
	}
}
