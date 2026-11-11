// Package provenance verifies optional GitHub Artifact Attestations evidence
// for native app binaries.
//
// The daemon computes the status; a manifest cannot declare itself verified.
// Verification uses an offline Sigstore trusted root and checks the shipped
// executable digest, DSSE signature, certificate chain, transparency-log
// inclusion/timestamp and the expected GitHub repository/workflow identity.
// Verified origin attests build integrity under that trust policy; it is not a
// statement that the source or runtime behavior is safe.
package provenance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"google.golang.org/protobuf/encoding/protojson"

	"capsulad/internal/manifest"
)

type Status string

const (
	StatusNotProvided Status = "PROVENANCE_NOT_PROVIDED"
	StatusVerified    Status = "PROVENANCE_VERIFIED"
	StatusFailed      Status = "PROVENANCE_VERIFICATION_FAILED"
	StatusUnverified  Status = "PROVENANCE_UNVERIFIED"
	StatusUnsupported Status = "PROVENANCE_UNSUPPORTED_RUNTIME"
)

type Result struct {
	AppID              string
	Status             Status
	Detail             string
	SubjectSHA256      string
	Repository         string
	RepositoryID       string
	Workflow           string
	CommitSHA          string
	VerifiedAt         time.Time
	TrustedRootCurrent bool
}

// Config controls the trust policy.
type Config struct {
	// TrustedRootPath is an offline Sigstore trusted_root.json. Verification
	// never depends on GitHub reachability at app startup.
	TrustedRootPath string
	// MaxRootAge flags a stale root in diagnostics; it does not block
	// verification because trust-root update policy is an operator decision.
	MaxRootAge time.Duration
}

// Verify checks one installed native package.
func Verify(appID, appDir string, m *manifest.Manifest, cfg Config) Result {
	res := Result{AppID: appID, VerifiedAt: time.Now().UTC()}
	if m.Entrypoint.Runtime != manifest.RuntimeNative {
		res.Status = StatusUnsupported
		res.Detail = "provenance verification applies to native binaries only; script packages report unsupported"
		return res
	}
	if m.GitHubAttestation == nil {
		res.Status = StatusNotProvided
		res.Detail = "no GitHub attestation evidence packaged"
		return res
	}
	decl := m.GitHubAttestation
	subjectPath, err := safeJoin(appDir, decl.Subject)
	if err != nil {
		res.Status = StatusFailed
		res.Detail = "attestation subject: " + err.Error()
		return res
	}
	data, err := os.ReadFile(subjectPath)
	if err != nil {
		res.Status = StatusFailed
		res.Detail = "read executable: " + err.Error()
		return res
	}
	sum := sha256.Sum256(data)
	res.SubjectSHA256 = hex.EncodeToString(sum[:])

	if cfg.TrustedRootPath == "" {
		res.Status = StatusUnverified
		res.Detail = "no Sigstore trusted root configured; evidence cannot be evaluated"
		return res
	}
	bundlePath, err := safeJoin(appDir, decl.Bundle)
	if err != nil {
		res.Status = StatusFailed
		res.Detail = "attestation bundle: " + err.Error()
		return res
	}
	out, err := verifyBundle(bundlePath, cfg.TrustedRootPath, decl.Repository, decl.Workflow, "sha256", sum[:])
	if err != nil {
		res.Status = StatusFailed
		res.Detail = err.Error()
		return res
	}
	res.Status = StatusVerified
	res.Repository = out.Repository
	res.Workflow = out.Workflow
	res.CommitSHA = out.CommitSHA
	res.Detail = "verified executable digest, DSSE signature, certificate chain, transparency log and GitHub workflow identity; " +
		"verified origin is not a safety guarantee"
	if exp, ok := rootExpiry(cfg.TrustedRootPath); ok {
		res.TrustedRootCurrent = time.Since(exp) < 0
	}
	return res
}

type verifyOutcome struct {
	Repository string
	Workflow   string
	CommitSHA  string
	SAN        string
	Issuer     string
}

// verifyBundle is the cryptographic core, separated so tests can verify a real
// Sigstore fixture without possessing its subject artifact.
func verifyBundle(bundlePath, trustedRootPath, repository, expectedWorkflow, algorithm string, digest []byte) (verifyOutcome, error) {
	line, err := firstJSONLine(bundlePath)
	if err != nil {
		return verifyOutcome{}, err
	}
	var pb protobundle.Bundle
	if err := protojson.Unmarshal([]byte(line), &pb); err != nil {
		return verifyOutcome{}, fmt.Errorf("parse attestation bundle: %w", err)
	}
	b, err := bundle.NewBundle(&pb)
	if err != nil {
		return verifyOutcome{}, fmt.Errorf("invalid Sigstore bundle: %w", err)
	}
	trusted, err := root.NewTrustedRootFromPath(trustedRootPath)
	if err != nil {
		return verifyOutcome{}, fmt.Errorf("trusted root: %w", err)
	}
	verifier, err := verify.NewSignedEntityVerifier(trusted,
		verify.WithTransparencyLog(1),
		verify.WithObserverTimestamps(1),
		verify.WithSignedCertificateTimestamps(1),
	)
	if err != nil {
		return verifyOutcome{}, fmt.Errorf("verifier: %w", err)
	}
	repo := regexp.QuoteMeta(repository)
	san := "^https://github\\.com/" + repo + "/"
	if expectedWorkflow != "" {
		san += regexp.QuoteMeta(expectedWorkflow) + "@refs/.+$"
	} else {
		san += ".+$"
	}
	identity, err := verify.NewShortCertificateIdentity("https://token.actions.githubusercontent.com", "", "", san)
	if err != nil {
		return verifyOutcome{}, fmt.Errorf("identity policy: %w", err)
	}
	policy := verify.NewPolicy(
		verify.WithArtifactDigest(algorithm, digest),
		verify.WithCertificateIdentity(identity),
	)
	result, err := verifier.Verify(b, policy)
	if err != nil {
		return verifyOutcome{}, fmt.Errorf("verification failed: %w", err)
	}
	out := verifyOutcome{}
	if result.Signature != nil && result.Signature.Certificate != nil {
		out.SAN = result.Signature.Certificate.SubjectAlternativeName
		out.Issuer = result.Signature.Certificate.CertificateIssuer
	}
	out.Repository, out.Workflow = parseGitHubSAN(out.SAN)
	if result.Statement != nil && result.Statement.Predicate != nil {
		var predicate map[string]any
		raw, err := protojson.Marshal(result.Statement.Predicate)
		if err == nil {
			_ = json.Unmarshal(raw, &predicate)
			out.CommitSHA = extractCommit(predicate)
		}
	}
	return out, nil
}

func parseGitHubSAN(san string) (repo, workflow string) {
	const prefix = "https://github.com/"
	if !strings.HasPrefix(san, prefix) {
		return "", ""
	}
	rest := strings.TrimPrefix(san, prefix)
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		return "", ""
	}
	repo = parts[0] + "/" + parts[1]
	if len(parts) == 3 {
		wf := parts[2]
		if i := strings.Index(wf, "@refs/"); i >= 0 {
			wf = wf[:i]
		}
		workflow = wf
	}
	return repo, workflow
}

func extractCommit(predicate map[string]any) string {
	// SLSA v0.2: invocation.environment.GITHUB_SHA
	if inv, ok := predicate["invocation"].(map[string]any); ok {
		if env, ok := inv["environment"].(map[string]any); ok {
			if sha, ok := env["GITHUB_SHA"].(string); ok {
				return sha
			}
		}
		if src, ok := inv["configSource"].(map[string]any); ok {
			if d, ok := src["digest"].(map[string]any); ok {
				for _, key := range []string{"sha1", "sha256"} {
					if v, ok := d[key].(string); ok {
						return v
					}
				}
			}
		}
	}
	// SLSA v1: buildDefinition.externalParameters.workflow.ref / resolvedDependencies
	if bd, ok := predicate["buildDefinition"].(map[string]any); ok {
		if ext, ok := bd["externalParameters"].(map[string]any); ok {
			if wf, ok := ext["workflow"].(map[string]any); ok {
				if ref, ok := wf["ref"].(string); ok {
					return ref
				}
			}
		}
	}
	return ""
}

func firstJSONLine(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read attestation bundle: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "{") {
			return "", fmt.Errorf("attestation bundle line is not a JSON object")
		}
		return line, nil
	}
	return "", fmt.Errorf("attestation bundle is empty")
}

func safeJoin(root, rel string) (string, error) {
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == "." || strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("path %q escapes the package", rel)
	}
	full := filepath.Join(root, clean)
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absFull, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(absFull, absRoot+string(filepath.Separator)) && absFull != absRoot {
		return "", fmt.Errorf("path %q escapes the package", rel)
	}
	return absFull, nil
}

func rootExpiry(path string) (time.Time, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}, false
	}
	return st.ModTime().Add(30 * 24 * time.Hour), true
}
