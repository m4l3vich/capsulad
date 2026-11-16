package wakeword

import (
	"fmt"
	"slices"
	"strings"
)

// FailureCause is the stable machine-readable reason a training run was
// refused. Values are part of the operator contract: the daemon maps them to
// gRPC codes and the Web UI turns them into advice, so a value never changes
// meaning. A cause is added, never renamed.
type FailureCause string

const (
	// CauseInvalidRequest: the request itself is incomplete (for example a
	// missing model id). The caller can fix it without re-recording.
	CauseInvalidRequest FailureCause = "invalid_request"
	// CauseConfiguration: the device cannot train at all (missing input
	// processing policy, missing negative corpus path).
	CauseConfiguration FailureCause = "configuration"

	CauseTooFewWordTakes   FailureCause = "too_few_word_takes"
	CauseTooFewRoomTakes   FailureCause = "too_few_room_takes"
	CauseTooFewSpeechTakes FailureCause = "too_few_speech_takes"
	// CauseTakeTooShort: a submitted take is below the minimum length for its
	// partition (room/speech/false) or for the held-out window.
	CauseTakeTooShort FailureCause = "take_too_short"
	// CauseUnusableTake: the word could not be located in a take, or the take
	// is silent, clipped or otherwise not trainable audio.
	CauseUnusableTake   FailureCause = "unusable_take"
	CauseDuplicateTakes FailureCause = "duplicate_takes"
	// CauseShortContext: a training signal does not have the pre-word or
	// post-word audio the classifier window needs.
	CauseShortContext      FailureCause = "short_context"
	CauseNoPositiveWindows FailureCause = "no_positive_windows"
	CauseNoNegativeWindows FailureCause = "no_negative_windows"
	// CauseInsufficientHeldout: the submitted takes cannot fill the tune or
	// final-test partition, so no honest certificate is possible.
	CauseInsufficientHeldout FailureCause = "insufficient_heldout"
	CauseTuneGateFailed      FailureCause = "tune_gate_failed"
	CauseFinalGateFailed     FailureCause = "final_gate_failed"
	// CauseAuxiliaryGateFailed: the external CNEG off-policy corpus fired. It
	// is a veto, not the processed-audio certificate; it stays a distinct cause.
	CauseAuxiliaryGateFailed FailureCause = "auxiliary_gate_failed"
	CauseCorpusMissing       FailureCause = "corpus_missing"
	CauseCorpusUnusable      FailureCause = "corpus_unusable"
	CauseFitFailed           FailureCause = "fit_failed"
	// CauseInternal: a fault in the trainer or in the produced artifact, not
	// something the operator can fix by recording differently.
	CauseInternal FailureCause = "internal"
)

// Split names used by SplitMetrics and GateSplit. They match the keys of the
// provenance map so a failure and an installed model describe the same splits.
const (
	SplitTune         = "tune"
	SplitFinalTest    = "final_test"
	SplitAuxTune      = "auxiliary_tune"
	SplitAuxFinalTest = "auxiliary_final_test"
)

// FailingTake identifies one submitted take an operator must fix. Index is the
// 1-based position inside its partition, counted the way the selection was
// submitted (and therefore the way the Web UI numbers the takes). SampleID is
// empty when the trainer only knows the partition position.
type FailingTake struct {
	Partition string
	Index     int
	SampleID  string
	Cause     FailureCause
	Detail    string
}

// SplitMetrics is the measured policy result of one evaluated split, copied
// from the evaluator that produced it. Nothing is recomputed for display.
type SplitMetrics struct {
	Split            string
	WordRecordings   int
	Detected         int
	Recall           float64
	FalseActivations int
}

// TrainingFailure is the structured reason training was refused. It carries
// every distinct cause, every failing take and every measured split, so a
// caller can act on the whole result instead of one formatted sentence. It is
// the only error type Train returns for a refusal; cancellation stays a
// context error and an untouched installed model is guaranteed as before.
type TrainingFailure struct {
	Causes []FailureCause
	Takes  []FailingTake
	Splits []SplitMetrics
	// Threshold is the calibrated threshold the splits were measured at, or 0
	// when calibration could not choose one.
	Threshold float64
	Detail    string
}

func (e *TrainingFailure) Error() string {
	if e == nil {
		return "wakeword training failed"
	}
	if e.Detail != "" {
		return "wakeword training: " + e.Detail
	}
	causes := make([]string, len(e.Causes))
	for i, cause := range e.Causes {
		causes[i] = string(cause)
	}
	return "wakeword training: " + strings.Join(causes, ", ")
}

// Cause returns the primary cause (the first recorded one).
func (e *TrainingFailure) Cause() FailureCause {
	if e == nil || len(e.Causes) == 0 {
		return CauseInternal
	}
	return e.Causes[0]
}

// HasCause reports whether the failure records any of the given causes.
func (e *TrainingFailure) HasCause(causes ...FailureCause) bool {
	if e == nil {
		return false
	}
	for _, cause := range causes {
		if slices.Contains(e.Causes, cause) {
			return true
		}
	}
	return false
}

// failureCollector accumulates every problem a run finds, so one refusal
// reports all distinct causes, all failing takes and all measured splits
// instead of aborting on the first. The detail text is a join of the recorded
// notes; the structured fields remain the authoritative record.
type failureCollector struct {
	causes    []FailureCause
	takes     []FailingTake
	splits    []SplitMetrics
	notes     []string
	threshold float64
}

func (f *failureCollector) cause(cause FailureCause, detail string) {
	if !slices.Contains(f.causes, cause) {
		f.causes = append(f.causes, cause)
	}
	f.notes = append(f.notes, detail)
}

func (f *failureCollector) take(take FailingTake) {
	if !slices.Contains(f.causes, take.Cause) {
		f.causes = append(f.causes, take.Cause)
	}
	f.takes = append(f.takes, take)
	f.notes = append(f.notes, fmt.Sprintf("%s recording %d: %s", take.Partition, take.Index, take.Detail))
}

func (f *failureCollector) split(metrics SplitMetrics) {
	f.splits = append(f.splits, metrics)
}

func (f *failureCollector) failed() bool { return len(f.causes) > 0 }

func (f *failureCollector) err() error {
	if !f.failed() {
		return nil
	}
	return &TrainingFailure{
		Causes:    slices.Clone(f.causes),
		Takes:     slices.Clone(f.takes),
		Splits:    slices.Clone(f.splits),
		Threshold: f.threshold,
		Detail:    strings.Join(f.notes, "; "),
	}
}

// GateReport is the quality certificate the trainer stored in
// Classifier.Trained: the calibrated threshold and the tune, final-test and
// auxiliary off-policy measurements. It is read from the artifact; the
// Web UI never recomputes a metric.
type GateReport struct {
	Threshold float64
	Splits    []SplitMetrics
}

// GateReportFromTrained reads the metrics block of a classifier's training
// provenance. ok is false when the artifact predates it (or was hand-built), so
// callers show nothing rather than inventing a number.
func GateReportFromTrained(trained map[string]any) (GateReport, bool) {
	metrics, ok := asMap(trained["metrics"])
	if !ok {
		return GateReport{}, false
	}
	report := GateReport{Threshold: asFloat(metrics["threshold"])}
	for _, entry := range []struct{ split, key string }{
		{SplitTune, "tune"},
		{SplitFinalTest, "final_test"},
	} {
		raw, ok := asMap(metrics[entry.key])
		if !ok {
			continue
		}
		report.Splits = append(report.Splits, splitMetricsFromMap(entry.split, raw))
	}
	if aux, ok := asMap(metrics["auxiliary_offpolicy"]); ok {
		for _, entry := range []struct{ split, key string }{
			{SplitAuxTune, "tune"},
			{SplitAuxFinalTest, "final_test"},
		} {
			raw, ok := asMap(aux[entry.key])
			if !ok {
				continue
			}
			report.Splits = append(report.Splits, splitMetricsFromMap(entry.split, raw))
		}
	}
	return report, len(report.Splits) > 0
}

// GateReport returns the certificate recorded when this model was trained.
func (m *Classifier) GateReport() (GateReport, bool) {
	if m == nil {
		return GateReport{}, false
	}
	return GateReportFromTrained(m.Trained)
}

func splitMetricsFromMap(split string, raw map[string]any) SplitMetrics {
	return SplitMetrics{
		Split:            split,
		WordRecordings:   asInt(raw["word_recordings"]),
		Detected:         asInt(raw["detected"]),
		Recall:           asFloat(raw["recall"]),
		FalseActivations: asInt(raw["false_activations"]),
	}
}

// metricsToSplit copies the evaluator's result under a split name.
func metricsToSplit(split string, m gateMetrics) SplitMetrics {
	return SplitMetrics{
		Split:            split,
		WordRecordings:   m.WordRecordings,
		Detected:         m.Detected,
		Recall:           m.recall(),
		FalseActivations: m.FalseActivations,
	}
}

// asMap accepts both an in-process map and the map[string]any a JSON decode
// produces.
func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// asFloat accepts the int the trainer writes in-process and the float64 a
// JSON decode yields from the same artifact.
func asFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case uint64:
		return float64(n)
	default:
		return 0
	}
}

func asInt(v any) int { return int(asFloat(v)) }
