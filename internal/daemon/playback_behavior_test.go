package daemon

import (
	"context"
	"testing"
	"time"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
)

func TestPlaybackCancellationDiscardsQueuedAudio(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	waitForAppSelection(t, d, "display.segment.front", clockAppID)
	client := newBuiltinAppClient(d, clockAppID)
	defer client.Close()
	for _, halfClose := range []bool{false, true} {
		name := "open-input"
		if halfClose {
			name = "draining-input"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream, err := client.audio.Playback(client.context(ctx))
			if err != nil {
				t.Fatal(err)
			}
			if err := stream.Send(&frameworkv1.PlaybackRequest{Request: &frameworkv1.PlaybackRequest_Open{
				Open: &frameworkv1.OpenPlayback{EndpointId: "audio.output.speaker", Format: formatProto(d.audio.PlaybackFormat())},
			}}); err != nil {
				t.Fatal(err)
			}
			started, err := stream.Recv()
			if err != nil || started.GetStarted().GetStreamId() == "" {
				t.Fatalf("playback start: %v, %v", started, err)
			}
			id := started.GetStarted().GetStreamId()
			// Queue multiple seconds, then cancel before they can drain. This
			// must not leave either an open stream or an audible queued tail.
			pcm := make([]byte, 48000*2*2)
			for range 3 {
				if err := stream.Send(&frameworkv1.PlaybackRequest{Request: &frameworkv1.PlaybackRequest_Chunk{
					Chunk: &frameworkv1.AudioChunk{Pcm: pcm},
				}}); err != nil {
					t.Fatal(err)
				}
			}
			if halfClose {
				if err := stream.CloseSend(); err != nil {
					t.Fatal(err)
				}
			}
			cancel()
			waitFor(t, time.Second, "cancelled playback removed before queued tail drains", func() bool {
				for _, active := range d.audio.ActiveStreams() {
					if active.StreamID == id {
						return false
					}
				}
				return true
			})
		})
	}
}
