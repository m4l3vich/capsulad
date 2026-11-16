package input

import (
	"strconv"
	"strings"
	"time"

	"capsulad/internal/backend"
	"capsulad/internal/ids"
	"capsulad/internal/routing"
)

// The Mini panel exposes five discrete gpio keys and no raw coordinates:
// microphone mute (north, KEY_MICMUTE 0xf8), volume up (east, 0x73),
// circle/play-pause (south, 0xa4), volume down (west, 0x72) and the unmarked
// centre (BTN_TRIGGER_HAPPY1 0x2c0 in one DTB, KEY_VOICECOMMAND 0x246 in the
// other). The node advertises EV_SYN|EV_KEY only (EV=3), so no position is ever
// available and a gesture can only be inferred from ordered key edges.
//
// Raw capture settles what the hardware actually reports: a finger swipe across
// the whole panel emits exactly one press, SYN, release, SYN and nothing else.
// An upward swipe reports volume_down, a rightward swipe reports mic_toggle, a
// downward swipe reports mic_toggle and a leftward swipe reports volume_up. A
// swipe that starts at the physical centre can report the centre key instead of
// an edge, and one capture showed mic_toggle for one gesture and centre for the
// next.
//
// Two consequences drive this resolver:
//
//   - A bare volume edge is indistinguishable from a deliberate volume tap, so
//     it resolves as its own key on its own edge. Nothing waits for a possible
//     opposite edge: no contact is ever deferred, and a lone tap has zero added
//     latency.
//   - The centre key is the usual gesture evidence. A swipe is recognized when
//     the centre appears in the contact sequence: a volume edge pressed while
//     the centre is still down (no press-gap bound, because a deliberate
//     two-finger action is naturally slower than panelInterContact), a centre
//     released and followed by an edge inside panelCentreGateWindow (the usable
//     form: this pad reports one key at a time), or the documented opposite ->
//     centre -> direction chain. A full crossing of the pad can also report
//     only its two end-stop edges; opposite volume edges pressed inside
//     panelFullSwipeWindow with no centre between them are one full-panel
//     swipe.
//
// Every key keeps its own press/release semantics and acts immediately; a
// gesture suppresses only the contact that completes it. Because nothing is
// deferred, the first opposite edge of a chain has already produced its own
// volume step when the direction edge resolves the gesture. The measured
// hardware emits one key per swipe, so the chain is a documented shape rather
// than an observed one. A full-panel crossing is resolved the same way: the
// leading edge has already stepped, so the second edge reverts that one step
// and suppresses its own; a lone tap and a slow opposite pair are unaffected.
const (
	// panelInterContact bounds the press-to-press distance of the chain and of
	// every shadow that is not a centre gate. The released centre gate uses
	// panelCentreGateWindow and the held gate panelGestureTotal: a centre tap is
	// evidence on its own.
	panelInterContact = 250 * time.Millisecond
	panelGestureTotal = 1500 * time.Millisecond
	// panelCentreGateWindow bounds the released-centre form of a swipe. The
	// panel reports one key at a time, so "hold the centre and tap an edge" is
	// not physically possible on the unit: the natural action is a short centre
	// tap followed by an edge. The centre tap is deliberate evidence on its own,
	// and gating costs no volume latency — only the edge that completes the
	// gesture is suppressed — so the window is generous enough for a two-finger
	// roll: 700 ms, the same budget as a hold.
	panelCentreGateWindow = 700 * time.Millisecond
	// panelFullSwipeWindow bounds the full-panel crossing: two opposite volume
	// edges with no centre between them, pressed this close together, are one
	// finger crossing the whole pad. Deliberate opposite taps on the unit are
	// ~1.2 s apart and a measured hand crossing is 90–180 ms, so 400 ms covers
	// a slow real crossing while staying far from a deliberate pair. Both edges
	// keep acting on their own press; the step of the leading edge is reverted
	// when the second edge resolves the gesture, so nothing waits for a
	// possible opposite edge and a lone tap keeps zero added latency. A slower
	// opposite pair stays two deliberate steps.
	panelFullSwipeWindow = 400 * time.Millisecond
	// panelContactLimit bounds the contact shadow. The longest form is the
	// opposite -> centre -> direction chain; a longer run is abandoned.
	panelContactLimit = 5
)

// panelDirection is the panel perimeter clockwise from north. Only the
// full-capture shadow uses it: a closed traversal is the one advertised gesture
// the framework resolver never needs, because outside a capture those five
// contacts must stay their own keys.
var panelDirection = map[string]int{
	"mic_toggle":  0, // north
	"volume_up":   1, // east
	"play_pause":  2, // south
	"volume_down": 3, // west
}

// panelCentreSwipe maps the volume edge of a centre-gated pair to the media
// direction it selects. This is the measured centre evidence: the centre
// reports the start of a centre-anchored contact and the volume edge reports
// the end of it, either pressed while the centre is still down (for panels that
// report overlapping contacts) or inside panelCentreGateWindow after the centre
// was released.
var panelCentreSwipe = map[string]string{
	"volume_up":   "swipe_right",
	"volume_down": "swipe_left",
}

// panelChainID maps the documented opposite -> centre -> direction crossing to
// its gesture from the outer contacts. The direction contact is centre-gated
// and therefore suppressed; the opposite contact already resolved its own key
// on its own edge. It is an ordered switch, not a map, so the event path never
// allocates a lookup key.
func panelChainID(opposite, direction string) (string, bool) {
	switch {
	case opposite == "volume_down" && direction == "volume_up":
		return "swipe_right", true // W, C, E
	case opposite == "volume_up" && direction == "volume_down":
		return "swipe_left", true // E, C, W
	case opposite == "play_pause" && direction == "mic_toggle":
		return "swipe_up", true // S, C, N
	case opposite == "mic_toggle" && direction == "play_pause":
		return "swipe_down", true // N, C, S
	}
	return "", false
}

type panelContact struct {
	key       string
	pressNS   uint64
	releaseNS uint64
	released  bool
	// overlapsPrevious records that this contact began while an earlier contact
	// in the same sequence was still down.
	overlapsPrevious bool
	// priorVolume is the master level immediately before this contact's volume
	// step; volumeKnown and stepApplied bound when it may be restored. Only a
	// full-panel swipe reads them, to undo the leading edge's step. The fixed
	// fields keep the event path allocation-free.
	priorVolume float64
	volumeKnown bool
	stepApplied bool
}

// panelSequence is the bounded shadow of recent contacts. It never delays a
// key: it exists only to recognize a gesture whose evidence spans more than one
// contact. The fixed array keeps the event path allocation-free.
type panelSequence struct {
	contacts [panelContactLimit]panelContact
	n        int
}

// reset clears the shadow without discarding its storage.
func (s *panelSequence) reset() { s.n = 0 }

// retainHeldCentre restarts the shadow after a match with the centre contact
// alone when it is still down. A held centre keeps gating further edge taps:
// the finger that anchors the gesture is still on the key, so a second edge
// during the same hold is the same evidence again. The retained contact keeps
// its original press, so the total budget still runs from the first centre
// contact. Without a held centre the shadow is simply cleared.
func (s *panelSequence) retainHeldCentre() {
	if i := s.heldCentre(); i >= 0 {
		s.contacts[0] = s.contacts[i]
		s.n = 1
		return
	}
	s.n = 0
}

// panelOppositeVolumePair reports whether key is the opposite volume edge to
// prev. It is the only bare pair that can complete a full-panel crossing.
func panelOppositeVolumePair(prev, key string) bool {
	return (prev == "volume_down" && key == "volume_up") ||
		(prev == "volume_up" && key == "volume_down")
}

// panelPendingAllows reports whether a press may continue the pending shadow.
// A centre is special: while it is held, only a volume edge continues the gate
// and the sequence budget is the only bound; once released, the edge has the
// inter-contact budget measured from the release, not from the press.
func panelPendingAllows(seq *panelSequence, key string, ts uint64) bool {
	if seq.n == 0 {
		return true
	}
	last := &seq.contacts[seq.n-1]
	if last.key == "center" {
		if !last.released {
			if key == "volume_up" || key == "volume_down" {
				gap := panelGapFrom(last.pressNS, ts)
				return gap >= 0 && gap <= panelGestureTotal
			}
			return panelWithin(last.pressNS, ts)
		}
		if last.releaseNS == 0 {
			return false
		}
		gap := panelGapFrom(last.releaseNS, ts)
		return gap >= 0 && gap <= panelCentreGateWindow
	}
	if panelOppositeVolumePair(last.key, key) {
		// A full-panel crossing is measured press-to-press with its own,
		// wider window; every other bare contact keeps the inter-contact bound.
		gap := panelGapFrom(last.pressNS, ts)
		return gap >= 0 && gap <= panelFullSwipeWindow
	}
	return panelWithin(last.pressNS, ts)
}

// completedHeldCentreGate reports whether the two newest contacts are the held
// form of the centre gate: the centre is still down and the edge began inside
// that hold. Only that shape keeps the centre as an anchor after a match.
func (s *panelSequence) completedHeldCentreGate() bool {
	if s.n < 2 || panelTier(s) != "centre" {
		return false
	}
	previous, completing := &s.contacts[s.n-2], &s.contacts[s.n-1]
	return previous.key == "center" && !previous.released && completing.overlapsPrevious
}

// heldCentre returns the index of the most recent centre contact that has not
// been released, or -1. The centre gate only needs the centre to still be down;
// contacts in between do not disqualify it.
func (s *panelSequence) heldCentre() int {
	for i := s.n - 1; i >= 0; i-- {
		if c := &s.contacts[i]; c.key == "center" && !c.released {
			return i
		}
	}
	return -1
}

// push appends one contact; the caller has already ensured there is room.
func (s *panelSequence) push(c panelContact) {
	s.contacts[s.n] = c
	s.n++
}

// joined reports whether any tracked contact is still down, i.e. whether the
// contact about to be pushed overlaps one already held. It is called before the
// push so the new contact never counts as its own predecessor.
func (s *panelSequence) joined() bool {
	for i := range s.n {
		if !s.contacts[i].released {
			return true
		}
	}
	return false
}

// release records the release of one still-down contact.
func (s *panelSequence) release(key string, ts uint64) {
	for i := s.n - 1; i >= 0; i-- {
		if c := &s.contacts[i]; c.key == key && !c.released {
			c.released = true
			c.releaseNS = ts
			return
		}
	}
}

func eventNS(clock *ids.Clock, ns uint64) uint64 {
	if ns != 0 {
		return ns
	}
	return clock.NowNS()
}

// remainingHold keeps a hold threshold measured from the physical press even
// though the press may be read late.
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

// panelPress resolves one contact's own action on its own press edge. Nothing
// waits: the only contact whose action is withheld is the one that completes a
// recognized gesture.
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

	seq := &m.panelSeq
	centreGap := time.Duration(-1)
	centrePending := false
	if seq.n > 0 && seq.contacts[seq.n-1].key == "center" {
		centreGap = panelGapFrom(seq.contacts[seq.n-1].pressNS, ts)
		centrePending = true
	}
	volumeEdge := key == "volume_up" || key == "volume_down"
	oppositePair := volumeEdge && seq.n > 0 && panelOppositeVolumePair(seq.contacts[seq.n-1].key, key)
	heldCentre := centrePending && !seq.contacts[seq.n-1].released
	heldCentreEdge := heldCentre && volumeEdge && centreGap >= 0 && centreGap <= panelGestureTotal
	reason := ""
	overBudgetCentre := heldCentre && volumeEdge && centreGap > panelGestureTotal
	switch {
	case overBudgetCentre:
		// A held centre gates an edge with no press-gap bound, so the only
		// bound left is the sequence's total budget.
		reason = "gesture total"
	case !panelPendingAllows(seq, key, ts) && !heldCentreEdge:
		reason = "contact gap"
	case seq.n == panelContactLimit:
		reason = "five contacts"
	}
	if reason != "" {
		// The candidate never completed. A centre followed by a volume edge
		// always emits the centre-specific decision, including beyond the
		// released-gate window and a held centre's total budget.
		if centrePending && volumeEdge {
			m.logs.Infof("input", "", "panel centre -> %s p+%s decision=keys tier=centre reason=%s press_gap=%s window=%dms from %s",
				key, panelGapMS(panelGapFrom(seq.contacts[0].pressNS, ts)), reason, panelGapMS(centreGap),
				panelCentreGateWindow/time.Millisecond, panelContactTrace(seq))
		} else if oppositePair {
			// The leading step is already applied and stays applied: a pair
			// outside the crossing window is two deliberate steps, and no
			// gesture is produced from it.
			m.logs.Infof("input", "", "panel full-panel crossing rejected tier=keys decision=keys reason=%s press_gap=%s window=%dms; both edges stay volume steps: %s",
				reason, panelGapMS(panelGapFrom(seq.contacts[seq.n-1].pressNS, ts)),
				panelFullSwipeWindow/time.Millisecond, panelContactTrace(seq))
		} else {
			m.logs.Infof("input", "", "panel gesture rejected tier=%s decision=keys reason=%s press_gap=%s window=%dms: %s",
				panelTier(seq), reason, panelGapMS(panelGapFrom(seq.contacts[seq.n-1].pressNS, ts)),
				panelInterContact/time.Millisecond, panelContactTrace(seq))
		}
		seq.reset()
	}
	overlap := seq.joined()
	seq.push(panelContact{key: key, pressNS: ts, overlapsPrevious: overlap})

	gestureID := ""
	gestureMode, gestureCapture := mode, captureID
	fullPanel := false
	var restoreLevel float64
	restore := false
	if reason == "" {
		gestureID, _ = panelMatch(seq)
		// A full-panel swipe is recognized only after its leading edge already
		// stepped: capture that step's undo information before the sequence is
		// reset. One revert per gesture, and only when the step applied.
		if gestureID != "" {
			if _, ok := panelFullSwipe(seq); ok {
				fullPanel = true
				if leading := &seq.contacts[seq.n-2]; leading.volumeKnown && leading.stepApplied && m.h.VolumeRestore != nil {
					restoreLevel = leading.priorVolume
					restore = true
				}
			}
		}
	}
	if gestureID != "" {
		tier := panelTier(seq)
		gap := centreGap
		if fullPanel {
			// A full-panel match has no centre: its evidence is the
			// press-to-press distance of the two volume edges.
			gap = panelGapFrom(seq.contacts[seq.n-2].pressNS, ts)
		}
		m.logs.Infof("input", "", "panel gesture %s resolved tier=%s press_gap=%s window=%dms from %s",
			gestureID, tier, panelGapMS(gap), panelGestureWindow(tier),
			panelContactTrace(seq))
		m.panelSwallow[key] = true
		if seq.completedHeldCentreGate() {
			// The centre is still down: it stays the anchor for another edge
			// tapped during the same physical hold. Its original press
			// timestamp keeps the total gesture budget honest and its release
			// still resolves normally.
			seq.retainHeldCentre()
		} else {
			// Every other match ends the sequence, so a chain cannot leave a
			// stale centre for the next, unrelated edge to join.
			seq.reset()
		}
	} else {
		m.registerPanelPressLocked(key, mode, captureID)
	}
	m.panelMu.Unlock()
	if gestureID != "" {
		if restore {
			m.h.VolumeRestore(restoreLevel)
			m.logs.Infof("input", "", "panel full-panel swipe reverted the leading volume step")
		}
		m.dispatchPanelGesture(gestureID, gestureMode, gestureCapture)
	} else if mode == routing.TouchPartial && appOwnedPanelKey(key) {
		m.applyAppPanelKey(key, true, ts, captureID)
	} else {
		volumeKey := key == "volume_up" || key == "volume_down"
		var priorVolume float64
		known := false
		if volumeKey && m.h.VolumeLevel != nil {
			// Read before the step: this is the level a later full-panel swipe
			// restores if this edge turns out to lead one.
			priorVolume = m.h.VolumeLevel()
			known = true
		}
		applied := m.applySystemPanelKey(key, true, ts, captureID, appID, mode)
		if volumeKey {
			m.recordVolumeStep(key, ts, priorVolume, known, applied)
		}
	}
}

// recordVolumeStep attaches a volume contact's undo information to the shadow.
// The step has already run, so only the newest matching contact can be the one
// that stepped; a gesture or reset in between leaves nothing to record.
func (m *Manager) recordVolumeStep(key string, pressNS uint64, level float64, known, applied bool) {
	if !known && !applied {
		return
	}
	m.panelMu.Lock()
	defer m.panelMu.Unlock()
	for i := m.panelSeq.n - 1; i >= 0; i-- {
		if c := &m.panelSeq.contacts[i]; c.key == key && c.pressNS == pressNS {
			c.priorVolume = level
			c.volumeKnown = known
			c.stepApplied = applied
			return
		}
	}
}

// registerPanelPressLocked records the destination for one contact's own press.
func (m *Manager) registerPanelPressLocked(key string, mode routing.TouchMode, captureID string) {
	if mode == routing.TouchPartial && appOwnedPanelKey(key) {
		// Partial capture owns the circle and centre keys; their releases must
		// reach the same capture even after a routing change.
		m.panelForward[key] = captureID
		return
	}
	m.panelHandled[key] = true
}

// panelRelease resolves one contact's own release on its own edge. A release
// whose press was consumed by a gesture or delivered to a capture belongs to
// that owner, not to the framework.
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
	if wasDown {
		m.panelSeq.release(key, ts)
		m.fullSeq.release(key, ts)
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
		return
	}
	m.applySystemPanelKey(key, false, ts, captureID, appID, mode)
}

// panelWithin reports whether next follows prev inside the inter-contact bound.
// A late read can deliver events out of order, so an inversion is never inside.
func panelWithin(prev, next uint64) bool {
	return next >= prev && next-prev <= uint64(panelInterContact)
}

// panelGapFrom renders the non-negative distance between two kernel timestamps;
// a late read can deliver events out of order, so an inversion is treated as an
// unknown (negative) gap.
func panelGapFrom(prev, ts uint64) time.Duration {
	if ts < prev {
		return -1
	}
	return time.Duration(ts - prev)
}

// panelMatch decides whether the newest contact completes a gesture. Precedence
// is the three-contact centre chain, then the two-contact centre gate. The held
// gate requires the edge to overlap the centre and stays bounded by the total
// budget; only the released gate uses panelInterContact. A sequence without the
// centre key is never a gesture: the hardware adds no other evidence, so bare
// edges are their own keys.
// panelCentreGate reports the swipe a centre contact gates. Held centre: the
// edge began while the centre was still down, so the overlap is the evidence
// and only the sequence budget bounds it. Released centre: the edge must follow
// the *release* inside panelCentreGateWindow — measured from the release, not
// the press, so a centre held for a while still gates a quick follow-up and a
// stale centre cannot gate an unrelated edge.
func panelCentreGate(centre, edge *panelContact) (string, bool) {
	if centre.key != "center" {
		return "", false
	}
	id, ok := panelCentreSwipe[edge.key]
	if !ok {
		return "", false
	}
	if !centre.released {
		if edge.overlapsPrevious && edge.pressNS >= centre.pressNS &&
			edge.pressNS-centre.pressNS <= uint64(panelGestureTotal) {
			return id, true
		}
		return "", false
	}
	if centre.releaseNS == 0 || edge.pressNS < centre.releaseNS {
		return "", false
	}
	gap := panelGapFrom(centre.releaseNS, edge.pressNS)
	if gap < 0 || gap > panelCentreGateWindow {
		return "", false
	}
	return id, true
}

// panelFullSwipe reports the full-panel crossing completed by the newest two
// contacts: opposite volume edges with no centre between them, pressed inside
// panelFullSwipeWindow. The pad reports one key at a time, so a finger crossing
// the whole panel is exactly this pair. It is consulted after the centre forms,
// so a documented chain or centre gate always wins.
func panelFullSwipe(seq *panelSequence) (string, bool) {
	n := seq.n
	if n < 2 {
		return "", false
	}
	first, second := &seq.contacts[n-2], &seq.contacts[n-1]
	if second.pressNS < first.pressNS || second.pressNS-first.pressNS > uint64(panelFullSwipeWindow) {
		return "", false
	}
	if !panelOppositeVolumePair(first.key, second.key) {
		return "", false
	}
	if n >= 3 && seq.contacts[n-3].key == first.key {
		// Two same-direction taps immediately before the opposite edge are a
		// deliberate volume sequence, not one hand crossing: the repeat says
		// the finger was stepping, and the opposite edge is its own step.
		return "", false
	}
	if second.key == "volume_up" {
		return "swipe_right", true // west -> east
	}
	return "swipe_left", true // east -> west
}

// panelMatch reports the gesture a contact sequence completes: the three-contact
// chain, then the two-contact centre gate, then a full-panel crossing. A
// sequence with neither the centre key nor an opposite volume pair is never a
// gesture: the hardware adds no other evidence, so bare edges are their own
// keys.
func panelMatch(seq *panelSequence) (string, bool) {
	n := seq.n
	if n >= 3 {
		a, b, c := seq.contacts[n-3], seq.contacts[n-2], seq.contacts[n-1]
		if b.key == "center" &&
			panelWithin(a.pressNS, b.pressNS) && panelWithin(b.pressNS, c.pressNS) &&
			c.pressNS >= a.pressNS && c.pressNS-a.pressNS <= uint64(panelGestureTotal) {
			if id, ok := panelChainID(a.key, c.key); ok {
				return id, true
			}
		}
	}
	if n >= 2 {
		if id, ok := panelCentreGate(&seq.contacts[n-2], &seq.contacts[n-1]); ok {
			return id, true
		}
	}
	if id, ok := panelFullSwipe(seq); ok {
		return id, true
	}
	return "", false
}

// closedCircle reports the direction of one closed perimeter traversal:
// five contacts returning to the first key, each beginning while the previous
// was still down, advancing one perimeter step in a consistent direction and
// inside the same inter-contact and total budget as every other form. It is
// consulted only by the full-capture shadow, where every key is forwarded
// anyway, so recognizing a circle costs no key action and suppressions none.
func closedCircle(seq *panelSequence) (string, bool) {
	if seq.n != panelContactLimit || seq.contacts[0].key != seq.contacts[4].key {
		return "", false
	}
	first, last := seq.contacts[0].pressNS, seq.contacts[4].pressNS
	if last < first || last-first > uint64(panelGestureTotal) {
		return "", false
	}
	step := 0
	for i := range panelContactLimit {
		c := &seq.contacts[i]
		dir, ok := panelDirection[c.key]
		if !ok {
			return "", false
		}
		if i == 0 {
			continue
		}
		if !c.overlapsPrevious {
			return "", false
		}
		prevDir := panelDirection[seq.contacts[i-1].key]
		s := (dir - prevDir + 4) % 4
		if s == 0 {
			return "", false
		}
		if step == 0 {
			step = s
		} else if s != step {
			return "", false
		}
	}
	switch step {
	case 1:
		return "circle_cw", true
	case 3:
		return "circle_ccw", true
	}
	return "", false
}

// panelTier names the evidence that governed the current sequence, without
// allocating in the event path. Only these five names are used: chain (opposite
// -> centre -> direction), centre (the centre gate), full (the full-panel
// crossing), overlap (contacts inside one touch) and keys (individual key
// effects).
func panelTier(seq *panelSequence) string {
	n := seq.n
	if n >= 3 {
		a, b, c := seq.contacts[n-3], seq.contacts[n-2], seq.contacts[n-1]
		if b.key == "center" && panelWithin(a.pressNS, b.pressNS) &&
			panelWithin(b.pressNS, c.pressNS) && c.pressNS >= a.pressNS &&
			c.pressNS-a.pressNS <= uint64(panelGestureTotal) {
			if _, ok := panelChainID(a.key, c.key); ok {
				return "chain"
			}
		}
	}
	if n >= 2 {
		if _, ok := panelCentreGate(&seq.contacts[n-2], &seq.contacts[n-1]); ok {
			return "centre"
		}
		if _, ok := panelFullSwipe(seq); ok {
			return "full"
		}
	}
	if n >= 2 && seq.contacts[n-1].overlapsPrevious {
		return "overlap"
	}
	return "keys"
}

// panelGestureWindow renders the bound that applied to a matched gesture, so a
// field log cannot be misread: the centre gate is far more generous than the
// volume window, and a full-panel crossing has its own 400 ms press window.
func panelGestureWindow(tier string) int64 {
	switch tier {
	case "centre":
		return panelCentreGateWindow.Milliseconds()
	case "full":
		return panelFullSwipeWindow.Milliseconds()
	}
	return panelInterContact.Milliseconds()
}

// panelGapMS renders a signed duration for the field log; unknown is "-".
func panelGapMS(d time.Duration) string {
	if d < 0 {
		return "-"
	}
	return strconv.FormatInt(int64(d/time.Millisecond), 10) + "ms"
}

// panelContactTrace renders the bounded field-log shape of a sequence: key,
// press/release offsets in milliseconds from the first press, the signed gap
// from the previous release, and the overlap flag. Any recognized form fits in
// panelContactLimit contacts. It carries no payload data.
func panelContactTrace(seq *panelSequence) string {
	if seq.n == 0 {
		return "none"
	}
	base := seq.contacts[0].pressNS
	var b strings.Builder
	for i := range seq.n {
		c := &seq.contacts[i]
		if i > 0 {
			b.WriteString(" | ")
			if prev := &seq.contacts[i-1]; prev.released {
				gapMS := (int64(c.pressNS) - int64(prev.releaseNS)) / int64(time.Millisecond)
				b.WriteString("gap ")
				b.WriteString(strconv.FormatInt(gapMS, 10))
				b.WriteString("ms ")
			}
		}
		b.WriteString(c.key)
		b.WriteString(" p+")
		b.WriteString(strconv.FormatUint((c.pressNS-base)/uint64(time.Millisecond), 10))
		b.WriteString("ms")
		if c.released {
			b.WriteString(" r+")
			b.WriteString(strconv.FormatUint((c.releaseNS-base)/uint64(time.Millisecond), 10))
			b.WriteString("ms")
		} else {
			b.WriteString(" down")
		}
		if c.overlapsPrevious {
			b.WriteString(" overlap")
		}
	}
	return b.String()
}

// applyAppPanelKey forwards a circle/centre key to a partial capture. A press
// that is still down is remembered so its release reaches the same capture even
// after a routing change.
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

// applySystemPanelKey performs the framework action for one per-key contact and
// reports whether a volume press actually moved the value. Ownership changes
// can race the delivery, so the current mode is re-checked: a talk or centre
// press must never act after a capture took the key, and no key acts during
// full capture.
func (m *Manager) applySystemPanelKey(key string, pressed bool, ts uint64, captureID, appID string, mode routing.TouchMode) bool {
	_, _, current := m.state()
	if current == routing.TouchFull {
		return false
	}
	if appOwnedPanelKey(key) && current != routing.TouchNone {
		return false
	}
	switch key {
	case "volume_up", "volume_down":
		step := m.h.VolumeUp
		if key == "volume_down" {
			step = m.h.VolumeDown
		}
		if pressed {
			m.logs.Infof("input", "", "key %s press (mode=%s)", key, mode)
			return m.pressVolume(key, step)
		}
		m.logs.Debugf("input", "", "key %s release (mode=%s)", key, mode)
		m.releaseVolume(key)
	case "mic_toggle":
		if pressed {
			m.logs.Infof("input", "", "key mic_toggle press (mode=%s)", mode)
			if m.h.MicToggle != nil {
				m.h.MicToggle()
			}
			return false
		}
		m.logs.Debugf("input", "", "key mic_toggle release (mode=%s)", mode)
	case "play_pause":
		if pressed {
			if m.pressTalkKey(key, ts) {
				return false
			}
			m.beginKeyActivation(keyOrigin(key), ts)
			return false
		}
		m.releaseTalkKey(key, ts)
	case "center":
		m.handlePrimary(backend.InputEvent{MonotonicNS: ts, Key: &backend.KeyEvent{Key: key, Pressed: pressed}},
			captureID, appID, mode, false)
	}
	return false
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
	m.panelMu.Lock()
	if !pressed {
		m.fullSeq.release(key, ts)
		m.fullDown[key] = false
		m.panelMu.Unlock()
		return
	}
	if m.fullDown[key] {
		m.panelMu.Unlock()
		return
	}
	m.fullDown[key] = true
	seq := &m.fullSeq
	gap := time.Duration(-1)
	if seq.n > 0 {
		gap = panelGapFrom(seq.contacts[seq.n-1].pressNS, ts)
	}
	heldCentreEdge := false
	if seq.n > 0 {
		last := &seq.contacts[seq.n-1]
		heldCentreEdge = last.key == "center" && !last.released &&
			(key == "volume_up" || key == "volume_down") && gap >= 0 && gap <= panelGestureTotal
	}
	if seq.n > 0 && !panelWithin(seq.contacts[seq.n-1].pressNS, ts) && !heldCentreEdge {
		seq.reset()
	} else if seq.n == panelContactLimit {
		seq.reset()
	}
	seq.push(panelContact{key: key, pressNS: ts, overlapsPrevious: seq.joined()})
	id, ok := panelMatch(seq)
	if !ok {
		// Only the shadow recognizes a circle: every key it sees has already
		// been forwarded, so the traversal's five contacts stay their own keys.
		id, ok = closedCircle(seq)
	}
	var tier, trace string
	if ok {
		tier, trace = panelTier(seq), panelContactTrace(seq)
		// A centre that is still down keeps gating further edge taps; every
		// key was forwarded anyway, so nothing else is retained.
		seq.retainHeldCentre()
	}
	m.panelMu.Unlock()
	if ok {
		m.logs.Infof("input", "", "panel gesture %s resolved in full capture tier=%s press_gap=%s window=%dms from %s",
			id, tier, panelGapMS(gap), panelInterContact/time.Millisecond, trace)
		m.dispatchPanelGesture(id, routing.TouchFull, captureID)
	}
}

// cancelPanelSequence abandons the contact shadow without producing actions or
// app key events. It is used when capture ownership takes the affected keys.
func (m *Manager) cancelPanelSequence(reason string) {
	m.panelExecMu.Lock()
	defer m.panelExecMu.Unlock()
	m.panelMu.Lock()
	had := m.panelSeq.n > 0
	m.panelSeq.reset()
	m.panelDown = map[string]bool{}
	m.panelSwallow = map[string]bool{}
	m.panelForward = map[string]string{}
	m.panelHandled = map[string]bool{}
	m.fullSeq.reset()
	m.fullDown = map[string]bool{}
	m.panelMu.Unlock()
	if had {
		m.logs.Infof("input", "", "panel gesture recognition cancelled: %s", reason)
	}
}

// cancelPanelForRouting drops the contact shadow on a routing change, because a
// candidate that spans the change would deliver its gesture to the new owner.
// Full capture additionally clears the framework key bookkeeping: it bypasses
// panelRelease for the whole duration.
func (m *Manager) cancelPanelForRouting(captureID string, mode routing.TouchMode) {
	m.panelExecMu.Lock()
	defer m.panelExecMu.Unlock()
	m.panelMu.Lock()
	if mode == routing.TouchFull {
		// Full capture bypasses panelRelease for the whole duration, so every
		// framework key/release bookkeeping entry becomes stale even when no
		// sequence is pending (a held key may already have been applied).
		// Clear it unconditionally; the matching held system action is stopped
		// exactly once by OnRoutingChanged's talk/volume cleanup.
		had := m.panelSeq.n > 0
		m.panelSeq.reset()
		m.panelDown = map[string]bool{}
		m.panelSwallow = map[string]bool{}
		m.panelForward = map[string]string{}
		m.panelHandled = map[string]bool{}
		m.panelMu.Unlock()
		if had {
			m.logs.Infof("input", "", "panel gesture recognition cancelled: capture ownership changed")
		}
		return
	}
	// A routing change ends any candidate that spans it: the shadow only
	// recognizes a gesture, so dropping it never replays a key action.
	m.panelSeq.reset()
	m.fullSeq.reset()
	m.fullDown = map[string]bool{}
	m.panelMu.Unlock()
}
