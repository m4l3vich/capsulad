package daemon

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/artifact"
	"capsulad/internal/provenance"
	"capsulad/internal/settings"
	"capsulad/internal/uichanges"
)

// Package administration tests. They exercise the daemon-owned lifecycle the
// browser and the CLI drive: a streamed archive becomes an installed package
// that is visible without a restart, a replacement reports the version it
// replaced but keeps stored settings, a running app refuses both operations,
// and a built-in ID is never touched.

// packageDirForTest stages one package directory with a native entrypoint and
// one declared setting, so an archive built from it is a valid artifact.
func packageDirForTest(t *testing.T, id, version, marker string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pkg")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`id = %q
name = %q
version = %q

[entrypoint]
runtime = "native"
path = "app"
architecture = "aarch64"
abi = "linux-musl"

[[settings]]
key = "greeting"
type = "text"
`, id, "Package "+id, version)
	files := map[string]string{
		"manifest.toml": manifest,
		"app":           "#!/bin/sh\nexit 0\n",
		"marker.txt":    marker,
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		mode := os.FileMode(0o644)
		if name == "app" {
			mode = 0o755
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// packageArchiveForTest writes the canonical artifact for dir and returns its
// bytes, so a test installs through the same transport the CLI and the browser
// use.
func packageArchiveForTest(t *testing.T, dir string) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "pkg.tar.gz")
	if err := artifact.Write(dir, out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// appSettingValue returns the stored raw value for one app setting.
func appSettingValue(t *testing.T, d *Daemon, appID, key string) string {
	t.Helper()
	_, values, err := d.appCfg.Private(appID)
	if err != nil {
		t.Fatalf("app settings for %s: %v", appID, err)
	}
	return string(values[key])
}

func TestInstallPackageStreamActivatesInLiveRegistry(t *testing.T) {
	d, _, _ := newTestDaemon(t)
	signals, cancel := d.uiChanges.Subscribe()
	defer cancel()

	const appID = "test.uploaded"
	first := packageArchiveForTest(t, packageDirForTest(t, appID, "1.0.0", "first"))

	out, err := d.InstallPackageStream(context.Background(), bytes.NewReader(first))
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if out.Replaced || out.PreviousVersion != "" {
		t.Fatalf("fresh install reported replaced=%v previous=%q", out.Replaced, out.PreviousVersion)
	}
	if out.App == nil || out.App.ID() != appID || out.App.Manifest.Version != "1.0.0" {
		t.Fatalf("installed app = %+v, want %s 1.0.0", out.App, appID)
	}
	// Visible without a daemon restart, which is the whole point of the live
	// registry path.
	live, ok := d.reg.Get(appID)
	if !ok || live.Manifest.Version != "1.0.0" {
		t.Fatalf("live registry after install = %+v (present=%v), want 1.0.0", live, ok)
	}
	if listing := d.appProto(live); !listing.GetCanUninstall() || listing.GetSystemApp() {
		t.Fatalf("installed package listing = can_uninstall=%v system=%v, want a removable package",
			listing.GetCanUninstall(), listing.GetSystemApp())
	}
	// The browser is told to re-read the installed set. Boot-time invalidations
	// share the hub, so wait for the package signal rather than the first one.
	waitFor(t, 2*time.Second, "package invalidation", func() bool {
		select {
		case signal := <-signals:
			return signal == uichanges.Apps
		default:
			return false
		}
	})

	// A stored setting must survive a package replacement.
	if _, _, _, err := d.appCfg.Update(appID, 0, []settings.Change{{Key: "greeting", Value: "kept"}}); err != nil {
		t.Fatalf("store app setting: %v", err)
	}

	second := packageArchiveForTest(t, packageDirForTest(t, appID, "2.0.0", "second"))
	out, err = d.InstallPackageStream(context.Background(), bytes.NewReader(second))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !out.Replaced || out.PreviousVersion != "1.0.0" || out.App.Manifest.Version != "2.0.0" {
		t.Fatalf("update reported replaced=%v previous=%q version=%q",
			out.Replaced, out.PreviousVersion, out.App.Manifest.Version)
	}
	marker, err := os.ReadFile(filepath.Join(d.cfg.AppsDir, appID, "marker.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(marker)) != "second" {
		t.Fatalf("installed tree content = %q, want the replacement package", marker)
	}
	if got := appSettingValue(t, d, appID, "greeting"); got != `"kept"` {
		t.Fatalf("stored setting after update = %s, want \"kept\"", got)
	}

	if err := d.UninstallPackage(appID); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, ok := d.reg.Get(appID); ok {
		t.Fatal("uninstalled app is still in the live registry")
	}
	if _, err := os.Stat(filepath.Join(d.cfg.AppsDir, appID)); !os.IsNotExist(err) {
		t.Fatalf("package directory still present: %v", err)
	}

	// Settings outlive the package; reinstalling the same ID finds them.
	if _, err := d.InstallPackageStream(context.Background(), bytes.NewReader(second)); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if got := appSettingValue(t, d, appID, "greeting"); got != `"kept"` {
		t.Fatalf("stored setting after reinstall = %s, want \"kept\"", got)
	}
}

func TestInstallAndRemovalRefuseRunningApp(t *testing.T) {
	binary := buildProbe(t)
	const appID = "test.probe"
	d, _, _ := newTestDaemonWith(t, appInstall{
		id: appID, binaryName: "probe", binaryPath: binary,
		manifest: []byte(probeManifest),
	})
	if _, _, _, err := d.Launch(appID, nil); err != nil {
		t.Fatalf("launch: %v", err)
	}
	runID := string(waitReady(t, d, appID, 10*time.Second))

	replacement := packageArchiveForTest(t, packageDirForTest(t, appID, "9.9.9", "replacement"))
	_, err := d.InstallPackageStream(context.Background(), bytes.NewReader(replacement))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("install over a running app = %v, want FailedPrecondition", err)
	}
	if !strings.Contains(err.Error(), "is running") || !strings.Contains(err.Error(), runID) {
		t.Fatalf("refusal = %v, want the run id and the stop instruction", err)
	}
	// The refusal happens before the swap: the installed tree is untouched.
	installed, err := os.ReadFile(filepath.Join(d.cfg.AppsDir, appID, "manifest.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(installed), "9.9.9") {
		t.Fatalf("refused install replaced the package:\n%s", installed)
	}

	err = d.UninstallPackage(appID)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("remove a running app = %v, want FailedPrecondition", err)
	}
	if !strings.Contains(err.Error(), runID) || !strings.Contains(err.Error(), "stop it before removing") {
		t.Fatalf("removal refusal = %v, want the run id and the stop instruction", err)
	}

	// Stopping the run lifts the refusal; the same archive then applies.
	if !d.Stop(runID) {
		t.Fatalf("stop %s was not requested", runID)
	}
	waitFor(t, 5*time.Second, "run stopped", func() bool { return len(d.sup.List(appID)) == 0 })
	if _, err := d.InstallPackageStream(context.Background(), bytes.NewReader(replacement)); err != nil {
		t.Fatalf("install after stop: %v", err)
	}
	if app, ok := d.reg.Get(appID); !ok || app.Manifest.Version != "9.9.9" {
		t.Fatalf("live registry after replacement = %+v (present=%v), want 9.9.9", app, ok)
	}
}

func TestInstallPackageRefusesBuiltinAndInvalidArchive(t *testing.T) {
	d, _, _ := newTestDaemon(t)

	builtin := packageArchiveForTest(t, packageDirForTest(t, clockAppID, "9.9.9", "shadow"))
	if _, err := d.InstallPackageStream(context.Background(), bytes.NewReader(builtin)); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("install over a built-in = %v, want FailedPrecondition", err)
	}
	if app, ok := d.reg.Get(clockAppID); !ok || !d.reg.IsBuiltin(app.ID()) {
		t.Fatalf("built-in %s is no longer registered", clockAppID)
	}

	if _, err := d.InstallPackageStream(context.Background(), strings.NewReader("not a gzip stream")); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("non-gzip stream = %v, want InvalidArgument", err)
	}

	var escaping bytes.Buffer
	gz := gzip.NewWriter(&escaping)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "../escape.txt", Mode: 0o644, Size: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("xx")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InstallPackageStream(context.Background(), bytes.NewReader(escaping.Bytes())); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("escaping archive = %v, want InvalidArgument", err)
	}
	if _, err := os.Stat(filepath.Join(d.cfg.AppsDir, "..", "escape.txt")); !os.IsNotExist(err) {
		t.Fatalf("escaping archive wrote outside its staging tree: %v", err)
	}

	if _, err := d.InstallPackageStream(context.Background(), bytes.NewReader(nil)); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty stream = %v, want InvalidArgument", err)
	}
}

func TestUninstallPackageRefusesBuiltinAndUnknown(t *testing.T) {
	d, _, _ := newTestDaemon(t)
	if err := d.UninstallPackage(clockAppID); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("remove a built-in = %v, want FailedPrecondition", err)
	}
	if err := d.UninstallPackage("test.absent"); status.Code(err) != codes.NotFound {
		t.Fatalf("remove an absent app = %v, want NotFound", err)
	}
	if err := d.UninstallPackage(""); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("remove with no id = %v, want InvalidArgument", err)
	}
}

func TestInstallPackageDropsCachedProvenance(t *testing.T) {
	d, _, _ := newTestDaemon(t)
	const appID = "test.uploaded"
	d.provMu.Lock()
	d.prov = map[string]provenance.Result{appID: {AppID: appID, Detail: "stale"}}
	d.provMu.Unlock()

	archive := packageArchiveForTest(t, packageDirForTest(t, appID, "1.0.0", "first"))
	if _, err := d.InstallPackageStream(context.Background(), bytes.NewReader(archive)); err != nil {
		t.Fatal(err)
	}
	d.provMu.Lock()
	_, cached := d.prov[appID]
	d.provMu.Unlock()
	if cached {
		t.Fatal("cached provenance survived a package replacement")
	}
}

// --- transport adapter ------------------------------------------------------

type fakeInstallServer struct {
	grpc.ClientStreamingServer[frameworkv1.InstallApplicationRequest, frameworkv1.InstallApplicationResult]
	msgs   []*frameworkv1.InstallApplicationRequest
	next   int
	ctx    context.Context
	result *frameworkv1.InstallApplicationResult
}

func (f *fakeInstallServer) Recv() (*frameworkv1.InstallApplicationRequest, error) {
	if f.next >= len(f.msgs) {
		return nil, io.EOF
	}
	msg := f.msgs[f.next]
	f.next++
	return msg, nil
}

func (f *fakeInstallServer) SendAndClose(res *frameworkv1.InstallApplicationResult) error {
	f.result = res
	return nil
}

func (f *fakeInstallServer) Context() context.Context {
	if f.ctx == nil {
		return context.Background()
	}
	return f.ctx
}

func installOpen() *frameworkv1.InstallApplicationRequest {
	return &frameworkv1.InstallApplicationRequest{
		Chunk: &frameworkv1.InstallApplicationRequest_Open{Open: &frameworkv1.InstallApplicationOpen{FileName: "pkg.tar.gz"}},
	}
}

func installChunk(data []byte) *frameworkv1.InstallApplicationRequest {
	return &frameworkv1.InstallApplicationRequest{
		Chunk: &frameworkv1.InstallApplicationRequest_Content{Content: data},
	}
}

func TestInstallApplicationServiceStreamsChunks(t *testing.T) {
	d, _, _ := newTestDaemon(t)
	archive := packageArchiveForTest(t, packageDirForTest(t, "test.streamed", "3.0.0", "chunked"))

	// Two chunks, so the adapter's remainder handling is exercised.
	half := len(archive) / 2
	stream := &fakeInstallServer{msgs: []*frameworkv1.InstallApplicationRequest{
		installOpen(), installChunk(archive[:half]), installChunk(archive[half:]),
	}}
	svc := &applicationService{d: d}
	if err := svc.InstallApplication(stream); err != nil {
		t.Fatalf("install through the service: %v", err)
	}
	if stream.result == nil {
		t.Fatal("InstallApplication returned no result")
	}
	if got := stream.result.GetApplication().GetAppId(); got != "test.streamed" {
		t.Fatalf("result app = %q, want test.streamed", got)
	}
	if stream.result.GetApplication().GetVersion() != "3.0.0" || stream.result.GetReplaced() {
		t.Fatalf("result = %+v, want a fresh 3.0.0 install", stream.result)
	}
	if _, ok := d.reg.Get("test.streamed"); !ok {
		t.Fatal("streamed package is not in the live registry")
	}
}

func TestInstallApplicationRequiresOpenFrame(t *testing.T) {
	d, _, _ := newTestDaemon(t)
	archive := packageArchiveForTest(t, packageDirForTest(t, "test.streamed", "3.0.0", "x"))
	svc := &applicationService{d: d}

	missing := &fakeInstallServer{msgs: []*frameworkv1.InstallApplicationRequest{installChunk(archive)}}
	if err := svc.InstallApplication(missing); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("stream without an open frame = %v, want InvalidArgument", err)
	}
	repeated := &fakeInstallServer{msgs: []*frameworkv1.InstallApplicationRequest{installOpen(), installOpen()}}
	if err := svc.InstallApplication(repeated); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("second open frame = %v, want InvalidArgument", err)
	}
	empty := &fakeInstallServer{}
	if err := svc.InstallApplication(empty); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty stream = %v, want InvalidArgument", err)
	}
}

func TestInstallApplicationRejectsOversizedStream(t *testing.T) {
	// The compressed total is bounded before it reaches the staging tree. The
	// bound is lowered here so the refusal is provable without streaming 65 MiB
	// of a valid gzip stream.
	server := &fakeInstallServer{msgs: []*frameworkv1.InstallApplicationRequest{
		installOpen(), installChunk([]byte("0123456789abcdef")),
	}}
	src, err := openPackageStream(server)
	if err != nil {
		t.Fatalf("open frame: %v", err)
	}
	if src.limit != artifact.MaxArchiveBytes {
		t.Fatalf("production archive bound = %d, want %d", src.limit, artifact.MaxArchiveBytes)
	}
	src.limit = 8
	buf := make([]byte, 32)
	if _, err := src.Read(buf); err == nil {
		t.Fatal("a stream past the archive bound was accepted")
	}
	if status.Code(src.problem) != codes.InvalidArgument {
		t.Fatalf("bound refusal = %v, want InvalidArgument", src.problem)
	}
	if !strings.Contains(src.problem.Error(), "upload limit") {
		t.Fatalf("bound refusal message = %v, want the upload-limit bound", src.problem)
	}
	// The daemon's archive error must not mask the transport refusal.
	refined := src.refine(errors.New("artifact stream: expected a gzip-compressed tar"))
	if status.Code(refined) != codes.InvalidArgument || !strings.Contains(refined.Error(), "upload limit") {
		t.Fatalf("refined error = %v, want the upload-limit refusal", refined)
	}
}

func TestPackageRPCsRejectApplicationIdentity(t *testing.T) {
	ctx := appMetadata("run_test")
	d := &Daemon{cfg: Config{SandboxUIDBase: 20000}}
	svc := &applicationService{d: d}
	if err := svc.InstallApplication(&fakeInstallServer{ctx: ctx}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("app-identity install = %v, want PermissionDenied", err)
	}
	if _, err := svc.UninstallApplication(ctx, &frameworkv1.UninstallApplicationRequest{AppId: "other.app"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("app-identity remove = %v, want PermissionDenied", err)
	}
}

func TestInstallAdmissionBoundsConcurrentStaging(t *testing.T) {
	d, _, _ := newTestDaemon(t)
	if d.installSlot == nil {
		t.Fatal("daemon has no package-staging admission slot")
	}
	release, err := d.admitInstall(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("first admission: %v", err)
	}
	// A second upload waits only for the bounded budget, so a stalled upload
	// cannot pin it and two staging trees cannot share the data partition.
	if _, err := d.admitInstall(context.Background(), 20*time.Millisecond); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("busy admission = %v, want ResourceExhausted", err)
	}
	// A caller that goes away while waiting is cancelled instead of queued.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.admitInstall(ctx, time.Minute); status.Code(err) != codes.Canceled {
		t.Fatalf("cancelled admission = %v, want Canceled", err)
	}
	release()
	second, err := d.admitInstall(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("admission after release: %v", err)
	}
	second()
}

func TestInstallStreamReleasesAdmissionOnSuccessAndFailure(t *testing.T) {
	d, _, _ := newTestDaemon(t)
	archive := packageArchiveForTest(t, packageDirForTest(t, "test.admit", "1.0.0", "x"))

	// Two streamed installs of the same package run one after the other and
	// both succeed; the slot is not lost between them.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = d.InstallPackageStream(context.Background(), bytes.NewReader(archive))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent install %d: %v", i, err)
		}
	}

	// A rejected archive frees the slot for the next upload.
	if _, err := d.InstallPackageStream(context.Background(), strings.NewReader("not a gzip stream")); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad archive = %v, want InvalidArgument", err)
	}
	if _, err := d.InstallPackageStream(context.Background(), bytes.NewReader(archive)); err != nil {
		t.Fatalf("install after a rejected archive: %v", err)
	}
}

func TestProvenanceReadSerializesWithPackageSwap(t *testing.T) {
	// test.probe is installed by the harness with a real host binary.
	d, _, _ := newTestDaemon(t)
	svc := &applicationService{d: d}

	// A replacement holds the package mutex while it swaps and invalidates the
	// cache. A provenance read that ran concurrently could verify a mixed tree
	// and cache the result after the invalidation, so it must wait.
	d.pkgMu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := svc.GetBinaryProvenance(context.Background(), &frameworkv1.GetBinaryProvenanceRequest{AppId: "test.probe"})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("provenance read completed while a package swap held the mutex: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	d.pkgMu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("provenance read after the swap: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("provenance read did not resume after the swap released the mutex")
	}
	d.provMu.Lock()
	_, cached := d.prov["test.probe"]
	d.provMu.Unlock()
	if !cached {
		t.Fatal("provenance result was not cached for the installed package")
	}
}

// A raw client can open the install stream, send a prefix and then stop
// sending while keeping the connection open. The daemon must not let it hold
// the single staging slot: the supervising handler ends the RPC after the idle
// budget, which cancels the stream context, unblocks the extraction read, and
// lets the worker remove its staging tree and release the slot in its own
// defers. These invariants are asserted against the real gRPC service.
func TestStalledUploadReleasesAdmissionWithoutLeaks(t *testing.T) {
	d, _, _ := newTestDaemonPrepared(t, func(d *Daemon) {
		d.installIdleBudget = 300 * time.Millisecond
	})
	conn := dialTest(t, d.cfg.SocketPath)
	client := frameworkv1.NewApplicationServiceClient(conn)
	archive := packageArchiveForTest(t, packageDirForTest(t, "test.stalled", "1.0.0", "stalled"))
	prefix := archive[:len(archive)/3]

	stall := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		stream, err := client.InstallApplication(ctx)
		if err != nil {
			return err
		}
		if err := stream.Send(installOpen()); err != nil {
			return err
		}
		if err := stream.Send(installChunk(prefix)); err != nil {
			return err
		}
		// Keep the send side open and wait for the device's answer, which is
		// what a wedged client looks like from the server's side.
		res := &frameworkv1.InstallApplicationResult{}
		return stream.RecvMsg(res)
	}

	before := runtime.NumGoroutine()
	start := time.Now()
	if err := stall(); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("stalled upload = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("stalled upload was not bounded: %s", elapsed)
	}

	// The staging tree is gone and the slot is free: a normal install proceeds.
	waitFor(t, 10*time.Second, "staging tree removed", func() bool {
		entries, err := os.ReadDir(d.cfg.AppsDir)
		if err != nil {
			return false
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".upload-") {
				return false
			}
		}
		return true
	})
	installComplete := func(id, version string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		stream, err := client.InstallApplication(ctx)
		if err != nil {
			return err
		}
		data := packageArchiveForTest(t, packageDirForTest(t, id, version, "ok"))
		if err := stream.Send(installOpen()); err != nil {
			return err
		}
		half := len(data) / 2
		if err := stream.Send(installChunk(data[:half])); err != nil {
			return err
		}
		if err := stream.Send(installChunk(data[half:])); err != nil {
			return err
		}
		if _, err := stream.CloseAndRecv(); err != nil {
			return err
		}
		return nil
	}
	if err := installComplete("test.afterstall", "1.0.0"); err != nil {
		t.Fatalf("install after a stalled upload: %v", err)
	}
	if _, ok := d.reg.Get("test.afterstall"); !ok {
		t.Fatal("the install after the stalled upload is not in the live registry")
	}
	// The stalled attempt installed nothing.
	if _, ok := d.reg.Get("test.stalled"); ok {
		t.Fatal("a stalled upload installed a package")
	}
	if _, err := os.Stat(filepath.Join(d.cfg.AppsDir, "test.stalled")); !os.IsNotExist(err) {
		t.Fatalf("stalled upload left an installation behind: %v", err)
	}

	// Repeated stalls must not accumulate blocked readers or staging trees.
	for i := 0; i < 6; i++ {
		if err := stall(); status.Code(err) != codes.DeadlineExceeded {
			t.Fatalf("stall %d = %v, want DeadlineExceeded", i, err)
		}
	}
	waitFor(t, 15*time.Second, "goroutines settle", func() bool {
		return runtime.NumGoroutine() <= before+6
	})
}

// --- commit handshake -------------------------------------------------------
//
// A timeout may report "nothing was installed" only when it wins the handshake
// before the worker begins its commit. Once the worker has begun, the call must
// await and report the real outcome. These tests drive the supervisor decision
// directly with an injected clock and a controlled result channel, so both
// orderings are deterministic instead of racing a real upload.

func TestInstallCommitHandshakeIsExclusive(t *testing.T) {
	abandonedFirst := &installCommit{}
	if !abandonedFirst.abandon() {
		t.Fatal("first abandon lost")
	}
	if abandonedFirst.beginCommit() {
		t.Fatal("beginCommit succeeded after the supervisor abandoned the upload")
	}
	if abandonedFirst.committing() {
		t.Fatal("state reports committing after an abandon")
	}

	committedFirst := &installCommit{}
	if !committedFirst.beginCommit() {
		t.Fatal("first beginCommit lost")
	}
	if committedFirst.abandon() {
		t.Fatal("abandon succeeded after the commit began")
	}
	if !committedFirst.committing() {
		t.Fatal("state does not report committing after beginCommit")
	}

	// A direct caller passes no guard and always commits.
	var direct *installCommit
	if !direct.beginCommit() {
		t.Fatal("nil guard refused to commit")
	}
}

func TestSuperviseInstallNeverReportsACompletedInstallAsTimeout(t *testing.T) {
	// The finish and the timer tick can be ready in the same select. The result
	// must win every time; repeated runs make both arms of the select likely.
	for i := 0; i < 50; i++ {
		src := &packageStream{}
		src.touch()
		commit := &installCommit{}
		done := make(chan installOutcome, 1)
		done <- installOutcome{install: PackageInstall{Replaced: true, PreviousVersion: "1.0.0"}}
		late := func() time.Time { return time.Now().Add(2 * time.Hour) }
		out, err := superviseInstall(done, src, commit, time.Minute, time.Hour, time.Millisecond, late)
		if err != nil {
			t.Fatalf("iteration %d: completed install reported as %v", i, err)
		}
		if !out.install.Replaced || out.install.PreviousVersion != "1.0.0" {
			t.Fatalf("iteration %d: result = %+v, want the completed install", i, out.install)
		}
	}
}

func TestSuperviseInstallAwaitsOnceCommitBegins(t *testing.T) {
	src := &packageStream{}
	src.touch()
	commit := &installCommit{}
	if !commit.beginCommit() {
		t.Fatal("could not begin the commit")
	}
	done := make(chan installOutcome, 1)
	late := func() time.Time { return time.Now().Add(2 * time.Hour) }

	type result struct {
		out installOutcome
		err error
	}
	returned := make(chan result, 1)
	go func() {
		out, err := superviseInstall(done, src, commit, time.Minute, time.Hour, time.Millisecond, late)
		returned <- result{out: out, err: err}
	}()
	select {
	case got := <-returned:
		t.Fatalf("supervisor returned %+v/%v while the commit was in progress; it must await the outcome", got.out, got.err)
	case <-time.After(150 * time.Millisecond):
	}
	// A sentinel the assertion can actually distinguish: the awaited outcome
	// must be this value, not a synthesized one.
	done <- installOutcome{install: PackageInstall{Replaced: true, PreviousVersion: "awaited-2.0.0"}}
	select {
	case got := <-returned:
		if got.err != nil {
			t.Fatalf("awaiting supervisor reported %v, want the commit outcome", got.err)
		}
		if !got.out.install.Replaced || got.out.install.PreviousVersion != "awaited-2.0.0" {
			t.Fatalf("awaited result = %+v, want the worker's own outcome", got.out.install)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not return after the worker reported its outcome")
	}
}

func TestSuperviseInstallAbandonsBeforeCommit(t *testing.T) {
	src := &packageStream{}
	src.touch()
	commit := &installCommit{}
	done := make(chan installOutcome, 1)
	late := func() time.Time { return time.Now().Add(2 * time.Hour) }

	out, err := superviseInstall(done, src, commit, time.Minute, time.Hour, time.Millisecond, late)
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("stalled upload = %v (%+v), want DeadlineExceeded", err, out)
	}
	if commit.beginCommit() {
		t.Fatal("the worker could still commit after the timeout reported nothing was installed")
	}
	// A result that arrives after the decision is ignored: the call already
	// returned, and the worker cannot reach the swap.
	done <- installOutcome{install: PackageInstall{}}
}

func TestSuperviseInstallReportsPreCommitWorkerFailure(t *testing.T) {
	src := &packageStream{}
	src.touch()
	commit := &installCommit{}
	done := make(chan installOutcome, 1)
	done <- installOutcome{err: status.Error(codes.InvalidArgument, "artifact stream: bad archive")}
	late := func() time.Time { return time.Now().Add(2 * time.Hour) }
	for i := 0; i < 20; i++ {
		// Refill in case the drain consumed the value on an earlier iteration.
		if len(done) == 0 {
			done <- installOutcome{err: status.Error(codes.InvalidArgument, "artifact stream: bad archive")}
		}
		out, err := superviseInstall(done, src, commit, time.Minute, time.Hour, time.Millisecond, late)
		if err != nil {
			t.Fatalf("iteration %d: supervisor returned %v, want the worker's own error", i, err)
		}
		if status.Code(out.err) != codes.InvalidArgument {
			t.Fatalf("iteration %d: supervisor reported %v, want the worker's archive error", i, out.err)
		}
		commit = &installCommit{}
	}
}
