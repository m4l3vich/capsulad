// Package artifact reads, validates and writes Capsula application packages.
//
// The package format is pinned: a gzip-compressed tar (or an equivalent
// directory) with manifest.toml at the archive root, relative slash-separated
// entries, and regular files or directories only. Absolute paths, ".."
// components, symlinks, hardlinks, devices and FIFOs are rejected, so an
// installed package is a self-contained tree rather than a set of references
// into the host filesystem.
//
// Files are staged with one policy: 0755 when any execute bit is set and 0444
// otherwise. The package tree is immutable once installed; only the launcher
// treats the executable bit as meaningful.
package artifact

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"capsulad/internal/manifest"
)

// ManifestFile is the required root entry of every package.
const ManifestFile = "manifest.toml"

// Limits bounds extraction. The device data partition is 78.5 MiB, so a
// package that cannot be unpacked beside the installed ones is rejected before
// it consumes the space a retry needs.
type Limits struct {
	MaxFileBytes  int64
	MaxTotalBytes int64
	MaxEntries    int
}

// DefaultLimits is the package budget pinned by the appliance contract: no
// single file above 48 MiB and no package above 64 MiB unpacked.
var DefaultLimits = Limits{
	MaxFileBytes:  48 << 20,
	MaxTotalBytes: 64 << 20,
	MaxEntries:    4096,
}

// Package is an opened artifact: a staged package directory that follows the
// pinned mode policy, plus its parsed manifest. The directory can be renamed
// into an installation root as one unit.
type Package struct {
	Dir      string
	Manifest *manifest.Manifest

	stagingRoot string
}

// Close removes the staged tree. It is safe to call after the caller renamed
// Dir into its final location; the removal is then a no-op.
func (p *Package) Close() error {
	if p == nil {
		return nil
	}
	root := p.stagingRoot
	if root == "" {
		root = p.Dir
	}
	p.stagingRoot = ""
	p.Dir = ""
	if root == "" {
		return nil
	}
	return os.RemoveAll(root)
}

// Open stages src, which may be a package directory or a .tar.gz archive, and
// loads its manifest. The caller must Close the result.
func Open(src string) (*Package, error) {
	return OpenLimit(src, DefaultLimits)
}

// OpenLimit is Open with an explicit size budget.
func OpenLimit(src string, limits Limits) (*Package, error) {
	parent, err := os.MkdirTemp("", "capsula-package-")
	if err != nil {
		return nil, err
	}
	pkg, err := stageIn(parent, src, limits)
	if err != nil {
		_ = os.RemoveAll(parent)
		return nil, err
	}
	pkg.stagingRoot = parent
	return pkg, nil
}

// OpenIn stages a package inside parent. Callers that must atomically rename
// the package directory use this to keep the staging tree on the destination
// filesystem; the caller owns parent and should remove it afterwards.
func OpenIn(parent, src string) (*Package, error) {
	return stageIn(parent, src, DefaultLimits)
}

func stageIn(parent, src string, limits Limits) (*Package, error) {
	dir, err := os.MkdirTemp(parent, "pkg-")
	if err != nil {
		return nil, err
	}
	if err := Extract(src, dir, limits); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	m, err := manifest.Load(dir, filepath.Join(dir, ManifestFile))
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("package %s: %w", src, err)
	}
	return &Package{Dir: dir, Manifest: m}, nil
}

// Extract unpacks a package or deployment tree into dst. src may be a
// directory or a gzip-compressed tar. Missing root manifests are not checked
// here; Open and OpenIn do that for application packages.
func Extract(src, dst string, limits Limits) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("artifact %s: %w", src, err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	if info.IsDir() {
		return copyTree(src, dst, limits)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("artifact %s: expected a directory or a .tar.gz archive", src)
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("artifact %s: expected a gzip-compressed tar (.tar.gz): %w", src, err)
	}
	defer gz.Close()
	return extractTar(tar.NewReader(gz), dst, limits)
}

type budget struct {
	limits  Limits
	total   int64
	entries int
}

func (b *budget) entry(name string) error {
	b.entries++
	if b.limits.MaxEntries > 0 && b.entries > b.limits.MaxEntries {
		return fmt.Errorf("artifact entry %q: package has more than %d entries", name, b.limits.MaxEntries)
	}
	return nil
}

func (b *budget) add(name string, n int64) error {
	if n < 0 {
		return fmt.Errorf("artifact entry %q: negative size", name)
	}
	if b.limits.MaxFileBytes > 0 && n > b.limits.MaxFileBytes {
		return fmt.Errorf("artifact entry %q: %d bytes exceeds the %d-byte per-file limit", name, n, b.limits.MaxFileBytes)
	}
	b.total += n
	if b.limits.MaxTotalBytes > 0 && b.total > b.limits.MaxTotalBytes {
		return fmt.Errorf("artifact entry %q: package exceeds the %d-byte unpacked limit", name, b.limits.MaxTotalBytes)
	}
	return nil
}

func extractTar(tr *tar.Reader, dst string, limits Limits) error {
	b := budget{limits: limits}
	seen := make(map[string]bool)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read artifact: %w", err)
		}
		name, err := entryName(hdr.Name)
		if err != nil {
			return fmt.Errorf("artifact entry %q: %w", hdr.Name, err)
		}
		if name == "." {
			// A tar root entry ("." or "./") carries no content.
			continue
		}
		if err := b.entry(name); err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir, tar.TypeReg:
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("artifact entry %q: links are not allowed; store a regular file instead", hdr.Name)
		default:
			return fmt.Errorf("artifact entry %q: only regular files and directories are allowed", hdr.Name)
		}
		if seen[name] {
			return fmt.Errorf("artifact entry %q: duplicate path %q", hdr.Name, name)
		}
		seen[name] = true
		target, err := targetPath(dst, name)
		if err != nil {
			return fmt.Errorf("artifact entry %q: %w", hdr.Name, err)
		}
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("artifact entry %q: %w", hdr.Name, err)
			}
			continue
		}
		if err := b.add(name, hdr.Size); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("artifact entry %q: %w", hdr.Name, err)
		}
		if err := writeStream(target, tr, fileMode(os.FileMode(hdr.Mode)), hdr.Size, limits.MaxFileBytes); err != nil {
			return fmt.Errorf("artifact entry %q: %w", hdr.Name, err)
		}
	}
}

func copyTree(src, dst string, limits Limits) error {
	root, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	b := budget{limits: limits}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if err := b.entry(name); err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("package entry %q: %w", name, err)
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		case info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("package entry %q: symlinks are not allowed; store a regular file instead", name)
		case !info.Mode().IsRegular():
			return fmt.Errorf("package entry %q: only regular files and directories are allowed", name)
		}
		if err := b.add(name, info.Size()); err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := os.Open(p)
		if err != nil {
			return fmt.Errorf("package entry %q: %w", name, err)
		}
		err = writeStream(target, in, fileMode(info.Mode()), info.Size(), limits.MaxFileBytes)
		if closeErr := in.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return fmt.Errorf("package entry %q: %w", name, err)
		}
		return nil
	})
}

// writeStream copies exactly size bytes from r into a new file with mode. The
// file is written non-executable and chmodded only after the content is
// complete, so a killed extraction never leaves a partial executable behind.
func writeStream(target string, r io.Reader, mode os.FileMode, size, max int64) error {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	limit := size
	if max > 0 && max < limit {
		limit = max
	}
	n, err := io.Copy(f, io.LimitReader(r, limit+1))
	switch {
	case err != nil:
	case max > 0 && n > max:
		err = fmt.Errorf("exceeds the %d-byte per-file limit", max)
	case n > size:
		err = fmt.Errorf("entry is larger than its declared %d bytes", size)
	case n < size:
		err = io.ErrUnexpectedEOF
	default:
		err = f.Chmod(mode)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(target)
	}
	return err
}

func fileMode(mode fs.FileMode) os.FileMode {
	if mode&0o111 != 0 {
		return 0o755
	}
	return 0o444
}

// entryName validates one archive entry and returns its cleaned relative path.
func entryName(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("empty name")
	}
	if strings.ContainsRune(raw, 0) {
		return "", fmt.Errorf("name contains a NUL byte")
	}
	if path.IsAbs(raw) || strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("absolute paths are not allowed")
	}
	name := path.Clean(raw)
	if name == ".." || strings.HasPrefix(name, "../") || path.IsAbs(name) {
		return "", fmt.Errorf("path escapes the package root")
	}
	return name, nil
}

func targetPath(dst, name string) (string, error) {
	target := filepath.Join(dst, filepath.FromSlash(name))
	rel, err := filepath.Rel(dst, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path escapes the package root")
	}
	return target, nil
}

// Write packages dir into out as the canonical artifact. Entries are sorted,
// timestamps and ownership are zero, and the mode policy is applied, so the
// same tree always produces the same bytes.
func Write(dir, out string) error {
	return WriteLimit(dir, out, DefaultLimits)
}

// WriteLimit is Write with an explicit size budget.
func WriteLimit(dir, out string, limits Limits) error {
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("package %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("package %s is not a directory", dir)
	}
	if _, err := manifest.Load(root, filepath.Join(root, ManifestFile)); err != nil {
		return fmt.Errorf("package %s: %w", dir, err)
	}
	entries, err := collect(root, limits)
	if err != nil {
		return err
	}
	absOut, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(absOut), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(absOut), ".artifact-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if err := writeTar(tmp, entries); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, absOut); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

type entry struct {
	name string
	path string
	mode os.FileMode
	size int64
	dir  bool
}

func collect(root string, limits Limits) ([]entry, error) {
	b := budget{limits: limits}
	var entries []entry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if err := b.entry(name); err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("package entry %q: %w", name, err)
		}
		switch {
		case d.IsDir():
			entries = append(entries, entry{name: name, path: p, mode: 0o755, dir: true})
			return nil
		case info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("package entry %q: symlinks are not allowed; store a regular file instead", name)
		case !info.Mode().IsRegular():
			return fmt.Errorf("package entry %q: only regular files and directories are allowed", name)
		}
		if err := b.add(name, info.Size()); err != nil {
			return err
		}
		entries = append(entries, entry{name: name, path: p, mode: fileMode(info.Mode()), size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	return entries, nil
}

func writeTar(w io.Writer, entries []entry) error {
	gz := gzip.NewWriter(w)
	gz.Header.ModTime = time.Time{}
	gz.Header.OS = 255
	gz.Header.Name = ""
	gz.Header.Comment = ""
	gz.Header.Extra = nil
	tw := tar.NewWriter(gz)
	epoch := time.Unix(0, 0).UTC()
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     int64(e.mode),
			ModTime:  epoch,
			Format:   tar.FormatPAX,
			Uid:      0,
			Gid:      0,
			Uname:    "",
			Gname:    "",
			Typeflag: tar.TypeReg,
		}
		if e.dir {
			hdr.Name += "/"
			hdr.Typeflag = tar.TypeDir
		} else {
			hdr.Size = e.size
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("package entry %q: %w", e.name, err)
		}
		if e.dir {
			continue
		}
		f, err := os.Open(e.path)
		if err != nil {
			return fmt.Errorf("package entry %q: %w", e.name, err)
		}
		n, err := io.Copy(tw, f)
		closeErr := f.Close()
		if err != nil {
			return fmt.Errorf("package entry %q: %w", e.name, err)
		}
		if closeErr != nil {
			return fmt.Errorf("package entry %q: %w", e.name, closeErr)
		}
		if n != e.size {
			return fmt.Errorf("package entry %q changed while it was packaged", e.name)
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}
