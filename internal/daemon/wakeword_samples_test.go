package daemon

import (
	"context"
	"encoding/binary"
	"os"
	"strings"
	"testing"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/audio"
	"capsulad/internal/backend"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/presentations"
	"capsulad/internal/routing"
	"capsulad/internal/settings"
)

func TestRecordSampleRejectsMutedMicrophone(t *testing.T) {
	ctx := context.Background()
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	sim := backend.NewSimulator(backend.SimulatorOptions{})
	defer sim.Close()
	manager := audio.NewManager(ctx, clock, logs, sim.Audio(), audio.Options{})
	defer manager.Close()
	manager.SetMuted(true)
	d := &Daemon{ctx: ctx, audio: manager}
	_, err = d.RecordSample(ctx, "word", 400)
	if err == nil || !strings.Contains(err.Error(), "microphone is muted") {
		t.Fatalf("muted recording should fail immediately, got %v", err)
	}
	d.wkSampleMu.Lock()
	defer d.wkSampleMu.Unlock()
	if d.recording {
		t.Fatal("rejected recording retained the recording lock")
	}
}

func mustUserStore(t *testing.T, logs *logbus.Bus) *settings.UserStore {
	t.Helper()
	user, err := settings.OpenUser("", logs, nil)
	if err != nil {
		t.Fatal(err)
	}
	return user
}

// A production take must store the processed samples and declare their tap.
// The injected signal is a DC step: the raw vendor tap would keep it at 8192,
// while the declared high-pass removes it. The tail is checked because the
// first milliseconds legitimately contain the high-pass startup transient.
func TestRecordSampleStoresProcessedTakeWithProvenance(t *testing.T) {
	ctx := context.Background()
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	sim := backend.NewSimulator(backend.SimulatorOptions{})
	defer sim.Close()
	manager := audio.NewManager(ctx, clock, logs, sim.Audio(), audio.Options{})
	defer manager.Close()
	if err := manager.Start(); err != nil {
		t.Fatalf("start audio: %v", err)
	}
	pres := presentations.NewManager(clock, logs, nil)
	router := routing.New(clock, logs, pres)
	const ringID = "light.ring.main"
	router.SetResourceClass(ringID, "ring")
	d := &Daemon{
		ctx: ctx, clock: clock, cfg: Config{DataDir: t.TempDir()}, audio: manager, dev: sim,
		logs: logs, pres: pres, router: router,
		user: mustUserStore(t, logs),
		resources: map[string]*frameworkv1.ResourceInfo{
			ringID: {ResourceId: ringID, Kind: frameworkv1.ResourceKind_RESOURCE_LED_RING},
		},
	}

	const dc = 8192
	injected := make([]byte, 2*16000*2)
	for i := 0; i < len(injected); i += 2 {
		binary.LittleEndian.PutUint16(injected[i:], dc)
	}
	sim.InjectPCM(injected)

	sam, err := d.RecordSample(ctx, "room", 400)
	if err != nil {
		t.Fatalf("RecordSample: %v", err)
	}
	prov, found, err := audio.ReadCaptureProvenance(sam.Path)
	if err != nil || !found {
		t.Fatalf("provenance found=%v err=%v", found, err)
	}
	if prov.ProcessingTap != audio.CaptureProcessingTap || prov.SourceEndpoint != audio.CaptureEndpoint {
		t.Fatalf("provenance = %+v", prov)
	}
	pcm, err := readWAV(sam.Path)
	if err != nil {
		t.Fatal(err)
	}
	tail := pcm[len(pcm)-min(len(pcm), 3200):]
	for i, v := range tail {
		if v > 400 || v < -400 {
			t.Fatalf("tail sample %d = %d; raw DC reached the stored take", i, v)
		}
	}
	if err := d.deleteSample(sam.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sam.Path); !os.IsNotExist(err) {
		t.Fatalf("audio file survived delete: %v", err)
	}
	if _, err := os.Stat(audio.CaptureProvenancePath(sam.Path)); !os.IsNotExist(err) {
		t.Fatalf("provenance sidecar survived delete: %v", err)
	}
}
