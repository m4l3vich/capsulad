package input

import (
	"time"

	"capsulad/internal/backend"
	"capsulad/internal/ids"
	"capsulad/internal/routing"
)

// The Mini panel exposes five discrete gpio keys and no raw coordinates. A
// swipe or circle is therefore inferred from ordered press transitions, never
// from invented positions. Recognition is deliberately conservative: one
// gesture must complete inside a small inter-contact and total budget, and the
// per-key side effects of its constituent presses stay deferred until the
// sequence either becomes a gesture or fails.
const (
	panelInterContact = 250 * time.Millisecond
	panelGestureTotal = 1500 * time.Millisecond
)

// Perimeter indices, clockwise from north.
var panelDirection = map[string]int{
	"mic_toggle":  0, // north
	"volume_up":   1, // east
	"play_pause":  2, // south
	"volume_down": 3, // west
}

var panelSwipe = map[int]string{
	0: "swipe_up",
	1: "swipe_right",
	2: "swipe_down",
	3: "swipe_left",
}

// fullSwipe is opposite -> centre -> direction, matching a finger crossing the
// panel. Centre-first half-swipes are accepted separately when unambiguous.
var fullSwipe = []struct {
	opposite, direction, id string
}{
	{"volume_down", "volume_up", "swipe_right"}, // W, C, E
	{"volume_up", "volume_down", "swipe_left"},  // E, C, W
	{"play_pause", "mic_toggle", "swipe_up"},    // S, C, N
	{"mic_toggle", "play_pause", "swipe_down"},  // N, C, S
}

// oppositePair is the two-contact form of the same gesture: a crossing can
// report both opposite edges without the unmarked centre. It is the minimum
// contact evidence a swipe can leave; a crossing that reports fewer than two
// contacts is indistinguishable from a single key press and is not treated as
// a gesture. Without this form an emitted edge pair fails as two unrelated
// volume taps and leaks one volume step per edge, which is worse than the lost
// gesture.
var oppositePair = map[string]string{
	"volume_down|volume_up": "swipe_right", // W then E
	"volume_up|volume_down": "swipe_left",  // E then W
	"play_pause|mic_toggle": "swipe_up",    // S then N
	"mic_toggle|play_pause": "swipe_down",  // N then S
}

type panelContact struct {
	key       string
	pressNS   uint64
	releaseNS uint64
	released  bool
	// overlapsPrevious records that this contact began while an earlier contact
	// in the same sequence was still down. That is the physical signature of one
	// finger crossing the panel; contacts separated by a release are deliberate
	// taps, even when they arrive inside the inter-contact window.
	overlapsPrevious bool
}

type panelSequence struct {
	mode      routing.TouchMode
	captureID string
	appID     string
	startNS   uint64
	lastPress uint64
	contacts  []*panelContact
	timer     *time.Timer
}

func eventNS(clock *ids.Clock, ns uint64) uint64 {
	if ns != 0 {
		return ns
	}
	return clock.NowNS()
}

// remainingHold keeps a hold threshold measured from the physical press even
// though its effect is applied after the gesture-recognition window.
func remainingHold(clock *ids.Clock, startNS uint64, hold time.Duration) time.Duration {
	now := clock.NowNS()
	if now >= startNS {
		remaining := hold - time.Duration(now-startNS)
		if remaining > 0 {
			return remaining
		}
		return 0
	}
	return hold
}

func isPanelKey(key string) bool {
	switch key {
	case "volume_up", "volume_down", "mic_toggle", "play_pause", "center":
		return true
	}
	return false
}

func appOwnedPanelKey(key string) bool { return key == "play_pause" || key == "center" }

// handlePanelKey assumes the caller holds panelExecMu. Ownership was snapshot
// under that lock, so panel bookkeeping cannot race a capture transition.
func (m *Manager) handlePanelKey(e backend.InputEvent, captureID, appID string, mode routing.TouchMode) {
	ts := eventNS(m.clock, e.MonotonicNS)
	if e.Key.Pressed {
		m.panelPress(e.Key.Key, ts, captureID, appID, mode)
		return
	}
	m.panelRelease(e.Key.Key, ts, captureID, appID, mode)
}

func (m *Manager) panelPress(key string, ts uint64, captureID, appID string, mode routing.TouchMode) {
	m.panelMu.Lock()
	if m.panelSwallow[key] {
		m.panelMu.Unlock()
		m.logs.Debugf("input", "", "panel key %s press ignored: its release was consumed by a gesture", key)
		return
	}
	if m.panelDown[key] {
		// Backend repeat or a held key re-reported during the same contact.
		m.panelMu.Unlock()
		m.logs.Debugf("input", "", "duplicate panel press %s ignored", key)
		return
	}
	m.panelDown[key] = true

	var ops []func()
	if seq := m.panelSeq; seq != nil {
		gap := uint64(0)
		if ts > seq.lastPress {
			gap = ts - seq.lastPress
		}
		total := uint64(0)
		if ts > seq.startNS {
			total = ts - seq.startNS
		}
		if gap > uint64(panelInterContact) || total > uint64(panelGestureTotal) {
			ops = append(ops, m.failPanelLocked(seq, false)...)
		}
	}
	if m.panelSeq == nil {
		m.panelSeq = &panelSequence{
			mode: mode, captureID: captureID, appID: appID,
			startNS: ts, lastPress: ts,
		}
	}
	seq := m.panelSeq
	seq.lastPress = ts
	overlap := false
	for _, c := range seq.contacts {
		if !c.released {
			overlap = true
			break
		}
	}
	seq.contacts = append(seq.contacts, &panelContact{key: key, pressNS: ts, overlapsPrevious: overlap})

	id, complete, viable := matchPanelGesture(panelKeys(seq.contacts), panelOverlaps(seq.contacts))
	switch {
	case complete && mode != routing.TouchFull && len(seq.contacts) == 2 && !panelSystemGesture(id):
		// A two-contact vertical pair has no framework action. Outside a
		// capture, consuming it would silently swallow a circle and a mute
		// press; resolve both controls deliberately instead.
		ops = append(ops, m.resolvePanelContactsLocked(seq)...)
	case complete:
		ops = append(ops, m.consumeGestureLocked(seq, id)...)
	case viable:
		m.armPanelTimerLocked(seq)
	default:
		ops = append(ops, m.failPanelLocked(seq, true)...)
	}
	m.panelMu.Unlock()
	runPanelOps(ops)
}

func (m *Manager) panelRelease(key string, ts uint64, captureID, appID string, mode routing.TouchMode) {
	m.panelMu.Lock()
	wasDown := m.panelDown[key]
	m.panelDown[key] = false
	if m.panelSwallow[key] {
		delete(m.panelSwallow, key)
		m.panelMu.Unlock()
		m.logs.Debugf("input", "", "panel key %s release consumed by a gesture", key)
		return
	}
	if seq := m.panelSeq; seq != nil && wasDown {
		matched := false
		for i := len(seq.contacts) - 1; i >= 0; i-- {
			if c := seq.contacts[i]; c.key == key && !c.released {
				c.released = true
				c.releaseNS = ts
				matched = true
				break
			}
		}
		if matched {
			m.panelMu.Unlock()
			// A release does not finish recognition: the next contact may
			// still follow inside the inter-contact window.
			return
		}
	}
	forward := m.panelForward[key]
	delete(m.panelForward, key)
	handled := m.panelHandled[key]
	delete(m.panelHandled, key)
	m.panelMu.Unlock()

	if forward != "" {
		m.applyAppPanelKey(key, false, ts, forward)
		return
	}
	if !handled || !wasDown {
		if mode == routing.TouchPartial && appOwnedPanelKey(key) {
			// The press was discarded with a failed multi-key prefix.
			return
		}
		if !handled {
			return
		}
	}
	m.applySystemPanelKey(key, false, ts, captureID, appID, mode)
}

func (m *Manager) armPanelTimerLocked(seq *panelSequence) {
	if seq.timer != nil {
		seq.timer.Stop()
	}
	seq.timer = time.AfterFunc(panelInterContact, func() { m.panelTimeout(seq) })
}

func (m *Manager) panelTimeout(seq *panelSequence) {
	m.panelExecMu.Lock()
	defer m.panelExecMu.Unlock()
	m.panelMu.Lock()
	if m.panelSeq != seq {
		m.panelMu.Unlock()
		return
	}
	ops := m.failPanelLocked(seq, false)
	m.panelMu.Unlock()
	runPanelOps(ops)
}

// failPanelLocked resolves a sequence that did not complete. A single contact
// keeps exactly one action, preserving its press/release and held semantics. A
// multi-contact failure drops framework side effects (a partial swipe or an
// unfinished circle must not mute, step volume or trigger the player), while
// app-owned circle/centre keys are still delivered to a partial capture.
// keepNewest re-seats the last contact as the start of a fresh sequence when a
// later key made the prefix impossible.
func (m *Manager) failPanelLocked(seq *panelSequence, keepNewest bool) []func() {
	if seq.timer != nil {
		seq.timer.Stop()
	}
	m.panelSeq = nil
	if len(seq.contacts) == 0 {
		return nil
	}
	// Without an overlapping contact the finger left the panel between keys:
	// every contact is a deliberate tap (a duplicate press, a quick volume pair
	// or two different controls), not a failed gesture. Each keeps its action.
	if !panelJoined(seq.contacts) {
		return m.flushContactsLocked(seq, seq.contacts, true)
	}
	if keepNewest && len(seq.contacts) > 1 {
		older := seq.contacts[:len(seq.contacts)-1]
		newest := seq.contacts[len(seq.contacts)-1]
		// The older contacts were the start of a multi-key gesture attempt.
		// Their framework effects stay suppressed: a physical swipe that
		// fails recognition must not step volume, mute or summon. Only the
		// newest contact is re-seated and may still become a single action.
		ops := m.flushContactsLocked(seq, older, false)
		next := &panelSequence{
			mode: seq.mode, captureID: seq.captureID, appID: seq.appID,
			startNS: newest.pressNS, lastPress: newest.pressNS,
			contacts: []*panelContact{newest},
		}
		m.panelSeq = next
		m.armPanelTimerLocked(next)
		return ops
	}
	return m.flushContactsLocked(seq, seq.contacts, len(seq.contacts) == 1)
}

func (m *Manager) flushContactsLocked(seq *panelSequence, contacts []*panelContact, allowSystem bool) []func() {
	var ops []func()
	for _, c := range contacts {
		key, pressNS, releaseNS, released := c.key, c.pressNS, c.releaseNS, c.released
		if seq.mode == routing.TouchPartial && appOwnedPanelKey(key) {
			capt := seq.captureID
			ops = append(ops, func() { m.applyAppPanelKey(key, true, pressNS, capt) })
			if released {
				ops = append(ops, func() { m.applyAppPanelKey(key, false, releaseNS, capt) })
			} else {
				m.panelForward[key] = capt
			}
			continue
		}
		if !allowSystem {
			if !released {
				m.panelSwallow[key] = true
			}
			continue
		}
		captureID, appID, mode := seq.captureID, seq.appID, seq.mode
		ops = append(ops, func() { m.applySystemPanelKey(key, true, pressNS, captureID, appID, mode) })
		if released {
			ops = append(ops, func() { m.applySystemPanelKey(key, false, releaseNS, captureID, appID, mode) })
		} else {
			m.panelHandled[key] = true
		}
	}
	return ops
}

// resolvePanelContactsLocked abandons the gesture interpretation and applies
// each contact as its own key action. It is used when a recognized two-contact
// gesture has no framework effect, so a mute or circle press is never lost.
func (m *Manager) resolvePanelContactsLocked(seq *panelSequence) []func() {
	if seq.timer != nil {
		seq.timer.Stop()
	}
	m.panelSeq = nil
	return m.flushContactsLocked(seq, seq.contacts, true)
}

func (m *Manager) consumeGestureLocked(seq *panelSequence, id string) []func() {
	if seq.timer != nil {
		seq.timer.Stop()
	}
	m.panelSeq = nil
	// Releases of contacts still down belong to the gesture and must not
	// activate their single-key action later.
	for _, c := range seq.contacts {
		if !c.released {
			m.panelSwallow[c.key] = true
		}
	}
	mode, captureID := seq.mode, seq.captureID
	return []func(){func() { m.dispatchPanelGesture(id, mode, captureID) }}
}

func runPanelOps(ops []func()) {
	for _, op := range ops {
		op()
	}
}

func panelKeys(contacts []*panelContact) []string {
	keys := make([]string, len(contacts))
	for i, c := range contacts {
		keys[i] = c.key
	}
	return keys
}

func panelOverlaps(contacts []*panelContact) []bool {
	overlaps := make([]bool, len(contacts))
	for i, c := range contacts {
		overlaps[i] = c.overlapsPrevious
	}
	return overlaps
}

// panelJoined reports whether any contact began while an earlier one was still
// down. Only then was a physical gesture in progress.
func panelJoined(contacts []*panelContact) bool {
	for i := 1; i < len(contacts); i++ {
		if contacts[i].overlapsPrevious {
			return true
		}
	}
	return false
}

// panelSystemGesture reports whether a recognized gesture has a framework
// effect without a capture. Vertical swipes are capture-only, so their
// two-contact form must not consume framework controls.
func panelSystemGesture(id string) bool {
	return id == "swipe_left" || id == "swipe_right"
}

func matchPanelGesture(keys []string, overlaps []bool) (id string, complete, viable bool) {
	n := len(keys)
	// A two-contact gesture requires one continuous touch: the second contact
	// must begin while the first is still down. Two taps separated by a release
	// are deliberate controls and must not merge into a skip or a silent swipe.
	joined := n == 2 && len(overlaps) == n && overlaps[1]
	// Centre-first half swipe: C, direction.
	if joined && keys[0] == "center" {
		if dir, ok := panelDirection[keys[1]]; ok {
			return panelSwipe[dir], true, false
		}
	}
	// Two-contact swipe: the ordered opposite edges without the centre, as one
	// continuous crossing.
	if joined {
		if dir, ok := oppositePair[keys[0]+"|"+keys[1]]; ok {
			return dir, true, false
		}
	}
	// Across-panel swipe: opposite, centre, direction.
	if n == 3 && keys[1] == "center" {
		for _, p := range fullSwipe {
			if keys[0] == p.opposite && keys[2] == p.direction {
				return p.id, true, false
			}
		}
	}
	// A circle is a closed perimeter traversal, starting at any perimeter key.
	if n == 5 {
		if id, ok := closedCircle(keys); ok {
			return id, true, false
		}
		return "", false, false
	}
	if n == 1 {
		return "", false, isPanelKey(keys[0])
	}
	if n == 2 && keys[1] == "center" {
		if _, ok := panelDirection[keys[0]]; ok {
			return "", false, true
		}
	}
	if circlePrefix(keys) {
		return "", false, true
	}
	return "", false, false
}

func circlePrefix(keys []string) bool {
	if len(keys) == 0 || len(keys) > 4 {
		return false
	}
	seen := map[int]bool{}
	prev, step := -1, 0
	for _, key := range keys {
		dir, ok := panelDirection[key]
		if !ok || seen[dir] {
			return false
		}
		seen[dir] = true
		if prev >= 0 {
			s := (dir - prev + 4) % 4
			if s == 0 {
				return false
			}
			if step == 0 {
				step = s
			} else if s != step {
				return false
			}
			if step != 1 && step != 3 {
				return false
			}
		}
		prev = dir
	}
	return true
}

func closedCircle(keys []string) (string, bool) {
	if len(keys) != 5 || keys[0] != keys[4] {
		return "", false
	}
	seen := map[int]bool{}
	prev, step := -1, 0
	for i, key := range keys {
		dir, ok := panelDirection[key]
		if !ok {
			return "", false
		}
		if i < 4 {
			if seen[dir] {
				return "", false
			}
			seen[dir] = true
		}
		if prev >= 0 {
			s := (dir - prev + 4) % 4
			if s == 0 {
				return "", false
			}
			if step == 0 {
				step = s
			} else if s != step {
				return "", false
			}
		}
		prev = dir
	}
	switch step {
	case 1:
		return "circle_cw", true
	case 3:
		return "circle_ccw", true
	}
	return "", false
}

// applyAppPanelKey forwards a circle/centre key to a partial capture. A press
// that is still down is remembered so its release reaches the same capture even
// after the sequence state was resolved.
func (m *Manager) applyAppPanelKey(key string, pressed bool, ts uint64, captureID string) {
	if captureID == "" {
		return
	}
	m.deliver(captureID, backend.InputEvent{MonotonicNS: ts, Key: &backend.KeyEvent{Key: key, Pressed: pressed}})
	if !pressed {
		return
	}
	m.panelMu.Lock()
	if m.panelDown[key] {
		m.panelForward[key] = captureID
	}
	m.panelMu.Unlock()
}

// applySystemPanelKey performs the framework action for one per-key contact.
// Ownership changes can race the deferred delivery, so the current mode is
// re-checked: a talk or centre press must never act after a capture took the
// key, and no key acts during full capture.
func (m *Manager) applySystemPanelKey(key string, pressed bool, ts uint64, captureID, appID string, mode routing.TouchMode) {
	_, _, current := m.state()
	if current == routing.TouchFull {
		return
	}
	if appOwnedPanelKey(key) && current != routing.TouchNone {
		return
	}
	if !appOwnedPanelKey(key) && current == routing.TouchFull {
		return
	}
	switch key {
	case "volume_up", "volume_down":
		step := m.h.VolumeUp
		if key == "volume_down" {
			step = m.h.VolumeDown
		}
		if pressed {
			m.logs.Infof("input", "", "key %s press (mode=%s)", key, mode)
			m.pressVolume(key, step)
			return
		}
		m.logs.Debugf("input", "", "key %s release (mode=%s)", key, mode)
		m.releaseVolume(key)
	case "mic_toggle":
		if pressed {
			m.logs.Infof("input", "", "key mic_toggle press (mode=%s)", mode)
			if m.h.MicToggle != nil {
				m.h.MicToggle()
			}
			return
		}
		m.logs.Debugf("input", "", "key mic_toggle release (mode=%s)", mode)
	case "play_pause":
		if pressed {
			if m.pressTalkKey(key, ts) {
				return
			}
			m.beginKeyActivation(keyOrigin(key), ts)
			return
		}
		m.releaseTalkKey(key, ts)
	case "center":
		m.handlePrimary(backend.InputEvent{MonotonicNS: ts, Key: &backend.KeyEvent{Key: key, Pressed: pressed}},
			captureID, appID, mode, false)
	}
}

func (m *Manager) dispatchPanelGesture(id string, mode routing.TouchMode, captureID string) {
	if mode == routing.TouchPartial || mode == routing.TouchFull {
		if captureID == "" {
			return
		}
		m.deliver(captureID, backend.InputEvent{MonotonicNS: m.clock.NowNS(),
			Gesture: &backend.GestureEvent{ID: id, Phase: "end"}})
		m.logs.Infof("input", "", "panel gesture %s delivered to capture %s (mode=%s)", id, captureID, mode)
		return
	}
	switch id {
	case "swipe_left":
		m.logs.Infof("input", "", "panel gesture swipe_left: previous media")
		if m.h.MediaPrevious != nil {
			m.h.MediaPrevious()
		}
	case "swipe_right":
		m.logs.Infof("input", "", "panel gesture swipe_right: next media")
		if m.h.MediaNext != nil {
			m.h.MediaNext()
		}
	default:
		m.logs.Infof("input", "", "panel gesture %s recognized; no system action", id)
	}
}

// fullPanelKey shadow-recognizes gestures while every key is already being
// forwarded. Full capture disables all system side effects, so this recognizer
// only adds gesture events; it never defers or consumes a key.
func (m *Manager) fullPanelKey(key string, pressed bool, ts uint64, captureID string) {
	if !pressed {
		m.panelMu.Lock()
		m.fullDown[key] = false
		m.panelMu.Unlock()
		return
	}
	var gesture string
	m.panelMu.Lock()
	if m.fullDown[key] {
		m.panelMu.Unlock()
		return
	}
	overlap := false
	for other, down := range m.fullDown {
		if down && other != key {
			overlap = true
			break
		}
	}
	m.fullDown[key] = true
	if len(m.fullSeq) > 0 {
		gap := uint64(0)
		if ts > m.fullLastNS {
			gap = ts - m.fullLastNS
		}
		total := uint64(0)
		if ts > m.fullStartNS {
			total = ts - m.fullStartNS
		}
		if gap > uint64(panelInterContact) || total > uint64(panelGestureTotal) {
			m.fullSeq = nil
			m.fullOverlap = nil
		}
	}
	if len(m.fullSeq) == 0 {
		m.fullStartNS = ts
	}
	m.fullLastNS = ts
	m.fullSeq = append(m.fullSeq, key)
	m.fullOverlap = append(m.fullOverlap, overlap)
	if id, complete, viable := matchPanelGesture(m.fullSeq, m.fullOverlap); complete {
		gesture = id
		m.fullSeq = nil
		m.fullOverlap = nil
	} else if !viable {
		m.fullSeq = []string{key}
		m.fullOverlap = []bool{false}
		m.fullStartNS = ts
	}
	m.panelMu.Unlock()
	if gesture != "" {
		m.dispatchPanelGesture(gesture, routing.TouchFull, captureID)
	}
}

// cancelPanelSequence abandons deferred contacts without producing actions or
// app key events. It is used when capture ownership takes the affected keys.
func (m *Manager) cancelPanelSequence(reason string) {
	m.panelExecMu.Lock()
	defer m.panelExecMu.Unlock()
	m.panelMu.Lock()
	had := m.panelSeq != nil
	if m.panelSeq != nil && m.panelSeq.timer != nil {
		m.panelSeq.timer.Stop()
	}
	m.panelSeq = nil
	m.panelDown = map[string]bool{}
	m.panelSwallow = map[string]bool{}
	m.panelForward = map[string]string{}
	m.panelHandled = map[string]bool{}
	m.fullSeq = nil
	m.fullDown = map[string]bool{}
	m.panelMu.Unlock()
	if had {
		m.logs.Infof("input", "", "panel gesture recognition cancelled: %s", reason)
	}
}

// cancelPanelForRouting keeps an unrelated routing update from discarding a
// live sequence, but drops one whose effective owner or capture changed.
func (m *Manager) cancelPanelForRouting(captureID string, mode routing.TouchMode) {
	m.panelExecMu.Lock()
	defer m.panelExecMu.Unlock()
	m.panelMu.Lock()
	seq := m.panelSeq
	changed := seq != nil && (seq.mode != mode || (seq.mode == routing.TouchPartial && seq.captureID != captureID))
	if mode == routing.TouchFull {
		// Full capture bypasses panelRelease for the whole duration, so every
		// framework key/release bookkeeping entry becomes stale even when no
		// sequence is pending (a held key may already have been flushed).
		// Clear it unconditionally; the matching held system action is stopped
		// exactly once by OnRoutingChanged's talk/volume cleanup.
		if seq != nil && seq.timer != nil {
			seq.timer.Stop()
		}
		m.panelSeq = nil
		m.panelDown = map[string]bool{}
		m.panelSwallow = map[string]bool{}
		m.panelForward = map[string]string{}
		m.panelHandled = map[string]bool{}
		m.panelMu.Unlock()
		if seq != nil {
			m.logs.Infof("input", "", "panel gesture recognition cancelled: capture ownership changed")
		}
		return
	}
	if !changed {
		m.fullSeq = nil
		m.fullOverlap = nil
		m.fullDown = map[string]bool{}
		m.panelMu.Unlock()
		return
	}
	if seq != nil && seq.timer != nil {
		seq.timer.Stop()
	}
	m.panelSeq = nil
	m.panelDown = map[string]bool{}
	m.panelSwallow = map[string]bool{}
	m.panelForward = map[string]string{}
	m.panelHandled = map[string]bool{}
	m.fullSeq = nil
	m.fullOverlap = nil
	m.fullDown = map[string]bool{}
	m.panelMu.Unlock()
	m.logs.Infof("input", "", "panel gesture recognition cancelled: capture ownership changed")
}
