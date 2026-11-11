package provenance

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"capsulad/internal/manifest"
)

func TestGenuineSigstoreFixtureVerifies(t *testing.T) {
	bundlePath := filepath.Join("testdata", "bundle-provenance.json")
	rootPath := filepath.Join("testdata", "trusted-root-public-good.json")
	digest, err := hex.DecodeString("76176ffa33808b54602c7c35de5c6e9a4deb96066dba6533f50ac234f4f1f4c6b3527515dc17c06fbe2860030f410eee69ea20079bd3a2c6f3dcf3b329b10751")
	if err != nil {
		t.Fatal(err)
	}
	out, err := verifyBundle(bundlePath, rootPath, "sigstore/sigstore-js", ".github/workflows/release.yml", "sha512", digest)
	if err != nil {
		t.Fatalf("genuine bundle failed verification: %v", err)
	}
	if out.Repository != "sigstore/sigstore-js" {
		t.Fatalf("repository = %q", out.Repository)
	}
	if out.Workflow != ".github/workflows/release.yml" {
		t.Fatalf("workflow = %q", out.Workflow)
	}
	if out.CommitSHA == "" {
		t.Fatal("commit SHA was not extracted from the SLSA predicate")
	}
	// Wrong repository identity must fail.
	if _, err := verifyBundle(bundlePath, rootPath, "attacker/repo", "", "sha512", digest); err == nil {
		t.Fatal("bundle verified for a mismatched repository")
	}
	// Wrong digest must fail.
	bad := append([]byte(nil), digest...)
	bad[0] ^= 0xff
	if _, err := verifyBundle(bundlePath, rootPath, "sigstore/sigstore-js", "", "sha512", bad); err == nil {
		t.Fatal("bundle verified for a mismatched artifact digest")
	}
}

func TestStatusesWithoutEvidence(t *testing.T) {
	dir := t.TempDir()
	m := &manifest.Manifest{ID: "org.example.app", Entrypoint: manifest.Entrypoint{Runtime: manifest.RuntimeNative}}
	res := Verify(m.ID, dir, m, Config{})
	if res.Status != StatusNotProvided {
		t.Fatalf("status = %s, want NOT_PROVIDED", res.Status)
	}
	m.Entrypoint.Runtime = manifest.RuntimeQuickJS
	res = Verify(m.ID, dir, m, Config{})
	if res.Status != StatusUnsupported {
		t.Fatalf("status = %s, want UNSUPPORTED_RUNTIME", res.Status)
	}
	// Declared evidence with a missing subject fails loudly and is not marked verified.
	m.Entrypoint.Runtime = manifest.RuntimeNative
	subject := filepath.Join(dir, "bin")
	if err := os.MkdirAll(subject, 0o755); err != nil {
		t.Fatal(err)
	}
	binaryPath := filepath.Join(subject, "app")
	if err := os.WriteFile(binaryPath, []byte("not the attested binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	m.GitHubAttestation = &manifest.GitHubAttestation{
		Subject: "bin/app", Bundle: "missing.jsonl", Repository: "example/repo",
	}
	res = Verify(m.ID, dir, m, Config{TrustedRootPath: filepath.Join("testdata", "trusted-root-public-good.json")})
	if res.Status != StatusFailed {
		t.Fatalf("status = %s, want VERIFICATION_FAILED (%s)", res.Status, res.Detail)
	}
	if res.SubjectSHA256 == "" {
		t.Fatal("subject digest was not computed")
	}
}
