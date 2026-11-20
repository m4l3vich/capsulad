package session

// The receiver has one volume: the persisted framework master. AirPlay's dB
// value is a control position, not an amplitude: the reference maps it
// linearly onto the master slider (utilities/general_utilities.c
// airplayVolumeToUnitVolume). This app forwards that slider position to
// AudioService.SetMasterVolume and never scales sender PCM by it; the framework
// mixer applies the one master.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"time"
)

const (
	// airplayVolumeMinDB is the bottom of the AirPlay slider. The reference's
	// linear mapping turns it (and its -144 mute value) into unit 0.
	airplayVolumeMinDB = -30.0
	// airplayVolumeMuteDB is the reference's explicit mute value.
	airplayVolumeMuteDB = -144.0
	// masterVolumeTimeout bounds one framework write. The SDK call must never
	// outlive a request by an unbounded amount when the daemon stalls.
	masterVolumeTimeout = 3 * time.Second
	// masterVolumeEpsilon absorbs float32 storage quantization between the
	// value the framework applied and the value GetAudioState reports back. It
	// is deliberately far below any sender slider step, so a legitimate small
	// move is still mirrored.
	masterVolumeEpsilon = 1e-6
	// masterVolumeEchoWindow bounds correlation with a dvlc value this session
	// actually sent. It is consumed once and never used to ignore a divergent
	// first drag; the owner's next SET after an echo is applied normally.
	masterVolumeEchoWindow = 500 * time.Millisecond
)

// masterVolumeReadTimeout bounds one framework read. It is a variable so tests
// can shorten it; production request replies and the mirror watcher must never
// wait longer than the reservation/teardown budgets.
var masterVolumeReadTimeout = 2 * time.Second

var (
	errMasterVolumeNotOwner    = errors.New("only the authenticated active sender controls the master volume")
	errMasterVolumeUnavailable = errors.New("the framework master volume is not writable")
	errMasterVolumeInvalid     = errors.New("master volume must be a finite value in [0,1]")
)

// unitVolumeFromDB maps an AirPlay dB slider position to the framework's 0..1
// master scale. The valid AirPlay domain is {-144} ∪ [-30,0]: exactly -144 (and
// anything at or below the -30 dB floor) is unit 0, and the -30..0 span is
// linear (unitVolume = dB/30 + 1). A positive value is out of contract and
// reports false rather than raising the speaker to 100%; the pinned reference's
// unit conversion also yields 0 for it. Non-finite input is not a slider
// position and is ignored as well.
func unitVolumeFromDB(dB float64) (float64, bool) {
	if math.IsNaN(dB) || math.IsInf(dB, 0) {
		return 0, false
	}
	if dB > 0 {
		return 0, false
	}
	if dB <= airplayVolumeMinDB {
		return 0, true
	}
	return dB/30 + 1, true
}

// volumeDBFromUnit is the inverse used by /info, updateInfo and GET_PARAMETER.
// Unit 0 is the reference's mute value, -144 dB; otherwise the same linear
// transfer returns 30*unit-30 (0 dB at full scale). Non-finite or out-of-range
// input reports false rather than inventing a volume.
func volumeDBFromUnit(unit float64) (float64, bool) {
	if math.IsNaN(unit) || math.IsInf(unit, 0) || unit < 0 || unit > 1 {
		return 0, false
	}
	if unit <= 0 {
		return airplayVolumeMuteDB, true
	}
	if unit >= 1 {
		return 0, true
	}
	return 30*unit - 30, true
}

func validMasterUnit(unit float64) bool {
	return !math.IsNaN(unit) && !math.IsInf(unit, 0) && unit >= 0 && unit <= 1
}

func unitsEqual(a, b float64) bool {
	return math.Abs(a-b) <= masterVolumeEpsilon
}

// masterSnapshot returns the authoritative last-known master level. known is
// false until the framework has reported one; this cache is for replies and a
// slow-read fallback, never for a sender's display state.
func (m *Manager) masterSnapshot() (unit float64, known bool) {
	m.masterMu.Lock()
	defer m.masterMu.Unlock()
	return m.masterUnit, m.masterKnown
}

// masterWriteVersion returns the applied-write sequence. A read captures it
// before the framework call and uses it to discard a value that a newer write
// has already superseded.
func (m *Manager) masterWriteVersion() uint64 {
	m.masterMu.Lock()
	defer m.masterMu.Unlock()
	return m.masterWriteSeq
}

// storeMasterWrite records an applied framework write and advances the
// sequence, so any read that started earlier cannot overwrite it.
func (m *Manager) storeMasterWrite(unit float64) {
	if !validMasterUnit(unit) {
		return
	}
	m.masterMu.Lock()
	m.masterUnit = unit
	m.masterKnown = true
	m.masterWriteSeq++
	m.masterMu.Unlock()
}

// acceptMasterRead records a completed read only when no write landed while it
// was in flight. When one did, the write's level is the latest truth.
func (m *Manager) acceptMasterRead(seq uint64, unit float64) (float64, bool) {
	m.masterMu.Lock()
	defer m.masterMu.Unlock()
	if m.masterWriteSeq != seq {
		return m.masterUnit, m.masterKnown
	}
	if !validMasterUnit(unit) {
		return m.masterUnit, m.masterKnown
	}
	m.masterUnit = unit
	m.masterKnown = true
	return unit, true
}

// readMasterVolume performs one framework master read bounded by the caller's
// session context. It deliberately takes no ownership lock: a slow getter must
// never delay commit, session end, receiver close or an RTSP reply past the
// read bound, and closing the session cancels a parked read so the manager can
// join the session goroutine.
func (m *Manager) readMasterVolume(parent context.Context) (float64, bool) {
	read := m.cfg.MasterVolumeContext
	if read == nil {
		return m.masterSnapshot()
	}
	seq := m.masterWriteVersion()
	ctx, cancel := context.WithTimeout(parent, masterVolumeReadTimeout)
	defer cancel()
	level, ok := read(ctx)
	if !ok || !validMasterUnit(level) {
		return m.masterSnapshot()
	}
	return m.acceptMasterRead(seq, level)
}

// currentMasterVolume reads the master for an /info, updateInfo or
// GET_PARAMETER reply. It never touches a session's delivered/display state:
// a candidate's probe or the owner's own GET cannot consume a pending
// receiver-side change notification.
func (m *Manager) currentMasterVolume(parent context.Context) (float64, bool) {
	return m.readMasterVolume(parent)
}

// canWriteMaster reports whether s may still write. It is checked under the
// narrow write lock so a close or takeover that completed while a request was
// queued cannot authorize a stale write.
func (m *Manager) canWriteMaster(s *Session) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active == s && !m.closed
}

// setMasterVolume writes the framework master for one session. volumeOpMu is
// held across the ownership check and the framework call, so a takeover, a
// terminal session end or receiver close that runs while a write is in flight
// completes after it, and every later write from the old session sees
// owns=false. Resource locks are never held across the call.
func (m *Manager) setMasterVolume(s *Session, level float64) (float64, error) {
	if !validMasterUnit(level) {
		return 0, errMasterVolumeInvalid
	}
	setter := m.cfg.SetMasterVolume
	if setter == nil {
		return 0, errMasterVolumeUnavailable
	}
	m.volumeOpMu.Lock()
	defer m.volumeOpMu.Unlock()
	if !m.canWriteMaster(s) {
		return 0, errMasterVolumeNotOwner
	}
	ctx, cancel := context.WithTimeout(s.masterVolumeContext(), masterVolumeTimeout)
	defer cancel()
	applied, err := setter(ctx, level)
	if err != nil {
		return 0, fmt.Errorf("set framework master volume: %w", err)
	}
	if !validMasterUnit(applied) {
		return 0, fmt.Errorf("framework applied an invalid master volume %v", applied)
	}
	m.storeMasterWrite(applied)
	return applied, nil
}

// masterVolumeContext is the parent for a master write. A session that ends
// while a framework call is in flight cancels it; a literal Session in a test
// may have no context and falls back to Background.
func (s *Session) masterVolumeContext() context.Context {
	if s.masterCtx != nil {
		return s.masterCtx
	}
	return context.Background()
}

func (s *Session) cancelMasterVolume() {
	if s.masterCancel != nil {
		s.masterCancel()
	}
}

// noteSeedMaster records the master unit carried by a successfully written
// updateInfo. A replacement channel owns the seed and token: any older token
// is invalidated and a queued mirror pass is ordered out.
func (s *Session) noteSeedMaster(unit float64) {
	if !validMasterUnit(unit) {
		return
	}
	s.displayMu.Lock()
	s.displayUnit = unit
	s.displayKnown = true
	s.displaySeq++
	s.senderEpoch++
	s.mirrorArmed = false
	s.mirrorPending = false
	s.mirrorReserved = false
	s.mirrorTokenID++
	s.mirrorWriter = nil
	s.mirrorGen = 0
	s.displayMu.Unlock()
}

// deliveredMaster returns what the current event channel has been told, if it
// has been seeded by updateInfo or a previous dvlc.
func (s *Session) deliveredMaster() (float64, bool) {
	s.displayMu.Lock()
	defer s.displayMu.Unlock()
	return s.displayUnit, s.displayKnown
}

// senderDeliveryEpoch is captured with a master reading and rechecked under the
// event-writer order before a queued mirror may reserve the channel.
func (s *Session) senderDeliveryEpoch() uint64 {
	s.displayMu.Lock()
	defer s.displayMu.Unlock()
	return s.senderEpoch
}

// beginSenderWrite marks an accepted owner SET pending before its framework
// call, so a mirror pass cannot reserve or send while it is parked.
func (s *Session) beginSenderWrite() {
	s.displayMu.Lock()
	s.senderEpoch++
	s.senderPending++
	s.displayMu.Unlock()
}

// finishSenderWrite clears the pending boundary and bumps the epoch again, so a
// mirror pass that started during the parked SET aborts even if it read the old
// master. On success it records the sender's display value without clearing a
// valid older echo token (a delayed A must not undo genuine B).
func (s *Session) finishSenderWrite(unit float64, applied bool) {
	s.displayMu.Lock()
	if s.senderPending > 0 {
		s.senderPending--
	}
	s.senderEpoch++
	if applied && validMasterUnit(unit) {
		s.displayUnit = unit
		s.displayKnown = true
		s.displaySeq++
	}
	s.displayMu.Unlock()
}

// stageMirrorIfEpoch atomically validates the sender epoch/pending boundary and
// reserves the one echo token before any bytes. A stale pass returns false
// without touching an older valid token.
func (s *Session) stageMirrorIfEpoch(expectedEpoch uint64, unit float64, generation uint64, writer io.Writer) (tokenID, displaySeq uint64, ok bool) {
	s.displayMu.Lock()
	defer s.displayMu.Unlock()
	if s.senderEpoch != expectedEpoch || s.senderPending != 0 {
		return 0, 0, false
	}
	s.mirrorTokenID++
	s.mirrorUnit = unit
	s.mirrorWriter = writer
	s.mirrorGen = generation
	s.mirrorArmed = true
	s.mirrorPending = true
	s.mirrorReserved = true
	s.mirrorSentAt = time.Time{}
	return s.mirrorTokenID, s.displaySeq, true
}

// cancelMirror clears only the matching pending reservation after a failed
// write; a replacement seed or newer dvlc owns a different token.
func (s *Session) cancelMirror(tokenID uint64) {
	s.displayMu.Lock()
	if s.mirrorTokenID == tokenID {
		s.mirrorArmed = false
		s.mirrorPending = false
		s.mirrorReserved = false
	}
	s.displayMu.Unlock()
}

// completeMirror transitions the one notification from pending to sent. It
// checks the live writer/generation under eventMu before displayMu (the only
// nested order), so a replacement or unpublish invalidates delivery even if
// updateInfo has not run. Delivery still records when an echo consumed the
// token during the Write; a newer display update suppresses only that record,
// while the sent token remains for a delayed echo.
func (s *Session) completeMirror(tokenID, displaySeq uint64, unit float64, writer io.Writer, generation uint64) {
	s.eventMu.Lock()
	current, ready, currentGen := s.eventOut, s.eventReady, s.eventGen
	s.displayMu.Lock()
	if s.mirrorTokenID != tokenID || s.mirrorGen != generation {
		s.displayMu.Unlock()
		s.eventMu.Unlock()
		return
	}
	if !ready || current != writer || currentGen != generation {
		s.mirrorArmed = false
		s.mirrorPending = false
		s.mirrorReserved = false
		s.displayMu.Unlock()
		s.eventMu.Unlock()
		return
	}
	if s.mirrorReserved {
		s.mirrorReserved = false
		// What actually went on the wire is this dvlc, even if a newer genuine
		// SET advanced the display while Write was parked. Leave displaySeq
		// untouched in that superseded case: the next poll sees the newer
		// authoritative master against this delivered value and repairs the
		// sender presentation without writing the master.
		s.displayUnit = unit
		s.displayKnown = true
		if s.displaySeq == displaySeq {
			s.displaySeq++
		}
	}
	s.mirrorPending = false
	s.mirrorSentAt = time.Now()
	s.displayMu.Unlock()
	s.eventMu.Unlock()
}

// mirrorActive reports whether the one current-generation notification token is
// still armed. While it is pending or inside its post-delivery expiry window,
// a new mirror pass defers: consuming the echo or expiry releases the next
// regular pass to reconcile, with no extra polling or history.
func (s *Session) mirrorActive(writer io.Writer, generation uint64) bool {
	s.displayMu.Lock()
	defer s.displayMu.Unlock()
	if !s.mirrorArmed || s.mirrorGen != generation || s.mirrorWriter != writer {
		return false
	}
	if s.mirrorPending {
		return true
	}
	return !s.mirrorSentAt.IsZero() && time.Since(s.mirrorSentAt) <= masterVolumeEchoWindow
}

// takeMirrorEcho reports whether a sender SET exactly matches the one dvlc
// notification this session staged/sent on the live writer and generation. A
// pending write matches regardless of TTL; a sent value uses the narrow TTL
// that started at successful delivery. Consuming the token does not clear the
// delivery reservation. A replacement/unpublish invalidates matching at once.
func (s *Session) takeMirrorEcho(unit float64) bool {
	s.eventMu.Lock()
	current, currentGen := s.eventOut, s.eventGen
	s.displayMu.Lock()
	defer s.displayMu.Unlock()
	defer s.eventMu.Unlock()
	if !s.mirrorArmed || current != s.mirrorWriter || currentGen != s.mirrorGen || !unitsEqual(s.mirrorUnit, unit) {
		return false
	}
	if !s.mirrorPending && !s.mirrorSentAt.IsZero() && time.Since(s.mirrorSentAt) > masterVolumeEchoWindow {
		return false
	}
	s.mirrorArmed = false
	return true
}

// controlsMasterVolume reports whether this request arrives on a HAP-keyed
// channel of the promoted, still-current media owner. Transient pair-setup and
// pair-verify both key a channel; a plaintext legacy RAOP connection, a
// candidate that has only probed or paired, an event-only/type-130 control
// setup and a superseded session do not. Pause keeps ownership, so a paused
// sender may still change the device level.
func (s *Session) controlsMasterVolume() bool {
	if !s.promoted.Load() {
		return false
	}
	s.mu.Lock()
	authenticated := s.encrypted
	s.mu.Unlock()
	return authenticated && s.m.owns(s)
}

// applySenderVolume routes one SET_PARAMETER volume line to the master. It
// reports whether the value was applied. An unauthenticated, candidate,
// event-only, superseded or malformed request is a no-op: the reference
// answers SET_PARAMETER 200 and merely logs that a non-principal connection
// could not change the global volume, and refusing would invite a sender to
// hide its slider.
func (s *Session) applySenderVolume(dB float64) bool {
	if !s.controlsMasterVolume() {
		s.m.logf("session from %s: volume change ignored: not the authenticated active sender", s.peer.Address)
		return false
	}
	level, ok := unitVolumeFromDB(dB)
	if !ok {
		s.m.logf("session from %s: volume change ignored: %v dB is not a slider position", s.peer.Address, dB)
		return false
	}
	if s.takeMirrorEcho(level) {
		// A delayed echo of a dvlc we actually sent must not overwrite a newer
		// receiver-side level. The token is one-shot; the next drag applies.
		s.m.logf("session from %s: volume change ignored: delayed echo of the mirrored level", s.peer.Address)
		return false
	}
	// Order this accepted sender write before any queued mirror pass that may
	// still hold an older reading.
	s.beginSenderWrite()
	_, err := s.m.setMasterVolume(s, level)
	s.finishSenderWrite(level, err == nil)
	if err != nil {
		s.m.logf("session from %s: volume change failed: %v", s.peer.Address, err)
		return false
	}
	return true
}

// masterVolumeDB is the AirPlay dB value reported in /info, updateInfo and
// GET_PARAMETER. It always comes from the same master read the dvlc path uses.
// When the framework has never yielded a level, the safe reported value is the
// reference's -144 dB mute, not 0 dB: a fresh receiver must not make a sender
// draw or adopt a full-volume slider. This is a report only; nothing writes the
// actual master from this fallback.
func (s *Session) masterVolumeDB() float64 {
	unit, ok := s.m.currentMasterVolume(s.masterVolumeContext())
	if !ok {
		return airplayVolumeMuteDB
	}
	dB, ok := volumeDBFromUnit(unit)
	if !ok {
		return airplayVolumeMuteDB
	}
	return dB
}
