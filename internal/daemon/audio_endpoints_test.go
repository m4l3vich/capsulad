package daemon

import (
	"context"
	"testing"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/audio"
	"capsulad/internal/backend"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
)

type formatAudio struct {
	backend.AudioDevice
	format backend.AudioFormat
}

func (f formatAudio) CaptureFormat() backend.AudioFormat { return f.format }

// ListEndpoints must advertise the tap a capture actually receives: the DSP tap
// only when processing is enabled and the backend format is supported, and an
// empty tap for disabled DSP or an unsupported backend format.
func TestListEndpointsAdvertisesEffectiveProcessingTap(t *testing.T) {
	if testing.Short() {
		t.Skip("audio manager setup")
	}
	sim := backend.NewSimulator(backend.SimulatorOptions{})
	cases := []struct {
		name string
		dev  backend.AudioDevice
		opts audio.Options
		want string
	}{
		{"enabled", sim.Audio(), audio.Options{}, audio.CaptureProcessingTap},
		{"disabled", sim.Audio(), audio.Options{CaptureDSP: audio.CaptureDSPOptions{Disabled: true}}, ""},
		{"unsupported", formatAudio{AudioDevice: sim.Audio(),
			format: backend.AudioFormat{Format: backend.F32LE, RateHz: 48000, Channels: 2}}, audio.Options{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := ids.NewClock()
			logs, err := logbus.New(clock, 100, "")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			mgr := audio.NewManager(ctx, clock, logs, tc.dev, tc.opts)
			t.Cleanup(mgr.Close)
			d := &Daemon{dev: sim, audio: mgr}
			resp, err := (&audioService{d: d}).ListEndpoints(ctx, &frameworkv1.ListAudioEndpointsRequest{})
			if err != nil {
				t.Fatal(err)
			}
			var processed, raw *frameworkv1.AudioEndpointInfo
			for _, ep := range resp.Endpoints {
				switch ep.EndpointId {
				case audio.CaptureEndpoint:
					processed = ep
				case audio.RawCaptureEndpoint:
					raw = ep
				}
			}
			if processed == nil || raw == nil {
				t.Fatalf("capture endpoints missing: %v", resp.Endpoints)
			}
			if got, want := processed.ProcessingTap, tc.want; got != want {
				t.Fatalf("processed tap = %q, want %q", got, want)
			}
			if got := processed.ProcessingTap; got != mgr.CaptureProcessingTap() {
				t.Fatalf("advertised tap %q differs from effective %q", got, mgr.CaptureProcessingTap())
			}
			if raw.ProcessingTap != "" {
				t.Fatalf("raw endpoint advertises tap %q", raw.ProcessingTap)
			}
		})
	}
}
