package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"capsulad/internal/audio"
	"capsulad/internal/settings"
	"capsulad/internal/wakeword"
)

// negativesPath resolves the license-clean negative-training corpus. It ships
// separately from the daemon: training input, not a runtime requirement. The
// path is configurable because the corpus is large (tens of MiB) and /data is
// 78.5 MiB on the Mini; it normally lives outside the device until a training
// session.
func (d *Daemon) negativesPath() string {
	if d.cfg.WakewordNegatives != "" {
		return d.cfg.WakewordNegatives
	}
	return filepath.Join(d.cfg.DataDir, "wakeword", "negatives.bin")
}

// wakeTrainMu serializes on-device fits. Two concurrent trainings would exceed
// the Mini's memory long before either finished, so the second caller is
// rejected instead of queued. Deleting a model takes the same lock: the file a
// running fit is about to install must not be removed underneath it.
var wakeTrainMu sync.Mutex

// errWakeTrainingBusy marks a delete refused because a fit owns the training
// lock. It is a precondition of the device's state, not a bad request, so the
// RPC layer maps it to FAILED_PRECONDITION instead of INVALID_ARGUMENT.
var errWakeTrainingBusy = errors.New("training is running")

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

// TrainWakeword trains and installs one classifier from the explicitly selected
// takes. Training and the quality gates run on the device; nothing is uploaded.
//
// sel.ModelID selects the target: empty creates a new word with a fresh id, an
// existing id replaces exactly that artifact. Every other artifact, including
// its enablement and calibration, is left byte-identical: installing one word
// never re-gates another. The artifact is written only after Train returns a
// gated classifier and the install's final cancellation check passes, so a
// failed or cancelled run leaves the previous artifact untouched. Cancellation
// that lands after the atomic commit cannot undo the install: that run reports
// success because the model is live.
func (d *Daemon) TrainWakeword(ctx context.Context, sel trainSelection, progress func(stage string, fraction float64)) (*wakeword.Classifier, error) {
	if !wakeTrainMu.TryLock() {
		return nil, fmt.Errorf("training is already running; wait for it to finish")
	}
	defer wakeTrainMu.Unlock()
	if err := d.requireFreeSpace("training"); err != nil {
		return nil, err
	}
	valid, _ := d.rescanWakewords()
	modelID := trimModelID(sel.ModelID)
	if modelID == "" {
		modelID = d.newWakewordID(valid)
	} else if err := wakeword.ValidateModelID(modelID); err != nil {
		return nil, &wakeword.TrainingFailure{
			Causes: []wakeword.FailureCause{wakeword.CauseInvalidRequest},
			Detail: err.Error(),
		}
	}
	// Refuse a ninth artifact before the expensive fit. The settings layer caps
	// the per-word list at MaxWakewordBindings, so an extra file could never be
	// named by a binding and would just sit in the store unselectable.
	if !wakewordInstalled(valid, modelID) && len(valid) >= settings.MaxWakewordBindings {
		return nil, &wakeword.TrainingFailure{
			Causes: []wakeword.FailureCause{wakeword.CauseConfiguration},
			Detail: fmt.Sprintf("all %d wakeword slots are in use; delete a word before training another",
				settings.MaxWakewordBindings),
		}
	}
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
	sel.Display = trimModelID(sel.Display)
	en, inputs, err := d.loadSelection(sel)
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
		ID:                modelID,
		Display:           sel.Display,
		Language:          language,
		Seed:              seed,
		NegativesPath:     d.negativesPath(),
		InputProcessing:   inputs.Tap,
		RawTakesConverted: inputs.RawTakesConverted,
		Progress:          report,
	})
	if err != nil {
		return nil, err
	}
	// A cancelled browser request is a failure: do not install its result.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	report("install", 1)
	// InstallClassifier gates the atomic commit on ctx itself: cancellation
	// during its validation/engine build or immediately before the rename
	// leaves every installed artifact untouched. If the commit wins that race
	// the classifier is installed, so returning success is the truthful outcome
	// even when a late cancellation has already detached the browser stream.
	if err := d.InstallClassifier(ctx, clf); err != nil {
		return nil, err
	}
	return clf, nil
}

// classifierGate returns the model's training certificate for the model list,
// or nil when the artifact predates it.
func classifierGate(clf *wakeword.Classifier) *wakeword.GateReport {
	if report, ok := clf.GateReport(); ok {
		return &report
	}
	return nil
}

// restartWakewordEngine replaces the running ensemble after an install or a
// delete. Rebuilding the shared frontend and re-subscribing costs the whole
// ensemble about two seconds of warm-up before any word can score again; a
// per-word enable toggle or rebinding does not restart anything.
func (d *Daemon) restartWakewordEngine() {
	if d.wakeCancel != nil {
		d.wakeCancel()
		d.wakeCancel = nil
	}
	d.startWakewordEngine()
}

// assistantAudioActive reports whether an app a detection could address
// currently holds a capture or playback stream. Detection pauses while one
// does, so the device cannot hear its own output and re-trigger itself.
func (d *Daemon) assistantAudioActive() bool { return d.voiceAudioActive() }

// buildWakewordEngine constructs the shared-frontend ensemble over every valid
// artifact. A returned error detail means no inference may run: the words stay
// listed but unavailable, because scoring undeclared audio would be a silent
// policy violation.
func (d *Daemon) buildWakewordEngine(valid []wakeword.StoredModel) (*wakeword.Ensemble, string, string) {
	if len(valid) == 0 {
		return nil, "", ""
	}
	classifiers := make([]*wakeword.Classifier, 0, len(valid))
	for _, stored := range valid {
		classifiers = append(classifiers, stored.Classifier)
	}
	engine, err := wakeword.NewEnsemble(classifiers)
	if err != nil {
		d.logs.Errorf("wakeword", "", "ensemble: %v", err)
		return nil, "", err.Error()
	}
	// Inference consumes the same declared cleanup as application capture and
	// enrollment. Raw audio is not an accepted detection path: if the profile is
	// unavailable, fail closed and say so rather than score the vendor tap under
	// an unverified calibration.
	tap := d.captureTap()
	if tap == "" {
		d.logs.Errorf("wakeword", "", "capture preprocessing %q is unavailable; inference not started (raw audio is not an accepted detection path)", audio.CaptureProcessingTap)
		return nil, "", fmt.Sprintf("capture preprocessing %q is unavailable", audio.CaptureProcessingTap)
	}
	for _, stored := range valid {
		clf := stored.Classifier
		if declared, ok := clf.InputProcessingTap(); !ok {
			d.logs.Warnf("wakeword", "", "model %s has no input_audio provenance: it was trained on the physical raw tap, detection now runs on %s, so its calibrated threshold is unverified; detection stays enabled, retrain when convenient",
				clf.ID, tap)
		} else if declared != tap {
			d.logs.Warnf("wakeword", "", "model %s declares capture tap %q but the runtime delivers %q; threshold calibration is unverified; detection stays enabled",
				clf.ID, declared, tap)
		}
	}
	return engine, tap, ""
}

// startWakewordEngine installs the ensemble over every trained artifact. Without
// an artifact nothing is advertised and no audio is analysed: the framework does
// not pretend to detect a keyword it does not have.
func (d *Daemon) startWakewordEngine() {
	valid, invalid := d.rescanWakewords()
	engine, tap, detail := d.buildWakewordEngine(valid)
	// Remember why detection cannot run, so a later settings refresh does not
	// advertise a word as available while nothing scores.
	d.wkArtifacts.mu.Lock()
	d.wkArtifacts.unavailable = detail
	d.wkArtifacts.mu.Unlock()
	// The advertised list is applied even when no engine starts, so the setup
	// page can explain an unusable artifact and a per-word toggle always has an
	// authoritative state to edit.
	d.applyWakewordModels(valid, invalid, detail)
	if engine == nil {
		return
	}
	// Training records from the same capture tap. Suppress detections during a
	// take so saying the word cannot start assistant capture/playback inside it.
	d.wake.SetGate(func() bool {
		d.wkSampleMu.Lock()
		recording := d.recording
		d.wkSampleMu.Unlock()
		return !recording && !d.voiceAudioActive()
	})
	specs := make([]wakeword.DetectionSpec, 0, len(valid))
	for _, stored := range valid {
		specs = append(specs, wakeword.DetectionSpec{
			ModelID:   stored.ID,
			Threshold: stored.Classifier.Threshold,
		})
	}
	frames, cancel := d.audio.SubscribeFrames(16)
	// Two consecutive 80 ms windows above a word's calibrated threshold match the
	// policy training validated that model against. The cooldown is global: one
	// utterance must not activate two words.
	runner := wakeword.NewEnsembleRunner(d.wake, engine, specs, wakeword.RunnerOptions{
		MinConsecutive: 2, Trace: d.cfg.WakewordTrace, ProcessingTap: tap,
	})
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
				// audio into a replaced ensemble or overlap the two runners.
				select {
				case <-stop:
					return
				default:
				}
				runner.HandleFrame(frame)
			}
		}
	}()
	ids := make([]string, 0, len(specs))
	for _, spec := range specs {
		ids = append(ids, spec.ModelID)
		d.logs.Infof("wakeword", "", "classifier %s ready (threshold=%.2f)", spec.ModelID, spec.Threshold)
	}
	d.logs.Infof("wakeword", "", "detection running for %d word(s) over one shared frontend", len(specs))
}

// InstallClassifier validates a fully trained classifier and atomically replaces
// that word's artifact. Callers must only pass a classifier that came out of a
// successful Train, whose quality gates ran on held-out recordings.
//
// ctx is the training request's lifetime, not an installation budget: it is
// checked after the expensive validation/ensemble-build work and again by
// writeClassifier immediately before the atomic rename. A cancellation that wins
// either check leaves every installed artifact and the running ensemble
// untouched. Once the rename commits, the install is complete and the engine is
// restarted even if the request has gone away in the meantime; the persisted
// artifact and the running detection must never disagree.
func (d *Daemon) InstallClassifier(ctx context.Context, clf *wakeword.Classifier) error {
	if clf == nil {
		return fmt.Errorf("no classifier to install")
	}
	if err := clf.Validate(); err != nil {
		return err
	}
	if err := wakeword.ValidateModelID(clf.ID); err != nil {
		return err
	}
	// Building the ensemble that will run after the rename proves the artifact
	// is runnable on this device and that the other words still coexist with it,
	// before any file is touched.
	valid, _ := d.rescanWakewords()
	classifiers := make([]*wakeword.Classifier, 0, len(valid)+1)
	for _, stored := range valid {
		if stored.ID == clf.ID {
			continue
		}
		classifiers = append(classifiers, stored.Classifier)
	}
	classifiers = append(classifiers, clf)
	if _, err := wakeword.NewEnsemble(classifiers); err != nil {
		return fmt.Errorf("classifier is not runnable on this device: %w", err)
	}
	// The validation and engine build above are the costly preparation; a
	// browser cancellation that arrived while they ran must not install.
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := json.Marshal(clf)
	if err != nil {
		return fmt.Errorf("encode classifier: %w", err)
	}
	// Capture whether this replaces an artifact *before* the write, so the
	// durable list can tell "new word" from "retrained word". Checking after the
	// atomic rename would always find the file and treat a new word as an
	// existing one, leaving it live under legacy semantics.
	storeTarget := d.classifierPath(clf.ID)
	replacing := false
	if _, err := os.Stat(storeTarget); err == nil {
		replacing = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check installed artifact %s: %w", storeTarget, err)
	}
	if err := d.writeClassifier(ctx, clf.ID, raw); err != nil {
		return err
	}
	// The new artifact is committed. Refresh the installed set and keep the
	// durable per-word list containing every installed word; a new word starts
	// disabled and no other word is re-gated.
	freshID := clf.ID
	if replacing {
		// An in-place retrain keeps its entry and its binding.
		freshID = ""
	}
	d.invalidateWakewords()
	valid, _ = d.rescanWakewords()
	d.materializeWakewords(installedWakewordIDs(valid), freshID)
	d.restartWakewordEngine()
	return nil
}

// writeClassifier atomically replaces one artifact. The previous file stays
// readable until the fully written temporary file is renamed over it, so a failed
// write or a crash mid-write cannot leave a half model installed. Cancellation is
// checked after the temporary file exists but before the rename, so a cancel that
// lands during preparation removes the temporary file and keeps the previous
// artifact installed.
func (d *Daemon) writeClassifier(ctx context.Context, id string, raw []byte) error {
	if err := wakeword.ValidateModelID(id); err != nil {
		return err
	}
	store := d.wakewordStore()
	if err := os.MkdirAll(store.Dir(), 0o755); err != nil {
		return err
	}
	target := store.Path(id)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// DeleteWakewordClassifier removes one trained word: its artifact and, when the
// durable list is authoritative, its entry. Recordings are global training data
// and are never deleted here. The artifact of every other word stays
// byte-identical.
func (d *Daemon) DeleteWakewordClassifier(modelID string) error {
	id := trimModelID(modelID)
	if err := wakeword.ValidateModelID(id); err != nil {
		return err
	}
	// A running fit may be about to install this id; refuse instead of racing it.
	if !wakeTrainMu.TryLock() {
		return fmt.Errorf("%w; delete the wakeword after it finishes", errWakeTrainingBusy)
	}
	defer wakeTrainMu.Unlock()
	store := d.wakewordStore()
	if err := store.Delete(id); err != nil {
		return err
	}
	d.invalidateWakewords()
	valid, _ := d.rescanWakewords()
	d.materializeWakewords(installedWakewordIDs(valid), "")
	d.restartWakewordEngine()
	d.logs.Infof("wakeword", "", "classifier %s deleted; recordings were kept", id)
	return nil
}
