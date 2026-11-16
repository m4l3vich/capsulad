package daemon

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/backend"
)

type outputTail interface{ LastOutput() []byte }

// s16Code is the int16 code the mixer emits for one float level.
func s16Code(v float64) int { return int(v * 32767) }

func outputPeak(p []byte) int {
	pk := 0
	for i := 0; i+1 < len(p); i += 2 {
		v := int(int16(binary.LittleEndian.Uint16(p[i:])))
		if v < 0 {
			v = -v
		}
		if v > pk {
			pk = v
		}
	}
	return pk
}

func newVolumeDaemon(t *testing.T) *Daemon {
	t.Helper()
	base := shortTempDir(t)
	// The installation startup cue is a second mixer source. Without the
	// marker it overlaps every measurement taken from the simulator sink.
	if err := os.WriteFile(filepath.Join(base, startupSoundMarker), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := startEmptyDaemon(t, base)
	waitForAppSelection(t, d, "display.segment.front", clockAppID)
	waitForAppSelection(t, d, "light.ring.main", ringAppID)
	return d
}

func openVolumeStream(t *testing.T, client *builtinAppClient, format backend.AudioFormat) frameworkv1.AudioService_PlaybackClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	stream, err := client.audio.Playback(client.context(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&frameworkv1.PlaybackRequest{Request: &frameworkv1.PlaybackRequest_Open{
		Open: &frameworkv1.OpenPlayback{EndpointId: "audio.output.speaker", Format: formatProto(format)},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("started: %v", err)
	}
	return stream
}

func sendAndDrain(t *testing.T, stream frameworkv1.AudioService_PlaybackClient, pcm []byte) {
	t.Helper()
	if err := stream.Send(&frameworkv1.PlaybackRequest{Request: &frameworkv1.PlaybackRequest_Chunk{
		Chunk: &frameworkv1.AudioChunk{Pcm: pcm},
	}}); err != nil {
		t.Fatal(err)
	}
	_ = stream.CloseSend()
	// The daemon ends the event stream only after the mixer pulled the queued
	// tail and wrote it to the sink: this is the drain barrier.
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}
}

func fullScaleS16(frames int) []byte {
	pcm := make([]byte, frames*4)
	for i := range frames {
		binary.LittleEndian.PutUint16(pcm[i*4:], 32767)
		binary.LittleEndian.PutUint16(pcm[i*4+2:], 32767)
	}
	return pcm
}

func f32Constant(frames int, v float32) []byte {
	pcm := make([]byte, frames*8)
	for i := range frames {
		binary.LittleEndian.PutUint32(pcm[i*8:], math.Float32bits(v))
		binary.LittleEndian.PutUint32(pcm[i*8+4:], math.Float32bits(v))
	}
	return pcm
}

// TestDaemonMasterVolumeEndToEnd drives the administrator volume path and one
// app playback stream through gRPC, then measures the simulator sink after the
// stream has fully drained.
func TestDaemonMasterVolumeEndToEnd(t *testing.T) {
	for _, vol := range []float64{0.5, 0.2, 0.1} {
		vol := vol
		t.Run("master_"+floatLabel(vol), func(t *testing.T) {
			d := newVolumeDaemon(t)
			client := newBuiltinAppClient(d, clockAppID)
			defer client.Close()
			tail, ok := d.dev.Audio().(outputTail)
			if !ok {
				t.Fatal("simulator audio has no LastOutput")
			}
			if err := d.applyMasterVolume(vol); err != nil {
				t.Fatal(err)
			}
			waitFor(t, time.Second, "mixer volume applied", func() bool { return d.audio.MasterVolume() == vol })
			stream := openVolumeStream(t, client, d.audio.PlaybackFormat())
			sendAndDrain(t, stream, fullScaleS16(24000))
			time.Sleep(50 * time.Millisecond)
			peak := outputPeak(tail.LastOutput())
			want := s16Code(vol)
			t.Logf("master=%.2f sink peak=%d (%.3f FS)", vol, peak, float64(peak)/32767)
			if diff := peak - want; diff < -2 || diff > 2 {
				t.Errorf("master %.2f: sink peak %d, want %d ±2", vol, peak, want)
			}
		})
	}
}

// TestDaemonMasterVolumeUnityIsLimited is the digital proof that the limiter,
// not the int16 rail, bounds a full-scale stream at master 1.0.
func TestDaemonMasterVolumeUnityIsLimited(t *testing.T) {
	d := newVolumeDaemon(t)
	client := newBuiltinAppClient(d, clockAppID)
	defer client.Close()
	tail := d.dev.Audio().(outputTail)
	if err := d.applyMasterVolume(1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, "mixer volume applied", func() bool { return d.audio.MasterVolume() == 1 })
	stream := openVolumeStream(t, client, d.audio.PlaybackFormat())
	sendAndDrain(t, stream, fullScaleS16(24000))
	time.Sleep(50 * time.Millisecond)
	peak := outputPeak(tail.LastOutput())
	t.Logf("master=1.00 full-scale sink peak=%d (%.3f FS)", peak, float64(peak)/32767)
	if peak > s16Code(0.95)+1 {
		t.Fatalf("master 1.0 sink peak %d exceeds the limiter threshold", peak)
	}
}

// TestDaemonF32HotChunkIsClamped is the end-to-end form of R1: a documented
// F32 stream at amplitude 10 must not defeat master volume 0.1.
func TestDaemonF32HotChunkIsClamped(t *testing.T) {
	d := newVolumeDaemon(t)
	client := newBuiltinAppClient(d, clockAppID)
	defer client.Close()
	tail := d.dev.Audio().(outputTail)
	if err := d.applyMasterVolume(0.1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, "mixer volume applied", func() bool { return d.audio.MasterVolume() == 0.1 })
	format := backend.AudioFormat{Format: backend.F32LE, RateHz: 48000, Channels: 2}
	stream := openVolumeStream(t, client, format)
	sendAndDrain(t, stream, f32Constant(24000, 10))
	time.Sleep(50 * time.Millisecond)
	peak := outputPeak(tail.LastOutput())
	want := s16Code(0.1)
	t.Logf("F32 amplitude 10 at master 0.10 sink peak=%d (%.3f FS)", peak, float64(peak)/32767)
	if diff := peak - want; diff < -2 || diff > 2 {
		t.Fatalf("F32 amplitude 10 at master 0.1: sink peak %d, want %d", peak, want)
	}
}

// TestDaemonOverlapNeverExceedsMaster sums two different apps above full scale.
// The limiter keeps the digital bus at its threshold instead of the int16 rail.
func TestDaemonOverlapNeverExceedsMaster(t *testing.T) {
	d := newVolumeDaemon(t)
	clockClient := newBuiltinAppClient(d, clockAppID)
	defer clockClient.Close()
	ringClient := newBuiltinAppClient(d, ringAppID)
	defer ringClient.Close()
	tail := d.dev.Audio().(outputTail)
	if err := d.applyMasterVolume(0.9); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, "mixer volume applied", func() bool { return d.audio.MasterVolume() == 0.9 })
	first := openVolumeStream(t, clockClient, d.audio.PlaybackFormat())
	second := openVolumeStream(t, ringClient, d.audio.PlaybackFormat())
	if err := first.Send(&frameworkv1.PlaybackRequest{Request: &frameworkv1.PlaybackRequest_Chunk{
		Chunk: &frameworkv1.AudioChunk{Pcm: fullScaleS16(24000)},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := second.Send(&frameworkv1.PlaybackRequest{Request: &frameworkv1.PlaybackRequest_Chunk{
		Chunk: &frameworkv1.AudioChunk{Pcm: fullScaleS16(24000)},
	}}); err != nil {
		t.Fatal(err)
	}
	_ = first.CloseSend()
	_ = second.CloseSend()
	for _, stream := range []frameworkv1.AudioService_PlaybackClient{first, second} {
		for {
			if _, err := stream.Recv(); err != nil {
				break
			}
		}
	}
	time.Sleep(50 * time.Millisecond)
	peak := outputPeak(tail.LastOutput())
	t.Logf("two apps at master 0.90 sink peak=%d (%.3f FS)", peak, float64(peak)/32767)
	limit := s16Code(0.95) + 1
	if peak > limit {
		t.Fatalf("overlapping apps peaked %d, above the limiter threshold %d", peak, limit)
	}
	if peak < 30000 {
		t.Fatalf("overlapping apps peaked %d; the limiter did not engage", peak)
	}
}

// TestPlaybackStreamCapRPC pins the wire contract: the fifth concurrent
// playback stream of one app is RESOURCE_EXHAUSTED, not FAILED_PRECONDITION.
func TestPlaybackStreamCapRPC(t *testing.T) {
	d := newVolumeDaemon(t)
	client := newBuiltinAppClient(d, clockAppID)
	defer client.Close()
	format := d.audio.PlaybackFormat()
	for i := 0; i < 4; i++ {
		openVolumeStream(t, client, format)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.audio.Playback(client.context(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&frameworkv1.PlaybackRequest{Request: &frameworkv1.PlaybackRequest_Open{
		Open: &frameworkv1.OpenPlayback{EndpointId: "audio.output.speaker", Format: formatProto(format)},
	}}); err != nil {
		t.Fatal(err)
	}
	_, err = stream.Recv()
	t.Logf("5th concurrent playback stream: code=%v", status.Code(err))
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("5th stream error = %v (code %v), want RESOURCE_EXHAUSTED", err, status.Code(err))
	}
}

func floatLabel(v float64) string {
	switch v {
	case 1:
		return "1_0"
	case 0.5:
		return "0_5"
	case 0.2:
		return "0_2"
	case 0.1:
		return "0_1"
	}
	return "unknown"
}
