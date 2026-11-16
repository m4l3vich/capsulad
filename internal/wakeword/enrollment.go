package wakeword

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"math"
	"math/rand/v2"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"capsulad/internal/audio"
	"capsulad/internal/wakeword/oww"
)

// On-device enrollment training.
//
// The classifier never sees raw audio. Every streaming signal is first turned
// into the same 96-dimensional embedding stream the device runs, using the
// exact oww timing contract (EmbeddingEnd), and the classifier is fitted on
// 16-embedding windows. Training data is built from original recordings only;
// augmentation happens after the original takes and negative streams have been
// split into train/tune/final-test so no synthetic copy can leak into a
// held-out metric.
const (
	enrollmentSampleRate = 16000

	minWordRecordings    = 6                        // independent word takes
	minRoomRecordings    = 3                        // independent room/background takes
	minSpeechRecordings  = 3                        // independent unrelated-speech takes
	minRoomTakeSamples   = enrollmentSampleRate     // 1 s of background
	minSpeechTakeSamples = enrollmentSampleRate / 2 // 0.5 s of speech
	minFalseTakeSamples  = enrollmentSampleRate / 4 // 0.25 s hard negative

	// Aim for a few hundred augmented signals while bounding repetition
	// of each original when independent recordings are scarce.
	augmentTarget     = 320
	augmentPerTakeCap = 96

	trainingEpochs          = 40
	trainingBatchSize       = 64
	trainingLearningRate    = 0.001
	trainingPositiveWeight  = 2.0
	trainingNegativesPerPos = 4

	// Every epoch draws fresh random negatives from the whole pool; hard
	// negatives are an additional set refreshed by periodic mining. Both floors
	// keep coverage broad even for a small enrollment, and FitClassifier caps
	// each request by the pool (hard mining also by miningPoolCap).
	minRandomNegativesPerEpoch = 6000
	minHardNegativesPerEpoch   = 1000

	maxLocalNegativeRepeat = 1024
	embedWorkerCap         = 4

	// Activation policy the daemon runs: 80 ms embeddings, two consecutive
	// windows above threshold, then a 2.9 s cooldown.
	policyConsecutive      = 2
	policyCooldownSamples  = 46400 // 2.9 s at 16 kHz
	policyMaxDelaySamples  = 16000 // a detection may trail the word by at most 1 s
	maxPostWordStepSamples = 6400  // positive windows may end up to 400 ms after the word

	thresholdMin  = 0.05
	thresholdMax  = 0.99
	thresholdStep = 0.005
)

// Enrollment is the captured audio for one wake word. Takes are 16 kHz mono
// signed 16-bit PCM. Ambient and Speech are background/room and unrelated
// speech recordings; False are user-captured hard negatives.
type Enrollment struct {
	Takes   [][]int16
	Ambient [][]int16
	Speech  [][]int16
	False   [][]int16
}

// TrainOptions configures one training run.
type TrainOptions struct {
	ID       string
	Display  string
	Language string
	Seed     uint64
	// NegativesPath is a CNEG0002 corpus. It is required: the device does not
	// embed a bundled negative set and training must not fall back to one. Its
	// precomputed embeddings are external, off-policy training examples; the
	// tune/final-test certificate uses the local processed held-out takes.
	NegativesPath string
	// InputProcessing declares the capture preprocessing already applied to the
	// enrollment takes before they reach Train. It must name the same profile
	// runtime inference declares (for example audio.CaptureProcessingTap); an
	// empty value means the raw physical tap.
	InputProcessing string
	// RawTakesConverted reports how many selected takes were declarations or
	// legacy files of the raw physical tap, converted offline onto
	// InputProcessing. It is provenance, not a setting.
	RawTakesConverted int
	// Progress reports monotonic 0..1 fractions within each stage. It may be
	// called concurrently from embedding workers and is serialized here.
	Progress func(stage string, fraction float64)
}

// Train fits a version-3 classifier from enrollment audio and a licensed
// negative-feature corpus. It returns an error, and no classifier, unless the
// word can be found in every take and the calibrated policy clears the held-out
// quality gate. The caller installs the result only on success.
func Train(ctx context.Context, en Enrollment, opts TrainOptions) (*Classifier, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	opts = opts.withDefaults()
	if err := opts.check(); err != nil {
		return nil, err
	}
	var progressMu sync.Mutex
	progress := func(stage string, fraction float64) {
		if opts.Progress == nil {
			return
		}
		if fraction < 0 {
			fraction = 0
		} else if fraction > 1 {
			fraction = 1
		}
		progressMu.Lock()
		defer progressMu.Unlock()
		opts.Progress(stage, fraction)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("wakeword training cancelled: %w", err)
	}

	// Stage: validate. Find the word in every take; a take that does not
	// contain recognisable speech is rejected instead of trained on. Every
	// problem found here is collected: a refusal names all unusable takes and
	// all partition minima, so the operator does not fix one take only to be
	// refused by the next check.
	progress("validate", 0)
	var invalid failureCollector
	if len(en.Takes) < minWordRecordings {
		invalid.cause(CauseTooFewWordTakes, fmt.Sprintf(
			"need at least %d word recordings (*_word.wav), got %d", minWordRecordings, len(en.Takes)))
	}
	words := make([]wordTake, len(en.Takes))
	wordOK := make([]bool, len(en.Takes))
	for i, pcm := range en.Takes {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("wakeword training cancelled: %w", err)
		}
		start, end, err := trimWord(pcm)
		if err != nil {
			invalid.take(FailingTake{Partition: "word", Index: i + 1, Cause: CauseUnusableTake, Detail: err.Error()})
			continue
		}
		words[i] = wordTake{pcm: pcm, start: start, end: end}
		wordOK[i] = true
		progress("validate", float64(i+1)/float64(len(words)))
	}
	seenWord := make(map[uint64]int, len(words))
	for i := range words {
		if !wordOK[i] {
			continue
		}
		fp := takeFingerprint(words[i].pcm)
		if j, ok := seenWord[fp]; ok && slices.Equal(words[j].pcm, words[i].pcm) {
			invalid.take(FailingTake{Partition: "word", Index: i + 1, Cause: CauseDuplicateTakes,
				Detail: fmt.Sprintf("duplicates word recording %d; record independent takes", j+1)})
			continue
		}
		seenWord[fp] = i
	}
	rooms := invalid.audioSet("room/ambient", "room", en.Ambient, minRoomRecordings, minRoomTakeSamples, CauseTooFewRoomTakes)
	speech := invalid.audioSet("speech", "speech", en.Speech, minSpeechRecordings, minSpeechTakeSamples, CauseTooFewSpeechTakes)
	falseTakes := invalid.audioSet("false-activation", "false_activation", en.False, 0, minFalseTakeSamples, CauseTakeTooShort)
	if err := invalid.err(); err != nil {
		return nil, err
	}

	// Stage: prepare. Split ORIGINAL takes before any augmentation, and load
	// the licensed corpus. Everything used for augmentation comes from train
	// members only; tune/final-test members never contribute background noise or
	// preceding speech.
	progress("prepare", 0)
	wordPCM := make([][]int16, len(words))
	for i := range words {
		wordPCM[i] = words[i].pcm
	}
	wordSplit, err := splitByContent(wordPCM, 2, 2)
	if err != nil {
		return nil, splitFailure("word", err)
	}
	roomSplit, err := splitByContent(rooms, 1, 1)
	if err != nil {
		return nil, splitFailure("room", err)
	}
	speechSplit, err := splitByContent(speech, 1, 1)
	if err != nil {
		return nil, splitFailure("speech", err)
	}
	falseMinTune, falseMinTest := 0, 0
	if len(falseTakes) >= 3 {
		falseMinTune, falseMinTest = 1, 1
	}
	falseSplit, err := splitByContent(falseTakes, falseMinTune, falseMinTest)
	if err != nil {
		return nil, splitFailure("false_activation", err)
	}
	progress("prepare", 0.4)

	neg, err := LoadNegatives(opts.NegativesPath)
	if err != nil {
		cause := CauseCorpusUnusable
		if errors.Is(err, fs.ErrNotExist) {
			cause = CauseCorpusMissing
		}
		return nil, &TrainingFailure{Causes: []FailureCause{cause}, Detail: err.Error()}
	}
	if err := requireNegativesSplits(neg); err != nil {
		return nil, err
	}
	progress("prepare", 1)

	// Stage: augment. Originals of the training takes plus synthetic variants.
	progress("augment", 0)
	trainRooms := pickTakes(rooms, roomSplit.Train)
	tuneRooms := pickTakes(rooms, roomSplit.Tune)
	testRooms := pickTakes(rooms, roomSplit.Test)
	trainSpeech := pickTakes(speech, speechSplit.Train)
	perTake := (augmentTarget + len(wordSplit.Train) - 1) / len(wordSplit.Train)
	if perTake > augmentPerTakeCap {
		perTake = augmentPerTakeCap
	}
	if perTake < 1 {
		perTake = 1
	}

	// Build lazy embedding jobs. Materialising every augmented take up front
	// would keep tens of megabytes of PCM resident; each worker now generates
	// one signal, embeds it, and drops the PCM immediately.
	var tasks []embedTask
	addTask := func(materialize func() ([]int16, int, int, error)) {
		tasks = append(tasks, embedTask{materialize: materialize})
	}

	trainStart := len(tasks)
	for i, wi := range wordSplit.Train {
		wt := words[wi]
		addTask(func() ([]int16, int, int, error) {
			pcm, ws, we := padOriginal(wt.pcm, wt.start, wt.end)
			return pcm, ws, we, nil
		})
		for v := 0; v < perTake; v++ {
			variant := v
			addTask(func() ([]int16, int, int, error) {
				rng := rand.New(rand.NewPCG(
					derivedSeed(opts.Seed, i, variant),
					derivedSeed(opts.Seed^0xd1b54a32d192ed03, variant, i),
				))
				pcm, ws, we, err := augmentWordTake(rng, wt.pcm, wt.start, wt.end, trainRooms, trainSpeech)
				if err != nil {
					return nil, 0, 0, &TrainingFailure{
						Causes: []FailureCause{CauseUnusableTake},
						Takes:  []FailingTake{{Partition: "word", Index: wi + 1, Cause: CauseUnusableTake, Detail: err.Error()}},
						Detail: fmt.Sprintf("augmenting word recording %d: %v", wi+1, err),
					}
				}
				return pcm, ws, we, nil
			})
		}
		progress("augment", float64(i+1)/float64(len(wordSplit.Train)))
	}
	trainCount := len(tasks) - trainStart

	tuneWordStart := len(tasks)
	for _, wi := range wordSplit.Tune {
		wt := words[wi]
		addTask(func() ([]int16, int, int, error) {
			pcm, ws, we := padOriginal(wt.pcm, wt.start, wt.end)
			return pcm, ws, we, nil
		})
	}
	tuneWordCount := len(tasks) - tuneWordStart

	testWordStart := len(tasks)
	for _, wi := range wordSplit.Test {
		wt := words[wi]
		addTask(func() ([]int16, int, int, error) {
			pcm, ws, we := padOriginal(wt.pcm, wt.start, wt.end)
			return pcm, ws, we, nil
		})
	}
	testWordCount := len(tasks) - testWordStart

	// Local negatives are warmed with real room audio from their own split, not
	// tiled copies of themselves. The streaming frontend needs about two seconds
	// of context before its first full 16-embedding window, so a recording
	// embedded from sample zero would leave its first seconds outside the
	// classifier's real operating conditions.
	addNegative := func(pcm []int16, prerollRooms [][]int16) {
		addTask(func() ([]int16, int, int, error) {
			return warmNegative(capPCM(pcm), prerollRooms), -1, -1, nil
		})
	}
	trainFalseStart := len(tasks)
	for _, ti := range falseSplit.Train {
		addNegative(falseTakes[ti], trainRooms)
	}
	trainFalseCount := len(tasks) - trainFalseStart

	trainRoomStart := len(tasks)
	for _, ti := range roomSplit.Train {
		addNegative(rooms[ti], trainRooms)
	}
	trainRoomCount := len(tasks) - trainRoomStart

	trainSpeechStart := len(tasks)
	for _, ti := range speechSplit.Train {
		addNegative(speech[ti], trainRooms)
	}
	trainSpeechCount := len(tasks) - trainSpeechStart

	tuneNegStart := len(tasks)
	for _, ti := range roomSplit.Tune {
		addNegative(rooms[ti], tuneRooms)
	}
	for _, ti := range speechSplit.Tune {
		addNegative(speech[ti], tuneRooms)
	}
	for _, ti := range falseSplit.Tune {
		addNegative(falseTakes[ti], tuneRooms)
	}
	tuneNegCount := len(tasks) - tuneNegStart

	testNegStart := len(tasks)
	for _, ti := range roomSplit.Test {
		addNegative(rooms[ti], testRooms)
	}
	for _, ti := range speechSplit.Test {
		addNegative(speech[ti], testRooms)
	}
	for _, ti := range falseSplit.Test {
		addNegative(falseTakes[ti], testRooms)
	}
	testNegCount := len(tasks) - testNegStart

	progress("embed", 0)
	assets, err := oww.Load()
	if err != nil {
		return nil, fmt.Errorf("wakeword training: embedding assets: %w", err)
	}
	embedded, err := embedTasks(ctx, assets, tasks, func(done, total int) {
		progress("embed", float64(done)/float64(total))
	})
	if err != nil {
		return nil, err
	}

	// Each train take contributes one original signal followed by perTake
	// augmented variants, in that order. Map a signal back to the submitted word
	// take so a refusal names a take the operator actually recorded.
	signalsPerTake := perTake + 1
	wordTakeForSignal := func(i int) int {
		position := min((i-trainStart)/signalsPerTake, len(wordSplit.Train)-1)
		return wordSplit.Train[position] + 1
	}

	// Validate every signal's context before sampling windows from any of them,
	// so a refusal reports all short signals instead of the first.
	var trainingSamples int64
	var contextFailures failureCollector
	for i := trainStart; i < trainStart+trainCount; i++ {
		sig := &embedded[i]
		trainingSamples += int64(sig.samples)
		if sig.wordStart < minPrecontext {
			contextFailures.take(FailingTake{Partition: "word", Index: wordTakeForSignal(i), Cause: CauseShortContext,
				Detail: fmt.Sprintf("training signal %d has only %d ms of pre-word context, need %d ms",
					i-trainStart+1, sig.wordStart*1000/enrollmentSampleRate, minPrecontext*1000/enrollmentSampleRate)})
			continue
		}
		if post := sig.samples - sig.wordEnd; post < minPostSlack {
			contextFailures.take(FailingTake{Partition: "word", Index: wordTakeForSignal(i), Cause: CauseShortContext,
				Detail: fmt.Sprintf("training signal %d has only %d ms of post-word audio, need %d ms",
					i-trainStart+1, post*1000/enrollmentSampleRate, minPostSlack*1000/enrollmentSampleRate)})
		}
	}
	if err := contextFailures.err(); err != nil {
		return nil, err
	}
	augmented := trainCount - len(wordSplit.Train)

	// Positives: every complete-word context window ending between the word end
	// and 400 ms after it. The daemon fires only after two consecutive windows,
	// so a single labelled frame would not teach the streak the runtime expects.
	// The same streams also yield clean pre-word windows (ending by
	// minPrecontext), which teach ordinary speech, breath and room context
	// before the keyword instead of leaving that phase unseen. Windows ending
	// before the word end are not complete keyword examples: they teach
	// near-boundary partials as negatives so two partial scores cannot fire
	// prematurely and consume the cooldown before the complete word.
	positives := make([]TrainingExample, 0, trainCount*6)
	cleanPrefixCount := max(0, (minPrecontext-oww.FirstEmbeddingSamples)/oww.StepSamples-ContextFrames+2)
	prefixNegatives := make([]TrainingExample, 0, trainCount*cleanPrefixCount)
	var partialNegatives []TrainingExample
	var windowFailures failureCollector
	for i := trainStart; i < trainStart+trainCount; i++ {
		sig := &embedded[i]
		prefixNegatives = appendPrefixNegativeWindows(prefixNegatives, sig)
		first, last, err := positiveWindowRange(sig.count, sig.wordStart, sig.wordEnd)
		if err != nil {
			windowFailures.take(FailingTake{Partition: "word", Index: wordTakeForSignal(i), Cause: CauseNoPositiveWindows,
				Detail: fmt.Sprintf("training signal %d: %v", i-trainStart+1, err)})
			continue
		}
		for e := first; e <= last; e++ {
			positives = append(positives, TrainingExample{Features: sig.window(e), Positive: true})
		}
		start := max(first-4, ContextFrames-1)
		for e := start; e < first; e++ {
			partialNegatives = append(partialNegatives, TrainingExample{Features: sig.window(e), Positive: false})
		}
	}
	if err := windowFailures.err(); err != nil {
		return nil, err
	}
	if len(positives) == 0 {
		return nil, &TrainingFailure{Causes: []FailureCause{CauseNoPositiveWindows},
			Detail: "no positive training windows were produced"}
	}

	// Stage: negatives. CNEG train streams are already embeddings; local room,
	// speech and explicit false examples are embedded above. Every full 80 ms
	// phase is a view, never a copy, so the pool is bounded by the decoded
	// corpus and by the existing CNEG file size rather than by a preselected
	// quota that silently drops phases.
	progress("negatives", 0)
	corpusNegatives := corpusNegativeWindows(neg)
	var localNegatives []TrainingExample
	localRanges := [][2]int{
		{trainFalseStart, trainFalseStart + trainFalseCount},
		{trainRoomStart, trainRoomStart + trainRoomCount},
		{trainSpeechStart, trainSpeechStart + trainSpeechCount},
	}
	for _, rg := range localRanges {
		for i := rg[0]; i < rg[1]; i++ {
			sig := &embedded[i]
			localNegatives = appendStreamNegativeWindows(localNegatives, sig.flat, sig.count)
		}
	}
	localNegatives = append(localNegatives, partialNegatives...)
	localNegatives = append(localNegatives, prefixNegatives...)

	// Clean pre-word contexts, word partials and recorded negatives all belong
	// to the user's domain. Balance that whole local pool against the corpus.
	repeat := 1
	if len(corpusNegatives) > 0 && len(localNegatives) > 0 {
		repeat = min(maxLocalNegativeRepeat, (len(corpusNegatives)+len(localNegatives)-1)/len(localNegatives))
	}
	localViews := len(localNegatives) * repeat
	negatives := make([]TrainingExample, 0, len(corpusNegatives)+localViews)
	negatives = append(negatives, corpusNegatives...)
	for range repeat {
		negatives = append(negatives, localNegatives...)
	}
	if len(negatives) == 0 {
		return nil, &TrainingFailure{Causes: []FailureCause{CauseNoNegativeWindows},
			Detail: "negatives corpus and local room/speech/false recordings produced no training windows"}
	}
	// The pool is a structural view: the local block is repeated to balance it
	// against the corpus, but mining addresses each canonical window once, so a
	// duplicated view cannot consume several hard slots or a share of the
	// sampled budget. The shape costs two integers, not a copy per window.
	poolShape := NegativePoolShape{
		Canonical:  len(corpusNegatives) + len(localNegatives),
		RepeatSpan: len(localNegatives),
	}
	progress("negatives", 1)

	// Stage: train. Only train-split material is fitted; the classifier is
	// seeded so runs are reproducible.
	progress("train", 0)
	clf := NewClassifier(opts.ID, opts.Display, opts.Language, opts.Seed)
	fitLosses := make([]float64, 0, trainingEpochs)
	// Fresh random coverage every epoch, with a separate additive hard set.
	// FitClassifier clamps both requests: random to the pool size, hard to the
	// number of distinct windows and miningPoolCap.
	randomPerEpoch := max(trainingNegativesPerPos*len(positives), minRandomNegativesPerEpoch)
	hardPerEpoch := max(len(positives), minHardNegativesPerEpoch)
	fitErr := FitClassifier(ctx, clf, positives, negatives, FitOptions{
		Seed:                    opts.Seed,
		Epochs:                  trainingEpochs,
		BatchSize:               trainingBatchSize,
		LearningRate:            trainingLearningRate,
		RandomNegativesPerEpoch: randomPerEpoch,
		HardNegativesPerEpoch:   hardPerEpoch,
		NegativePool:            poolShape,
		PositiveWeight:          trainingPositiveWeight,
		Progress: func(epoch int, loss float64) {
			if math.IsNaN(loss) || math.IsInf(loss, 0) {
				loss = -1
			}
			progressMu.Lock()
			fitLosses = append(fitLosses, loss)
			progressMu.Unlock()
			progress("train", float64(epoch)/float64(trainingEpochs))
		},
	})
	if fitErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fitErr
		}
		return nil, &TrainingFailure{Causes: []FailureCause{CauseFitFailed},
			Detail: fmt.Sprintf("fitting the classifier failed: %v", fitErr)}
	}

	// Stage: calibrate. The threshold is chosen on the tune split only. The
	// policy certificate uses the local processed takes; CNEG streams are
	// precomputed from external audio and stay auxiliary/off-policy.
	progress("calibrate", 0)
	tuneWordEvals, err := evalFromEmbedded(ctx, clf, embedded[tuneWordStart:tuneWordStart+tuneWordCount], false)
	if err != nil {
		return nil, heldoutWordFailure(err, embedded[tuneWordStart:tuneWordStart+tuneWordCount], wordSplit.Tune)
	}
	tuneNegEvals, tuneAuxEvals, err := evalNegativeSignals(ctx, clf, neg, "tune", embedded[tuneNegStart:tuneNegStart+tuneNegCount])
	if err != nil {
		return nil, evalFailure(err)
	}
	if countScoreWindows(tuneNegEvals) == 0 {
		return nil, failureWithTakes(CauseInsufficientHeldout,
			fmt.Sprintf("held-out tune room/speech takes are too short for a %d-embedding window; record longer independent takes", ContextFrames),
			shortNegativeTakes(embedded[tuneNegStart:tuneNegStart+tuneNegCount], roomSplit.Tune, speechSplit.Tune, falseSplit.Tune))
	}
	tuning, err := calibrateThreshold(tuneWordEvals, tuneNegEvals)
	if err != nil {
		var failure *TrainingFailure
		if errors.As(err, &failure) {
			addGateTakeFailures(failure, CauseTuneGateFailed, tuning.bestAt,
				tuneWordEvals, wordSplit.Tune, tuneNegEvals, roomSplit.Tune, speechSplit.Tune, falseSplit.Tune,
				"the tune split never detected this recording at a feasible threshold", "this recording false-fired on the tune split")
		}
		return nil, err
	}
	threshold := tuning.threshold
	tuneMetrics := tuning.chosen
	auxTuneMetrics := measurePolicy(nil, tuneAuxEvals, threshold)
	progress("calibrate", 1)

	// Stage: final test. Evaluated once with the calibrated threshold; it never
	// chooses the threshold or changes the model. Only processed local takes
	// decide the pass/fail certificate; the CNEG result is reported separately.
	progress("finaltest", 0)
	testWordEvals, err := evalFromEmbedded(ctx, clf, embedded[testWordStart:testWordStart+testWordCount], false)
	if err != nil {
		return nil, heldoutWordFailure(err, embedded[testWordStart:testWordStart+testWordCount], wordSplit.Test)
	}
	testNegEvals, testAuxEvals, err := evalNegativeSignals(ctx, clf, neg, "test", embedded[testNegStart:testNegStart+testNegCount])
	if err != nil {
		return nil, evalFailure(err)
	}
	if countScoreWindows(testNegEvals) == 0 {
		return nil, failureWithTakes(CauseInsufficientHeldout,
			fmt.Sprintf("held-out final-test room/speech takes are too short for a %d-embedding window; record longer independent takes", ContextFrames),
			shortNegativeTakes(embedded[testNegStart:testNegStart+testNegCount], roomSplit.Test, speechSplit.Test, falseSplit.Test))
	}
	testMetrics := measurePolicy(testWordEvals, testNegEvals, threshold)
	auxTestMetrics := measurePolicy(nil, testAuxEvals, threshold)
	if err := finalGateError(testMetrics, auxTuneMetrics, auxTestMetrics, threshold); err != nil {
		var failure *TrainingFailure
		if errors.As(err, &failure) {
			addGateTakeFailures(failure, CauseFinalGateFailed, threshold,
				testWordEvals, wordSplit.Test, testNegEvals, roomSplit.Test, speechSplit.Test, falseSplit.Test,
				"the final test did not detect this recording at the installed threshold", "this recording false-fired on the final test")
		}
		return nil, err
	}
	progress("finaltest", 1)

	clf.Threshold = threshold
	clf.Trained = provenance(trainingProvenance{
		opts:            opts,
		words:           words,
		neg:             neg,
		wordSplit:       wordSplit,
		roomSplit:       roomSplit,
		speechSplit:     speechSplit,
		falseSplit:      falseSplit,
		roomTakes:       rooms,
		speechTakes:     speech,
		falseTakes:      falseTakes,
		trainingSignals: trainCount,
		trainingSamples: trainingSamples,
		augmented:       augmented,
		positiveWindows: len(positives),
		corpusWindows:   len(corpusNegatives),
		localWindows:    len(localNegatives),
		localViews:      localViews,
		localRepeat:     repeat,
		partialWindows:  len(partialNegatives),
		prefixWindows:   len(prefixNegatives),
		fitLosses:       fitLosses,
		tuneMetrics:     tuneMetrics,
		finalMetrics:    testMetrics,
		auxTuneMetrics:  auxTuneMetrics,
		auxTestMetrics:  auxTestMetrics,
		threshold:       threshold,
	})
	if err := clf.Validate(); err != nil {
		return nil, &TrainingFailure{Causes: []FailureCause{CauseInternal},
			Detail: fmt.Sprintf("trained classifier is invalid: %v", err)}
	}
	return clf, nil
}

// --- enrollment inputs ------------------------------------------------------

type wordTake struct {
	pcm   []int16
	start int
	end   int
}

// failureWithTakes builds a refusal with its failing takes attached.
func failureWithTakes(cause FailureCause, detail string, takes []FailingTake) *TrainingFailure {
	return &TrainingFailure{Causes: []FailureCause{cause}, Takes: takes, Detail: detail}
}

// evalFailure keeps cancellation intact and reports any other evaluation fault
// as an internal training fault.
func evalFailure(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var failure *TrainingFailure
	if errors.As(err, &failure) {
		return err
	}
	return &TrainingFailure{Causes: []FailureCause{CauseInternal}, Detail: err.Error()}
}

// heldoutWordFailure reports every held-out word take too short to be scored.
// An already-typed fault (a non-finite score, cancellation) passes through.
func heldoutWordFailure(err error, sigs []embeddedSignal, indices []int) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var failure *TrainingFailure
	if errors.As(err, &failure) {
		return err
	}
	takes := make([]FailingTake, 0, len(indices))
	for i := range sigs {
		if i >= len(indices) || sigs[i].count >= ContextFrames {
			continue
		}
		takes = append(takes, FailingTake{Partition: "word", Index: indices[i] + 1, Cause: CauseTakeTooShort,
			Detail: fmt.Sprintf("held-out recording is too short for a %d-embedding window", ContextFrames)})
	}
	return failureWithTakes(CauseInsufficientHeldout, err.Error(), takes)
}

// negativeTakeRef identifies one local held-out negative take.
type negativeTakeRef struct {
	partition string
	index     int
}

// negativeRefs lists the partition and 1-based submitted index of each local
// held-out negative, in the order evalNegativeSignals builds them: rooms,
// unrelated speech, then explicit false activations.
func negativeRefs(rooms, speech, falses []int) []negativeTakeRef {
	refs := make([]negativeTakeRef, 0, len(rooms)+len(speech)+len(falses))
	for _, index := range rooms {
		refs = append(refs, negativeTakeRef{partition: "room", index: index + 1})
	}
	for _, index := range speech {
		refs = append(refs, negativeTakeRef{partition: "speech", index: index + 1})
	}
	for _, index := range falses {
		refs = append(refs, negativeTakeRef{partition: "false_activation", index: index + 1})
	}
	return refs
}

// shortNegativeTakes reports the local held-out negatives whose embedding
// stream cannot hold one classifier window.
func shortNegativeTakes(sigs []embeddedSignal, rooms, speech, falses []int) []FailingTake {
	refs := negativeRefs(rooms, speech, falses)
	takes := make([]FailingTake, 0, len(refs))
	for i := range sigs {
		if i >= len(refs) || sigs[i].count >= ContextFrames {
			continue
		}
		takes = append(takes, FailingTake{Partition: refs[i].partition, Index: refs[i].index, Cause: CauseTakeTooShort,
			Detail: fmt.Sprintf("held-out take is too short for a %d-embedding window; record a longer independent take", ContextFrames)})
	}
	return takes
}

// addGateTakeFailures appends every held-out take that explains a gate
// refusal: word takes the policy missed and local negatives that false fired at
// the measured threshold.
func addGateTakeFailures(failure *TrainingFailure, cause FailureCause, threshold float64,
	words []evalSignal, wordIndices []int, negatives []evalSignal, rooms, speech, falses []int,
	wordDetail, negativeDetail string) {
	for i, word := range words {
		if evaluatePolicy(word, threshold).detected {
			continue
		}
		index := i + 1
		if i < len(wordIndices) {
			index = wordIndices[i] + 1
		}
		failure.Takes = append(failure.Takes, FailingTake{Partition: "word", Index: index, Cause: cause, Detail: wordDetail})
	}
	refs := negativeRefs(rooms, speech, falses)
	for i, negative := range negatives {
		if evaluatePolicy(negative, threshold).falseActivations == 0 {
			continue
		}
		ref := negativeTakeRef{partition: "negative", index: i + 1}
		if i < len(refs) {
			ref = refs[i]
		}
		failure.Takes = append(failure.Takes, FailingTake{Partition: ref.partition, Index: ref.index, Cause: cause, Detail: negativeDetail})
	}
}

func (o TrainOptions) withDefaults() TrainOptions {
	if strings.TrimSpace(o.Language) == "" {
		o.Language = "en"
	}
	if strings.TrimSpace(o.Display) == "" {
		o.Display = o.ID
	}
	return o
}

func (o TrainOptions) check() error {
	if strings.TrimSpace(o.ID) == "" {
		return &TrainingFailure{Causes: []FailureCause{CauseInvalidRequest}, Detail: "model id is required"}
	}
	if strings.TrimSpace(o.NegativesPath) == "" {
		return &TrainingFailure{Causes: []FailureCause{CauseConfiguration},
			Detail: "negatives corpus path is required (build CNEG0002 with tools/build_wakeword_negatives.py); inference and training do not embed a bundled negative set"}
	}
	if strings.TrimSpace(o.InputProcessing) == "" {
		return &TrainingFailure{Causes: []FailureCause{CauseConfiguration},
			Detail: fmt.Sprintf("input processing policy is required; declare the capture tap already applied to the takes (for example %q)", audio.CaptureProcessingTap)}
	}
	if o.InputProcessing != audio.CaptureProcessingTap {
		return &TrainingFailure{Causes: []FailureCause{CauseConfiguration},
			Detail: fmt.Sprintf("unsupported input processing %q; this build trains and runs only %q — convert legacy raw takes before training", o.InputProcessing, audio.CaptureProcessingTap)}
	}
	return nil
}

// audioSet records every count and length problem of one partition, so a
// refusal names all of them at once.
func (f *failureCollector) audioSet(name, partition string, takes [][]int16, minCount, minSamples int, tooFew FailureCause) [][]int16 {
	if len(takes) < minCount {
		f.cause(tooFew, fmt.Sprintf("need at least %d %s recordings, got %d", minCount, name, len(takes)))
	}
	for i, pcm := range takes {
		if len(pcm) < minSamples {
			f.take(FailingTake{Partition: partition, Index: i + 1, Cause: CauseTakeTooShort,
				Detail: fmt.Sprintf("%s recording is %d ms; need at least %d ms",
					name, len(pcm)*1000/enrollmentSampleRate, minSamples*1000/enrollmentSampleRate)})
		}
	}
	return takes
}

// splitFailure reports a partition that cannot be split into train, tune and
// final test: the submitted takes are too few for an honest certificate.
func splitFailure(partition string, err error) error {
	var failure *TrainingFailure
	if errors.As(err, &failure) {
		return err
	}
	return &TrainingFailure{Causes: []FailureCause{CauseInsufficientHeldout},
		Detail: fmt.Sprintf("%s takes: %v", partition, err)}
}

// --- deterministic splitting ------------------------------------------------

type indexSplit struct {
	Train []int
	Tune  []int
	Test  []int
}

// splitByContent partitions recordings by a hash of their PCM. Holdout
// membership depends only on the recording content, never on the optimizer or
// augmentation seed: changing the training seed cannot move a previously
// tested recording into the training set. Returned indices are sorted so
// provenance stays stable.
func splitByContent(takes [][]int16, minTune, minTest int) (indexSplit, error) {
	n := len(takes)
	if n == 0 {
		return indexSplit{}, nil
	}
	testCount, tuneCount, err := splitCounts(n, minTune, minTest)
	if err != nil {
		return indexSplit{}, err
	}
	fingerprints := make([]uint64, n)
	for i := range takes {
		fingerprints[i] = takeFingerprint(takes[i])
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ia, ib := order[a], order[b]
		if fingerprints[ia] != fingerprints[ib] {
			return fingerprints[ia] < fingerprints[ib]
		}
		return ia < ib
	})
	return indexSplit{
		Test:  sortedCopy(order[:testCount]),
		Tune:  sortedCopy(order[testCount : testCount+tuneCount]),
		Train: sortedCopy(order[testCount+tuneCount:]),
	}, nil
}

// splitCounts chooses stable group sizes: 25% tune and 25% final-test, with at
// least minTune/minTest takes in each and the remainder in training. The
// six-take minimum yields two recordings in each role.
func splitCounts(n, minTune, minTest int) (testCount, tuneCount int, err error) {
	if minTune < 0 {
		minTune = 0
	}
	if minTest < 0 {
		minTest = 0
	}
	testCount = n / 4
	if testCount < minTest {
		testCount = minTest
	}
	tuneCount = n / 4
	if tuneCount < minTune {
		tuneCount = minTune
	}
	trainCount := n - testCount - tuneCount
	for trainCount < 1 && tuneCount > minTune {
		tuneCount--
		trainCount++
	}
	for trainCount < 1 && testCount > minTest {
		testCount--
		trainCount++
	}
	if trainCount < 1 {
		return 0, 0, fmt.Errorf("need at least %d recordings to split into train/tune/final-test sets", 1+minTune+minTest)
	}
	return testCount, tuneCount, nil
}

// takeFingerprint hashes the exact PCM content of one recording.
func takeFingerprint(pcm []int16) uint64 {
	h := fnv.New64a()
	var buf [2]byte
	for _, v := range pcm {
		binary.LittleEndian.PutUint16(buf[:], uint16(v))
		_, _ = h.Write(buf[:])
	}
	return h.Sum64()
}

func sortedCopy(v []int) []int {
	out := append([]int(nil), v...)
	sort.Ints(out)
	return out
}

func pickTakes(all [][]int16, idx []int) [][]int16 {
	out := make([][]int16, 0, len(idx))
	for _, i := range idx {
		out = append(out, all[i])
	}
	return out
}

func derivedSeed(seed uint64, a, b int) uint64 {
	x := seed + 0x9e3779b97f4a7c15*uint64(a+1) + 0xbf58476d1ce4e5b9*uint64(b+1)
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

// --- embedding --------------------------------------------------------------

type embeddedSignal struct {
	flat      []float32 // time-major embeddings, immutable during training
	count     int
	samples   int // PCM samples that produced the stream, for exposure accounting
	wordStart int // sample offset of the marked word, or -1 for negatives
	wordEnd   int
}

func (s *embeddedSignal) window(endIdx int) []float32 {
	start := (endIdx - ContextFrames + 1) * oww.EmbeddingDim
	return s.flat[start : (endIdx+1)*oww.EmbeddingDim]
}

// embedTask lazily materialises one signal. Augmented takes are generated on
// demand inside a worker and dropped after embedding, so only feature streams
// survive and resident memory stays bounded.
type embedTask struct {
	materialize func() (pcm []int16, wordStart, wordEnd int, err error)
}

// embedTasks runs the same frontend live detection uses, with a bounded worker
// pool (one frontend per worker). Results keep their input order so training is
// reproducible.
func embedTasks(ctx context.Context, assets *oww.Assets, tasks []embedTask, progress func(done, total int)) ([]embeddedSignal, error) {
	out := make([]embeddedSignal, len(tasks))
	if len(tasks) == 0 {
		return out, nil
	}
	workers := runtime.NumCPU()
	if workers > embedWorkerCap {
		workers = embedWorkerCap
	}
	if workers > len(tasks) {
		workers = len(tasks)
	}
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan int, len(tasks))
	for i := range tasks {
		jobs <- i
	}
	close(jobs)

	var wg sync.WaitGroup
	var done atomic.Int64
	var firstErr error
	var errMu sync.Mutex
	reportErr := func(err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
	}
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			frontend, err := oww.NewFrontend(assets)
			if err != nil {
				reportErr(fmt.Errorf("wakeword training: embedding frontend: %w", err))
				return
			}
			for idx := range jobs {
				if ctx.Err() != nil {
					return
				}
				pcm, wordStart, wordEnd, err := tasks[idx].materialize()
				if err != nil {
					reportErr(fmt.Errorf("wakeword training: signal %d: %w", idx+1, err))
					return
				}
				flat, count, err := embedPCM(frontend, pcm)
				if err != nil {
					reportErr(fmt.Errorf("wakeword training: embedding signal %d: %w", idx+1, err))
					return
				}
				samples := len(pcm)
				pcm = nil // released before the next job; only the embeddings survive
				out[idx] = embeddedSignal{flat: flat, count: count, samples: samples, wordStart: wordStart, wordEnd: wordEnd}
				n := int(done.Add(1))
				if progress != nil {
					progress(n, len(tasks))
				}
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("wakeword training cancelled: %w", err)
	}
	errMu.Lock()
	err := firstErr
	errMu.Unlock()
	if err != nil {
		return nil, err
	}
	return out, nil
}

func embedPCM(f *oww.Frontend, pcm []int16) ([]float32, int, error) {
	f.Reset()
	chunks := f.Feed(pcm)
	flat := make([]float32, 0, len(chunks)*oww.EmbeddingDim)
	for _, v := range chunks {
		if len(v) != oww.EmbeddingDim {
			return nil, 0, fmt.Errorf("frontend returned %d values, want %d", len(v), oww.EmbeddingDim)
		}
		flat = append(flat, v...)
	}
	return flat, len(chunks), nil
}

// positiveWindowRange returns every 16-embedding window whose audio span covers
// the whole word and ends between the word end and 400 ms after it. Labelling
// the whole streak (instead of one frame) teaches the two-consecutive-window
// runtime policy. Windows that start after the word or end before it are never
// included.
func positiveWindowRange(count, wordStart, wordEnd int) (first, last int, err error) {
	if count < ContextFrames {
		return 0, 0, fmt.Errorf("produced %d embeddings, need %d", count, ContextFrames)
	}
	if wordEnd <= wordStart {
		return 0, 0, fmt.Errorf("invalid word bounds %d..%d", wordStart, wordEnd)
	}
	num := wordEnd - oww.FirstEmbeddingSamples
	if num < 0 {
		return 0, 0, fmt.Errorf("word ends before the first embedding (needs %.2f s of audio before it)",
			float64(oww.FirstEmbeddingSamples)/enrollmentSampleRate)
	}
	first = (num + oww.StepSamples - 1) / oww.StepSamples
	if first < ContextFrames-1 {
		first = ContextFrames - 1
	}
	for first < count && oww.EmbeddingEnd(first) < wordEnd {
		first++
	}
	if first >= count {
		return 0, 0, fmt.Errorf("recording ends before a full window after the word; record more post-roll")
	}
	last = first
	for last+1 < count && oww.EmbeddingEnd(last+1) <= wordEnd+maxPostWordStepSamples {
		last++
	}
	// The window's receptive field must start before the marked word. Later
	// windows only start later, so check the first and every one defensively.
	for e := first; e <= last; e++ {
		windowStart := oww.EmbeddingEnd(e-ContextFrames+1) - oww.FirstEmbeddingSamples
		if windowStart > wordStart {
			if e == first {
				return 0, 0, fmt.Errorf("word starts before the classifier window's receptive field; record more pre-roll")
			}
			last = e - 1
			break
		}
	}
	return first, last, nil
}

// --- negative windows -------------------------------------------------------

// appendStreamNegativeWindows adds every full 16-embedding window without
// allocating an intermediate per-stream slice or copying feature values.
func appendStreamNegativeWindows(out []TrainingExample, flat []float32, frames int) []TrainingExample {
	for area := 0; area+ContextFrames <= frames; area++ {
		out = append(out, TrainingExample{
			Features: flat[area*oww.EmbeddingDim : (area+ContextFrames)*oww.EmbeddingDim],
		})
	}
	return out
}

// corpusNegativeWindows returns every full negative window from every training
// stream. The pool is bounded by the decoded corpus and the existing CNEG file
// bound, not by a preselected quota, so no language, speaker, kind or phase is
// silently dropped before sampling.
func corpusNegativeWindows(neg *Negatives) []TrainingExample {
	streams := neg.StreamsForSplit("train")
	total := 0
	for _, st := range streams {
		if st.Frames >= ContextFrames {
			total += st.Frames - ContextFrames + 1
		}
	}
	out := make([]TrainingExample, 0, total)
	for _, st := range streams {
		out = appendStreamNegativeWindows(out, st.Embeddings, st.Frames)
	}
	return out
}

// appendPrefixNegativeWindows adds full context windows whose audio ends by the
// earliest possible keyword start. Every training stream is padded with at
// least minPrecontext of pre-word audio, and augmented streams place their
// preceding speech before that point, so these windows cannot contain the word.
// They teach the ordinary speech, breath and room context that precedes it.
func appendPrefixNegativeWindows(out []TrainingExample, sig *embeddedSignal) []TrainingExample {
	limit := minPrecontext
	if sig.wordStart >= 0 && sig.wordStart < limit {
		limit = sig.wordStart
	}
	for e := ContextFrames - 1; e < sig.count && oww.EmbeddingEnd(e) <= limit; e++ {
		out = append(out, TrainingExample{Features: sig.window(e)})
	}
	return out
}

func requireNegativesSplits(neg *Negatives) error {
	for _, split := range []string{"train", "tune", "test"} {
		if len(neg.StreamsForSplit(split)) == 0 {
			return &TrainingFailure{Causes: []FailureCause{CauseCorpusUnusable},
				Detail: fmt.Sprintf("negatives corpus has no %q streams; rebuild it with independent train/tune/test recordings", split)}
		}
	}
	return nil
}

// --- evaluation and calibration ---------------------------------------------

type evalSignal struct {
	scores    []float64 // one score per window, in time order
	wordStart int       // -1 for a pure negative stream
	wordEnd   int
}

func scoreStream(clf *Classifier, sig *embeddedSignal) ([]float64, error) {
	if sig.count < ContextFrames {
		return nil, nil
	}
	scores := make([]float64, sig.count-ContextFrames+1)
	for i := range scores {
		v := clf.Score(sig.window(i + ContextFrames - 1))
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, &TrainingFailure{Causes: []FailureCause{CauseInternal}, Detail: "classifier produced a non-finite score"}
		}
		scores[i] = v
	}
	return scores, nil
}

func evalFromEmbedded(ctx context.Context, clf *Classifier, sigs []embeddedSignal, skipShort bool) ([]evalSignal, error) {
	out := make([]evalSignal, 0, len(sigs))
	for i := range sigs {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("wakeword training cancelled: %w", err)
		}
		sig := &sigs[i]
		if sig.count < ContextFrames {
			if skipShort {
				continue
			}
			// The caller maps this to a structured refusal with the submitted
			// take indices; this layer only knows the evaluation position.
			return nil, fmt.Errorf("held-out recording %d is too short for a %d-embedding window", i+1, ContextFrames)
		}
		scores, err := scoreStream(clf, sig)
		if err != nil {
			return nil, err
		}
		out = append(out, evalSignal{scores: scores, wordStart: sig.wordStart, wordEnd: sig.wordEnd})
	}
	return out, nil
}

// evalNegativeSignals separates the policy-matching held-out negatives (the
// local room, speech and false-activation takes, already processed by the
// daemon) from the external CNEG streams. Only the former decide calibration
// and the final certificate; CNEG stays an auxiliary off-policy measurement.
func evalNegativeSignals(ctx context.Context, clf *Classifier, neg *Negatives, split string, audio []embeddedSignal) (policy, offPolicy []evalSignal, err error) {
	offPolicy = make([]evalSignal, 0, len(neg.StreamsForSplit(split)))
	for _, st := range neg.StreamsForSplit(split) {
		if err := ctx.Err(); err != nil {
			return nil, nil, fmt.Errorf("wakeword training cancelled: %w", err)
		}
		sig := embeddedSignal{flat: st.Embeddings, count: st.Frames, wordStart: -1, wordEnd: -1}
		scores, err := scoreStream(clf, &sig)
		if err != nil {
			return nil, nil, err
		}
		offPolicy = append(offPolicy, evalSignal{scores: scores, wordStart: -1, wordEnd: -1})
	}
	policy = make([]evalSignal, 0, len(audio))
	for i := range audio {
		if err := ctx.Err(); err != nil {
			return nil, nil, fmt.Errorf("wakeword training cancelled: %w", err)
		}
		sig := &audio[i]
		if sig.count < ContextFrames {
			continue
		}
		scores, err := scoreStream(clf, sig)
		if err != nil {
			return nil, nil, err
		}
		policy = append(policy, evalSignal{scores: scores, wordStart: -1, wordEnd: -1})
	}
	return policy, offPolicy, nil
}

type policyOutcome struct {
	detected         bool
	falseActivations int
}

// evaluatePolicy mirrors the daemon runner: two consecutive windows at or
// above the threshold fire, then a 2.9 s cooldown suppresses further fires
// while the score stays high.
func evaluatePolicy(sig evalSignal, threshold float64) policyOutcome {
	var out policyOutcome
	consecutive := 0
	lastFire := -1
	for i, score := range sig.scores {
		if math.IsNaN(score) || score < threshold {
			consecutive = 0
			continue
		}
		consecutive++
		if consecutive < policyConsecutive {
			continue
		}
		end := oww.EmbeddingEnd(i + ContextFrames - 1)
		if lastFire >= 0 && end-lastFire < policyCooldownSamples {
			continue
		}
		windowStart := end - oww.EmbeddingEnd(ContextFrames-1)
		if sig.wordStart < 0 {
			out.falseActivations++
		} else if end >= sig.wordEnd && windowStart <= sig.wordStart && end <= sig.wordEnd+policyMaxDelaySamples {
			out.detected = true
		} else {
			out.falseActivations++
		}
		lastFire = end
		consecutive = 0
	}
	return out
}

type gateMetrics struct {
	WordRecordings   int
	Detected         int
	FalseActivations int
}

func (m gateMetrics) recall() float64 {
	if m.WordRecordings == 0 {
		return 0
	}
	return float64(m.Detected) / float64(m.WordRecordings)
}

func (m gateMetrics) passes() bool {
	return m.WordRecordings >= 2 && m.Detected*5 >= m.WordRecordings*4 && m.FalseActivations == 0
}

func (m gateMetrics) wordSummary() string {
	return fmt.Sprintf("%d/%d word recordings (%.0f%%)", m.Detected, m.WordRecordings, m.recall()*100)
}

func countScoreWindows(sigs []evalSignal) int {
	total := 0
	for _, sig := range sigs {
		total += len(sig.scores)
	}
	return total
}

func measurePolicy(words, negatives []evalSignal, threshold float64) gateMetrics {
	m := gateMetrics{WordRecordings: len(words)}
	for _, w := range words {
		out := evaluatePolicy(w, threshold)
		if out.detected {
			m.Detected++
		}
		m.FalseActivations += out.falseActivations
	}
	for _, n := range negatives {
		m.FalseActivations += evaluatePolicy(n, threshold).falseActivations
	}
	return m
}

// finalGateError applies the installation gate. The processed-policy
// certificate is the local held-out result; the external CNEG tune and
// final-test results are auxiliary off-policy guards that can still block
// installation if they false fire. Neither is relabelled as the other, and
// neither auxiliary split participates in choosing the threshold. Every failed
// check is reported: a refusal never hides one cause behind another.
func finalGateError(test, auxTune, auxTest gateMetrics, threshold float64) error {
	var failed failureCollector
	failed.threshold = threshold
	if auxTune.FalseActivations > 0 {
		failed.cause(CauseAuxiliaryGateFailed, fmt.Sprintf("auxiliary off-policy corpus guard failed: %d false activations on CNEG tune streams; this is not a processed-audio certificate but blocks installation — inspect the selected takes and retrain",
			auxTune.FalseActivations))
	}
	if auxTest.FalseActivations > 0 {
		failed.cause(CauseAuxiliaryGateFailed, fmt.Sprintf("auxiliary off-policy corpus guard failed: %d false activations on CNEG final-test streams; this is not a processed-audio certificate but blocks installation — inspect the selected takes and retrain",
			auxTest.FalseActivations))
	}
	if !test.passes() {
		failed.cause(CauseFinalGateFailed, fmt.Sprintf("final test failed at threshold %.3f: detected %s with %d false activations on held-out processed takes; need at least 80%% of independent word recordings and zero false activations — add more independent recordings and retrain",
			threshold, test.wordSummary(), test.FalseActivations))
	}
	if !failed.failed() {
		return nil
	}
	failed.split(metricsToSplit(SplitFinalTest, test))
	failed.split(metricsToSplit(SplitAuxTune, auxTune))
	failed.split(metricsToSplit(SplitAuxFinalTest, auxTest))
	return failed.err()
}

// calibration is one tune-split search result. bestAt is the threshold that
// produced the strongest candidate, so a refusal can report the numbers and the
// takes it actually measured instead of an arbitrary point of the grid.
type calibration struct {
	threshold float64
	chosen    gateMetrics
	best      gateMetrics
	bestAt    float64
}

// calibrateThreshold searches the tune split for a feasible operating point.
// The feasible interval's midpoint is used so the choice keeps margin from
// both the recall floor and false activations. The final test never takes part.
// A refusal is a *TrainingFailure naming the tune gate and its best measured
// candidate; calibration itself never chooses a failing threshold.
func calibrateThreshold(words, negatives []evalSignal) (calibration, error) {
	if len(words) == 0 {
		return calibration{}, &TrainingFailure{Causes: []FailureCause{CauseInsufficientHeldout},
			Detail: "no held-out tune word recordings"}
	}
	steps := int(math.Round((thresholdMax - thresholdMin) / thresholdStep))
	var feasible []float64
	best := gateMetrics{}
	bestAt := thresholdMin
	for i := range steps + 1 {
		t := thresholdMin + float64(i)*thresholdStep
		m := measurePolicy(words, negatives, t)
		if m.passes() {
			feasible = append(feasible, t)
		}
		if m.Detected > best.Detected || (m.Detected == best.Detected && m.FalseActivations < best.FalseActivations) {
			best = m
			bestAt = t
		}
	}
	if len(feasible) == 0 {
		var failed failureCollector
		failed.threshold = bestAt
		failed.cause(CauseTuneGateFailed, fmt.Sprintf("no threshold met the quality gate: best candidate detected %s with %d false activations on held-out audio (need a threshold with at least 80%% recall and zero false activations)",
			best.wordSummary(), best.FalseActivations))
		failed.split(metricsToSplit(SplitTune, best))
		return calibration{best: best, bestAt: bestAt, chosen: best}, failed.err()
	}
	target := (feasible[0] + feasible[len(feasible)-1]) / 2
	chosen := feasible[0]
	for _, t := range feasible {
		if math.Abs(t-target) < math.Abs(chosen-target) {
			chosen = t
		}
	}
	return calibration{
		threshold: chosen,
		chosen:    measurePolicy(words, negatives, chosen),
		best:      best,
		bestAt:    bestAt,
	}, nil
}

// --- provenance -------------------------------------------------------------

type trainingProvenance struct {
	opts            TrainOptions
	words           []wordTake
	neg             *Negatives
	wordSplit       indexSplit
	roomSplit       indexSplit
	speechSplit     indexSplit
	falseSplit      indexSplit
	roomTakes       [][]int16
	speechTakes     [][]int16
	falseTakes      [][]int16
	trainingSignals int
	trainingSamples int64
	augmented       int
	positiveWindows int
	corpusWindows   int
	localWindows    int
	localViews      int
	localRepeat     int
	partialWindows  int
	prefixWindows   int
	fitLosses       []float64
	tuneMetrics     gateMetrics
	finalMetrics    gateMetrics
	auxTuneMetrics  gateMetrics
	auxTestMetrics  gateMetrics
	threshold       float64
}

func negativeAudioSeconds(takes [][]int16, indices []int) float64 {
	var samples int64
	for _, i := range indices {
		samples += int64(len(capPCM(takes[i])))
	}
	return float64(samples) / enrollmentSampleRate
}

func provenance(p trainingProvenance) map[string]any {
	trainStreams := p.neg.StreamsForSplit("train")
	tuneStreams := p.neg.StreamsForSplit("tune")
	testStreams := p.neg.StreamsForSplit("test")
	sources := make([]map[string]any, 0, len(p.neg.Sources))
	for _, src := range p.neg.Sources {
		entry := map[string]any{"id": src.ID, "url": src.URL, "license": src.License}
		if src.Attribution != "" {
			entry["attribution"] = src.Attribution
		}
		sources = append(sources, entry)
	}
	return map[string]any{
		"schema":   1,
		"method":   "oww-google-v1 embeddings + on-device classifier",
		"word":     p.opts.ID,
		"language": p.opts.Language,
		"seed":     p.opts.Seed,
		// Input provenance is explicit: a model trained on the raw vendor tap
		// is not interchangeable with one trained on framework.dsp.v1. Legacy
		// raw takes are converted before embedding and counted here; the CNEG
		// corpus is precomputed from its own source audio and is never
		// relabelled as framework-processed.
		"input_audio": map[string]any{
			"processing_tap":             p.opts.InputProcessing,
			"sample_rate":                enrollmentSampleRate,
			"channels":                   1,
			"raw_takes_converted":        p.opts.RawTakesConverted,
			"negative_corpus_processing": "external precomputed embeddings; auxiliary off-policy, not reinterpreted and not a processed-policy certificate",
		},
		"takes": map[string]any{
			"word": map[string]int{
				"total": len(p.words), "train": len(p.wordSplit.Train),
				"tune": len(p.wordSplit.Tune), "final_test": len(p.wordSplit.Test),
			},
			"room": map[string]int{
				"total": len(p.roomTakes), "train": len(p.roomSplit.Train),
				"tune": len(p.roomSplit.Tune), "final_test": len(p.roomSplit.Test),
			},
			"speech": map[string]int{
				"total": len(p.speechTakes), "train": len(p.speechSplit.Train),
				"tune": len(p.speechSplit.Tune), "final_test": len(p.speechSplit.Test),
			},
			"false_activation": map[string]int{
				"total": len(p.falseTakes), "train": len(p.falseSplit.Train),
				"tune": len(p.falseSplit.Tune), "final_test": len(p.falseSplit.Test),
			},
		},
		"augmentation": map[string]any{
			"training_signals":   p.trainingSignals,
			"augmented_variants": p.augmented,
			"positive_windows":   p.positiveWindows,
			"speed_range":        []float64{0.9, 1.1},
			"gain_db":            []float64{-6, 6},
			"room_noise_db":      []float64{roomNoiseMinDB, roomNoiseMaxDB},
			"preceding_speech":   true,
		},
		"negatives": map[string]any{
			"file":                    filepath.Base(p.opts.NegativesPath),
			"feature_extractor":       p.neg.FeatureExtractor,
			"embedding_dim":           p.neg.EmbeddingDim,
			"hop_samples":             p.neg.HopSamples,
			"first_embedding_samples": p.neg.FirstEmbeddingSamples,
			"preroll_samples":         p.neg.PrerollSamples,
			"sources":                 sources,
			"streams":                 len(p.neg.Streams),
			"policy":                  "auxiliary off-policy: precomputed from external corpus audio, not framework-processed; never used as the processed-policy certificate, but its tune/final-test false-activation guards can block installation",
		},
		"negative_sampling": map[string]any{
			"corpus_windows":          p.corpusWindows,
			"local_windows":           p.localWindows,
			"local_repeated_views":    p.localViews,
			"local_repeat_factor":     p.localRepeat,
			"local_preroll_ms":        minPrecontext * 1000 / enrollmentSampleRate,
			"partial_word_windows":    p.partialWindows,
			"pre_word_prefix_windows": p.prefixWindows,
			"training_windows":        p.corpusWindows + p.localViews,
		},
		"policy": map[string]any{
			"hop_ms":                 80,
			"consecutive":            policyConsecutive,
			"cooldown_ms":            2900,
			"max_detection_delay_ms": 1000,
		},
		"fit_loss_curve": p.fitLosses,
		"metrics": map[string]any{
			"threshold":  p.threshold,
			"tune":       metricsToMap(p.tuneMetrics),
			"final_test": metricsToMap(p.finalMetrics),
			// Separate, non-certifying measurements on the external corpus.
			"auxiliary_offpolicy": map[string]any{
				"tune":       metricsToMap(p.auxTuneMetrics),
				"final_test": metricsToMap(p.auxTestMetrics),
			},
		},
		"exposure_seconds": map[string]float64{
			"word_recordings": audioSecondsWordTakes(p.words),
			"training_audio":  float64(p.trainingSamples) / enrollmentSampleRate,
			"negatives_train": p.neg.ExposureSeconds(trainStreams) +
				negativeAudioSeconds(p.roomTakes, p.roomSplit.Train) +
				negativeAudioSeconds(p.speechTakes, p.speechSplit.Train) +
				negativeAudioSeconds(p.falseTakes, p.falseSplit.Train),
			"negatives_tune": p.neg.ExposureSeconds(tuneStreams) +
				negativeAudioSeconds(p.roomTakes, p.roomSplit.Tune) +
				negativeAudioSeconds(p.speechTakes, p.speechSplit.Tune) +
				negativeAudioSeconds(p.falseTakes, p.falseSplit.Tune),
			"negatives_final_test": p.neg.ExposureSeconds(testStreams) +
				negativeAudioSeconds(p.roomTakes, p.roomSplit.Test) +
				negativeAudioSeconds(p.speechTakes, p.speechSplit.Test) +
				negativeAudioSeconds(p.falseTakes, p.falseSplit.Test),
		},
		"certification": "not certified: small enrollment set; the tune/final-test certificate uses only held-out original processed takes, never augmented copies; CNEG results are auxiliary off-policy measurements and do not certify processed-audio quality, but their zero-false-activation tune/final-test guards block installation",
	}
}

func metricsToMap(m gateMetrics) map[string]any {
	return map[string]any{
		"word_recordings":   m.WordRecordings,
		"detected":          m.Detected,
		"recall":            m.recall(),
		"false_activations": m.FalseActivations,
	}
}

func audioSecondsWordTakes(takes []wordTake) float64 {
	var samples int64
	for _, t := range takes {
		samples += int64(len(t.pcm))
	}
	return float64(samples) / enrollmentSampleRate
}

func audioSecondsTakes(takes [][]int16) float64 {
	var samples int64
	for _, pcm := range takes {
		samples += int64(len(pcm))
	}
	return float64(samples) / enrollmentSampleRate
}
