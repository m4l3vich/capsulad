package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"capsulad/internal/artifact"
	"capsulad/internal/manifest"
)

const cliTestManifest = `
id = "org.example.tested"
name = "Tested app"
version = "0.3.0"
[entrypoint]
runtime = "native"
path = "bin/app"
architecture = "aarch64"
abi = "linux-gnu"
`

func writeTestFile(t *testing.T, root, rel, body string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func TestAppPackageWritesCanonicalArtifact(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "manifest.toml", cliTestManifest, 0o644)
	writeTestFile(t, dir, "bin/app", "#!/bin/sh\n", 0o755)
	out := filepath.Join(t.TempDir(), "out.tar.gz")
	if err := runAppPackage([]string{dir, "-out", out}); err != nil {
		t.Fatalf("package: %v", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("artifact missing: %v", err)
	}
	pkg, err := artifact.Open(out)
	if err != nil {
		t.Fatalf("open written artifact: %v", err)
	}
	defer pkg.Close()
	if pkg.Manifest.ID != "org.example.tested" || pkg.Manifest.Version != "0.3.0" {
		t.Fatalf("manifest = %s %s", pkg.Manifest.ID, pkg.Manifest.Version)
	}
}

func TestAppPackageDefaultName(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "manifest.toml", cliTestManifest, 0o644)
	writeTestFile(t, dir, "bin/app", "#!/bin/sh\n", 0o755)
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(previous); err != nil {
			t.Fatal(err)
		}
	}()
	if err := runAppPackage([]string{dir}); err != nil {
		t.Fatalf("package: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "org.example.tested-0.3.0.tar.gz")); err != nil {
		t.Fatalf("default artifact name: %v", err)
	}
}

// A dry run inspects the bundle, executes the incoming daemon's -version probe
// and prints a plan without writing into the data directory.
func TestDeployDryRunChangesNothing(t *testing.T) {
	sameFilesystemHook(t, false)
	bundle := t.TempDir()
	writeTestFile(t, bundle, "capsulad", "#!/bin/sh\necho 'capsulad test (sdk 0.5)'\n", 0o755)
	writeTestFile(t, bundle, "lib/capsulad-native.json", `{"abi_major":1,"abi_minor":0,"library":"libcapsulad.so.1"}`, 0o644)
	writeTestFile(t, bundle, "boot/S99btctl.sh", "#!/bin/sh\nexit 0\n", 0o755)
	dataDir := t.TempDir()
	initScript := filepath.Join(t.TempDir(), "S99btctl.sh")
	writeTestFile(t, filepath.Dir(initScript), filepath.Base(initScript), "#!/bin/sh\nexit 0\n", 0o755)

	if err := runDeploy([]string{bundle, "-dry-run", "-init-script", initScript}, dataDir, ""); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("dry run wrote into the data dir: %v", entries)
	}
}

func TestDeployRejectsInvalidBootHook(t *testing.T) {
	sameFilesystemHook(t, false)
	bundle := t.TempDir()
	writeTestFile(t, bundle, "boot/S99btctl.sh", "#!/bin/sh\nif [ -f x ]; then\n", 0o755)
	err := runDeploy([]string{bundle, "-dry-run"}, t.TempDir(), "")
	if err == nil || !strings.Contains(err.Error(), "not a valid POSIX shell script") {
		t.Fatalf("error = %v, want a shell validation failure", err)
	}
}

func TestDeployRejectsEmptyBundle(t *testing.T) {
	sameFilesystemHook(t, false)
	err := runDeploy([]string{t.TempDir(), "-dry-run"}, t.TempDir(), "")
	if err == nil || !strings.Contains(err.Error(), "no actionable payload") {
		t.Fatalf("error = %v, want an empty-bundle rejection", err)
	}
}

func TestDeployIgnoresUnknownEntries(t *testing.T) {
	sameFilesystemHook(t, false)
	bundle := t.TempDir()
	writeTestFile(t, bundle, "capsulad", "#!/bin/sh\necho 'capsulad test'\n", 0o755)
	writeTestFile(t, bundle, "unexpected.txt", "ignored", 0o644)
	initScript := filepath.Join(t.TempDir(), "S99btctl.sh")
	writeTestFile(t, filepath.Dir(initScript), filepath.Base(initScript), "#!/bin/sh\nexit 0\n", 0o755)
	if err := runDeploy([]string{bundle, "-dry-run", "-init-script", initScript}, t.TempDir(), ""); err != nil {
		t.Fatalf("dry run: %v", err)
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// the command printed.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := fn()
	os.Stdout = old
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String(), runErr
}

// A dry run must not execute the bundle's daemon: the plan says the probe is
// deferred, even when the binary would fail the real probe.
func TestDeployDryRunDefersDaemonProbe(t *testing.T) {
	sameFilesystemHook(t, false)
	bundle := t.TempDir()
	writeTestFile(t, bundle, "capsulad", "#!/bin/sh\nexit 7\n", 0o755)
	dataDir := t.TempDir()
	out, err := captureStdout(t, func() error {
		return runDeploy([]string{bundle, "-dry-run"}, dataDir, "")
	})
	if err != nil {
		t.Fatalf("dry run with a non-runnable daemon: %v", err)
	}
	if !strings.Contains(out, "daemon probe: deferred (-dry-run)") {
		t.Fatalf("plan does not defer the probe:\n%s", out)
	}
	if !strings.Contains(out, "verifying their origin is the operator's decision") {
		t.Fatalf("plan does not state the bundle-trust decision:\n%s", out)
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("dry run wrote into the data dir: %v", entries)
	}
}

// The bundle binary runs as root, so the probe must not inherit the operator's
// environment.
func TestVerifyDaemonBinaryUsesMinimalEnvironment(t *testing.T) {
	t.Setenv("CAPSULAD_PROBE_LEAK", "secret")
	t.Setenv("OPERATOR_SECRET", "secret")
	binary := filepath.Join(t.TempDir(), "capsulad")
	script := "#!/bin/sh\n" +
		"[ -z \"$CAPSULAD_PROBE_LEAK\" ] || exit 9\n" +
		"[ -z \"$OPERATOR_SECRET\" ] || exit 9\n" +
		"[ -n \"$HOME\" ] || exit 8\n" +
		"[ \"$LANG\" = C ] || exit 7\n" +
		"echo 'capsulad test (sdk 0.5)'\n"
	writeTestFile(t, filepath.Dir(binary), filepath.Base(binary), script, 0o755)
	if err := verifyDaemonBinary(binary); err != nil {
		t.Fatalf("probe with a minimal environment: %v", err)
	}
}

// The bundle budget bounds what a deployment unpacks into the staging
// filesystem, which is not the 78.5 MiB data partition.
func TestBundleLimitsFitStaging(t *testing.T) {
	if bundleLimits.MaxFileBytes != 64<<20 || bundleLimits.MaxTotalBytes != 128<<20 || bundleLimits.MaxEntries != 8192 {
		t.Fatalf("bundle limits = %+v, want 64 MiB per file, 128 MiB total, 8192 entries", bundleLimits)
	}
	if extractionRatio != 2 || stagingReserve != 26<<20 {
		t.Fatalf("staging budget = ratio %d, reserve %d; want ratio 2 and a 26 MiB reserve", extractionRatio, stagingReserve)
	}
}

// The preflight refuses extraction when the archive plus its unpacked tree
// exceeds the free space, and stays silent when free space is unknown.
func TestCheckExtractionSpace(t *testing.T) {
	old := freeSpace
	defer func() { freeSpace = old }()
	dir := t.TempDir()

	freeSpace = func(string) (uint64, bool, error) { return 8 << 20, true, nil }
	err := checkExtractionSpace(dir, 1<<20)
	if err == nil || !strings.Contains(err.Error(), "free space") {
		t.Fatalf("low space error = %v, want an actionable free-space error", err)
	}

	// A ~25 MiB bundle archive must fit in the ~95 MiB temporary filesystem of
	// the Capsula Mini, where the extracted tree and the compressed backup of
	// the old daemon have to live beside it.
	freeSpace = func(string) (uint64, bool, error) { return 95 << 20, true, nil }
	if err := checkExtractionSpace(dir, 25<<20); err != nil {
		t.Fatalf("25 MiB archive on a 95 MiB staging filesystem: %v", err)
	}
	// A bundle whose extracted tree cannot fit is refused before it fills the
	// staging filesystem.
	if err := checkExtractionSpace(dir, 45<<20); err == nil {
		t.Fatal("45 MiB archive on a 95 MiB staging filesystem was accepted")
	}

	freeSpace = func(string) (uint64, bool, error) { return 256 << 20, true, nil }
	if err := checkExtractionSpace(dir, 1<<20); err != nil {
		t.Fatalf("sufficient space: %v", err)
	}

	freeSpace = func(string) (uint64, bool, error) { return 0, false, nil }
	if err := checkExtractionSpace(dir, 1<<20); err != nil {
		t.Fatalf("unknown free space should skip the preflight: %v", err)
	}
}

func TestUnpackBundleRefusesWhenStagingIsFull(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "bundle.tar.gz")
	buildTestArchive(t, archive, map[string]string{"capsulad": "#!/bin/sh\nexit 0\n"})
	old := freeSpace
	defer func() { freeSpace = old }()
	freeSpace = func(string) (uint64, bool, error) { return 1 << 20, true, nil }
	_, _, err := unpackBundle(archive, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "free space") {
		t.Fatalf("unpack error = %v, want a free-space rejection", err)
	}
}

// A directory bundle is inspected in place, so the staging preflight never
// rejects it.
func TestUnpackBundleSkipsPreflightForDirectories(t *testing.T) {
	bundle := t.TempDir()
	writeTestFile(t, bundle, "capsulad", "#!/bin/sh\nexit 0\n", 0o755)
	old := freeSpace
	defer func() { freeSpace = old }()
	freeSpace = func(string) (uint64, bool, error) { return 0, true, nil }
	root, archive, err := unpackBundle(bundle, t.TempDir())
	if err != nil {
		t.Fatalf("directory bundle: %v", err)
	}
	if archive {
		t.Fatal("directory bundle reported as an archive")
	}
	if root != bundle {
		t.Fatalf("bundle root = %s, want %s", root, bundle)
	}
}

// buildTestArchive writes a .tar.gz with the given regular files.
func buildTestArchive(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// Directory bundles are inspected in place, so a symlink would smuggle a file
// from outside the bundle into an installation.
func TestDeployRejectsSymlinkedBundleEntries(t *testing.T) {
	payload := filepath.Join(t.TempDir(), "host-file")
	if err := os.WriteFile(payload, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		set  func(t *testing.T, bundle string)
	}{
		{
			name: "daemon binary",
			set: func(t *testing.T, bundle string) {
				if err := os.Symlink(payload, filepath.Join(bundle, "capsulad")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "lib directory",
			set: func(t *testing.T, bundle string) {
				link := filepath.Join(bundle, "lib")
				if err := os.Symlink(filepath.Dir(payload), link); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "app archive",
			set: func(t *testing.T, bundle string) {
				if err := os.MkdirAll(filepath.Join(bundle, "apps"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(payload, filepath.Join(bundle, "apps", "app.tar.gz")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "boot hook",
			set: func(t *testing.T, bundle string) {
				if err := os.MkdirAll(filepath.Join(bundle, "boot"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(payload, filepath.Join(bundle, "boot", bootHookName)); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sameFilesystemHook(t, false)
			bundle := t.TempDir()
			tc.set(t, bundle)
			err := runDeploy([]string{bundle, "-dry-run"}, t.TempDir(), "")
			if err == nil || !strings.Contains(err.Error(), "symlinks are not supported") {
				t.Fatalf("error = %v, want a symlink rejection", err)
			}
		})
	}
}

func TestDeployNativeRequirementAcceptsSingleFileRuntime(t *testing.T) {
	lib := t.TempDir()
	writeTestFile(t, lib, "capsulad-native.json", `{"abi_major":1,"abi_minor":0,"library":"libcapsulad.so.1"}`, 0o644)
	writeTestFile(t, lib, "libcapsulad.so.1.0.0", "runtime", 0o644)
	d := &deployment{
		libSrc: lib,
		packages: []*artifact.Package{{
			Manifest: &manifest.Manifest{ID: "org.example.native", NativeSDK: &manifest.NativeSDK{ABIMajor: 1}},
		}},
	}
	if err := d.checkNativeRequirements(); err != nil {
		t.Fatalf("bundle runtime without ABI links rejected: %v", err)
	}
	d.packages[0].Manifest.NativeSDK.ABIMajor = 2
	if err := d.checkNativeRequirements(); err == nil || !strings.Contains(err.Error(), "major 2") {
		t.Fatalf("incompatible requirement error = %v, want an ABI major mismatch", err)
	}
}

func TestSyncLibSkipsUnchangedAndReplacesChangedFiles(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, src, "capsulad-native.json", `{"abi_major":1,"abi_minor":0,"library":"libcapsulad.so.1"}`, 0o644)
	writeTestFile(t, src, "libcapsulad.so.1.0.0", "first runtime", 0o644)
	dst := filepath.Join(t.TempDir(), "lib")

	if err := syncLib(src, dst); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	target := filepath.Join(dst, "libcapsulad.so.1.0.0")
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := syncLib(src, dst); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("unchanged runtime was rewritten: before=%v after=%v", before, after)
	}

	writeTestFile(t, src, "libcapsulad.so.1.0.0", "changed runtime", 0o644)
	if err := syncLib(src, dst); err != nil {
		t.Fatalf("sync changed runtime: %v", err)
	}
	replaced, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(after, replaced) {
		t.Fatal("changed runtime retained the old inode")
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "changed runtime" {
		t.Fatalf("changed runtime contents = %q, %v", body, err)
	}
}

func TestSyncLibCreatesAndPreservesRuntimeLayout(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, src, "capsulad-native.json", `{"abi_major":1,"abi_minor":0,"library":"libcapsulad.so.1"}`, 0o644)
	writeTestFile(t, src, "libcapsulad.so.1.0.0", "runtime", 0o644)
	dst := filepath.Join(t.TempDir(), "lib")

	if err := syncLib(src, dst); err != nil {
		t.Fatalf("sync: %v", err)
	}
	for name, want := range map[string]string{
		"libcapsulad.so":   "libcapsulad.so.1",
		"libcapsulad.so.1": "libcapsulad.so.1.0.0",
	} {
		got, err := os.Readlink(filepath.Join(dst, name))
		if err != nil || got != want {
			t.Errorf("link %s = %q, %v; want %q", name, got, err, want)
		}
	}
	links := []string{"libcapsulad.so", "libcapsulad.so.1"}
	before := make([]os.FileInfo, len(links))
	for i, name := range links {
		info, err := os.Lstat(filepath.Join(dst, name))
		if err != nil {
			t.Fatal(err)
		}
		before[i] = info
	}
	if err := syncLib(src, dst); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	for i, name := range links {
		after, err := os.Lstat(filepath.Join(dst, name))
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(before[i], after) {
			t.Errorf("identical link %s was replaced", name)
		}
	}
}

func TestSyncLibRejectsUnsafeRuntimeLinkTargets(t *testing.T) {
	for _, library := range []string{"/outside/libcapsulad.so.1", "../outside/libcapsulad.so.1"} {
		t.Run(library, func(t *testing.T) {
			src := t.TempDir()
			writeTestFile(t, src, "capsulad-native.json", `{"abi_major":1,"abi_minor":0,"library":"`+library+`"}`, 0o644)
			writeTestFile(t, src, "libcapsulad.so.1.0.0", "runtime", 0o644)
			if err := syncLib(src, filepath.Join(t.TempDir(), "lib")); err == nil || !strings.Contains(err.Error(), "plain file name") {
				t.Fatalf("unsafe metadata library error = %v, want path rejection", err)
			}
		})
	}
}

func TestSyncLibReplacesAbsoluteAndEscapingInstalledLinks(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, src, "capsulad-native.json", `{"abi_major":1,"abi_minor":0,"library":"libcapsulad.so.1"}`, 0o644)
	writeTestFile(t, src, "libcapsulad.so.1.0.0", "runtime", 0o644)
	dst := filepath.Join(t.TempDir(), "lib")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/lib/libcapsulad.so.1", filepath.Join(dst, "libcapsulad.so")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../elsewhere/libcapsulad.so.1.0.0", filepath.Join(dst, "libcapsulad.so.1")); err != nil {
		t.Fatal(err)
	}
	if err := syncLib(src, dst); err != nil {
		t.Fatalf("sync: %v", err)
	}
	for name, want := range map[string]string{
		"libcapsulad.so":   "libcapsulad.so.1",
		"libcapsulad.so.1": "libcapsulad.so.1.0.0",
	} {
		got, err := os.Readlink(filepath.Join(dst, name))
		if err != nil || got != want {
			t.Errorf("link %s = %q, %v; want the relative %q", name, got, err, want)
		}
	}
}

func TestSyncLibReplacesDereferencedABIFileCopiesWithLinks(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, src, "capsulad-native.json", `{"abi_major":1,"abi_minor":0,"library":"libcapsulad.so.1"}`, 0o644)
	writeTestFile(t, src, "libcapsulad.so.1.0.0", "runtime", 0o644)
	dst := filepath.Join(t.TempDir(), "lib")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dst, "libcapsulad.so.1.0.0", "runtime", 0o644)
	writeTestFile(t, dst, "libcapsulad.so.1", "runtime", 0o644)
	writeTestFile(t, dst, "libcapsulad.so", "runtime", 0o644)
	if err := syncLib(src, dst); err != nil {
		t.Fatalf("sync: %v", err)
	}
	for name, want := range map[string]string{
		"libcapsulad.so":   "libcapsulad.so.1",
		"libcapsulad.so.1": "libcapsulad.so.1.0.0",
	} {
		got, err := os.Readlink(filepath.Join(dst, name))
		if err != nil || got != want {
			t.Errorf("link %s = %q, %v; want %q", name, got, err, want)
		}
	}
}

func TestSyncLibLeavesCorrectPreexistingLayoutUntouched(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, src, "capsulad-native.json", `{"abi_major":1,"abi_minor":0,"library":"libcapsulad.so.1"}`, 0o644)
	writeTestFile(t, src, "libcapsulad.so.1.0.0", "runtime", 0o644)
	dst := filepath.Join(t.TempDir(), "lib")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dst, "capsulad-native.json", `{"abi_major":1,"abi_minor":0,"library":"libcapsulad.so.1"}`, 0o644)
	writeTestFile(t, dst, "libcapsulad.so.1.0.0", "runtime", 0o644)
	for name, target := range map[string]string{
		"libcapsulad.so":   "libcapsulad.so.1",
		"libcapsulad.so.1": "libcapsulad.so.1.0.0",
	} {
		if err := os.Symlink(target, filepath.Join(dst, name)); err != nil {
			t.Fatal(err)
		}
	}
	links := []string{"libcapsulad.so", "libcapsulad.so.1"}
	before := make([]os.FileInfo, len(links))
	for i, name := range links {
		info, err := os.Lstat(filepath.Join(dst, name))
		if err != nil {
			t.Fatal(err)
		}
		before[i] = info
	}
	if err := syncLib(src, dst); err != nil {
		t.Fatalf("sync existing layout: %v", err)
	}
	for i, name := range links {
		after, err := os.Lstat(filepath.Join(dst, name))
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(before[i], after) {
			t.Errorf("pre-existing link %s was replaced", name)
		}
	}
}

func TestSyncLibDoesNotWriteThroughManagedFileSymlinkToDirectory(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, src, "capsulad-native.json", `{}`, 0o644)
	writeTestFile(t, src, "managed", "bundle", 0o644)
	dst := filepath.Join(t.TempDir(), "lib")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	link := filepath.Join(dst, "managed")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := syncLib(src, dst); err != nil {
		t.Fatalf("sync through managed symlink path: %v", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("wrote through managed symlink: %v", entries)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("managed symlink was not replaced by a regular file: mode %v", info.Mode())
	}
}

func TestSyncLibLeavesLinksAloneWithoutMatchingRuntimeFile(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, src, "capsulad-native.json", `{"abi_major":1,"abi_minor":0,"library":"libcapsulad.so.1"}`, 0o644)
	dst := filepath.Join(t.TempDir(), "lib")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("libcapsulad.so.1", filepath.Join(dst, "libcapsulad.so")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing.so", filepath.Join(dst, "libcapsulad.so.1")); err != nil {
		t.Fatal(err)
	}
	before := make([]os.FileInfo, 2)
	for i, name := range []string{"libcapsulad.so", "libcapsulad.so.1"} {
		info, err := os.Lstat(filepath.Join(dst, name))
		if err != nil {
			t.Fatal(err)
		}
		before[i] = info
	}
	if err := syncLib(src, dst); err != nil {
		t.Fatalf("sync partial runtime: %v", err)
	}
	for i, name := range []string{"libcapsulad.so", "libcapsulad.so.1"} {
		after, err := os.Lstat(filepath.Join(dst, name))
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(before[i], after) {
			t.Errorf("link %s changed without a matching library", name)
		}
	}
}

func TestSyncLibRefusesSymlinkedRuntimeDirectories(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, src, "capsulad-native.json", "{}", 0o644)
	writeTestFile(t, src, "nested/lib.so", "x", 0o644)

	outside := t.TempDir()
	dataDir := t.TempDir()

	linkLib := filepath.Join(dataDir, "lib")
	if err := os.Symlink(outside, linkLib); err != nil {
		t.Fatal(err)
	}
	if err := syncLib(src, linkLib); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked lib dir error = %v, want a symlink refusal", err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("wrote through a symlinked lib dir: %v, %v", entries, err)
	}

	realLib := filepath.Join(t.TempDir(), "lib")
	if err := os.MkdirAll(realLib, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(realLib, "nested")); err != nil {
		t.Fatal(err)
	}
	if err := syncLib(src, realLib); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked lib subdirectory error = %v, want a symlink refusal", err)
	}
}

// The boot hook is only transiently replaced: a failed deployment restores the
// previous script so the rollback restart runs the known-good hook.
func TestActivateBootHookRestoresPreviousOnRollback(t *testing.T) {
	dir := t.TempDir()
	hook := filepath.Join(dir, "S99btctl.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n# original\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "hook")
	if err := os.WriteFile(src, []byte("#!/bin/sh\nexit 9\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &deployment{}
	if err := d.activateBootHook(src, hook); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(hook); err != nil || strings.Contains(string(body), "original") {
		t.Fatalf("hook was not replaced: %q, %v", body, err)
	}
	if _, err := os.Stat(hook + ".prev"); err != nil {
		t.Fatalf("previous hook was not preserved: %v", err)
	}

	err := d.rollback(nil, errors.New("health gate failed"))
	if err == nil || !strings.Contains(err.Error(), "health gate failed") {
		t.Fatalf("rollback error = %v, want the original failure", err)
	}
	body, err := os.ReadFile(hook)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "original") {
		t.Fatalf("previous hook was not restored: %q", string(body))
	}
	if _, err := os.Stat(hook + ".prev"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("transient hook backup survived rollback: %v", err)
	}
}

// A relative -data-dir is resolved once and reported as an absolute path.
func TestDeployResolvesDataDirAbsolute(t *testing.T) {
	sameFilesystemHook(t, false)
	work := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(previous); err != nil {
			t.Fatal(err)
		}
	}()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	bundle := t.TempDir()
	writeTestFile(t, bundle, "lib/capsulad-native.json", `{"abi_major":1,"abi_minor":0,"library":"libcapsulad.so.1"}`, 0o644)

	out, err := captureStdout(t, func() error {
		return runDeploy([]string{bundle, "-dry-run"}, "relative-data", "")
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	want := "data dir: " + filepath.Join(cwd, "relative-data")
	if !strings.Contains(out, want) {
		t.Fatalf("plan does not print the absolute data dir %q:\n%s", want, out)
	}
}

// A successful deployment deletes the transient backups instead of keeping a
// durable copy of the replaced binaries or hook.
func TestAcceptDropsTransientBackups(t *testing.T) {
	dir := t.TempDir()
	daemon := filepath.Join(dir, "capsulad")
	hook := filepath.Join(dir, "S99btctl.sh")
	for _, p := range []string{daemon, daemon + ".prev", hook, hook + ".prev"} {
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d := &deployment{hook: &activation{target: hook, previous: hook + ".prev", hadBefore: true}}
	d.accept([]activation{{target: daemon, previous: daemon + ".prev", hadBefore: true}})
	for _, p := range []string{daemon + ".prev", hook + ".prev"} {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("transient backup %s survived acceptance: %v", p, err)
		}
	}
	for _, p := range []string{daemon, hook} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("activated file %s disappeared: %v", p, err)
		}
	}
}

// sameFilesystemHook pins the filesystem decision for one test so both
// activation paths are exercised on a single filesystem.
func sameFilesystemHook(t *testing.T, same bool) {
	t.Helper()
	old := sameFilesystem
	sameFilesystem = func(string, string) bool { return same }
	t.Cleanup(func() { sameFilesystem = old })
}

// A staged binary on another filesystem cannot be renamed into place, and the
// 78.5 MiB data partition cannot hold the old and new daemon at once: the
// replaced binary is compressed into staging, the target is released and the
// staged bytes are written directly to it.
func TestInstallBinaryCompressesBackupAcrossFilesystems(t *testing.T) {
	sameFilesystemHook(t, false)
	dataDir := t.TempDir()
	target := filepath.Join(dataDir, daemonBinaryName)
	writeTestFile(t, dataDir, daemonBinaryName, "old daemon\n", 0o755)

	d := &deployment{staging: t.TempDir()}
	staged := filepath.Join(d.staging, daemonBinaryName)
	if err := os.WriteFile(staged, []byte("new daemon\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	a, err := d.installBinary(staged, target, false)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if a.kind != activationCompressed {
		t.Fatalf("activation kind = %d, want the compressed backup", a.kind)
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "new daemon\n" {
		t.Fatalf("installed binary = %q, %v", body, err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("installed mode = %04o", info.Mode().Perm())
	}
	if _, err := os.Stat(staged); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("staged input survived activation: %v", err)
	}
	// The backup lives in staging and is compressed; no sibling copy exists on
	// the target filesystem.
	if a.previous != filepath.Join(d.staging, daemonBinaryName+".prev.gz") {
		t.Fatalf("backup path = %s", a.previous)
	}
	if _, err := os.Stat(target + ".prev"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("uncompressed sibling backup on the target filesystem: %v", err)
	}
	f, err := os.Open(a.previous)
	if err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		t.Fatalf("backup is not gzip: %v", err)
	}
	body, err := io.ReadAll(gz)
	gz.Close()
	f.Close()
	if err != nil || string(body) != "old daemon\n" {
		t.Fatalf("backup content = %q, %v", body, err)
	}

	if err := a.restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "old daemon\n" {
		t.Fatalf("restored binary = %q, %v", body, err)
	}
	info, err = os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("restored mode = %04o", info.Mode().Perm())
	}
	if _, err := os.Stat(a.previous); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("backup survived restore: %v", err)
	}
}

// The same-filesystem path keeps the free atomic rename and a sibling .prev.
func TestInstallBinaryRenamesOnSameFilesystem(t *testing.T) {
	sameFilesystemHook(t, true)
	dataDir := t.TempDir()
	target := filepath.Join(dataDir, daemonBinaryName)
	writeTestFile(t, dataDir, daemonBinaryName, "old daemon\n", 0o755)
	staged := filepath.Join(t.TempDir(), daemonBinaryName)
	if err := os.WriteFile(staged, []byte("new daemon\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &deployment{staging: t.TempDir()}

	a, err := d.installBinary(staged, target, false)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if a.kind != activationRename {
		t.Fatalf("activation kind = %d, want the atomic rename", a.kind)
	}
	if body, err := os.ReadFile(a.previous); err != nil || string(body) != "old daemon\n" {
		t.Fatalf("sibling backup = %q, %v", body, err)
	}
	if err := a.restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "old daemon\n" {
		t.Fatalf("restored binary = %q, %v", body, err)
	}
	// Acceptance drops the backup; restore consumes it.
	a.discard()
	if _, err := os.Stat(a.previous); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("backup survived restore: %v", err)
	}
}

// A first install has nothing to back up, on either path.
func TestInstallBinaryWithoutPreviousTarget(t *testing.T) {
	for _, same := range []bool{true, false} {
		sameFilesystemHook(t, same)
		dataDir := t.TempDir()
		target := filepath.Join(dataDir, daemonBinaryName)
		staged := filepath.Join(t.TempDir(), daemonBinaryName)
		if err := os.WriteFile(staged, []byte("new daemon\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		d := &deployment{staging: t.TempDir()}
		a, err := d.installBinary(staged, target, false)
		if err != nil {
			t.Fatalf("same filesystem %v: install: %v", same, err)
		}
		if a.hadBefore {
			t.Fatalf("same filesystem %v: reported a replaced file", same)
		}
		if a.previous != "" {
			if _, err := os.Stat(a.previous); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("same filesystem %v: backup written without a replaced file: %v", same, err)
			}
		}
		if body, err := os.ReadFile(target); err != nil || string(body) != "new daemon\n" {
			t.Fatalf("same filesystem %v: installed binary = %q, %v", same, body, err)
		}
		if err := a.restore(); err != nil {
			t.Fatalf("same filesystem %v: restore: %v", same, err)
		}
		if _, err := os.Stat(target); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("same filesystem %v: first install survived rollback: %v", same, err)
		}
	}
}

// The boot hook cannot be renamed into place on the overlayfs root, so the
// replacement and the backup are both written in place. Rewriting the same
// inode proves no rename happened.
func TestActivateBootHookCopiesInsteadOfRenaming(t *testing.T) {
	dir := t.TempDir()
	hook := filepath.Join(dir, bootHookName)
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n# original\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(hook)
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "hook")
	if err := os.WriteFile(src, []byte("#!/bin/sh\nexit 9\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	d := &deployment{}
	if err := d.activateBootHook(src, hook); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(hook)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("boot hook was replaced by a rename instead of a copy-up")
	}
	if body, err := os.ReadFile(hook); err != nil || strings.Contains(string(body), "original") {
		t.Fatalf("hook was not replaced: %q, %v", body, err)
	}
	if after.Mode().Perm() != 0o755 {
		t.Fatalf("hook mode = %04o", after.Mode().Perm())
	}
	if body, err := os.ReadFile(hook + ".prev"); err != nil || !strings.Contains(string(body), "original") {
		t.Fatalf("previous hook backup = %q, %v", body, err)
	}

	if err := d.rollback(nil, errors.New("health gate failed")); err == nil || !strings.Contains(err.Error(), "health gate failed") {
		t.Fatalf("rollback error = %v, want the original failure", err)
	}
	restored, err := os.Stat(hook)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, restored) {
		t.Fatal("rollback replaced the hook inode instead of writing it back")
	}
	if body, err := os.ReadFile(hook); err != nil || !strings.Contains(string(body), "original") {
		t.Fatalf("previous hook was not restored: %q, %v", body, err)
	}
	if _, err := os.Stat(hook + ".prev"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("transient hook backup survived rollback: %v", err)
	}
}

// A stopped daemon releases its binary inode only when the feeder that took
// over the hardware watchdog is gone. The legacy Go feeder is a copy of the old
// daemon and is asked to hand back; the shell feeder holds no such inode.
func TestReleaseLegacyFeederRequestsHandbackOnlyForTheLegacyFeeder(t *testing.T) {
	dataDir := t.TempDir()
	runDir := filepath.Join(dataDir, "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(runDir, watchdogFeederPID)
	handback := filepath.Join(runDir, watchdogHandback)
	oldWait := watchdogHandbackWait
	watchdogHandbackWait = 2 * time.Second
	defer func() { watchdogHandbackWait = oldWait }()
	d := &deployment{dataDir: dataDir}

	if err := os.WriteFile(pidFile, []byte("shell 4242"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.releaseLegacyFeeder(); err != nil {
		t.Fatalf("shell feeder: %v", err)
	}
	if _, err := os.Stat(handback); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("handback requested from the shell feeder: %v", err)
	}
	if _, err := os.Stat(pidFile); err != nil {
		t.Fatalf("shell feeder pid file removed: %v", err)
	}

	// The legacy feeder watches for the marker and clears its pid file.
	if err := os.WriteFile(pidFile, []byte("4242"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			if _, err := os.Stat(handback); err == nil {
				_ = os.Remove(pidFile)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	if err := d.releaseLegacyFeeder(); err != nil {
		t.Fatalf("legacy feeder: %v", err)
	}
	<-done
	if _, err := os.Stat(pidFile); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("feeder wait ended with the pid file still present")
	}
	if _, err := os.Stat(handback); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("stale handback marker left behind")
	}

	// A feeder that never exits reports failure: the old daemon's inode is
	// still pinned, so the caller must not remove the target.
	watchdogHandbackWait = 200 * time.Millisecond
	if err := os.WriteFile(pidFile, []byte("4242"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := d.releaseLegacyFeeder()
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("stuck feeder error = %v", err)
	}
	if _, err := os.Stat(handback); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("stale handback marker left behind by the failed wait")
	}
}

// deployFakeScript is a daemon stand-in: it answers the two probes the
// deployment depends on.
const deployFakeScript = `#!/bin/sh
case "$1" in
-version) echo 'capsulad test (sdk 0.5)' ;;
-health) exit 0 ;;
esac
exit 0
`

// A full deployment stops the daemon first, installs with nothing running and
// restarts once. It never stages inside the data directory, and it leaves no
// backup behind.
func TestDeployStopsInstallsAndRestarts(t *testing.T) {

	sameFilesystemHook(t, false)
	dataDir := t.TempDir()
	writeTestFile(t, dataDir, daemonBinaryName, "#!/bin/sh\n# old daemon\n", 0o755)

	bundle := t.TempDir()
	writeTestFile(t, bundle, daemonBinaryName, deployFakeScript, 0o755)
	writeTestFile(t, bundle, cliBinaryName, deployFakeScript, 0o755)

	calls := filepath.Join(t.TempDir(), "calls")
	initScript := filepath.Join(t.TempDir(), bootHookName)
	writeTestFile(t, filepath.Dir(initScript), bootHookName,
		"#!/bin/sh\necho \"$1 $CAPSULAD_DATA_DIR\" >> "+calls+"\nexit 0\n", 0o755)

	out, err := captureStdout(t, func() error {
		return runDeploy([]string{bundle, "-init-script", initScript}, dataDir, "")
	})
	if err != nil {
		t.Fatalf("deploy: %v\n%s", err, out)
	}
	for _, want := range []string{"stopped the daemon through", "installed daemon", "daemon healthy"} {
		if !strings.Contains(out, want) {
			t.Fatalf("deploy output lacks %q:\n%s", want, out)
		}
	}
	logged, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	want := "stop " + dataDir + "\nrestart " + dataDir + "\n"
	if string(logged) != want {
		t.Fatalf("boot service calls = %q, want %q", logged, want)
	}
	body, err := os.ReadFile(filepath.Join(dataDir, daemonBinaryName))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != deployFakeScript {
		t.Fatalf("installed daemon = %q", body)
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".prev") || strings.HasSuffix(e.Name(), ".prev.gz") || e.Name() == ".deploy-staging" {
			t.Fatalf("deploy left %q in the data directory", e.Name())
		}
	}
}

// Staging lives under the temporary directory and honors -staging; the plan
// reports it so an operator can see where the space is needed.
func TestDeployStagesOutsideTheDataDirectory(t *testing.T) {
	sameFilesystemHook(t, false)
	bundle := t.TempDir()
	writeTestFile(t, bundle, "lib/capsulad-native.json", `{"abi_major":1,"abi_minor":0,"library":"libcapsulad.so.1"}`, 0o644)

	defaultStaging := filepath.Join(os.TempDir(), "capsulad-deploy-")
	out, err := captureStdout(t, func() error {
		return runDeploy([]string{bundle, "-dry-run"}, t.TempDir(), "")
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !strings.Contains(out, "staging: "+defaultStaging) {
		t.Fatalf("default staging is not under %s:\n%s", os.TempDir(), out)
	}

	parent := t.TempDir()
	out, err = captureStdout(t, func() error {
		return runDeploy([]string{bundle, "-dry-run", "-staging", parent}, t.TempDir(), "")
	})
	if err != nil {
		t.Fatalf("dry run with -staging: %v", err)
	}
	if !strings.Contains(out, "staging: "+filepath.Join(parent, "capsulad-deploy-")) {
		t.Fatalf("-staging parent ignored:\n%s", out)
	}

	// Staging inside the data directory is the space shortage this command
	// exists to avoid.
	dataDir := t.TempDir()
	err = runDeploy([]string{bundle, "-dry-run", "-staging", filepath.Join(dataDir, "staging")}, dataDir, "")
	if err == nil || !strings.Contains(err.Error(), "inside the data directory") {
		t.Fatalf("staging inside the data directory: error = %v", err)
	}
}

// Staging on the data filesystem is allowed only when that filesystem can hold
// the largest staged tree beside both reserves; a host or simulator data
// directory on one big volume qualifies, the 78.5 MiB device partition does
// not. Symlinks are resolved before the check, so they cannot hide the shared
// partition.
func TestDeployChecksStagingOnTheDataFilesystem(t *testing.T) {
	sameFilesystemHook(t, true)
	bundle := t.TempDir()
	writeTestFile(t, bundle, "lib/capsulad-native.json", `{"abi_major":1,"abi_minor":0,"library":"libcapsulad.so.1"}`, 0o644)

	// A single-filesystem host with room to spare keeps working.
	out, err := captureStdout(t, func() error {
		return runDeploy([]string{bundle, "-dry-run"}, t.TempDir(), "")
	})
	if err != nil {
		t.Fatalf("dry run on a roomy single-filesystem host: %v", err)
	}
	if !strings.Contains(out, "staging: "+os.TempDir()) {
		t.Fatalf("plan does not report the staging tree:\n%s", out)
	}

	oldFree := freeSpace
	t.Cleanup(func() { freeSpace = oldFree })
	tooSmall := uint64(bundleLimits.MaxTotalBytes) + stagingReserve + dataReserve - 1
	freeSpace = func(string) (uint64, bool, error) { return tooSmall, true, nil }

	err = runDeploy([]string{bundle, "-dry-run"}, t.TempDir(), "")
	if err == nil || !strings.Contains(err.Error(), "staging there needs") {
		t.Fatalf("staging on a full data filesystem: error = %v", err)
	}

	// A symlinked staging parent is resolved before the check.
	parent := t.TempDir()
	link := filepath.Join(t.TempDir(), "staging-link")
	if err := os.Symlink(parent, link); err != nil {
		t.Fatal(err)
	}
	err = runDeploy([]string{bundle, "-dry-run", "-staging", link}, t.TempDir(), "")
	if err == nil || !strings.Contains(err.Error(), "staging there needs") {
		t.Fatalf("symlinked staging parent: error = %v", err)
	}

	// Exactly enough is enough.
	freeSpace = func(string) (uint64, bool, error) { return tooSmall + 1, true, nil }
	if _, err := captureStdout(t, func() error {
		return runDeploy([]string{bundle, "-dry-run"}, t.TempDir(), "")
	}); err != nil {
		t.Fatalf("exactly sufficient space: %v", err)
	}
}

// A directory bundle belongs to the operator: a cross-filesystem activation
// copies its binary and leaves the bundle entry in place, so the same directory
// can be deployed again.
func TestInstallBinaryKeepsTheBundleInput(t *testing.T) {
	sameFilesystemHook(t, false)
	bundle := t.TempDir()
	staged := filepath.Join(bundle, daemonBinaryName)
	if err := os.WriteFile(staged, []byte("new daemon\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &deployment{staging: t.TempDir()}
	if _, err := d.installBinary(staged, filepath.Join(t.TempDir(), daemonBinaryName), false); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("bundle input removed: %v", err)
	}
}

// A failure after the daemon was stopped must not leave the device without a
// daemon: the previous stack is restarted before the error is reported, even
// when no binary was replaced yet.
func TestDeployRestartsPreviousStackAfterAFailedApply(t *testing.T) {
	sameFilesystemHook(t, false)
	dataDir := t.TempDir()
	// A directory where the binary belongs fails activation after the stop.
	if err := os.MkdirAll(filepath.Join(dataDir, daemonBinaryName), 0o755); err != nil {
		t.Fatal(err)
	}
	bundle := t.TempDir()
	writeTestFile(t, bundle, daemonBinaryName, deployFakeScript, 0o755)

	calls := filepath.Join(t.TempDir(), "calls")
	initScript := filepath.Join(t.TempDir(), bootHookName)
	writeTestFile(t, filepath.Dir(initScript), bootHookName,
		"#!/bin/sh\necho \"$1\" >> "+calls+"\nexit 0\n", 0o755)

	_, err := captureStdout(t, func() error {
		return runDeploy([]string{bundle, "-init-script", initScript, "-timeout", "500ms"}, dataDir, "")
	})
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("deploy error = %v, want the activation failure", err)
	}
	if !strings.Contains(err.Error(), "no binary or boot hook was replaced") {
		t.Fatalf("deploy error does not report the rollback state: %v", err)
	}
	logged, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if string(logged) != "stop\nrestart\n" {
		t.Fatalf("boot service calls = %q, want a stop followed by a restart", logged)
	}
}

// The staging backup of the replaced daemon is compressed while the feeder that
// took over the hardware watchdog is still feeding it. Gzip time must not land
// inside the ~21 s the deployment has to remove the old binary and write the
// new one, so compression precedes the handback; a same-filesystem rename needs
// no space and asks for the handback directly.
func TestInstallBinaryCompressesBeforeTheWatchdogHandback(t *testing.T) {
	var events []string
	oldCompress := compressFile
	compressFile = func(src, dst string) error {
		events = append(events, "compress")
		return oldCompress(src, dst)
	}
	oldRelease := releaseLegacyFeederFn
	releaseLegacyFeederFn = func(*deployment) error { events = append(events, "release"); return nil }
	t.Cleanup(func() {
		compressFile = oldCompress
		releaseLegacyFeederFn = oldRelease
	})

	dataDir := t.TempDir()
	target := filepath.Join(dataDir, daemonBinaryName)
	writeTestFile(t, dataDir, daemonBinaryName, "old daemon\n", 0o755)
	staged := filepath.Join(t.TempDir(), daemonBinaryName)
	if err := os.WriteFile(staged, []byte("new daemon\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &deployment{staging: t.TempDir(), stopped: true}

	sameFilesystemHook(t, false)
	if _, err := d.installBinary(staged, target, true); err != nil {
		t.Fatalf("install across filesystems: %v", err)
	}
	if got := strings.Join(events, ","); got != "compress,release" {
		t.Fatalf("events = %q, want \"compress,release\"", got)
	}

	events = nil
	sameFilesystemHook(t, true)
	if err := os.WriteFile(staged, []byte("newer daemon\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := d.installBinary(staged, target, true); err != nil {
		t.Fatalf("install on one filesystem: %v", err)
	}
	if got := strings.Join(events, ","); got != "release" {
		t.Fatalf("same-filesystem events = %q, want only the handback", got)
	}
}

// The tiny boot hook and the CLI are installed before the daemon, so that after
// the watchdog handback only the daemon's own remove/write and the restart are
// left inside the ~21 s window.
func TestDeployInstallsHookAndCLIBeforeTheDaemon(t *testing.T) {
	sameFilesystemHook(t, false)
	dataDir := t.TempDir()
	writeTestFile(t, dataDir, daemonBinaryName, "#!/bin/sh\n# old daemon\n", 0o755)

	calls := filepath.Join(t.TempDir(), "calls")
	initScript := filepath.Join(t.TempDir(), bootHookName)
	writeTestFile(t, filepath.Dir(initScript), bootHookName,
		"#!/bin/sh\necho \"$1\" >> "+calls+"\nexit 0\n", 0o755)

	bundle := t.TempDir()
	writeTestFile(t, bundle, daemonBinaryName, deployFakeScript, 0o755)
	writeTestFile(t, bundle, cliBinaryName, deployFakeScript, 0o755)
	writeTestFile(t, bundle, "boot/"+bootHookName,
		"#!/bin/sh\n# new hook\necho \"$1\" >> "+calls+"\nexit 0\n", 0o755)

	out, err := captureStdout(t, func() error {
		return runDeploy([]string{bundle, "-init-script", initScript}, dataDir, "")
	})
	if err != nil {
		t.Fatalf("deploy: %v\n%s", err, out)
	}
	last := -1
	for _, want := range []string{
		"installed boot hook",
		"installed " + filepath.Join(dataDir, cliBinaryName),
		"installed daemon",
		"daemon healthy",
	} {
		i := strings.Index(out, want)
		if i < 0 {
			t.Fatalf("deploy output lacks %q:\n%s", want, out)
		}
		if i < last {
			t.Fatalf("deploy output is out of order at %q:\n%s", want, out)
		}
		last = i
	}
	logged, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if string(logged) != "stop\nrestart\n" {
		t.Fatalf("boot service calls = %q, want a stop followed by a restart", logged)
	}
	body, err := os.ReadFile(initScript)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "# new hook") {
		t.Fatalf("boot hook was not installed: %q", body)
	}
	if _, err := os.Stat(initScript + ".prev"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("transient hook backup survived acceptance: %v", err)
	}
}

// The boot hook is written in place, so a failed write leaves it truncated. The
// activation is not recorded in that case, which means the install path itself
// has to put the previous script back.
func TestActivateBootHookRestoresTheHookWhenTheInstallFails(t *testing.T) {
	dir := t.TempDir()
	hook := filepath.Join(dir, bootHookName)
	original := "#!/bin/sh\n# original\nexit 0\n"
	if err := os.WriteFile(hook, []byte(original), 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory opens but cannot be read, so the copy fails after the
	// destination was truncated.
	src := filepath.Join(t.TempDir(), "hook")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}

	d := &deployment{}
	err := d.activateBootHook(src, hook)
	if err == nil || !strings.Contains(err.Error(), "install boot hook") {
		t.Fatalf("error = %v, want an install failure", err)
	}
	body, err := os.ReadFile(hook)
	if err != nil || string(body) != original {
		t.Fatalf("hook after a failed install = %q, %v", body, err)
	}
	info, err := os.Stat(hook)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("hook mode = %04o", info.Mode().Perm())
	}
	if _, err := os.Stat(hook + ".prev"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("backup left behind after a successful restore: %v", err)
	}
	if d.hook != nil {
		t.Fatal("a failed install recorded an activation")
	}
}

// A first install of the hook has nothing to restore, and a failed write must
// not leave a partial script behind.
func TestActivateBootHookRemovesAPartialFirstInstall(t *testing.T) {
	hook := filepath.Join(t.TempDir(), bootHookName)
	src := filepath.Join(t.TempDir(), "hook")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	d := &deployment{}
	if err := d.activateBootHook(src, hook); err == nil {
		t.Fatal("installing from a directory succeeded")
	}
	if _, err := os.Stat(hook); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("partial hook left behind: %v", err)
	}
	if d.hook != nil {
		t.Fatal("a failed install recorded an activation")
	}
}

// A feeder that still pins the old daemon's inode stops the cross-filesystem
// activation before the target is removed: the binary stays intact and the
// useless backup is dropped.
func TestInstallBinaryAbortsWhenTheFeederWillNotRelease(t *testing.T) {
	sameFilesystemHook(t, false)
	old := releaseLegacyFeederFn
	releaseLegacyFeederFn = func(*deployment) error { return errors.New("feeder still running") }
	t.Cleanup(func() { releaseLegacyFeederFn = old })

	dataDir := t.TempDir()
	target := filepath.Join(dataDir, daemonBinaryName)
	writeTestFile(t, dataDir, daemonBinaryName, "old daemon\n", 0o755)
	d := &deployment{staging: t.TempDir(), stopped: true}
	staged := filepath.Join(d.staging, daemonBinaryName)
	if err := os.WriteFile(staged, []byte("new daemon\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := d.installBinary(staged, target, true); err == nil || !strings.Contains(err.Error(), "feeder still running") {
		t.Fatalf("install error = %v, want the feeder failure", err)
	}
	body, err := os.ReadFile(target)
	if err != nil || string(body) != "old daemon\n" {
		t.Fatalf("target after the aborted install = %q, %v", body, err)
	}
	entries, err := os.ReadDir(d.staging)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".prev.gz") {
			t.Fatalf("useless backup left behind: %s", e.Name())
		}
	}
}

// When the stop cannot be verified the daemon may already be gone, so the
// previous stack is started again instead of reporting a failure on a silent
// device.
func TestDeployRestartsTheStackWhenTheStopCannotBeVerified(t *testing.T) {
	sameFilesystemHook(t, false)
	old := waitDaemonStoppedFn
	waitDaemonStoppedFn = func(string, string, time.Duration) error { return errors.New("still serving") }
	t.Cleanup(func() { waitDaemonStoppedFn = old })

	dataDir := t.TempDir()
	bundle := t.TempDir()
	writeTestFile(t, bundle, daemonBinaryName, deployFakeScript, 0o755)
	calls := filepath.Join(t.TempDir(), "calls")
	initScript := filepath.Join(t.TempDir(), bootHookName)
	writeTestFile(t, filepath.Dir(initScript), bootHookName,
		"#!/bin/sh\necho \"$1\" >> "+calls+"\nexit 0\n", 0o755)

	_, err := captureStdout(t, func() error {
		return runDeploy([]string{bundle, "-init-script", initScript}, dataDir, "")
	})
	if err == nil || !strings.Contains(err.Error(), "still serving") {
		t.Fatalf("deploy error = %v, want the stop verification failure", err)
	}
	if !strings.Contains(err.Error(), "the previous stack was restarted") {
		t.Fatalf("deploy error does not report the restart: %v", err)
	}
	logged, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if string(logged) != "stop\nrestart\n" {
		t.Fatalf("boot service calls = %q, want a stop followed by a restart", logged)
	}
	if _, err := os.Stat(filepath.Join(dataDir, daemonBinaryName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the deployment installed a binary it should not have: %v", err)
	}
}

// With -restart never the daemon is not stopped by the deployment. Removing the
// binary of a daemon that still answers frees nothing and could not be written
// again, so the replacement is refused instead of attempted.
func TestInstallBinaryRefusesToReplaceAServingDaemon(t *testing.T) {
	sameFilesystemHook(t, false)
	old := daemonServing
	daemonServing = func(string) bool { return true }
	t.Cleanup(func() { daemonServing = old })

	dataDir := t.TempDir()
	target := filepath.Join(dataDir, daemonBinaryName)
	writeTestFile(t, dataDir, daemonBinaryName, "old daemon\n", 0o755)
	d := &deployment{dataDir: dataDir, staging: t.TempDir(), socket: filepath.Join(dataDir, "run", "hal.sock")}
	staged := filepath.Join(d.staging, daemonBinaryName)
	if err := os.WriteFile(staged, []byte("new daemon\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := d.installBinary(staged, target, true); err == nil || !strings.Contains(err.Error(), "still answers") {
		t.Fatalf("install error = %v, want the serving-daemon refusal", err)
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "old daemon\n" {
		t.Fatalf("target after the refused install = %q, %v", body, err)
	}

	// The same refusal never applies to the CLI: its inode is not the daemon.
	cliTarget := filepath.Join(dataDir, cliBinaryName)
	if _, err := d.installBinary(staged, cliTarget, false); err != nil {
		t.Fatalf("cli install: %v", err)
	}
}

// -restart never installs without stopping the daemon: no stop call, and the
// binary is replaced in place.
func TestDeployNeverRestartInstallsWithoutStopping(t *testing.T) {
	sameFilesystemHook(t, false)
	old := daemonServing
	daemonServing = func(string) bool { return false }
	t.Cleanup(func() { daemonServing = old })

	dataDir := t.TempDir()
	writeTestFile(t, dataDir, daemonBinaryName, "#!/bin/sh\n# old daemon\n", 0o755)
	bundle := t.TempDir()
	writeTestFile(t, bundle, daemonBinaryName, deployFakeScript, 0o755)
	calls := filepath.Join(t.TempDir(), "calls")
	initScript := filepath.Join(t.TempDir(), bootHookName)
	writeTestFile(t, filepath.Dir(initScript), bootHookName,
		"#!/bin/sh\necho \"$1\" >> "+calls+"\nexit 0\n", 0o755)

	out, err := captureStdout(t, func() error {
		return runDeploy([]string{bundle, "-init-script", initScript, "-restart", "never"}, dataDir, "")
	})
	if err != nil {
		t.Fatalf("deploy: %v\n%s", err, out)
	}
	if !strings.Contains(out, "restart skipped (-restart never)") {
		t.Fatalf("deploy output does not report the skipped restart:\n%s", out)
	}
	if _, err := os.Stat(calls); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the boot service was invoked with -restart never: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(dataDir, daemonBinaryName)); err != nil || string(body) != deployFakeScript {
		t.Fatalf("installed daemon = %q, %v", body, err)
	}
}

// A binary that cannot be restored keeps the staging tree: the compressed backup
// in it is the only remaining copy.
func TestInstallBinaryKeepsTheBackupWhenRestoreFails(t *testing.T) {
	sameFilesystemHook(t, false)
	old := compressFile
	compressFile = func(string, dst string) error { return os.WriteFile(dst, []byte("not gzip"), 0o600) }
	t.Cleanup(func() { compressFile = old })

	dataDir := t.TempDir()
	target := filepath.Join(dataDir, daemonBinaryName)
	writeTestFile(t, dataDir, daemonBinaryName, "old daemon\n", 0o755)
	d := &deployment{dataDir: dataDir, staging: t.TempDir(), stopped: true}
	// A directory opens but cannot be read, so the write fails after the target
	// was truncated and the restore then fails on the corrupt backup.
	staged := filepath.Join(t.TempDir(), daemonBinaryName)
	if err := os.MkdirAll(staged, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := d.installBinary(staged, target, true)
	if err == nil || !strings.Contains(err.Error(), "backup kept at") {
		t.Fatalf("install error = %v, want the kept-backup report", err)
	}
	if !d.keepStaging {
		t.Fatal("staging would be removed although it holds the only backup")
	}
	if _, err := os.Stat(filepath.Join(d.staging, daemonBinaryName+".prev.gz")); err != nil {
		t.Fatalf("backup missing from staging: %v", err)
	}
}

// The rollback path reports an unrestorable backup with its path and keeps the
// staging tree that holds it.
func TestRollbackKeepsStagingWhenABackupCannotBeRestored(t *testing.T) {
	d := &deployment{staging: t.TempDir()}
	backup := filepath.Join(d.staging, daemonBinaryName+".prev.gz")
	if err := os.WriteFile(backup, []byte("not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := activation{
		target:    filepath.Join(t.TempDir(), daemonBinaryName),
		previous:  backup,
		kind:      activationCompressed,
		mode:      0o755,
		hadBefore: true,
	}
	err := d.rollback([]activation{a}, errors.New("health gate failed"))
	if err == nil || !strings.Contains(err.Error(), "rollback incomplete") {
		t.Fatalf("rollback error = %v", err)
	}
	if !strings.Contains(err.Error(), backup) {
		t.Fatalf("rollback error does not name the kept backup: %v", err)
	}
	if !d.keepStaging {
		t.Fatal("staging would be removed although it holds the only backup")
	}
}

// The data filesystem has to absorb the part of a binary write that removing
// the old file cannot free, before the daemon is stopped.
func TestDeployRefusesWhenTheDataPartitionCannotTakeTheBinaries(t *testing.T) {
	sameFilesystemHook(t, false)
	oldFree := freeSpace
	freeSpace = func(string) (uint64, bool, error) { return 2 << 20, true, nil }
	t.Cleanup(func() { freeSpace = oldFree })

	dataDir := t.TempDir()
	writeTestFile(t, dataDir, daemonBinaryName, "#!/bin/sh\n# old daemon\n", 0o755)
	bundle := t.TempDir()
	writeTestFile(t, bundle, daemonBinaryName, deployFakeScript, 0o755)
	calls := filepath.Join(t.TempDir(), "calls")
	initScript := filepath.Join(t.TempDir(), bootHookName)
	writeTestFile(t, filepath.Dir(initScript), bootHookName,
		"#!/bin/sh\necho \"$1\" >> "+calls+"\nexit 0\n", 0o755)

	_, err := captureStdout(t, func() error {
		return runDeploy([]string{bundle, "-init-script", initScript}, dataDir, "")
	})
	if err == nil || !strings.Contains(err.Error(), "free space") {
		t.Fatalf("deploy error = %v, want a data-space rejection", err)
	}
	if _, err := os.Stat(calls); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the daemon was stopped before the space check: %v", err)
	}
}

// The CLI that is running from the data directory cannot free its own inode, so
// its full staged size is required; a replaceable CLI only needs the delta.
func TestCheckDataSpaceCountsThePinnedCLI(t *testing.T) {
	sameFilesystemHook(t, false)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	selfInfo, err := os.Stat(self)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	cliTarget := filepath.Join(dataDir, cliBinaryName)
	if err := os.Link(self, cliTarget); err != nil {
		t.Skipf("hardlink unavailable: %v", err)
	}
	staged := filepath.Join(t.TempDir(), cliBinaryName)
	f, err := os.Create(staged)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(selfInfo.Size()); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	d := &deployment{dataDir: dataDir, cliStaged: staged}

	size, ok := d.pinnedWriteSize(staged, cliTarget, true)
	if !ok || size != uint64(selfInfo.Size()) {
		t.Fatalf("pinned write size = %d, %v; want the full staged size", size, ok)
	}
	if size, ok := d.pinnedWriteSize(staged, cliTarget, false); !ok || size != 0 {
		t.Fatalf("replaceable write size = %d, %v; want 0", size, ok)
	}
	if !runningExecutable(cliTarget) {
		t.Fatal("the hardlinked CLI was not recognised as the running executable")
	}

	oldFree := freeSpace
	t.Cleanup(func() { freeSpace = oldFree })
	freeSpace = func(string) (uint64, bool, error) {
		return uint64(selfInfo.Size()) + dataReserve - 1, true, nil
	}
	if err := d.checkDataSpace(); err == nil || !strings.Contains(err.Error(), "free space") {
		t.Fatalf("checkDataSpace error = %v, want the pinned CLI to be counted", err)
	}
	freeSpace = func(string) (uint64, bool, error) {
		return uint64(selfInfo.Size()) + dataReserve, true, nil
	}
	if err := d.checkDataSpace(); err != nil {
		t.Fatalf("checkDataSpace with sufficient space: %v", err)
	}
}

// A daemon that is alive but not answering still holds the socket and the
// binary: the pid from its own pid file has to be gone too.
func TestWaitDaemonStoppedRequiresThePIDToBeGone(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "hal.sock")
	pidFile := filepath.Join(dir, daemonPIDName)

	if err := waitDaemonStopped(socket, pidFile, 100*time.Millisecond); err != nil {
		t.Fatalf("no pid file: %v", err)
	}

	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	err := waitDaemonStopped(socket, pidFile, 200*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not stop") {
		t.Fatalf("live pid error = %v, want a stop failure", err)
	}
	if !strings.Contains(err.Error(), "still names running pid") {
		t.Fatalf("live pid error does not name the process: %v", err)
	}

	if err := os.WriteFile(pidFile, []byte("4194304"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := waitDaemonStopped(socket, pidFile, 200*time.Millisecond); err != nil {
		t.Fatalf("stale pid: %v", err)
	}
}
