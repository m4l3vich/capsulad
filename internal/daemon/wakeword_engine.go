package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"capsulad/internal/wakeword"
)

// classifierPath resolves the trained keyword model location.
func (d *Daemon) classifierPath() string {
	if d.cfg.WakewordModel != "" {
		return d.cfg.WakewordModel
	}
	return filepath.Join(d.cfg.DataDir, "wakeword", "model.json")
}

// negativesPath resolves the license-clean negative-training corpus. It ships
// separately from the daemon: training input, not a runtime requirement.
func (d *Daemon) negativesPath() string {
	return filepath.Join(d.cfg.DataDir, "wakeword", "negatives.bin")
}

// wakeTrainMu serializes on-device fits. Two concurrent trainings would exceed
// the Mini's memory long before either finished, so the second caller is
// rejected instead of queued.
var wakeTrainMu sync.Mutex

// trainStageLog explains each enrollment stage in the browser log. The stages
// come from wakeword.Train; unknown stages are still reported verbatim.
var trainStageLog = map[string]string{
	"load":      "reading the selected takes",
	"validate":  "validating takes and rejecting silent or clipped ones",
	"prepare":   "finding the word and building train, tuning and final-test splits",
	"augment":   "augmenting speed, gain, room noise and preceding speech",
	"embed":     "extracting 96-dimensional embeddings on the device",
	"negatives": "loading the license-clean negative corpus",
	"train":     "fitting the embedding classifier",
	"calibrate": "calibrating the threshold on held-out recordings",
	"finaltest": "checking held-out words and false activations",
	"install":   "installing the classifier and restarting detection",
}

// trainStageRange maps an enrollment stage's own 0..1 fraction into the whole
// job, so the browser progress bar advances monotonically instead of resetting
// when each stage starts.
var trainStageRange = map[string][2]float64{
	"load":      {0.00, 0.02},
	"validate":  {0.02, 0.10},
	"prepare":   {0.10, 0.18},
	"augment":   {0.18, 0.30},
	"embed":     {0.30, 0.58},
	"negatives": {0.58, 0.62},
	"train":     {0.62, 0.82},
	"calibrate": {0.82, 0.92},
	"finaltest": {0.92, 0.96},
	"install":   {0.96, 0.99},
}

// TrainWakeword trains and installs a classifier from the explicitly selected
// takes. Training and the quality gates run on the device; nothing is uploaded.
// The installed model is replaced only after Train returns a gated classifier
// and ctx is still live, so a failed or cancelled run leaves it untouched.
func (d *Daemon) TrainWakeword(ctx context.Context, sel trainSelection, progress func(stage string, fraction float64)) (*wakeword.Classifier, error) {
	if !wakeTrainMu.TryLock() {
		return nil, fmt.Errorf("training is already running; wait for it to finish")
	}
	defer wakeTrainMu.Unlock()
	if progress == nil {
		progress = func(string, float64) {}
	}
	var lastFraction float64
	var reportMu sync.Mutex
	report := func(stage string, fraction float64) {
		reportMu.Lock()
		defer reportMu.Unlock()
		if fraction < 0 {
			fraction = 0
		} else if fraction > 1 {
			fraction = 1
		}
		bounds, ok := trainStageRange[stage]
		lo, hi := bounds[0], bounds[1]
		if !ok {
			lo, hi = 0, 1
		}
		value := lo + (hi-lo)*fraction
		if value < lastFraction {
			value = lastFraction
		}
		lastFraction = value
		progress(stage, value)
	}
	report("load", 0)
	sel.Display = strings.TrimSpace(sel.Display)
	en, err := d.loadSelection(sel)
	if err != nil {
		return nil, err
	}
	report("load", 1)
	seed := sel.Seed
	if seed == 0 {
		seed = uint64(time.Now().UnixNano())
	}
	language := sel.Language
	if language == "" {
		language = "ru"
	}
	clf, err := wakeword.Train(ctx, en, wakeword.TrainOptions{
		ID:            "word.personal",
		Display:       sel.Display,
		Language:      language,
		Seed:          seed,
		NegativesPath: d.negativesPath(),
		Progress:      report,
	})
	if err != nil {
		return nil, err
	}
	// A cancelled browser request is a failure: do not install its result.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	report("install", 1)
	if err := d.InstallClassifier(clf); err != nil {
		return nil, err
	}
	return clf, nil
}

// restartWakewordEngine replaces a running engine after a classifier install.
func (d *Daemon) restartWakewordEngine() {
	if d.wakeCancel != nil {
		d.wakeCancel()
		d.wakeCancel = nil
	}
	d.startWakewordEngine()
}

// assistantAudioActive reports whether the selected assistant currently holds a
// capture or playback stream. Detection pauses while it does, so the assistant
// cannot hear its own output and re-trigger itself.
func (d *Daemon) assistantAudioActive() bool {
	appID := d.assistantAppID()
	if appID == "" {
		return false
	}
	for _, st := range d.audio.ActiveStreams() {
		if st.AppID == appID {
			if d.cfg.WakewordTrace {
				d.logs.Debugf("wakeword", "", "gate held by stream=%s app=%s input=%t", st.StreamID, st.AppID, st.Input)
			}
			return true
		}
	}
	return false
}

// startWakewordEngine installs a trained classifier when one exists. Without a
// model nothing is advertised and no audio is analysed: the framework does not
// pretend to detect a keyword it does not have.
func (d *Daemon) startWakewordEngine() {
	path := d.classifierPath()
	clf, err := wakeword.LoadClassifier(path)
	if err != nil {
		if !os.IsNotExist(err) {
			d.logs.Warnf("wakeword", "", "classifier %s unusable: %v", path, err)
			// Keep an installed but unusable artifact visible so the setup page
			// can say it must be retrained. It is never advertised as available:
			// the engine runs no inference until a version 3 model loads.
			d.wake.SetModels([]wakeword.Model{{
				ID: "word.personal", DisplayName: "Installed model needs retraining",
				EngineID: "framework.embedding-mlp", Available: false,
			}})
		}
		return
	}
	engine, err := wakeword.NewEngine(clf)
	if err != nil {
		d.logs.Warnf("wakeword", "", "engine: %v", err)
		return
	}
	// Training records from the same capture tap. Suppress detections during a
	// take so saying the word cannot start assistant capture/playback inside it.
	d.wake.SetGate(func() bool {
		d.wkSampleMu.Lock()
		recording := d.recording
		d.wkSampleMu.Unlock()
		return !recording && !d.assistantAudioActive()
	})
	d.wake.SetModels([]wakeword.Model{{
		ID: clf.ID, DisplayName: clf.Display, Language: clf.Language,
		EngineID: "framework.embedding-mlp", Available: true,
	}})
	frames, cancel := d.audio.SubscribeFrames(16)
	// Two consecutive 80 ms windows above the calibrated threshold match the
	// policy training validated the model against.
	runner := wakeword.NewRunner(d.wake, engine, clf.ID, wakeword.RunnerOptions{Threshold: clf.Threshold, MinConsecutive: 2, Trace: d.cfg.WakewordTrace})
	stop := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	d.wakeCancel = func() {
		once.Do(func() {
			close(stop)
			cancel()
			<-done
		})
	}
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case frame, ok := <-frames:
				if !ok {
					return
				}
				// Cancellation closes a buffered subscription. Do not drain old
				// audio into a replaced model or overlap the two runners.
				select {
				case <-stop:
					return
				default:
				}
				runner.HandleFrame(frame)
			}
		}
	}()
	d.logs.Infof("wakeword", "", "classifier %s installed from %s (context=%d projection=%d hidden=%d threshold=%.2f)",
		clf.ID, path, clf.ContextFrames, clf.Projection, clf.Hidden, clf.Threshold)
}
