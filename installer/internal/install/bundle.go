package install

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxExtractedCLI bounds the device CLI taken out of a bundle.
const maxExtractedCLI = 128 << 20

// BundleHasCLI reports whether the deployment bundle carries a root
// `capsulad-cli`. The installer extracts that copy to run every device command,
// so a bundle assembled with BUNDLE_CLI=0 is refused up front with the exact
// remedy rather than failing after the daemon is stopped.
func BundleHasCLI(bundlePath string) (bool, error) {
	return bundleHasFile(bundlePath, "capsulad-cli")
}

func bundleHasFile(bundlePath, want string) (bool, error) {
	f, err := os.Open(bundlePath)
	if err != nil {
		return false, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return false, fmt.Errorf("%s is not a gzip archive: %w", bundlePath, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if rootEntry(hdr.Name) == want {
			return true, nil
		}
	}
}

// ExtractBundleCLI copies the bundle's root capsulad-cli to destDir and marks
// it executable. It never executes the extracted file on the host.
func ExtractBundleCLI(bundlePath, destDir string) (string, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	f, err := os.Open(bundlePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if rootEntry(hdr.Name) != "capsulad-cli" {
			continue
		}
		if hdr.Size <= 0 || hdr.Size > maxExtractedCLI {
			return "", fmt.Errorf("bundle capsulad-cli has implausible size %d", hdr.Size)
		}
		dest := filepath.Join(destDir, "capsulad-cli")
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return "", err
		}
		if _, err := io.CopyN(out, tr, hdr.Size); err != nil {
			out.Close()
			return "", err
		}
		if err := out.Close(); err != nil {
			return "", err
		}
		return dest, nil
	}
	return "", fmt.Errorf("%s carries no root capsulad-cli", bundlePath)
}

// rootEntry normalises "./capsulad-cli" and "capsulad-cli" to one name.
func rootEntry(name string) string {
	name = strings.TrimPrefix(name, "./")
	if i := strings.IndexByte(name, '/'); i >= 0 {
		return ""
	}
	return name
}

// BundleEntrySize returns the declared size of one root entry in a bundle. The
// stock preflight uses it to learn the staged daemon's size without extracting
// the bundle.
func BundleEntrySize(bundlePath, want string) (int64, error) {
	f, err := os.Open(bundlePath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return 0, fmt.Errorf("%s has no root %s", bundlePath, want)
		}
		if err != nil {
			return 0, err
		}
		if rootEntry(hdr.Name) == want && hdr.Typeflag == tar.TypeReg {
			return hdr.Size, nil
		}
	}
}
