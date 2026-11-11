package wakeword

import (
	"fmt"
	"math"
	"math/rand/v2"
)

// Audio-side helpers for enrollment: finding the spoken word inside a take and
// building labelled synthetic takes from real recordings.
//
// All PCM is 16 kHz mono signed 16-bit. Augmentation never changes the sample
// rate: speed changes are linear resampling, which shifts both formants and
// pitch together exactly like a real speaker talking faster or slower.
const (
	trimFrameSamples   = 320   // 20 ms analysis frame
	trimHopSamples     = 160   // 10 ms analysis hop
	trimPeakFloor      = 120   // RMS below this is treated as silence
	trimBridgeFrames   = 15    // 150 ms pause bridged inside one word
	trimMinRunFrames   = 6     // 60 ms; shorter isolated blips are noise, not speech
	trimAdjacentFrames = 3     // 30 ms; a short onset/tail touching the word belongs to it
	onsetMargin        = 1600  // 100 ms of natural onset kept around the word
	decayMargin        = 2400  // 150 ms of natural decay kept around the word
	minWordSamples     = 2400  // 150 ms
	maxWordSamples     = 25600 // 1.6 s; longer takes are not one wake word
	minPrecontext      = 36800 // 2.3 s of audio before the word
	precontextJitter   = 9600  // up to 0.6 s of extra leading context
	minPostSlack       = 12800 // 0.8 s of audio after the word
	postSlackJitter    = 9600  // up to 0.6 s of extra trailing slack
	roomNoiseMinDB     = -24.0 // background noise level relative to the word
	roomNoiseMaxDB     = -10.0
	speechLevelMinDB   = -6.0
	speechLevelMaxDB   = 3.0
	maxNegativeTakeNS  = 30 * enrollmentSampleRate
)

// trimWord locates the spoken word inside one take using robust frame energy.
// It rejects silent, clipped and implausibly long takes instead of training on
// them: a classifier cannot learn the word from a take that does not contain it.
func trimWord(pcm []int16) (start, end int, err error) {
	if len(pcm) < minWordSamples {
		return 0, 0, fmt.Errorf("recording is %d ms, shorter than the %d ms minimum",
			len(pcm)*1000/enrollmentSampleRate, minWordSamples*1000/enrollmentSampleRate)
	}
	clipped := 0
	for _, v := range pcm {
		a := int(v)
		if a < 0 {
			a = -a
		}
		if a >= 32000 {
			clipped++
		}
	}
	if clipped*100 > len(pcm) {
		return 0, 0, fmt.Errorf("recording clips on %.1f%% of samples; lower the capture gain",
			float64(clipped)*100/float64(len(pcm)))
	}
	energies := frameRMS(pcm, trimFrameSamples, trimHopSamples)
	if len(energies) == 0 {
		return 0, 0, fmt.Errorf("recording is empty")
	}
	peak := 0.0
	for _, e := range energies {
		if e > peak {
			peak = e
		}
	}
	if peak < trimPeakFloor {
		return 0, 0, fmt.Errorf("recording is silent or too quiet (peak frame RMS %.0f); record closer to the microphone", peak)
	}
	sorted := append([]float64(nil), energies...)
	sortFloat64s(sorted)
	floor := sorted[len(sorted)/5]
	threshold := floor + (peak-floor)*0.12
	if threshold < trimPeakFloor {
		threshold = trimPeakFloor
	}
	// Group above-threshold frames into speech runs. A wake word may contain a
	// short internal pause, so runs closer than trimBridgeFrames are merged.
	// Short runs touching a sustained span within trimAdjacentFrames are its
	// onset or offset; detached short runs are clicks, rustles or room noises
	// and stay out of the labelled word.
	type speechGroup struct {
		start, end int
		peak       float64
	}
	var groups []speechGroup
	var pending speechGroup
	hasPending := false
	runStart := -1
	runPeak := 0.0
	flushRun := func(after int) {
		if runStart < 0 {
			return
		}
		start, peak := runStart, runPeak
		length := after - start
		runStart = -1
		runPeak = 0

		if length >= trimMinRunFrames {
			// A short run touching the word is its soft onset, exactly like the
			// adjacent short tail below. It must sit within trimAdjacentFrames
			// of the sustained run; a click across a real pause is dropped.
			if hasPending {
				if gap := start - pending.end - 1; gap <= trimAdjacentFrames {
					start = pending.start
					if pending.peak > peak {
						peak = pending.peak
					}
				}
				hasPending = false
			}
			if len(groups) > 0 {
				g := &groups[len(groups)-1]
				gap := start - g.end - 1
				if gap <= trimBridgeFrames {
					g.end = after - 1
					if peak > g.peak {
						g.peak = peak
					}
					return
				}
			}
			groups = append(groups, speechGroup{start: start, end: after - 1, peak: peak})
			return
		}

		if len(groups) > 0 {
			g := &groups[len(groups)-1]
			if gap := start - g.end - 1; gap <= trimAdjacentFrames {
				g.end = after - 1
				if peak > g.peak {
					g.peak = peak
				}
				return
			}
		}
		// Buffer the latest short blob as a possible onset for the next
		// sustained run. Adjacent short runs merge; a detached one replaces the
		// older click instead of accumulating noise.
		if hasPending {
			if gap := start - pending.end - 1; gap <= trimAdjacentFrames {
				pending.end = after - 1
				if peak > pending.peak {
					pending.peak = peak
				}
				return
			}
		}
		pending = speechGroup{start: start, end: after - 1, peak: peak}
		hasPending = true
	}
	for i, e := range energies {
		if e >= threshold {
			if runStart < 0 {
				runStart = i
				runPeak = e
			} else if e > runPeak {
				runPeak = e
			}
			continue
		}
		if runStart >= 0 {
			flushRun(i)
		}
	}
	if runStart >= 0 {
		flushRun(len(energies))
	}
	if len(groups) == 0 {
		return 0, 0, fmt.Errorf("no sustained speech found above the noise floor; record closer to the microphone")
	}
	// The wake word is the loudest sustained span in the take. Selecting it
	// instead of the outermost blip keeps the labelled bounds on actual speech.
	best := 0
	for i := range groups {
		if groups[i].peak > groups[best].peak {
			best = i
		}
	}
	first, last := groups[best].start, groups[best].end
	start = first * trimHopSamples
	end = last*trimHopSamples + trimFrameSamples
	if end > len(pcm) {
		end = len(pcm)
	}
	if end-start < minWordSamples {
		return 0, 0, fmt.Errorf("word is only %d ms; record the full word", (end-start)*1000/enrollmentSampleRate)
	}
	if end-start > maxWordSamples {
		return 0, 0, fmt.Errorf("speech spans %d ms; record just the wake word (max %d ms)",
			(end-start)*1000/enrollmentSampleRate, maxWordSamples*1000/enrollmentSampleRate)
	}
	if last >= len(energies)-1 {
		return 0, 0, fmt.Errorf("speech runs to the end of the recording; leave a short pause after the word")
	}
	return start, end, nil
}

// frameRMS returns the RMS of overlapping analysis frames.
func frameRMS(pcm []int16, frame, hop int) []float64 {
	if len(pcm) < frame {
		return nil
	}
	out := make([]float64, 0, (len(pcm)-frame)/hop+1)
	for off := 0; off+frame <= len(pcm); off += hop {
		var sum float64
		for _, v := range pcm[off : off+frame] {
			sum += float64(v) * float64(v)
		}
		out = append(out, math.Sqrt(sum/float64(frame)))
	}
	return out
}

func sortFloat64s(v []float64) {
	// Small slices only; insertion sort keeps the package dependency-free and
	// avoids an allocation-heavy sort interface.
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

func rmsInt16(pcm []int16) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var sum float64
	for _, v := range pcm {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(pcm)))
}

func clamp16(v float64) int16 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(math.Round(v))
}

func dbToAmp(db float64) float64 { return math.Pow(10, db/20) }

// resampleLinear changes playback speed by ratio (>1 = faster/shorter) while
// keeping the sample rate.
func resampleLinear(src []int16, ratio float64) []int16 {
	if len(src) == 0 || ratio == 1 {
		return append([]int16(nil), src...)
	}
	n := int(math.Round(float64(len(src)) / ratio))
	if n < 1 {
		n = 1
	}
	out := make([]int16, n)
	last := len(src) - 1
	for i := range out {
		pos := float64(i) * ratio
		j := int(pos)
		if j >= last {
			out[i] = src[last]
			continue
		}
		frac := pos - float64(j)
		out[i] = clamp16(float64(src[j])*(1-frac) + float64(src[j+1])*frac)
	}
	return out
}

// augmentParams is one deterministic augmentation draw.
type augmentParams struct {
	speed    float64
	gainDB   float64
	roomDB   float64
	withTalk bool
}

func sampleAugmentParams(rng *rand.Rand) augmentParams {
	return augmentParams{
		speed:    0.9 + rng.Float64()*0.2,
		gainDB:   -6 + rng.Float64()*12,
		roomDB:   roomNoiseMinDB + rng.Float64()*(roomNoiseMaxDB-roomNoiseMinDB),
		withTalk: rng.Float64() < 0.6,
	}
}

// augmentWordTake builds one synthetic positive take. The word is resampled,
// gained, placed after at least minPrecontext of audio, optionally preceded by
// unrelated speech, and mixed with an independent room-noise recording from the
// training split. The returned bounds mark the word in the synthetic stream.
func augmentWordTake(rng *rand.Rand, take []int16, wordStart, wordEnd int, rooms, speech [][]int16) ([]int16, int, int, error) {
	if wordStart < 0 || wordEnd > len(take) || wordEnd <= wordStart {
		return nil, 0, 0, fmt.Errorf("invalid word bounds %d..%d", wordStart, wordEnd)
	}
	params := sampleAugmentParams(rng)
	segFrom := wordStart - onsetMargin
	if segFrom < 0 {
		segFrom = 0
	}
	segTo := wordEnd + decayMargin
	if segTo > len(take) {
		segTo = len(take)
	}
	seg := resampleLinear(take[segFrom:segTo], params.speed)
	wordOutStart := int(math.Round(float64(wordStart-segFrom) / params.speed))
	wordOutEnd := int(math.Round(float64(wordEnd-segFrom) / params.speed))
	if wordOutStart < 0 {
		wordOutStart = 0
	}
	if wordOutEnd > len(seg) {
		wordOutEnd = len(seg)
	}
	if wordOutEnd <= wordOutStart {
		return nil, 0, 0, fmt.Errorf("augmentation collapse: word bounds %d..%d", wordOutStart, wordOutEnd)
	}

	preLen := minPrecontext + rng.IntN(precontextJitter+1)
	postLen := minPostSlack + rng.IntN(postSlackJitter+1)
	total := preLen + len(seg) + postLen
	mix := make([]float64, total)

	wordGain := dbToAmp(params.gainDB)
	for i, v := range seg {
		mix[preLen+i] = float64(v) * wordGain
	}
	wordRMS := rmsInt16(seg) * wordGain

	if params.withTalk && len(speech) > 0 {
		sp := speech[rng.IntN(len(speech))]
		if len(sp) >= enrollmentSampleRate/4 {
			maxLen := 14400 // 0.9 s
			minLen := 6400  // 0.4 s
			if len(sp) < minLen {
				minLen = len(sp)
			}
			if maxLen > len(sp) {
				maxLen = len(sp)
			}
			spLen := minLen + rng.IntN(maxLen-minLen+1)
			spStart := 0
			if len(sp) > spLen {
				spStart = rng.IntN(len(sp) - spLen + 1)
			}
			spSeg := sp[spStart : spStart+spLen]
			spRMS := rmsInt16(spSeg)
			if spRMS > 0 && wordRMS > 0 {
				level := wordRMS * dbToAmp(speechLevelMinDB+rng.Float64()*(speechLevelMaxDB-speechLevelMinDB)) / spRMS
				gap := 1600 + rng.IntN(4000)
				off := preLen - gap - spLen
				if off < 1600 {
					off = 1600
				}
				for i, v := range spSeg {
					if off+i < len(mix) {
						mix[off+i] += float64(v) * level
					}
				}
			}
		}
	}

	if len(rooms) > 0 && wordRMS > 0 {
		noise := roomNoise(rng, rooms, total)
		noiseRMS := rmsInt16(noise)
		if noiseRMS > 0 {
			level := wordRMS * dbToAmp(params.roomDB) / noiseRMS
			for i, v := range noise {
				mix[i] += float64(v) * level
			}
		}
	}

	out := make([]int16, total)
	for i, v := range mix {
		out[i] = clamp16(v)
	}
	return out, preLen + wordOutStart, preLen + wordOutEnd, nil
}

// roomNoise tiles a room recording over n samples starting at a random offset.
// Recorded rooms are shorter than an augmented take, so the loop is intentional;
// each augmented example still uses only its own split's recordings.
func roomNoise(rng *rand.Rand, rooms [][]int16, n int) []int16 {
	var candidates [][]int16
	for _, r := range rooms {
		if len(r) > 0 {
			candidates = append(candidates, r)
		}
	}
	if len(candidates) == 0 {
		return make([]int16, n)
	}
	room := candidates[rng.IntN(len(candidates))]
	out := make([]int16, n)
	off := 0
	if len(room) > 1 {
		off = rng.IntN(len(room))
	}
	for i := range out {
		out[i] = room[(off+i)%len(room)]
	}
	return out
}

// padOriginal builds the neutral (un-augmented) stream used for training
// originals and for held-out evaluation. It places the real recording after a
// fixed 2.3 s lead-in so the 16-embedding window has its full receptive field.
// Both padding regions use the take's captured background instead of an
// artificial constant mel floor. Held-out metrics still count real takes,
// not augmented copies.
func padOriginal(take []int16, wordStart, wordEnd int) ([]int16, int, int) {
	segFrom := wordStart - onsetMargin
	if segFrom < 0 {
		segFrom = 0
	}
	segTo := wordEnd + decayMargin
	if segTo > len(take) {
		segTo = len(take)
	}
	segLen := segTo - segFrom
	out := make([]int16, minPrecontext+segLen+minPostSlack)
	fillBackground(out[:minPrecontext], take[:segFrom], take[segTo:], true)
	copy(out[minPrecontext:], take[segFrom:segTo])
	fillBackground(out[minPrecontext+segLen:], take[segTo:], take[:segFrom], false)
	return out, minPrecontext + (wordStart - segFrom), minPrecontext + (wordEnd - segFrom)
}

// fillBackground tiles captured non-word audio over dst. Lead-in padding is
// aligned to end at its original boundary; trailing padding starts at its
// original boundary. If the preferred side is unavailable, use the other.
// Takes with no usable background keep silence.
func fillBackground(dst []int16, preferred, fallback []int16, alignEnd bool) {
	src := preferred
	if len(src) < trimFrameSamples {
		src = fallback
	}
	if len(src) < trimFrameSamples {
		return
	}
	j := 0
	if alignEnd {
		j = (len(src) - len(dst)%len(src)) % len(src)
	}
	for i := range dst {
		dst[i] = src[j]
		j++
		if j == len(src) {
			j = 0
		}
	}
}

// warmNegative prefixes a local negative recording with real room audio from
// the same split, matching the streaming frontend's warm context. A recording
// embedded from sample zero leaves roughly its first two seconds before the
// first full 16-embedding window, so those seconds never reach the classifier
// even though the live engine is already warm by then. Only room recordings are
// repeated; the speech or false-activation clip appears exactly once, unchanged.
func warmNegative(pcm []int16, rooms [][]int16) []int16 {
	// Enrollment validation guarantees a nonempty room in every partition.
	out := make([]int16, minPrecontext+len(pcm))
	fillBackground(out[:minPrecontext], rooms[0], nil, false)
	copy(out[minPrecontext:], pcm)
	return out
}

// capPCM bounds a negative recording so one long file cannot exhaust device
// memory during training.
func capPCM(pcm []int16) []int16 {
	if len(pcm) > maxNegativeTakeNS {
		return pcm[:maxNegativeTakeNS]
	}
	return pcm
}
