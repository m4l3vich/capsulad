package audio

import (
	"os"
	"strings"
	"testing"
)

func TestCaptureProvenanceRoundTripAndRejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/take.wav"
	prov := CaptureProvenance{
		ProcessingTap:  CaptureProcessingTap,
		SourceEndpoint: CaptureEndpoint,
		RateHz:         16000,
		Channels:       1,
	}
	if err := WriteCaptureProvenance(path, prov); err != nil {
		t.Fatal(err)
	}
	got, found, err := ReadCaptureProvenance(path)
	if err != nil || !found {
		t.Fatalf("read found=%v err=%v", found, err)
	}
	if got.ProcessingTap != CaptureProcessingTap || got.SourceEndpoint != CaptureEndpoint ||
		got.RateHz != 16000 || got.Channels != 1 || got.Version != CaptureProvenanceVersion {
		t.Fatalf("round trip = %+v", got)
	}

	// A missing sidecar is the legacy raw declaration, not an error.
	if _, found, err := ReadCaptureProvenance(dir + "/absent.wav"); err != nil || found {
		t.Fatalf("missing sidecar found=%v err=%v", found, err)
	}

	sidecar := CaptureProvenancePath(path)
	if err := os.WriteFile(sidecar, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadCaptureProvenance(path); err == nil || !strings.Contains(err.Error(), "provenance") {
		t.Fatalf("malformed sidecar error = %v", err)
	}

	if err := os.WriteFile(sidecar, []byte(`{"version":99,"processing_tap":"framework.dsp.v1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadCaptureProvenance(path); err == nil || !strings.Contains(err.Error(), "do not reinterpret") {
		t.Fatalf("newer sidecar error = %v", err)
	}
}
