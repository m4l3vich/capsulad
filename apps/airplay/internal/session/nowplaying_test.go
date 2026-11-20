package session

// Regression tests for the AirPlay 2 now-playing path used by Apple Music.
//
// A modern sender does not send DMAP metadata; it posts command plists as
// POST /command on the main encrypted RTSP control channel (the receiver also
// tolerates them on its bidirectional event channel). The receiver must read
// the nested npi-text record, publish its text and timeline through the media
// contract, and turn the artwork data into ring pixels. The same command's
// "replace" policy must clear fields the new record omits, so a different cover
// cannot keep the previous track's information or colours.

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"testing"
	"time"

	"capsulad/apps/airplay/internal/hapcrypt"
	"capsulad/apps/airplay/internal/media"
	"capsulad/apps/airplay/internal/plist"
)

// nowPlayingCommand builds the command plist a sender posts for one
// now-playing record.
func nowPlayingCommand(t *testing.T, mergePolicy string, fields map[string]any) []byte {
	t.Helper()
	body, err := plist.EncodeBinary(map[string]any{
		"type": "updateMRNowPlayingInfo",
		"params": map[string]any{
			"type":        "npi-text",
			"mergePolicy": mergePolicy,
			"params":      fields,
		},
	})
	if err != nil {
		t.Fatalf("encode now-playing command: %v", err)
	}
	return body
}

// coverPNG renders a 96×96 PNG whose left and right halves are the two given
// colours, so the extracted palette is exactly those two colours in order.
func coverPNG(t *testing.T, left, right color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 96, 96))
	for y := range 96 {
		for x := range 96 {
			shade := left
			if x >= 48 {
				shade = right
			}
			img.SetRGBA(x, y, shade)
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return encoded.Bytes()
}

func writeEventPlist(t *testing.T, event *hapcrypt.Session, cseq int, body []byte) {
	t.Helper()
	header := fmt.Sprintf("POST /command RTSP/1.0\r\nCSeq: %d\r\nContent-Type: application/x-apple-binary-plist\r\nContent-Length: %d\r\n\r\n", cseq, len(body))
	if _, err := event.Write(append([]byte(header), body...)); err != nil {
		t.Fatalf("write event command: %v", err)
	}
}

// readEventBlock reads one HAP block with a bound. ReadOne has no deadline of
// its own, and a test that waits forever hides the missing response.
func readEventBlock(t *testing.T, event *hapcrypt.Session, what string) []byte {
	t.Helper()
	type result struct {
		block []byte
		err   error
	}
	done := make(chan result, 1)
	go func() {
		block, err := event.ReadOne()
		done <- result{block: block, err: err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("read %s: %v", what, got.err)
		}
		return got.block
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out reading %s", what)
		return nil
	}
}

// readEventMessage reassembles HAP blocks until one complete RTSP message is
// available. A large updateInfo is split across blocks by the channel's
// framing, so reading a single block can stop in the middle of a write and
// deadlock a net.Pipe sender against it.
func readEventMessage(t *testing.T, event *hapcrypt.Session, what string) []byte {
	t.Helper()
	var message []byte
	for len(message) < 64*1024 {
		message = append(message, readEventBlock(t, event, what)...)
		if completeRTSPMessage(message) {
			return message
		}
	}
	t.Fatalf("%s did not form a complete RTSP message", what)
	return nil
}

func completeRTSPMessage(message []byte) bool {
	end := bytes.Index(message, []byte("\r\n\r\n"))
	if end < 0 {
		return false
	}
	length := 0
	for _, line := range bytes.Split(message[:end], []byte("\r\n")) {
		name, value, found := bytes.Cut(line, []byte(":"))
		if !found || !bytes.EqualFold(bytes.TrimSpace(name), []byte("Content-Length")) {
			continue
		}
		if _, err := fmt.Sscanf(string(bytes.TrimSpace(value)), "%d", &length); err != nil {
			return false
		}
	}
	return len(message) >= end+4+length
}

func waitBoard(t *testing.T, board *media.Board, what string, cond func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		last = board.Snapshot()
		if cond(last) {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; player = %#v", what, last)
	return nil
}

// TestAirPlay2NowPlayingCommandDrivesMediaRingAndCleanup is the session-level
// regression for Apple Music over AirPlay 2: pair-verify, encrypted SETUP and
// RECORD, then the command plists the sender really posts. It asserts the
// media contract's title/artist/album/timeline, two different cover palettes
// on the ring, the merge/interruption policy, playback-state feedback and
// teardown cleanup.
func TestAirPlay2NowPlayingCommandDrivesMediaRingAndCleanup(t *testing.T) {
	mgr, transport, _, ring, _, clientKey, pairingID := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	board := mgr.board

	stream, client := dialSender(t, mgr, transport)
	defer stream.Close()

	shared := pairVerify(t, client, pairingID, clientKey)
	control := hapcrypt.ControlChannelKeys(shared)
	encrypted, err := hapcrypt.NewSession(stream, control.Inbound, control.Outbound)
	if err != nil {
		t.Fatalf("control channel: %v", err)
	}
	client.setStream(&rwStream{rw: encrypted})

	shk := make([]byte, 32)
	setupBody, err := plist.EncodeBinary(map[string]any{
		"streams": []any{map[string]any{
			"type":        int64(streamTypeRealtime),
			"audioFormat": int64(1 << 11),
			"spf":         int64(352),
			"shk":         shk,
		}},
	})
	if err != nil {
		t.Fatalf("encode SETUP: %v", err)
	}
	setup, err := client.do("SETUP", "/1", "application/x-apple-binary-plist", setupBody)
	if err != nil || setup.status != 200 {
		t.Fatalf("SETUP = %v %v", setup, err)
	}
	waitUntil(t, "the encrypted sender to own the player", func() bool { return mgr.Active() != nil })

	record, err := client.do("RECORD", "/1", "", nil)
	if err != nil || record.status != 200 {
		t.Fatalf("RECORD = %v %v", record, err)
	}

	eventBody, err := plist.EncodeBinary(map[string]any{"gid": "npi-gid", "timingProtocol": "PTP"})
	if err != nil {
		t.Fatalf("encode event SETUP: %v", err)
	}
	eventSetup, err := client.do("SETUP", "/1", "application/x-apple-binary-plist", eventBody)
	if err != nil || eventSetup.status != 200 {
		t.Fatalf("event SETUP = %v %v", eventSetup, err)
	}
	decoded, err := plist.Decode(eventSetup.body)
	if err != nil {
		t.Fatalf("decode event SETUP: %v", err)
	}
	eventDict, _ := decoded.(map[string]any)
	eventPort := int(plistUint(eventDict["eventPort"]))
	if eventPort == 0 {
		t.Fatalf("event SETUP eventPort = %v", eventDict["eventPort"])
	}
	eventStream, err := transport.dial(eventPort)
	if err != nil {
		t.Fatalf("dial event channel: %v", err)
	}
	defer eventStream.Close()
	eventKeys := hapcrypt.EventChannelKeys(shared)
	event, err := hapcrypt.NewSession(eventStream, eventKeys.Inbound, eventKeys.Outbound)
	if err != nil {
		t.Fatalf("event channel keys: %v", err)
	}
	// The receiver announces the channel with updateInfo; consume it before
	// the command so the next block is this command's response.
	updateInfo := readEventMessage(t, event, "event updateInfo")
	if !bytes.HasPrefix(updateInfo, []byte("POST /command")) {
		t.Fatalf("event updateInfo = %q", updateInfo)
	}

	firstCover := coverPNG(t, color.RGBA{R: 255, A: 255}, color.RGBA{B: 255, A: 255})
	first := nowPlayingCommand(t, "replace", map[string]any{
		nowPlayingKeyTitle:       "Track One",
		nowPlayingKeyArtist:      "Artist One",
		nowPlayingKeyAlbum:       "Album One",
		nowPlayingKeyDuration:    240.5,
		nowPlayingKeyElapsedTime: 12.25,
		nowPlayingKeyArtwork:     firstCover,
	})
	writeEventPlist(t, event, 1, first)
	if response := readEventMessage(t, event, "the now-playing response"); !bytes.HasPrefix(response, []byte("RTSP/1.0 200")) {
		t.Fatalf("now-playing response = %q", response)
	}

	player := waitBoard(t, board, "the first track in the media contract", func(player map[string]any) bool {
		return player["title"] == "Track One"
	})
	if player["artist"] != "Artist One" || player["album"] != "Album One" {
		t.Fatalf("first track metadata = %#v", player)
	}
	if player["durationMs"] != "240500" || player["positionMs"] != "12250" {
		t.Fatalf("first track timeline = duration %v position %v", player["durationMs"], player["positionMs"])
	}
	if player["mediaName"] != "Track One — Artist One" {
		t.Fatalf("first track mediaName = %v", player["mediaName"])
	}

	// The ring replaces its neutral startup breath with the cover palette:
	// first pixel is the left (red) end, last is the right (blue) end.
	waitRing := func(what string, cond func([]byte) bool) []byte {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if frame := ring.lastFrame(); len(frame) == 36 && cond(frame) {
				return frame
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s; frame = %v", what, ring.lastFrame())
		return nil
	}
	waitRing("the first cover palette", func(frame []byte) bool {
		return frame[0] > 20 && frame[1] < 8 && frame[2] < 8 &&
			frame[33] < 8 && frame[34] < 8 && frame[35] > 20
	})

	// The same command envelope also arrives on the main encrypted RTSP
	// connection (Shairport Sync dispatches POST /command there). A record
	// carrying PlaybackRate 0 is metadata only: it updates the title but must
	// not pause the transport or discard queued audio.
	controlUpdate := nowPlayingCommand(t, "update", map[string]any{
		nowPlayingKeyTitle:        "Track One (Control)",
		nowPlayingKeyPlaybackRate: 0.0,
	})
	if response, err := client.do("POST", "/command", "application/x-apple-binary-plist", controlUpdate); err != nil || response.status != 200 {
		t.Fatalf("control POST /command = %v %v", response, err)
	}
	player = waitBoard(t, board, "the control-channel update", func(player map[string]any) bool {
		return player["title"] == "Track One (Control)"
	})
	if player["playbackState"] != string(media.StatePlaying) {
		t.Fatalf("PlaybackRate 0 changed playback state to %v", player["playbackState"])
	}
	if rate := mgr.Active().tvRate(); rate != 1 {
		t.Fatalf("PlaybackRate 0 moved the transport rate to %v", rate)
	}

	// A different cover and a replace update must replace every field the new
	// record carries, including the artwork.
	secondCover := coverPNG(t, color.RGBA{G: 255, A: 255}, color.RGBA{R: 255, G: 255, A: 255})
	second := nowPlayingCommand(t, "replace", map[string]any{
		nowPlayingKeyTitle:       "Track Two",
		nowPlayingKeyArtist:      "Artist Two",
		nowPlayingKeyAlbum:       "Album Two",
		nowPlayingKeyDuration:    180.0,
		nowPlayingKeyElapsedTime: 0.0,
		nowPlayingKeyArtwork:     secondCover,
	})
	// A full replacement with a different cover also travels the real Apple
	// path: POST /command on the encrypted control channel, not only the
	// tolerant event-channel path.
	if response, err := client.do("POST", "/command", "application/x-apple-binary-plist", second); err != nil || response.status != 200 {
		t.Fatalf("control POST /command full replacement = %v %v", response, err)
	}
	waitBoard(t, board, "the second track", func(player map[string]any) bool { return player["title"] == "Track Two" })
	waitRing("the second cover palette", func(frame []byte) bool {
		return frame[0] < 8 && frame[1] > 20 && frame[2] < 8 &&
			frame[33] > 20 && frame[34] > 20 && frame[35] < 8
	})

	// A nested record without the exact npi-text marker is malformed. It must
	// not be read as a replace and clear the live title, artist or cover.
	malformed, err := plist.EncodeBinary(map[string]any{
		"type": "updateMRNowPlayingInfo",
		"params": map[string]any{
			"mergePolicy": "replace",
			"params":      map[string]any{nowPlayingKeyTitle: "Malformed"},
		},
	})
	if err != nil {
		t.Fatalf("encode malformed nested record: %v", err)
	}
	writeEventPlist(t, event, 3, malformed)
	if response := readEventMessage(t, event, "the malformed nested response"); !bytes.HasPrefix(response, []byte("RTSP/1.0 200")) {
		t.Fatalf("malformed nested response = %q", response)
	}
	time.Sleep(100 * time.Millisecond)
	player = board.Snapshot()
	if player["title"] != "Track Two" || player["artist"] != "Artist Two" {
		t.Fatalf("a malformed nested replace changed the record: %#v", player)
	}
	if frame := ring.lastFrame(); !(frame[0] < 8 && frame[1] > 20 && frame[33] > 20 && frame[34] > 20) {
		t.Fatalf("a malformed nested replace cleared the cover palette: %v", frame)
	}

	// An "update" keeps fields the record omits: a title-only update must not
	// erase the artist, album or the current cover.
	update := nowPlayingCommand(t, "update", map[string]any{nowPlayingKeyTitle: "Track Two (Remix)"})
	writeEventPlist(t, event, 4, update)
	if response := readEventMessage(t, event, "the update response"); !bytes.HasPrefix(response, []byte("RTSP/1.0 200")) {
		t.Fatalf("update response = %q", response)
	}
	player = waitBoard(t, board, "the merged update", func(player map[string]any) bool {
		return player["title"] == "Track Two (Remix)"
	})
	if player["artist"] != "Artist Two" || player["album"] != "Album Two" {
		t.Fatalf("update dropped fields = %#v", player)
	}
	time.Sleep(150 * time.Millisecond)
	if frame := ring.lastFrame(); !(frame[0] < 8 && frame[1] > 20 && frame[33] > 20 && frame[34] > 20) {
		t.Fatalf("update without artwork changed the cover palette: %v", frame)
	}

	// A later replace that omits artwork clears the previous cover instead of
	// leaving the last track's colours under a new title.
	replace := nowPlayingCommand(t, "replace", map[string]any{nowPlayingKeyTitle: "Track Three"})
	writeEventPlist(t, event, 5, replace)
	if response := readEventMessage(t, event, "the clearing replace response"); !bytes.HasPrefix(response, []byte("RTSP/1.0 200")) {
		t.Fatalf("clearing replace response = %q", response)
	}
	player = waitBoard(t, board, "the clearing replace", func(player map[string]any) bool {
		return player["title"] == "Track Three"
	})
	if _, present := player["artist"]; present {
		t.Fatalf("replace kept the old artist: %#v", player)
	}
	if _, present := player["album"]; present {
		t.Fatalf("replace kept the old album: %#v", player)
	}
	if player["positionMs"] != "0" {
		t.Fatalf("replace kept the old position: %v", player["positionMs"])
	}
	waitRing("the neutral breath after the cover was cleared", func(frame []byte) bool {
		return frame[0] == frame[1] && frame[1] == frame[2] &&
			frame[33] == frame[34] && frame[34] == frame[35]
	})

	// A playback-state command is authoritative feedback for the player, and
	// a command with no now-playing fields must not be read as one.
	stateBody, err := plist.EncodeBinary(map[string]any{
		"type":   "updateMRPlaybackState",
		"params": map[string]any{"mrPlaybackState": int64(2)},
	})
	if err != nil {
		t.Fatalf("encode playback state: %v", err)
	}
	writeEventPlist(t, event, 6, stateBody)
	if response := readEventMessage(t, event, "the playback-state response"); !bytes.HasPrefix(response, []byte("RTSP/1.0 200")) {
		t.Fatalf("playback-state response = %q", response)
	}
	waitBoard(t, board, "the paused state", func(player map[string]any) bool {
		return player["playbackState"] == string(media.StatePaused)
	})

	supported, err := plist.EncodeBinary(map[string]any{
		"type": "updateMRSupportedCommands",
		"params": map[string]any{
			"mrSupportedCommandsFromSender": []any{[]byte("not a command")},
		},
	})
	if err != nil {
		t.Fatalf("encode supported commands: %v", err)
	}
	writeEventPlist(t, event, 7, supported)
	if response := readEventMessage(t, event, "the supported-commands response"); !bytes.HasPrefix(response, []byte("RTSP/1.0 200")) {
		t.Fatalf("supported-commands response = %q", response)
	}
	if player := board.Snapshot(); player["title"] != "Track Three" {
		t.Fatalf("an unrelated command changed the metadata: %#v", player)
	}

	// TEARDOWN releases the player and the ring, as it does for every sender.
	if response, err := client.do("TEARDOWN", "/1", "", nil); err != nil || response.status != 200 {
		t.Fatalf("TEARDOWN = %v %v", response, err)
	}
	waitUntil(t, "the player to leave the contract", func() bool { return !board.Live() })
	waitUntil(t, "the ring to close", func() bool {
		_, closed := ring.snapshot()
		return closed
	})
	if players, _ := mgr.MediaSnapshot()["players"].([]any); len(players) != 0 {
		t.Fatalf("teardown left a player: %v", players)
	}
}

// TestNowPlayingPlistCandidateStateReplaysOnPromotion covers the ownership
// half of the same path: an event plist that arrives before the connection
// owns the player is stored, never shown over the current player, and is
// replayed (text, timeline and artwork) when that connection is promoted.
func TestNowPlayingPlistCandidateStateReplaysOnPromotion(t *testing.T) {
	mgr, transport, _, ring, _, _, _ := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	board := mgr.board
	stream, client := dialSender(t, mgr, transport)
	defer stream.Close()

	cover := coverPNG(t, color.RGBA{R: 255, A: 255}, color.RGBA{B: 255, A: 255})
	pending := nowPlayingCommand(t, "replace", map[string]any{
		nowPlayingKeyTitle:       "Pending Title",
		nowPlayingKeyArtist:      "Pending Artist",
		nowPlayingKeyAlbum:       "Pending Album",
		nowPlayingKeyDuration:    90.0,
		nowPlayingKeyElapsedTime: 4.5,
		nowPlayingKeyArtwork:     cover,
	})
	if response, err := client.do("POST", "/command", "application/x-apple-binary-plist", pending); err != nil || response.status != 200 {
		t.Fatalf("candidate POST /command = %v %v", response, err)
	}
	if board.Live() {
		t.Fatal("a candidate's now-playing command started the player")
	}
	if player := board.Snapshot(); player["title"] != nil {
		t.Fatalf("a candidate's metadata reached the board: %#v", player)
	}

	// A playback-state command that arrives before ownership is replayed on
	// promotion too; it must not touch the live player while still a candidate.
	pendingState, err := plist.EncodeBinary(map[string]any{
		"type":   "updateMRPlaybackState",
		"params": map[string]any{"mrPlaybackState": int64(2)},
	})
	if err != nil {
		t.Fatalf("encode pending playback state: %v", err)
	}
	if response, err := client.do("POST", "/command", "application/x-apple-binary-plist", pendingState); err != nil || response.status != 200 {
		t.Fatalf("candidate playback state = %v %v", response, err)
	}
	if board.Snapshot()["playbackState"] != string(media.StateStopped) {
		t.Fatalf("a candidate's playback state reached the idle board: %#v", board.Snapshot())
	}

	announce(t, client)
	waitUntil(t, "the candidate to be promoted", func() bool { return mgr.Active() != nil })
	player := waitBoard(t, board, "the replayed metadata", func(player map[string]any) bool {
		return player["title"] == "Pending Title"
	})
	if player["artist"] != "Pending Artist" || player["album"] != "Pending Album" {
		t.Fatalf("replayed metadata = %#v", player)
	}
	if player["durationMs"] != "90000" || player["positionMs"] != "4500" {
		t.Fatalf("replayed timeline = duration %v position %v", player["durationMs"], player["positionMs"])
	}
	if player["playbackState"] != string(media.StatePaused) {
		t.Fatalf("promotion did not replay the playback state: %v", player["playbackState"])
	}

	setup, err := client.doHeaders("SETUP", "/1", nil,
		"Transport: RTP/AVP/TCP;unicast;interleaved=0-1")
	if err != nil || setup.status != 200 {
		t.Fatalf("SETUP = %v %v", setup, err)
	}
	if record, err := client.do("RECORD", "/1", "", nil); err != nil || record.status != 200 {
		t.Fatalf("RECORD = %v %v", record, err)
	}
	waitUntil(t, "the replayed cover palette", func() bool {
		frame := ring.lastFrame()
		return len(frame) == 36 && frame[0] > 20 && frame[1] < 8 && frame[2] < 8 &&
			frame[33] < 8 && frame[34] < 8 && frame[35] > 20
	})
}

// TestNowPlayingTimelineOnlyReplaysOnPromotion covers the timeline-only half
// of candidate replay: a record that carries duration and elapsed time but no
// title, artist, album or artwork must still reach the media contract after
// promotion.
func TestNowPlayingTimelineOnlyReplaysOnPromotion(t *testing.T) {
	mgr, transport, _, _, _, _, _ := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	board := mgr.board
	stream, client := dialSender(t, mgr, transport)
	defer stream.Close()

	timeline := nowPlayingCommand(t, "replace", map[string]any{
		nowPlayingKeyDuration:    125.0,
		nowPlayingKeyElapsedTime: 7.5,
	})
	if response, err := client.do("POST", "/command", "application/x-apple-binary-plist", timeline); err != nil || response.status != 200 {
		t.Fatalf("candidate timeline POST /command = %v %v", response, err)
	}
	if board.Live() {
		t.Fatal("a candidate's timeline started the player")
	}

	announce(t, client)
	waitUntil(t, "the candidate to be promoted", func() bool { return mgr.Active() != nil })
	player := waitBoard(t, board, "the replayed timeline", func(player map[string]any) bool {
		return player["durationMs"] == "125000"
	})
	if player["positionMs"] != "7500" {
		t.Fatalf("replayed position = %v, want 7500", player["positionMs"])
	}
	if _, present := player["title"]; present {
		t.Fatalf("a timeline-only record invented a title: %#v", player)
	}
}

// TestNowPlayingAndPromotionSharePublicationLock proves a live now-playing
// update and the candidate replay used by promotion both publish through the
// session's one serialization lock. A board write attempted while that lock is
// held must not become visible.
func TestNowPlayingAndPromotionSharePublicationLock(t *testing.T) {
	mgr, transport, _, _, _, _, _ := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	board := mgr.board
	stream, client := dialSender(t, mgr, transport)
	defer stream.Close()
	announce(t, client)
	waitUntil(t, "the announced owner", func() bool { return mgr.Active() != nil })
	owner := mgr.Active()
	if owner == nil {
		t.Fatal("no active session")
	}

	owner.publishMu.Lock()
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		owner.applyNowPlayingInfo(map[string]any{nowPlayingKeyTitle: "Serialized Live"}, true)
		close(done)
	}()
	<-started
	time.Sleep(50 * time.Millisecond)
	if title, _ := board.Snapshot()["title"].(string); title == "Serialized Live" {
		owner.publishMu.Unlock()
		t.Fatal("a live now-playing update published while the serialization lock was held")
	}
	owner.publishMu.Unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the live now-playing update did not finish after the lock was released")
	}
	waitBoard(t, board, "the serialized live record", func(player map[string]any) bool {
		return player["title"] == "Serialized Live"
	})

	owner.mu.Lock()
	owner.metadata.Title = "Serialized Replay"
	owner.mu.Unlock()
	gen := owner.boardGeneration()
	owner.publishMu.Lock()
	replayStarted := make(chan struct{})
	replayDone := make(chan struct{})
	go func() {
		close(replayStarted)
		owner.replayPendingState(gen)
		close(replayDone)
	}()
	<-replayStarted
	time.Sleep(50 * time.Millisecond)
	if title, _ := board.Snapshot()["title"].(string); title == "Serialized Replay" {
		owner.publishMu.Unlock()
		t.Fatal("promotion replay published while the serialization lock was held")
	}
	owner.publishMu.Unlock()
	select {
	case <-replayDone:
	case <-time.After(2 * time.Second):
		t.Fatal("promotion replay did not finish after the lock was released")
	}
	waitBoard(t, board, "the serialized replay", func(player map[string]any) bool {
		return player["title"] == "Serialized Replay"
	})
}

// TestFlatNowPlayingRecordStillApplies keeps the older flat record shape
// separate from the npi-text command envelope. It carries the record directly
// in params, without the nested params.params layer.
func TestFlatNowPlayingRecordStillApplies(t *testing.T) {
	mgr, transport, _, _, _, _, _ := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	board := mgr.board
	stream, client := dialSender(t, mgr, transport)
	defer stream.Close()
	announce(t, client)
	waitUntil(t, "the announced owner", func() bool { return mgr.Active() != nil })

	flat, err := plist.EncodeBinary(map[string]any{
		"params": map[string]any{
			nowPlayingKeyTitle:  "Flat Title",
			nowPlayingKeyArtist: "Flat Artist",
		},
	})
	if err != nil {
		t.Fatalf("encode flat record: %v", err)
	}
	if response, err := client.do("POST", "/command", "application/x-apple-binary-plist", flat); err != nil || response.status != 200 {
		t.Fatalf("flat POST /command = %v %v", response, err)
	}
	player := waitBoard(t, board, "the flat record", func(player map[string]any) bool {
		return player["title"] == "Flat Title"
	})
	if player["artist"] != "Flat Artist" {
		t.Fatalf("flat record artist = %v", player["artist"])
	}
}
