//go:build linux && avahifixture

// This fixture runs the real Linux D-Bus adapter against a fake public
// org.freedesktop.Avahi.Server. It is excluded from normal test runs because
// it needs dbus-daemon and owns org.freedesktop.Avahi on a private bus:
//
//	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c -tags avahifixture \
//	    -o /tmp/avahi-verify/discovery.test ./internal/discovery
//	docker run --rm -v /tmp/avahi-verify:/v:ro debian:latest sh -c '
//	    apt-get -qq update && apt-get -qq install -y dbus &&
//	    /v/discovery.test -test.run TestAvahiDbusFixture -test.v'
//
// Run only this test: dbus.SystemBus caches DBUS_SYSTEM_BUS_ADDRESS on first
// use. The portable apply ordering is covered on every platform by
// avahi_apply_test.go; this file additionally proves the adapter method names,
// argument types and no-redundant-write behavior over a real bus.
package discovery

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

type fixtureBehavior int

const (
	fixtureNormal fixtureBehavior = iota
	fixtureRedundantRace
	fixtureRedundantStale
	fixtureCollision
)

type fixtureAvahi struct {
	mu            sync.Mutex
	state         int32
	hostname      string
	fqdn          string
	mode          fixtureBehavior
	probeOnSet    bool
	probeArm      bool
	probing       chan struct{}
	hostNameReads int
	writes        []string
	conn          *dbus.Conn
}

func (f *fixtureAvahi) GetState() (int32, *dbus.Error) {
	f.mu.Lock()
	state := f.state
	if f.probeArm && state == avahiServerRegistering {
		// Let the test observe that the adapter saw the probing state and then
		// release RUNNING + StateChanged itself; no wall-clock delay decides
		// whether probing was exercised.
		select {
		case f.probing <- struct{}{}:
		default:
		}
	}
	f.mu.Unlock()
	return state, nil
}

func (f *fixtureAvahi) GetHostName() (string, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hostNameReads++
	return f.hostname, nil
}

func (f *fixtureAvahi) GetHostNameFqdn() (string, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fqdn, nil
}

func (f *fixtureAvahi) SetHostName(label string) *dbus.Error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, label)
	switch f.mode {
	case fixtureRedundantRace:
		// Another writer reached the requested name first.
		f.hostname, f.fqdn = label, label+".local"
		return dbus.NewError("org.freedesktop.Avahi.InvalidOperationError",
			[]any{"The requested operation is invalid because redundant."})
	case fixtureRedundantStale:
		return dbus.NewError("org.freedesktop.Avahi.InvalidOperationError",
			[]any{"The requested operation is invalid because redundant."})
	case fixtureCollision:
		return dbus.NewError("org.freedesktop.Avahi.HostNameCollisionError",
			[]any{"Host name collision"})
	}
	f.hostname, f.fqdn = label, label+".local"
	if f.probeOnSet {
		// A real successful SetHostName may return while Avahi is REGISTERING
		// (probing) the changed name. The test observes this through GetState
		// and then releases RUNNING itself; no second setter is used.
		f.probeOnSet = false
		f.state = avahiServerRegistering
	}
	return nil
}

func (f *fixtureAvahi) armProbing(probe bool) {
	f.mu.Lock()
	f.probeOnSet = probe
	f.probeArm = probe
	if probe {
		select {
		case <-f.probing:
		default:
		}
	}
	f.mu.Unlock()
}

func (f *fixtureAvahi) hostNameReadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hostNameReads
}

func (f *fixtureAvahi) setCurrent(hostname string, state int32) {
	f.mu.Lock()
	f.hostname = hostname
	f.fqdn = hostname + ".local"
	f.state = state
	f.mu.Unlock()
}

func (f *fixtureAvahi) setMode(mode fixtureBehavior) {
	f.mu.Lock()
	f.mode = mode
	f.mu.Unlock()
}

func (f *fixtureAvahi) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

func (f *fixtureAvahi) writeList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes...)
}

func (f *fixtureAvahi) emit(t *testing.T, state int32) {
	t.Helper()
	if err := f.conn.Emit("/", "org.freedesktop.Avahi.Server.StateChanged", state); err != nil {
		t.Fatalf("emit StateChanged: %v", err)
	}
}

func fixtureStatus(t *testing.T, m Manager, hostname, advertised string, detail bool) {
	t.Helper()
	status := m.Status()
	if !status.Supported {
		t.Fatalf("status Supported = false (%+v)", status)
	}
	if status.Hostname != hostname || status.Advertised != advertised || (status.Detail != "") != detail {
		t.Fatalf("status = %+v, want hostname=%q advertised=%q detail=%v", status, hostname, advertised, detail)
	}
}

func fixtureWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestAvahiDbusFixture(t *testing.T) {
	if _, err := exec.LookPath("dbus-daemon"); err != nil {
		t.Skip("dbus-daemon is not installed")
	}
	out, err := exec.Command("dbus-daemon", "--session", "--fork", "--print-address", "--print-pid").Output()
	if err != nil {
		t.Fatalf("start dbus-daemon: %v", err)
	}
	lines := strings.Fields(string(out))
	if len(lines) < 2 {
		t.Fatalf("dbus-daemon returned %q, want address and pid", out)
	}
	address := lines[0]
	pid, err := strconv.Atoi(lines[1])
	if err != nil {
		t.Fatalf("dbus-daemon pid %q: %v", lines[1], err)
	}
	t.Cleanup(func() {
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Signal(syscall.SIGTERM)
		}
	})
	if address == "" {
		t.Fatal("dbus-daemon returned no address")
	}
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", address)
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", address)

	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Fatalf("session bus: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	reply, err := conn.RequestName("org.freedesktop.Avahi", dbus.NameFlagDoNotQueue)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("own Avahi name: reply=%v err=%v", reply, err)
	}
	fake := &fixtureAvahi{state: stateRunning, hostname: "capsula", fqdn: "capsula.local",
		probing: make(chan struct{}, 1), conn: conn}
	if err := conn.Export(fake, "/", "org.freedesktop.Avahi.Server"); err != nil {
		t.Fatalf("export fake server: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m := Open(nil, "linux")
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	// The startup apply of the already active default label confirms the
	// advertisement without calling SetHostName.
	if err := m.Apply(ctx, "capsula"); err != nil {
		t.Fatalf("startup Apply: %v", err)
	}
	fixtureStatus(t, m, "capsula", "capsula.local", false)
	if writes := fake.writeCount(); writes != 0 {
		t.Fatalf("startup apply issued %d SetHostName calls: %v", writes, fake.writeList())
	}

	// A changed label reaches SetHostName exactly once.
	if err := m.Apply(ctx, "kitchen"); err != nil {
		t.Fatalf("Apply kitchen: %v", err)
	}
	fixtureStatus(t, m, "kitchen", "kitchen.local", false)
	if writes := fake.writeCount(); writes != 1 {
		t.Fatalf("Apply kitchen wrote %d times, want 1", writes)
	}

	// A responder restart reloads its own config; the StateChanged replay
	// restores the persisted label instead of leaving the default active.
	fake.setCurrent("capsula", avahiServerRegistering)
	fake.emit(t, avahiServerRegistering)
	fake.setCurrent("capsula", stateRunning)
	fake.emit(t, stateRunning)
	fixtureWait(t, "restart reapply", func() bool { return fake.writeCount() == 2 })
	fixtureStatus(t, m, "kitchen", "kitchen.local", false)

	// Stop the monitor before exercising the adapter's error paths: a pending
	// coalesced replay must not overwrite the status being asserted.
	if err := m.Close(); err != nil {
		t.Fatalf("Close monitor: %v", err)
	}

	// A successful SetHostName may return while Avahi is probing the changed
	// name (RUNNING → Set → REGISTERING → RUNNING). The fake signals the test
	// when the adapter's GetState observes REGISTERING; the test then releases
	// RUNNING + StateChanged. Apply must return a clean advertisement with
	// exactly one setter call.
	m = Open(nil, "linux")
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start probing monitor: %v", err)
	}
	fake.setCurrent("capsula-old", stateRunning)
	fake.setMode(fixtureNormal)
	fake.armProbing(true)
	writesBefore := fake.writeCount()
	applyDone := make(chan error, 1)
	go func() { applyDone <- m.Apply(ctx, "probing-name") }()
	select {
	case <-fake.probing:
	case <-time.After(2 * time.Second):
		t.Fatal("confirmation never observed Avahi REGISTERING after the setter")
	}
	fake.emit(t, avahiServerRegistering)
	fake.setCurrent("probing-name", stateRunning)
	fake.emit(t, stateRunning)
	select {
	case err := <-applyDone:
		if err != nil {
			t.Fatalf("post-setter probing Apply: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("post-setter probing Apply did not return")
	}
	fixtureStatus(t, m, "probing-name", "probing-name.local", false)
	if writes := fake.writeCount(); writes != writesBefore+1 {
		t.Fatalf("post-setter probing wrote %d times, want exactly one", writes-writesBefore)
	}

	// The RUNNING transition wakes a monitor follow-up. Force a visible pass and
	// assert it confirms the active label without another setter.
	hostReads := fake.hostNameReadCount()
	fake.emit(t, avahiServerRegistering)
	fake.emit(t, stateRunning)
	fixtureWait(t, "monitor follow-up after probing", func() bool {
		return fake.hostNameReadCount() > hostReads
	})
	if writes := fake.writeCount(); writes != writesBefore+1 {
		t.Fatalf("monitor follow-up wrote the active label: %v", fake.writeList())
	}
	fixtureStatus(t, m, "probing-name", "probing-name.local", false)
	fake.armProbing(false)
	if err := m.Close(); err != nil {
		t.Fatalf("Close probing monitor: %v", err)
	}

	// The remaining adapter error paths do not need a monitor.
	m = Open(nil, "linux")

	// Redundant reply after another writer reached the requested name: the
	// current-name check confirms success and the status stays clean.
	fake.setCurrent("capsula", stateRunning)
	fake.setMode(fixtureRedundantRace)
	if err := m.Apply(ctx, "office"); err != nil {
		t.Fatalf("verified redundant Apply: %v", err)
	}
	fixtureStatus(t, m, "office", "office.local", false)

	// A redundant reply with a mismatched current name stays a real failure.
	fake.setCurrent("capsula-old", stateRunning)
	fake.setMode(fixtureRedundantStale)
	if err := m.Apply(ctx, "taken"); err == nil {
		t.Fatal("stale redundant reply was reported as success")
	} else if !strings.Contains(err.Error(), "redundant") {
		t.Fatalf("stale redundant error = %v", err)
	}
	fixtureStatus(t, m, "taken", "", true)

	// A collision keeps the responder's observed FQDN in the error.
	fake.setCurrent("capsula-2", stateRunning)
	fake.setMode(fixtureCollision)
	err = m.Apply(ctx, "capsula")
	if err == nil {
		t.Fatal("collision was reported as success")
	}
	if !strings.Contains(err.Error(), "already in use") || !strings.Contains(err.Error(), "capsula-2.local") {
		t.Fatalf("collision error = %v", err)
	}
	fixtureStatus(t, m, "capsula", "", true)

	// An authoritative non-RUNNING state is not an advertisement, even when the
	// current name matches; the apply must not write into an unready responder.
	fake.setCurrent("capsula", 3)
	fake.setMode(fixtureNormal)
	before := fake.writeCount()
	if err := m.Apply(ctx, "capsula"); err == nil {
		t.Fatal("collision-state responder was reported as advertised")
	} else if !strings.Contains(err.Error(), "in collision") {
		t.Fatalf("unready error = %v", err)
	}
	if writes := fake.writeCount(); writes != before {
		t.Fatalf("unready responder received %d writes", writes-before)
	}
	fixtureStatus(t, m, "capsula", "", true)

	// Close is idempotent and releases the monitor.
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
