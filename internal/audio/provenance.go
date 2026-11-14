package audio

import (
	"encoding/json"
	"fmt"
	"os"
)

// CaptureProvenanceVersion is the only sidecar schema this build writes or
// reads. A newer version is an error, not something to reinterpret.
const CaptureProvenanceVersion = 1

// CaptureProvenance declares the preprocessing baked into a stored capture.
// It exists so enrollment can tell an already-processed take from a legacy
// recording of the raw vendor tap without guessing from a filename or
// rewriting the audio. ProcessingTap empty means the file holds the physical
// raw tap and must be converted before it is used as training input.
type CaptureProvenance struct {
	Version        int    `json:"version"`
	ProcessingTap  string `json:"processing_tap"`
	SourceEndpoint string `json:"source_endpoint"`
	RateHz         int    `json:"rate_hz"`
	Channels       int    `json:"channels"`
}

// CaptureProvenancePath is the sidecar path for one capture file.
func CaptureProvenancePath(capturePath string) string { return capturePath + ".json" }

// WriteCaptureProvenance atomically writes the sidecar. Callers should write
// it before the audio file: a take without a readable sidecar is treated as a
// legacy raw recording, so publishing the declaration first can never cause a
// processed take to be converted a second time after a crash.
func WriteCaptureProvenance(capturePath string, p CaptureProvenance) error {
	p.Version = CaptureProvenanceVersion
	raw, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("encode capture provenance: %w", err)
	}
	raw = append(raw, '\n')
	path := CaptureProvenancePath(capturePath)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadCaptureProvenance returns found=false when the capture predates the
// sidecar (a legacy raw take). A present but malformed or newer sidecar is an
// error: silently treating it as raw would process the audio twice.
func ReadCaptureProvenance(capturePath string) (CaptureProvenance, bool, error) {
	raw, err := os.ReadFile(CaptureProvenancePath(capturePath))
	if err != nil {
		if os.IsNotExist(err) {
			return CaptureProvenance{}, false, nil
		}
		return CaptureProvenance{}, false, err
	}
	var p CaptureProvenance
	if err := json.Unmarshal(raw, &p); err != nil {
		return CaptureProvenance{}, false, fmt.Errorf("capture provenance %s: %w", CaptureProvenancePath(capturePath), err)
	}
	if p.Version != CaptureProvenanceVersion {
		return CaptureProvenance{}, false, fmt.Errorf("capture provenance %s: version %d, want %d; do not reinterpret this take",
			CaptureProvenancePath(capturePath), p.Version, CaptureProvenanceVersion)
	}
	return p, true, nil
}
