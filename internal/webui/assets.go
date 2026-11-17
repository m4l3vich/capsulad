package webui

import (
	"errors"
	"io/fs"
	"net/http"
	"syscall"

	"capsulad/internal/assets"
)

// The shared asset store administrators manage from the browser. The same
// directory is mounted read-only at /assets inside every app sandbox, so these
// routes are browser-admin only: session authentication, origin check and CSRF
// are enforced by sessionOnly, and an API key is never accepted. Raw upload
// bodies pass through untouched because sessionOnly parses only form-encoded
// requests.

// apiAssets lists the catalog with its total size.
func (s *Server) apiAssets(w http.ResponseWriter, r *http.Request) {
	if s.Assets == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "asset store is unavailable")
		return
	}
	list := s.Assets.List("")
	var total int64
	for _, entry := range list {
		total += entry.Size
	}
	writeJSONValue(w, http.StatusOK, map[string]any{
		"assets":      list,
		"total_bytes": total,
	})
}

// uploadAsset writes one raw body under ?path=<store-relative path>. The store
// creates subdirectories, replaces an existing regular file atomically and
// rejects traversal, absolute paths, symlinks, reserved names and bodies over
// assets.MaxFileBytes.
func (s *Server) uploadAsset(w http.ResponseWriter, r *http.Request) {
	if s.Assets == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "asset store is unavailable")
		return
	}
	target, ok := assetPathParam(r)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, `exactly one "path" query parameter is required`)
		return
	}
	body := http.MaxBytesReader(w, r.Body, assets.MaxFileBytes)
	entry, err := s.Assets.Write(target, body)
	if err != nil {
		s.writeAssetError(w, err)
		return
	}
	writeJSONValue(w, http.StatusOK, map[string]any{"asset": entry})
}

// deleteAsset removes one regular file. Directories and symlinks are never
// removed through this route.
func (s *Server) deleteAsset(w http.ResponseWriter, r *http.Request) {
	if s.Assets == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "asset store is unavailable")
		return
	}
	target, ok := assetPathParam(r)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, `exactly one "path" query parameter is required`)
		return
	}
	if err := s.Assets.Remove(target); err != nil {
		s.writeAssetError(w, err)
		return
	}
	writeJSONValue(w, http.StatusOK, map[string]any{"ok": true})
}

func assetPathParam(r *http.Request) (string, bool) {
	values, ok := r.URL.Query()["path"]
	if !ok || len(values) != 1 {
		return "", false
	}
	return values[0], true
}

func (s *Server) writeAssetError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	switch {
	case errors.Is(err, assets.ErrTooLarge), errors.As(err, &tooLarge):
		writeJSONError(w, http.StatusRequestEntityTooLarge, "asset exceeds the 32 MiB limit")
	case errors.Is(err, assets.ErrStoreFull):
		writeJSONError(w, http.StatusRequestEntityTooLarge, "asset store is full")
	case errors.Is(err, assets.ErrDeleted):
		writeJSONError(w, http.StatusConflict, err.Error())
	case errors.Is(err, syscall.ENOSPC):
		writeJSONError(w, http.StatusInsufficientStorage, "device storage is full")
	case errors.Is(err, assets.ErrInvalidPath):
		writeJSONError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, fs.ErrNotExist):
		writeJSONError(w, http.StatusNotFound, "asset not found")
	case errors.Is(err, fs.ErrPermission):
		writeJSONError(w, http.StatusForbidden, "asset store permission denied")
	default:
		// Do not reflect a host path or filesystem detail to the browser.
		if s.Logs != nil {
			s.Logs.Warnf("webui", "", "asset store operation failed: %v", err)
		}
		writeJSONError(w, http.StatusInternalServerError, "asset store operation failed")
	}
}
