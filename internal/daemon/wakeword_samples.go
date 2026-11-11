package daemon

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"capsulad/internal/wakeword"
)

const (
	sampleRate      = 16000
	defaultRecordMS = 1600
)

// sample is one recorded training take on disk.
type sample struct {
	ID         string
	Label      string
	Path       string
	DurationMS uint32
	RecordedMS int64
}

func (d *Daemon) sampleDir() string {
	return filepath.Join(d.cfg.DataDir, "wakeword", "samples")
}

// recordingLabel maps a request label to the class stored on disk and whether
// the take contains speech, which needs the audible start cue. Room tone and
// generic "background" takes stay silent; phrases and the wakeword are spoken.
func recordingLabel(raw string) (stored string, speech bool, err error) {
	switch strings.TrimSpace(raw) {
	case "", "word":
		return "word", true, nil
	case "speech", "phrase":
		return "background", true, nil
	case "room", "background":
		return "background", false, nil
	default:
		return "", false, fmt.Errorf("label must be %q, %q or %q", "word", "speech", "room")
	}
}

// RecordSample captures one take from the framework audio tap. The microphone
// stream is shared with application captures; no second device handle is opened.
func (d *Daemon) RecordSample(ctx context.Context, label string, durationMS uint32) (sample, error) {
	storedLabel, speech, err := recordingLabel(label)
	if err != nil {
		return sample{}, err
	}
	if durationMS == 0 {
		durationMS = defaultRecordMS
	}
	if durationMS < 400 || durationMS > 10000 {
		return sample{}, fmt.Errorf("duration %d ms out of range 400-10000", durationMS)
	}
	if d.audio.Muted() {
		return sample{}, fmt.Errorf("microphone is muted")
	}
	if d.assistantAudioActive() {
		return sample{}, fmt.Errorf("assistant is using audio; wait until it finishes before recording")
	}
	d.wkSampleMu.Lock()
	if d.recording {
		d.wkSampleMu.Unlock()
		return sample{}, fmt.Errorf("a recording is already in progress")
	}
	d.recording = true
	d.wkSampleMu.Unlock()
	defer func() {
		d.wkSampleMu.Lock()
		d.recording = false
		d.wkSampleMu.Unlock()
	}()

	d.beginRecordingIndicator()
	defer d.endRecordingIndicator()
	if speech {
		// The tones must finish before capture starts so they never end up in
		// the take. The browser shows this as a short "listen" phase.
		if err := d.playRecordingCue(ctx); err != nil {
			return sample{}, err
		}
	}

	frames, cancel := d.audio.SubscribeFrames(64)
	defer cancel()
	want := int(durationMS) * sampleRate / 1000
	pcm := make([]int16, 0, want)
	deadline := time.NewTimer(time.Duration(durationMS+500) * time.Millisecond)
	defer deadline.Stop()
	var previousSequence uint64
	for len(pcm) < want {
		select {
		case <-ctx.Done():
			return sample{}, ctx.Err()
		case <-d.ctx.Done():
			return sample{}, fmt.Errorf("daemon stopping")
		case <-deadline.C:
			return sample{}, fmt.Errorf("timed out waiting for %d ms of audio", durationMS)
		case f, ok := <-frames:
			if !ok {
				return sample{}, fmt.Errorf("audio capture stopped")
			}
			if d.audio.Muted() {
				return sample{}, fmt.Errorf("microphone muted during recording")
			}
			if f.RateHz != sampleRate || len(f.PCM)%2 != 0 {
				return sample{}, fmt.Errorf("expected %d Hz mono S16LE audio", sampleRate)
			}
			if f.Discontinuity || (previousSequence != 0 && f.Sequence != previousSequence+1) {
				return sample{}, fmt.Errorf("audio capture gap; record the take again")
			}
			previousSequence = f.Sequence
			for i := 0; i+1 < len(f.PCM); i += 2 {
				pcm = append(pcm, int16(binary.LittleEndian.Uint16(f.PCM[i:])))
			}
		}
	}
	pcm = pcm[:want]
	id := fmt.Sprintf("wk_%d_%s", time.Now().UnixMilli(), storedLabel)
	dir := d.sampleDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return sample{}, err
	}
	path := filepath.Join(dir, id+".wav")
	if err := writeWAV(path, pcm); err != nil {
		return sample{}, err
	}
	return sample{ID: id, Label: storedLabel, Path: path, DurationMS: durationMS, RecordedMS: time.Now().UnixMilli()}, nil
}

// writeWAV stores 16 kHz mono 16-bit PCM with a canonical 44-byte header.
func writeWAV(path string, pcm []int16) error {
	data := make([]byte, len(pcm)*2)
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(data[i*2:], uint16(v))
	}
	var header [44]byte
	copy(header[0:], "RIFF")
	binary.LittleEndian.PutUint32(header[4:], uint32(36+len(data)))
	copy(header[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(header[16:], 16)
	binary.LittleEndian.PutUint16(header[20:], 1) // PCM
	binary.LittleEndian.PutUint16(header[22:], 1) // mono
	binary.LittleEndian.PutUint32(header[24:], sampleRate)
	binary.LittleEndian.PutUint32(header[28:], sampleRate*2)
	binary.LittleEndian.PutUint16(header[32:], 2)
	binary.LittleEndian.PutUint16(header[34:], 16)
	copy(header[36:], "data")
	binary.LittleEndian.PutUint32(header[40:], uint32(len(data)))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(header[:], data...), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readWAV accepts the canonical header written above.
func readWAV(path string) ([]int16, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < 44 || string(raw[0:4]) != "RIFF" || string(raw[8:16]) != "WAVEfmt " {
		return nil, fmt.Errorf("not a WAV file: %s", path)
	}
	rate := binary.LittleEndian.Uint32(raw[24:])
	channels := binary.LittleEndian.Uint16(raw[22:])
	if rate != sampleRate || channels != 1 {
		return nil, fmt.Errorf("expected %d Hz mono, got %d Hz / %d ch", sampleRate, rate, channels)
	}
	data := raw[44:]
	pcm := make([]int16, len(data)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(data[i*2:]))
	}
	return pcm, nil
}

func (d *Daemon) listSamples() ([]sample, error) {
	entries, err := os.ReadDir(d.sampleDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []sample
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".wav") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".wav")
		label := "word"
		if strings.HasSuffix(id, "_background") {
			label = "background"
		}
		path := filepath.Join(d.sampleDir(), e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		pcm, err := readWAV(path)
		dur := uint32(0)
		if err == nil {
			dur = uint32(len(pcm) * 1000 / sampleRate)
		}
		out = append(out, sample{ID: id, Label: label, Path: path, DurationMS: dur, RecordedMS: info.ModTime().UnixMilli()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RecordedMS > out[j].RecordedMS })
	return out, nil
}

func (d *Daemon) deleteSample(id string) error {
	if strings.ContainsAny(id, `/\`) || id == "" {
		return fmt.Errorf("invalid sample id")
	}
	return os.Remove(filepath.Join(d.sampleDir(), id+".wav"))
}

// trainSelection is one on-device training request. The lists are the
// explicitly selected takes, never "all recordings": a setup must not pick up
// an older word or background take that the user did not choose.
type trainSelection struct {
	Display  string
	Language string
	Seed     uint64
	Word     []string
	Room     []string
	Speech   []string
	False    []string
}

// badSelectionError marks a request the caller can fix by recording or
// selecting different takes. The RPC reports it as InvalidArgument.
type badSelectionError struct{ err error }

func (e badSelectionError) Error() string { return e.err.Error() }
func (e badSelectionError) Unwrap() error { return e.err }

func badSelection(format string, args ...any) error {
	return badSelectionError{fmt.Errorf(format, args...)}
}

// minimumTakes are the honest prerequisites shown in the browser and enforced
// again here so direct API callers cannot train an unsupported model.
const (
	minWordTakes   = 6
	minRoomTakes   = 3
	minSpeechTakes = 3
	// maxTakesTotal bounds decoded PCM: the Mini has 234 MiB and the
	// authenticated UI selects fifteen takes, so a much larger set is a caller
	// mistake, not a legitimate job.
	maxTakesTotal = 64
)

// loadTake reads one recorded WAV by id. Selection is validated before any file
// is opened, so a missing or wrong-kind id is reported once, clearly.
func (d *Daemon) loadTake(id string) ([]int16, error) {
	if strings.ContainsAny(id, `/\`) || id == "" || strings.Contains(id, "..") {
		return nil, badSelection("invalid sample id %q", id)
	}
	pcm, err := readWAV(filepath.Join(d.sampleDir(), id+".wav"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, badSelection("sample %q no longer exists; reload the page and select the takes again", id)
		}
		return nil, fmt.Errorf("sample %s: %w", id, err)
	}
	return pcm, nil
}

// loadSelection validates and decodes exactly the selected takes into the
// enrollment input. Kinds come from the on-disk label, not from the caller.
func (d *Daemon) loadSelection(sel trainSelection) (wakeword.Enrollment, error) {
	sel.Display = strings.TrimSpace(sel.Display)
	if sel.Display == "" {
		return wakeword.Enrollment{}, badSelection("choose the wake word before training")
	}
	if len([]rune(sel.Display)) > 100 {
		return wakeword.Enrollment{}, badSelection("wake word is longer than 100 characters")
	}
	if len(sel.Word) < minWordTakes {
		return wakeword.Enrollment{}, badSelection("record at least %d wakeword takes; this setup has %d", minWordTakes, len(sel.Word))
	}
	if len(sel.Room) < minRoomTakes {
		return wakeword.Enrollment{}, badSelection("record at least %d room-noise takes; this setup has %d", minRoomTakes, len(sel.Room))
	}
	if len(sel.Speech) < minSpeechTakes {
		return wakeword.Enrollment{}, badSelection("record at least %d unrelated-phrase takes; this setup has %d", minSpeechTakes, len(sel.Speech))
	}
	if total := len(sel.Word) + len(sel.Room) + len(sel.Speech) + len(sel.False); total > maxTakesTotal {
		return wakeword.Enrollment{}, badSelection("select at most %d takes for one training run (got %d)", maxTakesTotal, total)
	}
	samples, err := d.listSamples()
	if err != nil {
		return wakeword.Enrollment{}, err
	}
	labels := make(map[string]string, len(samples))
	for _, sam := range samples {
		labels[sam.ID] = sam.Label
	}
	seen := make(map[string]string, len(sel.Word)+len(sel.Room)+len(sel.Speech)+len(sel.False))
	check := func(role, want string, ids []string) error {
		for _, id := range ids {
			if strings.TrimSpace(id) == "" {
				return badSelection("empty sample id in the %s selection", role)
			}
			if prev, dup := seen[id]; dup {
				return badSelection("sample %q is selected twice (%s and %s)", id, prev, role)
			}
			seen[id] = role
			label, ok := labels[id]
			if !ok {
				return badSelection("sample %q is not on this device; reload the page and select the takes again", id)
			}
			if label != want {
				return badSelection("sample %q is a %s take, but was selected as %s", id, label, role)
			}
		}
		return nil
	}
	if err := check("wakeword", "word", sel.Word); err != nil {
		return wakeword.Enrollment{}, err
	}
	if err := check("room-noise", "background", sel.Room); err != nil {
		return wakeword.Enrollment{}, err
	}
	if err := check("unrelated-phrase", "background", sel.Speech); err != nil {
		return wakeword.Enrollment{}, err
	}
	if err := check("false-activation", "background", sel.False); err != nil {
		return wakeword.Enrollment{}, err
	}
	load := func(ids []string) ([][]int16, error) {
		out := make([][]int16, 0, len(ids))
		for _, id := range ids {
			pcm, err := d.loadTake(id)
			if err != nil {
				return nil, err
			}
			out = append(out, pcm)
		}
		if len(out) == 0 {
			out = nil
		}
		return out, nil
	}
	var en wakeword.Enrollment
	if en.Takes, err = load(sel.Word); err != nil {
		return wakeword.Enrollment{}, err
	}
	if en.Ambient, err = load(sel.Room); err != nil {
		return wakeword.Enrollment{}, err
	}
	if en.Speech, err = load(sel.Speech); err != nil {
		return wakeword.Enrollment{}, err
	}
	if en.False, err = load(sel.False); err != nil {
		return wakeword.Enrollment{}, err
	}
	return en, nil
}

// InstallClassifier validates a fully trained classifier and atomically
// replaces the installed model. Callers must only pass a classifier that came
// out of a successful Train, whose quality gates ran on held-out recordings.
func (d *Daemon) InstallClassifier(clf *wakeword.Classifier) error {
	if clf == nil {
		return fmt.Errorf("no classifier to install")
	}
	if err := clf.Validate(); err != nil {
		return err
	}
	// Building the engine here proves the artifact runs on this device before
	// the old model file is touched.
	if _, err := wakeword.NewEngine(clf); err != nil {
		return fmt.Errorf("classifier is not runnable on this device: %w", err)
	}
	raw, err := json.Marshal(clf)
	if err != nil {
		return fmt.Errorf("encode classifier: %w", err)
	}
	if err := d.writeClassifier(raw); err != nil {
		return err
	}
	d.restartWakewordEngine()
	return nil
}

// writeClassifier atomically replaces the model file. The previous model stays
// readable until the fully written temporary file is renamed over it, so a
// failed write or a crash mid-write cannot leave a half model installed.
func (d *Daemon) writeClassifier(raw []byte) error {
	path := d.classifierPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return nil
}
