package webui

import (
	"errors"
	"io/fs"
	"net/http"
	"path/filepath"
)

// Application package storage reporting for the browser.
//
// Both routes answer one question the administrator needs before a package
// mutation: how much room does the applications filesystem have? The capacity
// comes from the registry root, not "/", so a data partition with a small
// reserve is not misreported as the whole root filesystem. A platform that
// cannot stat it answers available=false rather than fabricating zeros.

// packageSizeEntryLimit bounds the uninstall-size walk. A package is at most a
// few thousand entries (artifact.MaxEntries); this ceiling exists so a
// pathological tree cannot hold the request open indefinitely.
const packageSizeEntryLimit = 100_000

var errPackageTreeTooLarge = errors.New("package tree has too many entries to size")

// appStorage reports the applications filesystem capacity for the install
// dialog: total, unprivileged free and used bytes.
func (s *Server) appStorage(w http.ResponseWriter, _ *http.Request) {
	if s.Apps == nil {
		writeJSONValue(w, http.StatusOK, map[string]any{
			"available": false, "total_bytes": uint64(0), "free_bytes": uint64(0), "used_bytes": uint64(0),
		})
		return
	}
	total, free, ok := diskUsageAt(s.Apps.Root())
	if !ok {
		writeJSONValue(w, http.StatusOK, map[string]any{
			"available": false, "total_bytes": uint64(0), "free_bytes": uint64(0), "used_bytes": uint64(0),
		})
		return
	}
	used := uint64(0)
	if total > free {
		used = total - free
	}
	writeJSONValue(w, http.StatusOK, map[string]any{
		"available":   true,
		"total_bytes": total,
		"free_bytes":  free,
		"used_bytes":  used,
	})
}

// appPackageStorage reports one installed package's size and the free space it
// would release. The browser shows this in the uninstall confirmation; the
// daemon still owns the decision to remove it.
func (s *Server) appPackageStorage(w http.ResponseWriter, r *http.Request) {
	if s.Apps == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "application packages are unavailable")
		return
	}
	appID := r.PathValue("appID")
	app, ok := s.Apps.Get(appID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "This application is not installed.")
		return
	}

	var size int64
	if !s.Apps.IsBuiltin(appID) {
		measured, err := packageDirSize(app.Dir)
		if err != nil {
			if s.Logs != nil {
				s.Logs.Warnf("webui", appID, "package size unavailable: %v", err)
			}
			writeJSONError(w, http.StatusInternalServerError, "The package size could not be read.")
			return
		}
		size = measured
	}

	total, free, available := diskUsageAt(s.Apps.Root())
	writeJSONValue(w, http.StatusOK, map[string]any{
		"size_bytes":  size,
		"free_bytes":  free,
		"total_bytes": total,
		"available":   available,
	})
}

// packageDirSize sums the regular files in one installed package. WalkDir does
// not follow symbolic links, so a package that links outside its tree cannot
// make the walk escape it; links themselves contribute no bytes.
func packageDirSize(dir string) (int64, error) {
	var size int64
	entries := 0
	err := filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		entries++
		if entries > packageSizeEntryLimit {
			return errPackageTreeTooLarge
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			size += info.Size()
		}
		return nil
	})
	return size, err
}
