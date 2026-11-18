package manifest

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// GuideFile is the fixed name of an application package's optional user
// guide. It is separate from an interface's declared `docs` Markdown: the
// guide is operator-facing help shown with the app, while interface docs
// describe a gRPC contract to callers.
const GuideFile = "README.md"

// MaxGuideBytes bounds the user guide. The document is one help page rendered
// beside the app, not a second package or an arbitrary attachment.
const MaxGuideBytes = 64 << 10

// ErrGuideNotFound reports a package without a user guide. It is a normal
// state: a package predating this convention, or one that deliberately has no
// guide, remains installable. Any other error means a guide exists but cannot
// be served.
var ErrGuideNotFound = errors.New("application user guide not found")

// ReadGuide reads the optional root README.md as bounded UTF-8 Markdown.
//
// The path is fixed, so no manifest field can select another file. The read
// re-checks containment component by component with O_NOFOLLOW: a symlink,
// device, FIFO, oversized file or invalid UTF-8 is refused instead of being
// followed or reflected to the browser.
func ReadGuide(root string) ([]byte, error) {
	// The Unix implementation below opens every component with O_NOFOLLOW.
	// Lstat keeps the same refusal on a host without that primitive; the fixed
	// single-component path means no parent directory is attacker-selected.
	if info, err := os.Lstat(filepath.Join(root, GuideFile)); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("user guide must not be a symlink")
	}
	f, err := openContainedRelative(root, GuideFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrGuideNotFound
		}
		return nil, fmt.Errorf("user guide: %w", err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("user guide: %w", err)
	}
	if !stat.Mode().IsRegular() {
		return nil, fmt.Errorf("user guide must be a regular file")
	}
	if stat.Size() > MaxGuideBytes {
		return nil, fmt.Errorf("user guide is %d bytes; limit is %d", stat.Size(), MaxGuideBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxGuideBytes+1))
	if err != nil {
		return nil, fmt.Errorf("user guide: %w", err)
	}
	if len(data) > MaxGuideBytes {
		return nil, fmt.Errorf("user guide exceeds the %d byte limit", MaxGuideBytes)
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("user guide must be valid UTF-8")
	}
	// Only leading/trailing whitespace (plus an optional UTF-8 BOM) is not a
	// guide. The check is deliberately on the raw bytes: an all-whitespace
	// document would render an empty help page.
	if strings.TrimSpace(strings.TrimPrefix(string(data), "\ufeff")) == "" {
		return nil, fmt.Errorf("user guide is empty")
	}
	return data, nil
}