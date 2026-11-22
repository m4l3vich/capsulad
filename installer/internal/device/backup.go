package device

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"capsulad/installer/internal/atomicfile"
)

// StockBackupIncludes are the vendor paths preserved before anything changes.
// /data is the whole overlay; the init scripts and network configuration are
// outside it and are copied explicitly when present.
var StockBackupIncludes = []string{
	"/etc/init.d/S97voice-agent.sh",
	"/etc/init.d/S99sysmon.sh",
	"/etc/init.d/S99btctl.sh",
	"/etc/wpa_supplicant.conf",
	"/etc/network",
}

// StockBackupExcludes are the generic paths that must not be captured:
// lost+found is filesystem noise and /data/capsdemo is the Rockchip stock data
// directory. BackupExcludes also removes the active platform's data directory.
var StockBackupExcludes = []string{"/data/lost+found", "/data/capsdemo"}

// BackupSentinel is written to stderr only after tar exits successfully. It
// is the transport-independent success signal: exec-out carries no exit status,
// and a pty merges stderr into stdout (the host strips the trailing marker).
const BackupSentinel = "__CAPSULAD_BACKUP_OK__"

// BackupExcludes returns the generic exclusions plus the active data directory.
// A reinstall on Amlogic stock must not capture the previous capsulad payload.
func BackupExcludes(dataDir string) []string {
	out := append([]string(nil), StockBackupExcludes...)
	if dataDir != "" && dataDir != "/data/capsdemo" {
		out = append(out, dataDir)
	}
	return out
}

// BackupCommand streams an uncompressed tar of /data (minus the exclusions)
// plus the stock network and init files that exist. The host gzips it, so the
// stored backup is a .tar.gz. The sentinel is emitted on stderr after tar; with
// set -e a failed tar exits before it, so a truncated stream is never accepted.
func BackupCommand(dataDir string) Command {
	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString("set -- data\n")
	b.WriteString("for p in")
	for _, inc := range StockBackupIncludes {
		b.WriteString(" ")
		b.WriteString(strings.TrimPrefix(inc, "/"))
	}
	b.WriteString("; do [ -e \"/$p\" ] && set -- \"$@\" \"$p\"; done\n")
	b.WriteString("tar -cf -")
	for _, ex := range BackupExcludes(dataDir) {
		b.WriteString(" --exclude=")
		b.WriteString(ShellQuote(strings.TrimPrefix(ex, "/")))
	}
	b.WriteString(" -C / \"$@\"\n")
	b.WriteString("echo " + BackupSentinel + " >&2\n")
	return Command{Args: []string{"sh", "-c", b.String()}}
}

// sentinelWriter buffers the final marker-length bytes of a stream so a
// trailing success sentinel can be stripped. A pty merges stderr into stdout,
// so the marker arrives at the end of the tar bytes; SSH and exec-out keep it
// on stderr, where it is checked separately.
type sentinelWriter struct {
	w      io.Writer
	marker []byte
	tail   []byte
}

func (s *sentinelWriter) Write(p []byte) (int, error) {
	combined := make([]byte, 0, len(s.tail)+len(p))
	combined = append(combined, s.tail...)
	combined = append(combined, p...)
	keep := len(s.marker)
	if len(combined) > keep {
		if _, err := s.w.Write(combined[:len(combined)-keep]); err != nil {
			return 0, err
		}
	}
	start := len(combined) - keep
	if start < 0 {
		start = 0
	}
	s.tail = append(s.tail[:0], combined[start:]...)
	return len(p), nil
}

// Finish writes any non-marker tail and reports whether the marker was present.
func (s *sentinelWriter) Finish() (bool, error) {
	found := bytes.HasSuffix(s.tail, s.marker)
	if found {
		s.tail = s.tail[:len(s.tail)-len(s.marker)]
	}
	if len(s.tail) > 0 {
		if _, err := s.w.Write(s.tail); err != nil {
			return false, err
		}
	}
	s.tail = nil
	return found, nil
}

// BackupResult describes a completed host-side backup.
type BackupResult struct {
	Path         string
	ManifestPath string
	Bytes        int64
	SHA256       string
	Entries      int
	Files        int
	Created      time.Time
	Includes     []string
	Excludes     []string
}

// BackupManifest is the small JSON written beside the archive.
type BackupManifest struct {
	Schema     int      `json:"schema"`
	CreatedUTC string   `json:"created_utc"`
	Host       string   `json:"host"`
	Address    string   `json:"address"`
	Platform   string   `json:"platform"`
	Archive    string   `json:"archive"`
	Bytes      int64    `json:"bytes"`
	SHA256     string   `json:"sha256"`
	Entries    int      `json:"entries"`
	Files      int      `json:"files"`
	Includes   []string `json:"includes"`
	Excludes   []string `json:"excludes"`
	Validated  bool     `json:"validated"`
	RetireOnly bool     `json:"retire_only"`
	Note       string   `json:"note"`
}

// BackupFilename builds <host>-<UTC timestamp>.tar.gz safe for every OS.
func BackupFilename(host string, now time.Time) string {
	return SanitizeHost(host) + "-" + now.UTC().Format("20060102T150405Z") + ".tar.gz"
}

// SanitizeHost makes an address safe as a filename component.
func SanitizeHost(host string) string {
	var b strings.Builder
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "._")
	if out == "" {
		return "device"
	}
	return out
}

// CreateBackup streams the stock userspace to dir, validates the archive and
// writes its manifest. It never deletes anything on the device.
func CreateBackup(ctx context.Context, dev Device, dir, address string, platform Platform, now time.Time) (BackupResult, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return BackupResult{}, err
	}
	name := BackupFilename(address, now)
	final := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, ".backup-*")
	if err != nil {
		return BackupResult{}, err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()
	gz := gzip.NewWriter(tmp)
	limited := &limitWriter{w: gz, remaining: MaxBackupStream}
	sw := &sentinelWriter{w: limited, marker: []byte(BackupSentinel + "\n")}
	stderr := &limitedBuffer{limit: 64 << 10}
	cmd := BackupCommand(platform.DataDir())
	cmd.Stdout = sw
	cmd.Stderr = stderr
	res, err := dev.Run(ctx, cmd)
	if limited.overflow {
		gz.Close()
		return BackupResult{}, fmt.Errorf("backup stream exceeded the %d byte cap", MaxBackupStream)
	}
	if stderr.overflow {
		gz.Close()
		return BackupResult{}, fmt.Errorf("backup stderr exceeded the %d byte cap", 64<<10)
	}
	if err != nil {
		gz.Close()
		return BackupResult{}, fmt.Errorf("stream backup: %w", err)
	}
	if res.ExitCode != 0 {
		gz.Close()
		return BackupResult{}, fmt.Errorf("stream backup failed (%d): %s", res.ExitCode, firstLine(string(res.Stderr)))
	}
	foundStream, finishErr := sw.Finish()
	if finishErr != nil {
		gz.Close()
		return BackupResult{}, fmt.Errorf("stream backup: %w", finishErr)
	}
	if err := gz.Close(); err != nil {
		return BackupResult{}, fmt.Errorf("finish backup gzip: %w", err)
	}
	foundStderr := bytes.Contains(stderr.Bytes(), []byte(BackupSentinel))
	if !foundStream && !foundStderr {
		return BackupResult{}, fmt.Errorf("backup stream ended without the success marker; the remote tar failed or the link dropped")
	}
	if err := tmp.Close(); err != nil {
		return BackupResult{}, err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return BackupResult{}, err
	}
	summary, err := ValidateBackup(tmpName)
	if err != nil {
		return BackupResult{}, fmt.Errorf("backup validation failed: %w", err)
	}
	if err := atomicfile.Replace(tmpName, final); err != nil {
		return BackupResult{}, err
	}
	result := BackupResult{
		Path:     final,
		Bytes:    summary.Bytes,
		SHA256:   summary.SHA256,
		Entries:  summary.Entries,
		Files:    summary.Files,
		Created:  now.UTC(),
		Includes: append([]string(nil), StockBackupIncludes...),
		Excludes: BackupExcludes(platform.DataDir()),
	}
	manifest := BackupManifest{
		Schema:     1,
		CreatedUTC: result.Created.Format(time.RFC3339),
		Host:       SanitizeHost(address),
		Address:    address,
		Platform:   string(platform),
		Archive:    name,
		Bytes:      result.Bytes,
		SHA256:     result.SHA256,
		Entries:    result.Entries,
		Files:      result.Files,
		Includes:   result.Includes,
		Excludes:   result.Excludes,
		Validated:  true,
		RetireOnly: true,
		Note:       "Nothing was deleted from the device. This is a pre-change copy of the vendor userspace for rollback.",
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return BackupResult{}, err
	}
	data = append(data, '\n')
	result.ManifestPath = final + ".manifest.json"
	if err := writePrivate(result.ManifestPath, data); err != nil {
		return BackupResult{}, err
	}
	return result, nil
}

// BackupSummary is the structural check of an archive.
type BackupSummary struct {
	Bytes   int64
	SHA256  string
	Entries int
	Files   int
}

// ValidateBackup proves the host has a complete, readable gzip tar and that no
// entry escapes the archive root. It reads to EOF so a truncated stream fails.
func ValidateBackup(path string) (BackupSummary, error) {
	f, err := os.Open(path)
	if err != nil {
		return BackupSummary{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return BackupSummary{}, err
	}
	if info.Size() == 0 {
		return BackupSummary{}, fmt.Errorf("empty archive")
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, io.NewSectionReader(f, 0, info.Size())); err != nil {
		return BackupSummary{}, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return BackupSummary{}, err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		return BackupSummary{}, fmt.Errorf("not a gzip stream: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var entries, files int
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return BackupSummary{}, fmt.Errorf("tar entry %d: %w", entries+1, err)
		}
		entries++
		name := hdr.Name
		if strings.HasPrefix(name, "/") {
			return BackupSummary{}, fmt.Errorf("absolute entry %q", name)
		}
		// POSIX archive names: only a literal .. component escapes the root.
		// Backslashes are valid filename bytes on the device (BusyBox may
		// render a space as \040), so they are not treated as separators.
		for _, part := range strings.Split(name, "/") {
			if part == ".." {
				return BackupSummary{}, fmt.Errorf("entry %q escapes the archive", name)
			}
		}
		if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA {
			files++
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return BackupSummary{}, fmt.Errorf("read entry %q: %w", name, err)
		}
	}
	if entries == 0 {
		return BackupSummary{}, fmt.Errorf("archive holds no entries")
	}
	// Drain the gzip trailer so a truncated stream fails its CRC/length check
	// instead of passing because the tar reader saw its end-of-archive marker.
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return BackupSummary{}, fmt.Errorf("gzip trailer: %w", err)
	}
	return BackupSummary{Bytes: info.Size(), SHA256: hex.EncodeToString(sum.Sum(nil)), Entries: entries, Files: files}, nil
}

func writePrivate(name string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(name), ".manifest-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return atomicfile.Replace(tmpName, name)
}
