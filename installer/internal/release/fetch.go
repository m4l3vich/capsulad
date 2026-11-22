package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"capsulad/installer/internal/atomicfile"
)

// Size ceilings bound a bundle supplied by a hostile or mistaken server. They
// are deliberately larger than the Echo Mini device budgets; the device
// deployer applies the real partition checks.
const (
	BundleMaxBytes = 512 << 20
	AppMaxBytes    = 256 << 20
)

// Progress reports bytes written for one artifact.
type Progress func(label string, done, total int64)

// Fetcher downloads a release over HTTP(S). Client is injectable so tests can
// use an in-process TLS server; a nil Client gets a normal verifying client.
type Fetcher struct {
	BaseURL  string
	Client   *http.Client
	CacheDir string
	Progress Progress
	Log      func(string, ...any)
}

// NewFetcher builds a fetcher rooted at base. An empty base means the latest
// GitHub release.
func NewFetcher(base, cacheDir string) *Fetcher {
	return &Fetcher{
		BaseURL:  NormalizeBaseURL(base),
		CacheDir: cacheDir,
		Client: &http.Client{
			Timeout: 0,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("too many redirects")
				}
				if req.URL.Scheme != "https" {
					host := req.URL.Hostname()
					if host != "127.0.0.1" && host != "localhost" && host != "::1" {
						return fmt.Errorf("refusing a redirect to non-HTTPS %s", req.URL.String())
					}
				}
				return nil
			},
		},
	}
}

// ManifestURL is the manifest location for this release base.
func (f *Fetcher) ManifestURL() string {
	return strings.TrimRight(f.BaseURL, "/") + "/" + ManifestFile
}

// FetchManifest downloads release.json. Release manifests are small.
func (f *Fetcher) FetchManifest(ctx context.Context) (*Manifest, error) {
	if err := f.checkScheme(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.ManifestURL(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch release manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch release manifest: HTTP %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("fetch release manifest: %w", err)
	}
	m, err := Load(data)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// Artifact returns a verified local file for one manifest entry, downloading it
// into the cache when needed. Cached bytes are re-hashed before reuse.
func (f *Fetcher) Artifact(ctx context.Context, version string, file File, max int64) (string, error) {
	if err := CheckRelativePath(file.Path); err != nil {
		return "", err
	}
	if file.Size <= 0 || file.Size > max {
		return "", fmt.Errorf("%s: declared size %d exceeds the %d byte limit", file.Path, file.Size, max)
	}
	if err := f.checkScheme(); err != nil {
		return "", err
	}
	dest := filepath.Join(f.CacheDir, cacheVersion(version), filepath.FromSlash(file.Path))
	if ok, err := verifyExisting(dest, file); err == nil && ok {
		f.logf("cached %s (%s)", file.Path, file.SHA256)
		return dest, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	u, err := url.JoinPath(f.BaseURL, file.Path)
	if err != nil {
		return "", err
	}
	if err := f.download(ctx, u, dest, file, max); err != nil {
		return "", err
	}
	return dest, nil
}

func (f *Fetcher) download(ctx context.Context, u, dest string, want File, max int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := f.client().Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", want.Path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %s", want.Path, resp.Status)
	}
	if resp.ContentLength > max {
		return fmt.Errorf("download %s: server announces %d bytes, over the %d byte limit", want.Path, resp.ContentLength, max)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".download-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()
	h := sha256.New()
	var written int64
	buf := make([]byte, 128<<10)
	report := f.Progress
	var last time.Time
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if written+int64(n) > max {
				return fmt.Errorf("download %s: over the %d byte limit", want.Path, max)
			}
			if _, werr := tmp.Write(buf[:n]); werr != nil {
				return werr
			}
			h.Write(buf[:n])
			written += int64(n)
			if report != nil && time.Since(last) > 100*time.Millisecond {
				report(want.Path, written, want.Size)
				last = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("download %s: %w", want.Path, rerr)
		}
	}
	if report != nil {
		report(want.Path, written, want.Size)
	}
	if written != want.Size {
		return fmt.Errorf("download %s: got %d bytes, manifest says %d", want.Path, written, want.Size)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want.SHA256 {
		return fmt.Errorf("download %s: sha256 %s does not match manifest %s", want.Path, got, want.SHA256)
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return atomicfile.Replace(tmpName, dest)
}

func (f *Fetcher) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return http.DefaultClient
}

func (f *Fetcher) logf(format string, args ...any) {
	if f.Log != nil {
		f.Log(format, args...)
	}
}

// checkScheme keeps release traffic on verified TLS. Loopback HTTP is allowed
// for an operator running a test server locally.
func (f *Fetcher) checkScheme() error {
	u, err := url.Parse(f.BaseURL)
	if err != nil {
		return fmt.Errorf("release URL: %w", err)
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	if u.Scheme == "http" && (host == "127.0.0.1" || host == "localhost" || host == "::1") {
		return nil
	}
	return fmt.Errorf("release URL %q must use https", f.BaseURL)
}

func verifyExisting(name string, want File) (bool, error) {
	info, err := os.Stat(name)
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() != want.Size {
		return false, nil
	}
	f, err := os.Open(name)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == want.SHA256, nil
}

func cacheVersion(version string) string {
	var b strings.Builder
	for _, r := range version {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" || out == "." || out == ".." {
		return "unknown"
	}
	return out
}
