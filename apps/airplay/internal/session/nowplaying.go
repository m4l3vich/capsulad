package session

// AirPlay 2 now-playing events.
//
// A modern AirPlay 2 sender does not use the legacy DMAP
// `application/x-dmap-tagged` SET_PARAMETER for now-playing information. It
// posts binary plists as `POST /command` on the main encrypted RTSP control
// channel (the same connection as `/feedback`); this receiver also accepts the
// same body on its bidirectional event channel, which is a tolerance rather
// than the Apple wire path. The command envelope is:
//
//	{"type":"updateMRNowPlayingInfo",
//	 "params":{"type":"npi-text",
//	           "mergePolicy":"replace"|"update",
//	           "params":{"kMRMediaRemoteNowPlayingInfoTitle":"…",
//	                     "kMRMediaRemoteNowPlayingInfoArtworkData":<data>, …}}}
//
// and a separate playback-state command:
//
//	{"type":"updateMRPlaybackState",
//	 "params":{"mrPlaybackState":1|2|3|4}}
//
// Shairport Sync reads exactly these keys and this nesting in
// metadata/metadata_hub_handle_command_plist.c, dispatched by rtsp.c's
// handle_command on the control connection. The keys are the public
// media-remote now-playing vocabulary, and the wrapper is what the sender
// puts on the wire; no private API is involved. "replace" resets the record
// before the event is merged, "update" keeps fields the event omits. The
// receiver therefore must not look for the keys in the outer "params" dict:
// a real Apple sender's title, album and artwork live one level deeper, and
// a flat lookup leaves both the media contract and the ring without them.

import (
	"math"
	"strings"

	"capsulad/apps/airplay/internal/artwork"
	"capsulad/apps/airplay/internal/dmap"
	"capsulad/apps/airplay/internal/media"
)

// The now-playing record keys the event carries.
const (
	nowPlayingKeyTitle        = "kMRMediaRemoteNowPlayingInfoTitle"
	nowPlayingKeyArtist       = "kMRMediaRemoteNowPlayingInfoArtist"
	nowPlayingKeyAlbum        = "kMRMediaRemoteNowPlayingInfoAlbum"
	nowPlayingKeyDuration     = "kMRMediaRemoteNowPlayingInfoDuration"
	nowPlayingKeyElapsedTime  = "kMRMediaRemoteNowPlayingInfoElapsedTime"
	nowPlayingKeyPlaybackRate = "kMRMediaRemoteNowPlayingInfoPlaybackRate"
	nowPlayingKeyArtwork      = "kMRMediaRemoteNowPlayingInfoArtworkData"
)

// maxNowPlayingMillis bounds a duration/elapsed conversion so a hostile real
// value cannot wrap the uint64 millisecond fields. It is far beyond any track.
const maxNowPlayingMillis = 1 << 40

// applyCommandPlist interprets one plist body from the sender's main RTSP
// control connection, its event channel or a plist SET_PARAMETER. A
// recognized now-playing command is applied as the sender wrote it; any other
// plist is still scanned for a device name and the legacy flat shape, so the
// older senders and fixtures keep working.
func (s *Session) applyCommandPlist(dict map[string]any) {
	if name := plistString(dict["deviceName"]); name != "" {
		s.setDeviceName(name)
	}
	switch command := plistString(dict["type"]); command {
	case "updateMRNowPlayingInfo":
		params, ok := plistDict(dict["params"])
		if !ok {
			return
		}
		fields, ok := nowPlayingFields(params)
		if !ok {
			return
		}
		replace := strings.EqualFold(plistString(params["mergePolicy"]), "replace")
		s.applyNowPlayingInfo(fields, replace)
		return
	case "updateMRPlaybackState":
		params, ok := plistDict(dict["params"])
		if !ok {
			return
		}
		if state, ok := plistUint64(params["mrPlaybackState"]); ok {
			s.setSenderPlaybackState(state)
		}
		return
	case "":
		// No command envelope: the pre-command flat form.
	default:
		// Other media-remote commands (supported commands, volume, …) carry
		// no now-playing fields and must not be read as one.
		return
	}
	fields := dict
	if nested, ok := plistDict(dict["params"]); ok {
		fields = nested
	}
	// nowPlayingKeyPlaybackRate is informational in the now-playing record.
	// The established receiver records it for display but never lets it move
	// the audio transport or discard queued audio; that is the job of
	// SETRATEANCHORTI/SETRATEANCHORTIME, PAUSE and RECORD. It is therefore
	// read only as part of the record shape and not applied here.
	s.applyNowPlayingInfo(fields, false)
}

// nowPlayingFields locates the dictionary an npi-text command carries. The
// observed wire shape nests it under params.params; a fixture or sender
// variant that puts the record directly in params is accepted as the same
// record because the record keys identify it, not the nesting depth.
func nowPlayingFields(params map[string]any) (map[string]any, bool) {
	kind := plistString(params["type"])
	if fields, ok := plistDict(params["params"]); ok {
		// A nested record requires the sender's exact npi-text marker. A
		// malformed command that omits it must not be interpreted as a
		// replace record and clear the live metadata.
		if kind != "npi-text" {
			return nil, false
		}
		return fields, true
	}
	// The legacy flat shape keeps its own rule: the record keys themselves
	// identify it, and npi-text is tolerated when the sender labels it.
	if kind != "" && !strings.EqualFold(kind, "npi-text") {
		return nil, false
	}
	if hasNowPlayingFields(params) {
		return params, true
	}
	return nil, false
}

func hasNowPlayingFields(fields map[string]any) bool {
	for _, key := range [...]string{
		nowPlayingKeyTitle, nowPlayingKeyArtist, nowPlayingKeyAlbum,
		nowPlayingKeyDuration, nowPlayingKeyElapsedTime,
		nowPlayingKeyPlaybackRate, nowPlayingKeyArtwork,
	} {
		if _, present := fields[key]; present {
			return true
		}
	}
	return false
}

// applyNowPlayingInfo merges one record into the session and publishes the
// result to the media contract. A replace clears fields the new record omits
// (including the cover palette), while an update keeps them. PlaybackRate is
// informational and deliberately does not touch the transport.
func (s *Session) applyNowPlayingInfo(fields map[string]any, replace bool) {
	title, hasTitle := fields[nowPlayingKeyTitle].(string)
	artist, hasArtist := fields[nowPlayingKeyArtist].(string)
	album, hasAlbum := fields[nowPlayingKeyAlbum].(string)
	durationSeconds, hasDuration := plistFloat(fields[nowPlayingKeyDuration])
	elapsedSeconds, hasElapsed := plistFloat(fields[nowPlayingKeyElapsedTime])

	// Decode artwork before taking any lock: it is the only CPU-heavy step and
	// must not stall the session's PCM publication. The following merge,
	// palette change and board write are one serialized publication, so a
	// concurrent update or promotion replay cannot interleave between them.
	var palette []artwork.RGB
	var decodeErr error
	if data := plistData(fields[nowPlayingKeyArtwork]); len(data) > 0 {
		palette, decodeErr = artwork.Palette(data)
	}

	s.publishMu.Lock()
	s.mu.Lock()
	if replace {
		s.metadata = dmap.Metadata{}
		s.npiDurationMS = 0
		s.npiElapsedMS = 0
		s.npiHasDuration = false
		s.npiHasElapsed = false
	}
	if hasTitle {
		s.metadata.Title = title
	}
	if hasArtist {
		s.metadata.Artist = artist
	}
	if hasAlbum {
		s.metadata.Album = album
	}
	if hasDuration {
		if ms, ok := nowPlayingMillis(durationSeconds); ok {
			s.npiDurationMS, s.npiHasDuration = ms, true
		} else {
			s.npiDurationMS, s.npiHasDuration = 0, false
		}
	}
	if hasElapsed {
		if ms, ok := nowPlayingMillis(elapsedSeconds); ok {
			s.npiElapsedMS, s.npiHasElapsed = ms, true
		} else {
			s.npiElapsedMS, s.npiHasElapsed = 0, false
		}
	}
	// The artwork belongs to the merged record. A replace without a decodable
	// image clears the previous cover instead of showing the old track's
	// colours; an update without the key leaves the current cover in place.
	if replace || (decodeErr == nil && len(palette) > 0) {
		s.palette = palette
	}
	metadata := s.metadata
	duration, position := s.nowPlayingProgressLocked()
	gen := s.boardGen
	s.mu.Unlock()

	if s.m.board != nil && gen != 0 {
		s.m.board.SetMetadata(gen, metadata.Title, metadata.Artist, metadata.Album, duration, position)
	}
	s.publishMu.Unlock()

	if decodeErr != nil {
		s.m.logf("session %s: now-playing artwork rejected: %v", s.peer.Address, decodeErr)
	}
	if replace || (decodeErr == nil && len(palette) > 0) {
		if length := len(palette); length > 0 {
			s.m.logf("session %s: cover art palette %d colour(s)", s.peer.Address, length)
		}
	}
	if metadata.Title != "" {
		s.m.logf("session %s: %s — %s", s.peer.Address, metadata.Title, metadata.Artist)
	}
}

// setSenderPlaybackState records the sender's play/pause/stop report. The
// state is kept for replay because a candidate can report it before it owns
// the board; promotion reapplies it with the same display precedence used
// live.
func (s *Session) setSenderPlaybackState(state uint64) {
	var next media.PlaybackState
	switch state {
	case 1:
		next = media.StatePlaying
	case 2:
		next = media.StatePaused
	case 3:
		next = media.StateStopped
	default:
		// 4 means "not available"; this player has no equivalent state and
		// must not invent one.
		return
	}
	s.publishMu.Lock()
	s.mu.Lock()
	s.pendingPlayback = next
	gen := s.boardGen
	buffered := s.hasBufferedMediaLocked()
	bufferedReady := s.bufferedPCMReadyLocked()
	s.mu.Unlock()
	shown, ok := senderPlaybackState(next, buffered, bufferedReady)
	if ok && s.m.board != nil && gen != 0 {
		s.m.board.SetPlayback(gen, shown)
	}
	s.publishMu.Unlock()
}

// senderPlaybackState applies the display precedence shared by a live report
// and its replay on promotion. A sender's paused/stopped report is
// authoritative over PCM-derived playing; "playing" is shown only once decoded
// PCM has reached the sink for a type-103 stream. It changes only the reported
// display state: the transport keeps its own rate/anchor rules.
func senderPlaybackState(report media.PlaybackState, buffered, bufferedReady bool) (media.PlaybackState, bool) {
	switch report {
	case media.StatePaused, media.StateStopped:
		return report, true
	case media.StatePlaying:
		if buffered && !bufferedReady {
			return media.StateBuffering, true
		}
		return media.StatePlaying, true
	default:
		return "", false
	}
}

// publishRealtimePCMPlaying reports playing after a realtime sink write.
// Buffered PCM uses bufferedStream.commitWrite, which ties the same report to
// the stream's current playout generation. An explicit paused or stopped
// report outranks either automatic evidence.
func (s *Session) publishRealtimePCMPlaying() {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	s.mu.Lock()
	gen := s.boardGen
	report := s.pendingPlayback
	s.mu.Unlock()
	if gen == 0 || s.m.board == nil {
		return
	}
	if report == media.StatePaused || report == media.StateStopped {
		return
	}
	s.m.board.SetPlayback(gen, media.StatePlaying)
}

// reconcilePlaybackState reapplies an explicit report after a stream becomes
// active. A connection can report playing before its type-103 SETUP; once that
// stream exists, the player returns to buffering until decoded PCM arrives.
func (s *Session) reconcilePlaybackState() {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	s.mu.Lock()
	gen := s.boardGen
	report := s.pendingPlayback
	buffered := s.hasBufferedMediaLocked()
	bufferedReady := s.bufferedPCMReadyLocked()
	s.mu.Unlock()
	if gen == 0 || s.m.board == nil || report == "" {
		return
	}
	if shown, ok := senderPlaybackState(report, buffered, bufferedReady); ok {
		s.m.board.SetPlayback(gen, shown)
	}
}

// publishTransportPaused reports a transport pause when publish is set. The
// caller retires the affected buffered generations before calling this, so no
// stale write can publish PLAYING after the pause. It never lets the pause
// replace a sender's explicit stopped state.
func (s *Session) publishTransportPaused(publish bool) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	s.mu.Lock()
	gen := s.boardGen
	report := s.pendingPlayback
	s.mu.Unlock()
	if !publish || gen == 0 || s.m.board == nil || report == media.StateStopped {
		return
	}
	s.m.board.SetPlayback(gen, media.StatePaused)
}

// nowPlayingProgressLocked returns the duration and position to publish. The
// AirPlay 2 plist record carries them; the legacy RAOP path reports a
// `progress:` text parameter instead. Callers hold s.mu.
func (s *Session) nowPlayingProgressLocked() (duration, position uint64) {
	if s.npiHasDuration || s.npiHasElapsed {
		return s.npiDurationMS, s.npiElapsedMS
	}
	if s.progress.has {
		position = uint64(s.progress.current)
		if s.progress.end > s.progress.start {
			duration = uint64(s.progress.end - s.progress.start)
		}
	}
	return duration, position
}

// nowPlayingMillis converts a seconds real from the now-playing record to
// milliseconds. A negative, non-finite or absurd value is not a timeline and
// is reported as absent.
func nowPlayingMillis(seconds float64) (uint64, bool) {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 {
		return 0, false
	}
	ms := seconds * 1000
	if ms > maxNowPlayingMillis {
		return 0, false
	}
	return uint64(ms), true
}
