// Package assets owns the framework-provided shared asset store.
//
// The store directory is the catalog: every regular file below the root,
// recursively, is a shared asset. There is no manifest, no license field and
// no required-base-asset flag. Apps see the same directory mounted read-only
// at /assets inside every sandbox, so an asset id is exactly the slash-relative
// path and its canonical app path is /assets/<id>.
//
// The catalog is enumerated on every call, so a file added through the Web UI
// (or copied onto the device) is visible without restarting the daemon.
package assets

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// CanonicalRoot is where every app sandbox mounts the store. The store is
	// read-only for apps; only the daemon's HTTP surface writes to it.
	CanonicalRoot = "/assets"

	// MaxFileBytes bounds one uploaded asset. The device data partition is
	// 78.5 MiB, so 32 MiB for a single file keeps the rest of the store and the
	// daemon's own data intact.
	MaxFileBytes int64 = 32 << 20

	// MaxStoreBytes bounds the whole store. The 78.5 MiB data partition also
	// holds apps, settings and logs, so the aggregate budget is below the
	// partition size rather than the sum of the per-file limits.
	MaxStoreBytes int64 = 48 << 20

	// storeMode is the store root mode. Sandboxes run as a distinct UID without
	// the daemon's group, so every app needs read and traversal permission;
	// writes remain root-owned and are enforced by the file permissions below.
	storeMode os.FileMode = 0o755

	// fileMode is the mode of a published asset and dirMode the mode of a
	// subdirectory created by Write.
	fileMode os.FileMode = 0o644
	dirMode  os.FileMode = 0o755

	// stagingPrefix marks the temporary file an upload is written to before its
	// atomic rename. Staging files are never published and clients may not use
	// names that begin with it.
	stagingPrefix = ".capsula-upload-"

	maxNameBytes = 255
)

// Errors distinguish a malformed request from a missing file or an oversized
// body so the HTTP surface can answer 400/404/413 without guessing at errno.
var (
	ErrInvalidPath = errors.New("invalid asset path")
	ErrTooLarge    = errors.New("asset exceeds the maximum size")
	ErrStoreFull   = errors.New("asset store is full")
)

// DefaultMediaType is used for extensions outside the explicit table.
const DefaultMediaType = "application/octet-stream"

// mediaTypes is deliberately a short explicit table. It is not a registry and
// not negotiable content sniffing: an asset's type follows its extension.
var mediaTypes = map[string]string{
	"wav":  "audio/wav",
	"mp3":  "audio/mpeg",
	"ogg":  "audio/ogg",
	"flac": "audio/flac",
	"png":  "image/png",
	"jpg":  "image/jpeg",
	"jpeg": "image/jpeg",
	"gif":  "image/gif",
	"svg":  "image/svg+xml",
	"webp": "image/webp",
	"json": "application/json",
	"txt":  "text/plain",
	"html": "text/html",
	"css":  "text/css",
	"js":   "text/javascript",
	"bin":  "application/octet-stream",
}

// MediaType reports the media type implied by an asset id or file name.
func MediaType(name string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	if mediaType, ok := mediaTypes[ext]; ok {
		return mediaType
	}
	return DefaultMediaType
}

// Info is one catalog entry. ID is the slash-relative path, Path is the
// canonical path inside every app sandbox.
type Info struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size_bytes"`
}

// Catalog is the store rooted at one directory. Listing walks the directory on
// demand; writes are staged in the target directory and renamed into place, so
// readers never observe a partial file.
//
// A published file's hash is cached by (size, mtime). The framework socket is
// reachable by every app, so an app can request List repeatedly; without the
// cache each call would re-read and re-hash the whole store.
type Catalog struct {
	root       string
	limit      int64
	storeLimit int64

	// mu serializes mutations of the store: budget accounting, staging and
	// publication must not interleave.
	mu sync.Mutex

	cacheMu sync.Mutex
	hashes  map[string]hashEntry
	// hashFile is the hash computation seam (tests count reads through it).
	hashFile func(string) (string, error)
}

// hashEntry is one cached hash, valid only while the file still has the same
// size and modification time.
type hashEntry struct {
	size  int64
	mtime time.Time
	sum   string
}

// Open prepares the store root and returns its catalog. The directory is part
// of the daemon's data tree, so a missing root is created rather than fatal.
func Open(root string) (*Catalog, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("assets: empty store root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("assets: resolve store root: %w", err)
	}
	if err := os.MkdirAll(abs, storeMode); err != nil {
		return nil, fmt.Errorf("assets: create store root: %w", err)
	}
	// MkdirAll does not adjust the mode of an existing directory, and the
	// daemon creates the data tree with restrictive modes. The store is bind
	// mounted into sandboxes whose processes run as another UID, so the root
	// must be readable and traversable regardless of how it was created.
	if err := os.Chmod(abs, storeMode); err != nil {
		return nil, fmt.Errorf("assets: set store root mode: %w", err)
	}
	return &Catalog{
		root:       abs,
		limit:      MaxFileBytes,
		storeLimit: MaxStoreBytes,
		hashes:     make(map[string]hashEntry),
		hashFile:   hashFile,
	}, nil
}

// Root is the host directory backing the store.
func (c *Catalog) Root() string { return c.root }

// List enumerates the store. prefix filters on the asset id (or on a canonical
// /assets/... prefix); an empty prefix lists everything. Entries are sorted by
// id. Symlinks, directories, special files and in-flight upload staging files
// are never published, and a file that cannot be hashed is skipped rather than
// reported with a partial hash.
func (c *Catalog) List(prefix string) []Info {
	c.mu.Lock()
	defer c.mu.Unlock()
	want := normalizePrefix(prefix)
	out := make([]Info, 0, 16)
	seen := make(map[string]bool, 16)
	_ = filepath.WalkDir(c.root, func(full string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			// WalkDir never follows a symlink; a symlinked file is not an asset.
			return nil
		}
		if strings.HasPrefix(entry.Name(), stagingPrefix) {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		id, err := c.relativeID(full)
		if err != nil {
			return nil
		}
		// Every published file counts as present, including ones this prefix
		// filters out: pruning must only drop entries that left the store.
		seen[id] = true
		if want != "" && !strings.HasPrefix(id, want) {
			return nil
		}
		sum, err := c.cachedHash(id, full, info)
		if err != nil {
			return nil
		}
		out = append(out, Info{
			ID:        id,
			Path:      CanonicalRoot + "/" + id,
			MediaType: MediaType(id),
			SHA256:    sum,
			Size:      info.Size(),
		})
		return nil
	})
	c.pruneHashes(seen)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// cachedHash returns the hash of full, reusing a cached value only while the
// file's size and modification time are unchanged.
func (c *Catalog) cachedHash(id, full string, info fs.FileInfo) (string, error) {
	c.cacheMu.Lock()
	entry, ok := c.hashes[id]
	c.cacheMu.Unlock()
	if ok && entry.size == info.Size() && entry.mtime.Equal(info.ModTime()) {
		return entry.sum, nil
	}
	compute := c.hashFile
	if compute == nil {
		compute = hashFile
	}
	sum, err := compute(full)
	if err != nil {
		return "", err
	}
	c.cacheMu.Lock()
	c.dropHashLocked(id)
	c.hashes[id] = hashEntry{size: info.Size(), mtime: info.ModTime(), sum: sum}
	c.cacheMu.Unlock()
	return sum, nil
}

func (c *Catalog) invalidateHashes(ids ...string) {
	c.cacheMu.Lock()
	for _, id := range ids {
		c.dropHashLocked(id)
	}
	c.cacheMu.Unlock()
}

func (c *Catalog) dropHashLocked(id string) {
	delete(c.hashes, id)
}

// pruneHashes drops cached entries whose files disappeared from the store.
func (c *Catalog) pruneHashes(seen map[string]bool) {
	c.cacheMu.Lock()
	for id := range c.hashes {
		if !seen[id] {
			delete(c.hashes, id)
		}
	}
	c.cacheMu.Unlock()
}

// Lookup validates rel and reports its entry without hashing the file. It is
// the containment/type check used where only metadata matters (for example the
// assistant's reply_asset setting).
func (c *Catalog) Lookup(rel string) (Info, error) {
	id, full, err := c.resolve(rel)
	if err != nil {
		return Info{}, err
	}
	if err := c.checkParents(id); err != nil {
		return Info{}, err
	}
	info, err := os.Lstat(full)
	if err != nil {
		return Info{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Info{}, fmt.Errorf("%w: %s is a symlink", ErrInvalidPath, id)
	}
	if !info.Mode().IsRegular() {
		return Info{}, fmt.Errorf("%w: %s is not a regular file", ErrInvalidPath, id)
	}
	return Info{
		ID:        id,
		Path:      CanonicalRoot + "/" + id,
		MediaType: MediaType(id),
		Size:      info.Size(),
	}, nil
}

// Write streams one asset into the store, creating subdirectories and
// atomically replacing an existing regular file. The returned entry carries the
// hash of the bytes actually written. Bodies over the per-file limit are
// rejected with ErrTooLarge, bodies that would exceed the aggregate store
// budget with ErrStoreFull, and the partial file is discarded in both cases.
func (c *Catalog) Write(rel string, body io.Reader) (Info, error) {
	id, full, err := c.resolve(rel)
	if err != nil {
		return Info{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.mkdirParents(id); err != nil {
		return Info{}, err
	}
	if info, statErr := os.Lstat(full); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return Info{}, fmt.Errorf("%w: %s is a symlink", ErrInvalidPath, id)
		}
		if !info.Mode().IsRegular() {
			return Info{}, fmt.Errorf("%w: %s already exists and is not a regular file", ErrInvalidPath, id)
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return Info{}, statErr
	}

	current, err := c.currentSizeExcluding(id)
	if err != nil {
		return Info{}, err
	}
	storeRemaining := c.storeLimit - current
	if storeRemaining < 0 {
		storeRemaining = 0
	}

	staging, err := os.CreateTemp(filepath.Dir(full), stagingPrefix+"*")
	if err != nil {
		return Info{}, err
	}
	stagingName := staging.Name()
	// A successful rename consumes the staging name; afterwards this is a
	// harmless ENOENT that guarantees no partial file survives a failure.
	defer func() { _ = os.Remove(stagingName) }()
	if err := staging.Chmod(fileMode); err != nil {
		_ = staging.Close()
		return Info{}, err
	}
	// The copy is bounded by the tighter of the per-file limit and what is
	// left in the aggregate store budget, so an oversized body is rejected
	// without filling the data partition first.
	limit := c.limit
	storeBound := false
	if storeRemaining < limit {
		limit = storeRemaining
		storeBound = true
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(staging, hasher), io.LimitReader(body, limit+1))
	if copyErr == nil && written > limit {
		if storeBound {
			copyErr = ErrStoreFull
		} else {
			copyErr = ErrTooLarge
		}
	}
	if copyErr == nil {
		copyErr = staging.Sync()
	}
	if closeErr := staging.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return Info{}, copyErr
	}
	if err := os.Rename(stagingName, full); err != nil {
		return Info{}, err
	}
	c.invalidateHashes(id)
	return Info{
		ID:        id,
		Path:      CanonicalRoot + "/" + id,
		MediaType: MediaType(id),
		SHA256:    hex.EncodeToString(hasher.Sum(nil)),
		Size:      written,
	}, nil
}

// Remove deletes one regular file. A directory (and therefore a directory
// tree) is never removed here, and a symlink is never followed.
func (c *Catalog) Remove(rel string) error {
	id, full, err := c.resolve(rel)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkParents(id); err != nil {
		return err
	}
	info, err := os.Lstat(full)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is a symlink", ErrInvalidPath, id)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file; directories are never removed", ErrInvalidPath, id)
	}
	if err := os.Remove(full); err != nil {
		return err
	}
	c.invalidateHashes(id)
	return nil
}

// resolve turns a store-relative request path into a clean id and host path.
// Only "/"-separated relative paths with ordinary components are accepted:
// absolute paths, traversal, empty components, backslashes, over-long names
// and the upload staging prefix are rejected before any filesystem access.
func (c *Catalog) resolve(rel string) (string, string, error) {
	text := strings.TrimSpace(rel)
	if text == "" {
		return "", "", fmt.Errorf("%w: path is empty", ErrInvalidPath)
	}
	if strings.IndexByte(text, 0) >= 0 {
		return "", "", fmt.Errorf("%w: path contains a NUL byte", ErrInvalidPath)
	}
	if strings.ContainsRune(text, '\\') {
		return "", "", fmt.Errorf("%w: use / as the path separator", ErrInvalidPath)
	}
	if strings.HasPrefix(text, "/") {
		return "", "", fmt.Errorf("%w: path must be relative to the asset store", ErrInvalidPath)
	}
	if len(text) > 4096 {
		return "", "", fmt.Errorf("%w: path is too long", ErrInvalidPath)
	}
	parts := strings.Split(text, "/")
	for _, part := range parts {
		switch {
		case part == "" || part == "." || part == "..":
			return "", "", fmt.Errorf("%w: %q is not a valid path component", ErrInvalidPath, part)
		case strings.HasPrefix(part, stagingPrefix):
			return "", "", fmt.Errorf("%w: %q is reserved for in-flight uploads", ErrInvalidPath, part)
		case len(part) > maxNameBytes:
			return "", "", fmt.Errorf("%w: path component is too long", ErrInvalidPath)
		}
	}
	id := strings.Join(parts, "/")
	return id, filepath.Join(c.root, filepath.FromSlash(id)), nil
}

// checkParents walks every parent component without following symlinks.
func (c *Catalog) checkParents(id string) error {
	current := c.root
	parts := strings.Split(id, "/")
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: path component %q is a symlink", ErrInvalidPath, part)
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: path component %q is not a directory", ErrInvalidPath, part)
		}
	}
	return nil
}

// mkdirParents creates missing parent components with 0755 and rejects any
// existing component that is a symlink or not a directory.
func (c *Catalog) mkdirParents(id string) error {
	current := c.root
	parts := strings.Split(id, "/")
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		for {
			info, err := os.Lstat(current)
			if err == nil {
				if info.Mode()&os.ModeSymlink != 0 {
					return fmt.Errorf("%w: path component %q is a symlink", ErrInvalidPath, part)
				}
				if !info.IsDir() {
					return fmt.Errorf("%w: path component %q is not a directory", ErrInvalidPath, part)
				}
				break
			}
			if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			if mkErr := os.Mkdir(current, dirMode); mkErr != nil {
				if errors.Is(mkErr, fs.ErrExist) {
					// A concurrent writer won the race; re-inspect the entry.
					continue
				}
				return mkErr
			}
			// Mkdir applies the process umask; the sandbox needs traversal
			// permission regardless of it.
			if chmodErr := os.Chmod(current, dirMode); chmodErr != nil {
				return chmodErr
			}
			break
		}
	}
	return nil
}

// currentSizeExcluding sums regular-file sizes below the store, except the
// target being replaced. Callers hold c.mu so writes cannot race the budget.
func (c *Catalog) currentSizeExcluding(exclude string) (int64, error) {
	var total int64
	err := filepath.WalkDir(c.root, func(full string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || strings.HasPrefix(entry.Name(), stagingPrefix) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		id, err := c.relativeID(full)
		if err != nil {
			return err
		}
		if id != exclude {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func (c *Catalog) relativeID(full string) (string, error) {
	rel, err := filepath.Rel(c.root, full)
	if err != nil {
		return "", err
	}
	id := filepath.ToSlash(rel)
	if id == "." || id == ".." || strings.HasPrefix(id, "../") {
		return "", fmt.Errorf("%w: %s escapes the store", ErrInvalidPath, full)
	}
	return id, nil
}

func normalizePrefix(prefix string) string {
	want := strings.TrimSpace(prefix)
	want = strings.TrimPrefix(want, CanonicalRoot+"/")
	if want == CanonicalRoot {
		return ""
	}
	return strings.TrimPrefix(want, "/")
}

func hashFile(full string) (string, error) {
	f, err := os.Open(full)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
