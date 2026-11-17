// Package media holds the AirPlay receiver's player state and the audio
// scalers that stand between the sender's volume and the framework mixer.
//
// The app exposes exactly one player to the framework's
// capsulad.contracts.media.v1.MediaPlayerService contract: the live AirPlay
// session. Volume is applied inside the app because master volume is an
// administrator-only setting for a supervised app (see
// docs/app_development/audio.md); the framework's master volume therefore
// remains the ceiling over everything this app contributes.
package media

import (
	"math"
	"strings"
	"sync"
)

// PlaybackState mirrors capsulad.contracts.media.v1.PlaybackState by name so
// the JSON stays canonical protobuf JSON.
type PlaybackState string

const (
	StateStopped   PlaybackState = "PLAYBACK_STOPPED"
	StatePlaying   PlaybackState = "PLAYBACK_PLAYING"
	StatePaused    PlaybackState = "PLAYBACK_PAUSED"
	StateBuffering PlaybackState = "PLAYBACK_BUFFERING"
)

// Actions mirrors capsulad.contracts.media.v1.MediaAction.
const (
	ActionPlay     = "MEDIA_PLAY"
	ActionPause    = "MEDIA_PAUSE"
	ActionSkipNext = "MEDIA_SKIP_NEXT"
	ActionSkipPrev = "MEDIA_SKIP_PREVIOUS"
)

// PlayerID is the only player this app ever exposes.
const PlayerID = "airplay"

// AppName is the display-friendly source application for every status.
const AppName = "AirPlay"

// Status is one player snapshot in canonical protobuf JSON form.
type Status struct {
	PlaybackState PlaybackState
	SourceName    string
	MediaName     string
	Title         string
	Artist        string
	Album         string
	DurationMS    uint64
	PositionMS    uint64
	Revision      uint64
}

// Snapshot renders the status as canonical protobuf JSON. 64-bit integers are
// strings there, and empty optional strings are omitted.
func (s Status) Snapshot() map[string]any {
	out := map[string]any{
		"playerId":         PlayerID,
		"playbackState":    string(s.PlaybackState),
		"supportedActions": []any{ActionPlay, ActionPause, ActionSkipNext, ActionSkipPrev},
		"appName":          AppName,
		"sourceName":       s.SourceName,
		"revision":         formatUint(s.Revision),
	}
	if s.MediaName != "" {
		out["mediaName"] = s.MediaName
	}
	if s.Title != "" {
		out["title"] = s.Title
	}
	if s.Artist != "" {
		out["artist"] = s.Artist
	}
	if s.Album != "" {
		out["album"] = s.Album
	}
	if s.DurationMS > 0 {
		out["durationMs"] = formatUint(s.DurationMS)
	}
	out["positionMs"] = formatUint(s.PositionMS)
	return out
}

func formatUint(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// Board is the mutable player state shared with the media-contract handlers.
// Every mutation bumps the revision and notifies subscribers, so a WatchPlayers
// stream converges on the newest snapshot.
//
// Mutations are scoped to one AirPlay session: BeginSession mints a generation
// and hands it to the caller, every later mutation must carry it, and a
// mutation carrying any other generation is dropped (StaleWrites counts the
// dropped calls). A superseded session therefore cannot repaint the player the
// newest sender owns, however late its playLoop wakes up.
type Board struct {
	mu       sync.Mutex
	state    Status
	gen      uint64 // newest generation handed out by BeginSession
	session  uint64 // generation that currently owns the player; 0 while idle
	stale    uint64 // mutations dropped because their generation was not current
	subs     map[uint64]chan map[string]any
	nextSub  uint64
	onChange func()
}

// NewBoard returns an idle board with no live session.
func NewBoard() *Board {
	return &Board{state: Status{PlaybackState: StateStopped, SourceName: "no sender"}, subs: map[uint64]chan map[string]any{}}
}

// SetOnChange installs a callback invoked (outside the lock) after a change.
func (b *Board) SetOnChange(fn func()) {
	b.mu.Lock()
	b.onChange = fn
	b.mu.Unlock()
}

// Status returns the current status with its revision.
func (b *Board) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Snapshot returns the canonical JSON player status.
func (b *Board) Snapshot() map[string]any {
	return b.Status().Snapshot()
}

// Live reports whether an AirPlay session currently owns the player.
func (b *Board) Live() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.session != 0
}

// CurrentSession returns the generation that owns the player, or 0 while no
// session is live.
func (b *Board) CurrentSession() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.session
}

// StaleWrites returns how many mutations were dropped because they carried a
// generation that was no longer current. Tests use it to prove the guard fires.
func (b *Board) StaleWrites() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stale
}

// BeginSession marks the player as present for the given sender and returns the
// generation that now owns it. Any previous generation is superseded: its
// later mutations are dropped.
func (b *Board) BeginSession(sourceName string) uint64 {
	b.mu.Lock()
	b.gen++
	b.session = b.gen
	generation := b.gen
	b.state.SourceName = sourceName
	b.state.PlaybackState = StateBuffering
	b.state.MediaName, b.state.Title, b.state.Artist, b.state.Album = "", "", "", ""
	b.state.DurationMS, b.state.PositionMS = 0, 0
	fn := b.finishLocked()
	b.mu.Unlock()
	if fn != nil {
		fn()
	}
	return generation
}

// SessionEnded returns the player to its idle state when session still owns it.
// A generation that was already superseded ends nothing, so a late teardown of
// the old session cannot stop the player the newest sender owns.
func (b *Board) SessionEnded(session uint64) {
	b.mu.Lock()
	if !b.currentLocked(session) {
		b.mu.Unlock()
		return
	}
	b.session = 0
	b.state.PlaybackState = StateStopped
	b.state.SourceName = "no sender"
	b.state.MediaName, b.state.Title, b.state.Artist, b.state.Album = "", "", "", ""
	b.state.DurationMS, b.state.PositionMS = 0, 0
	fn := b.finishLocked()
	b.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// SetSourceName updates the sender's display name within the current
// generation. The RTSP User-Agent that names the sender arrives after the
// session begins, so the name is refined without starting a new session.
func (b *Board) SetSourceName(session uint64, sourceName string) {
	b.mu.Lock()
	if !b.currentLocked(session) || b.state.SourceName == sourceName {
		b.mu.Unlock()
		return
	}
	b.state.SourceName = sourceName
	fn := b.finishLocked()
	b.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// SetPlayback changes the playback state of the session that owns the player.
func (b *Board) SetPlayback(session uint64, state PlaybackState) {
	b.mu.Lock()
	if !b.currentLocked(session) || b.state.PlaybackState == state {
		b.mu.Unlock()
		return
	}
	b.state.PlaybackState = state
	fn := b.finishLocked()
	b.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// SetMetadata replaces the structured metadata and progress of the session that
// owns the player.
func (b *Board) SetMetadata(session uint64, title, artist, album string, durationMS, positionMS uint64) {
	b.mu.Lock()
	if !b.currentLocked(session) ||
		(b.state.Title == title && b.state.Artist == artist && b.state.Album == album &&
			b.state.DurationMS == durationMS && b.state.PositionMS == positionMS) {
		b.mu.Unlock()
		return
	}
	b.state.Title, b.state.Artist, b.state.Album = title, artist, album
	b.state.DurationMS, b.state.PositionMS = durationMS, positionMS
	b.state.MediaName = mediaName(title, artist)
	fn := b.finishLocked()
	b.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// SetProgress updates only the playback position of the session that owns the
// player.
func (b *Board) SetProgress(session uint64, positionMS uint64) {
	b.mu.Lock()
	if !b.currentLocked(session) || b.state.PositionMS == positionMS {
		b.mu.Unlock()
		return
	}
	b.state.PositionMS = positionMS
	fn := b.finishLocked()
	b.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// currentLocked reports whether session owns the player. Callers must hold b.mu.
// A mutation for any other generation is counted as stale and must be dropped.
func (b *Board) currentLocked(session uint64) bool {
	if session != 0 && session == b.session {
		return true
	}
	b.stale++
	return false
}

// finishLocked bumps the revision, publishes the new snapshot to every
// subscriber and returns the onChange hook for the caller to run once it has
// released b.mu. Callers must hold b.mu.
func (b *Board) finishLocked() func() {
	b.state.Revision++
	b.publishLocked()
	return b.onChange
}

func mediaName(title, artist string) string {
	switch {
	case title == "" && artist == "":
		return ""
	case title == "":
		return artist
	case artist == "":
		return title
	default:
		return title + " — " + artist
	}
}

// publishLocked offers the current snapshot to every subscriber without ever
// blocking. Subscribers converge on the newest snapshot, so a slow consumer
// loses intermediate events rather than stalling the sender.
//
// It runs with b.mu held, and that is exactly what makes unsubscription safe:
// unsubscribe closes the channel under the same lock, so a send and a close can
// never overlap and nothing can be sent after the close. Holding the lock also
// keeps published events ordered by revision. Callers must hold b.mu.
func (b *Board) publishLocked() {
	event := map[string]any{"updated": b.state.Snapshot()}
	for _, ch := range b.subs {
		select {
		case ch <- event:
		default:
		}
	}
}

// Subscribe returns a snapshot event, a channel of incremental events and an
// unsubscribe function. The first event is always a full snapshot.
//
// The returned channel is closed exactly once, by unsubscribe, under the board
// lock that also guards every publication: no event is sent after the close, no
// close can race a send, and the close cannot happen while a publication is in
// flight. Events already buffered when unsubscribe returns stay readable; after
// that the channel reads as closed. Further calls of the returned function are
// no-ops, so an extra teardown can neither panic nor close the channel twice.
func (b *Board) Subscribe() (map[string]any, <-chan map[string]any, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	revision := b.nextSub
	b.nextSub++
	ch := make(chan map[string]any, 8)
	b.subs[revision] = ch
	snapshot := map[string]any{"snapshot": map[string]any{"players": []any{b.state.Snapshot()}}}
	unsubscribe := func() {
		b.mu.Lock()
		if existing, ok := b.subs[revision]; ok {
			delete(b.subs, revision)
			close(existing)
		}
		b.mu.Unlock()
	}
	return snapshot, ch, unsubscribe
}

// NamesFromSource is the display name shown for a connected sender. The AirPlay
// sender identifies itself in the RTSP User-Agent header and in the plist
// device info; when neither yields a name the peer address is used.
func NamesFromSource(userAgent, deviceName, peer string) string {
	if deviceName != "" {
		return deviceName
	}
	if name := nameFromUserAgent(userAgent); name != "" {
		return name
	}
	return peer
}

func nameFromUserAgent(ua string) string {
	// "AirPlay/560.12.1 (MacBook Pro; macOS 15.1)" or
	// "iTunes/12.13.2 (Windows; ...)".
	open := strings.IndexByte(ua, '(')
	if open < 0 {
		return ""
	}
	rest := ua[open+1:]
	if close := strings.IndexByte(rest, ')'); close >= 0 {
		rest = rest[:close]
	}
	fields := strings.Split(rest, ";")
	if len(fields) == 0 {
		return ""
	}
	return strings.TrimSpace(fields[0])
}

// GainFromDB converts a sender volume in decibels to a linear gain.
//
// AirPlay carries the sender's slider as dB in [-30, 0] with -144 (and
// anything at or below -80) meaning silence. The framework mixer's master
// volume still multiplies on top, so the app can never exceed the ceiling.
func GainFromDB(dB float64) float64 {
	if math.IsNaN(dB) || dB <= -80 {
		return 0
	}
	if dB > 0 {
		dB = 0
	}
	gain := math.Pow(10, dB/20)
	if gain < 0 {
		return 0
	}
	if gain > 1 {
		return 1
	}
	return gain
}

// ScalePCM16 scales interleaved 16-bit little-endian PCM in place by gain.
// A gain of 1 is a no-op; gain 0 produces silence.
func ScalePCM16(pcm []byte, gain float64) {
	if gain == 1 || len(pcm) < 2 {
		return
	}
	if gain <= 0 {
		for i := range pcm {
			pcm[i] = 0
		}
		return
	}
	for i := 0; i+1 < len(pcm); i += 2 {
		sample := int32(int16(uint16(pcm[i]) | uint16(pcm[i+1])<<8))
		scaled := int32(float64(sample) * gain)
		if scaled > 32767 {
			scaled = 32767
		}
		if scaled < -32768 {
			scaled = -32768
		}
		value := uint16(int16(scaled))
		pcm[i] = byte(value)
		pcm[i+1] = byte(value >> 8)
	}
}
