package session

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"capsulad/apps/airplay/internal/aac"
	"capsulad/apps/airplay/internal/airplayinfo"
	"capsulad/apps/airplay/internal/alac"
	"capsulad/apps/airplay/internal/artwork"
	"capsulad/apps/airplay/internal/dmap"
	"capsulad/apps/airplay/internal/media"
	"capsulad/apps/airplay/internal/pair"
	"capsulad/apps/airplay/internal/plist"
	"capsulad/apps/airplay/internal/raop"
	"capsulad/apps/airplay/internal/rtsp"
	"capsulad/apps/airplay/internal/tlv8"
)

// dispatch routes one RTSP request. The returned channel keys request a
// post-response switch to the encrypted HAP framing (pair-verify only).
func (s *Session) dispatch(req *rtsp.Request) (*rtsp.Response, *channelKeys, error) {
	path := req.Path()
	switch req.Method {
	case "OPTIONS":
		response := rtsp.NewResponse(req.CSeq(), 200, "")
		response.Set("Public", "ANNOUNCE, SETUP, RECORD, PAUSE, FLUSH, FLUSHBUFFERED, SETRATEANCHORTI, SETRATEANCHORTIME, TEARDOWN, OPTIONS, POST, GET, PUT")
		return response, nil, nil
	case "GET":
		if path == "/info" {
			return s.handleInfo(req)
		}
	case "POST":
		switch path {
		case "/pair-setup":
			return s.handlePairSetup(req)
		case "/pair-verify":
			return s.handlePairVerify(req)
		case "/fp-setup":
			return s.handleFairPlay(req)
		case "/fp-setup2", "/auth-setup":
			return s.refuse(req, 403, "this FairPlay variant is not implemented by this receiver")
		case "/pair-pin-start":
			// Upstream answers an empty 200 here (rtsp.c handle_pair_pin_start);
			// the setup code is fixed, so there is nothing to display.
			return s.respondOctet(req, nil)
		case "/pair-add":
			return s.handlePairAdministration(req, false)
		case "/pair-remove":
			return s.handlePairAdministration(req, true)
		case "/pair-list":
			return s.handlePairList(req)
		case "/feedback":
			return s.handleFeedback(req)
		case "/command":
			return s.handleSenderCommand(req)
		case "/audioMode", "/configure", "/setProperty":
			return s.handleAcknowledgement(req)
		}
	case "ANNOUNCE":
		return s.handleAnnounce(req)
	case "SETUP":
		return s.handleSetup(req)
	case "RECORD":
		return s.handleRecord(req)
	case "SET_PARAMETER":
		return s.handleSetParameter(req)
	case "GET_PARAMETER":
		return s.handleGetParameter(req)
	case "FLUSH":
		return s.handleFlush(req)
	case "FLUSHBUFFERED":
		return s.handleFlushBuffered(req)
	case "TEARDOWN":
		return s.handleTeardown(req)
	case "SETRATEANCHORTI", "SETRATEANCHORTIME":
		// The reference dispatch table names the operation SETRATEANCHORTI;
		// an iPhone field client sent SETRATEANCHORTIME. Both are observed
		// wire spellings of the same plist-bodied anchor operation.
		return s.handleSetRateAnchorTime(req)
	case "SETRATE":
		return s.refuse(req, 501, "SETRATE is not implemented by the reference receiver")
	case "SETMAGICCOOKIE":
		return s.handleAcknowledgement(req)
	case "SETPEERS", "SETPEERSX":
		// PTP peer lists are only meaningful when a multicast listener exists.
		if s.m.ptp == nil {
			s.m.logf("session: sender offered PTP peers but this receiver has no PTP clock")
		}
		return s.handleAcknowledgement(req)
	case "PUT":
		return s.handleAcknowledgement(req)
	case "PAUSE":
		s.setRate(0)
		return rtsp.NewResponse(req.CSeq(), 200, ""), nil, nil
	}
	return s.refuse(req, 404, "method or path not implemented")
}

func (s *Session) refuse(req *rtsp.Request, status int, detail string) (*rtsp.Response, *channelKeys, error) {
	s.m.logf("session from %s: refusing %s %s: %s", s.peer.Address, req.Method, req.Path(), detail)
	return rtsp.NewResponse(req.CSeq(), status, detail), nil, nil
}

func (s *Session) handleAcknowledgement(req *rtsp.Request) (*rtsp.Response, *channelKeys, error) {
	return rtsp.NewResponse(req.CSeq(), 200, ""), nil, nil
}

// handleFairPlay answers the sender's FairPlay handshake with the fixed
// protocol replies the upstream receiver serves. It completes the handshake;
// it does not decrypt DRM-protected content.
func (s *Session) handleFairPlay(req *rtsp.Request) (*rtsp.Response, *channelKeys, error) {
	body, err := s.readBody(req)
	if err != nil {
		return s.refuse(req, 400, err.Error())
	}
	reply := fairPlayReply(body)
	response := rtsp.NewResponse(req.CSeq(), 200, "")
	response.Set("Content-Type", "application/octet-stream")
	if reply == nil {
		s.m.logf("session from %s: /fp-setup request outside the supported handshake (%d bytes)", s.peer.Address, len(body))
		return response, nil, nil
	}
	response.Body = reply
	return response, nil, nil
}

// handleInfo answers GET /info with the reference device-information document
// (internal/airplayinfo). Stage 1 (a request body carrying `qualifier`) and
// stage 2 return the same document, which is what the reference does; the
// feature mask is the one mDNS advertises, so the two cannot disagree.
func (s *Session) handleInfo(req *rtsp.Request) (*rtsp.Response, *channelKeys, error) {
	qualifier := false
	if len(req.Body) > 0 {
		if value, err := plist.Decode(req.Body); err == nil {
			if dict, ok := value.(map[string]any); ok {
				_, qualifier = dict["qualifier"]
			}
		}
	}
	txt := airplayinfo.TXT(s.m.advertiseTXT())
	document, err := airplayinfo.DeviceInfo(s.infoConfig(), txt, qualifier)
	if err != nil {
		return s.refuse(req, 500, err.Error())
	}
	return s.respondPlist(req, document)
}

// infoConfig is the device-information configuration this session serves; the
// event channel's updateInfo document reuses it. Every capability field is
// filled: airplayinfo treats zero as a meaningful value (no audio format, no
// volume control), so leaving one unset would tell a sender this receiver can
// play nothing even while mDNS advertises the same format bits. The reference
// reports these values from one configuration in both /info and the DNS-SD
// records (rtsp.c:1194-1248).
func (s *Session) infoConfig() airplayinfo.Config {
	return airplayinfo.Config{
		Name:               s.displayName(),
		DeviceID:           s.m.cfg.DeviceID,
		PairingID:          s.m.store.Device.PairingID,
		Model:              s.m.cfg.Model,
		Manufacturer:       "capsulad",
		SourceVersion:      s.m.cfg.SourceVersion,
		Features:           s.m.advertisedFeatures(),
		StatusFlags:        s.m.advertisedStatus(),
		VolumeDB:           s.masterVolumeDB(),
		VV:                 airplayinfo.DefaultVV,
		VolumeControlType:  airplayinfo.DefaultVolumeControlType,
		AudioStreamFormats: airplayinfo.DefaultAudioStreamFormats,
		// Buffered formats come from the advertised feature mask, not a second
		// capability list: ALAC 48/24 stereo is always decodable and the two
		// AAC-LC stereo formats follow the FFmpeg feature bit. An override that
		// clears bit 40 (or both codec bits) therefore reports zero here too.
		BufferStreamFormats: bufferedStreamFormats(s.m.advertisedFeatures()),
		OutputLatencyMicros: uint64(s.m.cfg.FrameTarget.Microseconds()),
		PairingLTPK:         s.m.store.Device.LTPK[:],
		SenderAddress:       s.senderAddress(),
	}
}

// senderAddress is the document's senderAddress: the live control connection's
// "ip:port", which the reference formats from the peer (rtsp.c:1204-1207).
func (s *Session) senderAddress() string {
	if s.peer.Address == "" {
		return ""
	}
	if s.peer.Port == 0 {
		return s.peer.Address
	}
	return net.JoinHostPort(s.peer.Address, strconv.Itoa(s.peer.Port))
}

func (s *Session) displayName() string {
	if s.m.cfg.Name != "" {
		return s.m.cfg.Name
	}
	return "AirPlay Receiver"
}

// handlePairSetup runs the HAP pair-setup state machine (transient setup: the
// sender is not asked for a code).
func (s *Session) handlePairSetup(req *rtsp.Request) (*rtsp.Response, *channelKeys, error) {
	if s.setup == nil {
		s.setup = pair.NewSetupServer(s.m.store)
		s.setup.SetPIN(s.m.cfg.PIN)
	}
	body, err := s.readBody(req)
	if err != nil {
		return s.refuse(req, 400, err.Error())
	}
	response, err := s.setup.Handle(body)
	if err != nil {
		s.m.logf("session from %s: pair-setup failed: %v", s.peer.Address, err)
		return s.refuse(req, 470, "pair-setup failed")
	}
	if err := s.m.persistControllers(); err != nil {
		s.m.logf("session: persisting the pairing record: %v", err)
	}
	if s.setup.TransientComplete() && !s.encrypted {
		// Transient pairing ends at M4: the SRP session key is the shared
		// secret for both encrypted channels, and the controller never sends
		// M5/M6 or pair-verify (pyatv hap_transient.py, Shairport Sync
		// server_setup_response2). The response to M4 is still plaintext.
		keys, err := s.channelUpgrade(s.setup.SessionKey())
		if err != nil {
			return nil, nil, err
		}
		resp, _, err := s.respondPairSetup(req, response)
		s.m.logf("session from %s: transient pair-setup complete; channels keyed from the SRP secret", s.peer.Address)
		return resp, keys, err
	}
	return s.respondPairSetup(req, response)
}

// handlePairVerify completes the Curve25519 exchange and yields the shared
// secret the control, event and audio channels are keyed from.
func (s *Session) handlePairVerify(req *rtsp.Request) (*rtsp.Response, *channelKeys, error) {
	if s.verify == nil {
		s.verify = pair.NewVerifyServer(s.m.store)
	}
	body, err := s.readBody(req)
	if err != nil {
		return s.refuse(req, 400, err.Error())
	}
	// The accessory side has a shared secret as soon as it answered M1, but the
	// channel must stay plaintext until the M4 reply has been written: the
	// client's M3 is still an ordinary RTSP request.
	state := byte(0)
	if items, err := tlv8.Decode(body); err == nil {
		if raw, ok := tlv8.Get(items, tlv8.TagState); ok && len(raw) == 1 {
			state = raw[0]
		}
	}
	response, err := s.verify.Handle(body)
	if err != nil {
		s.m.logf("session from %s: pair-verify failed: %v", s.peer.Address, err)
		return s.refuse(req, 470, "pair-verify failed")
	}
	// M3 may fail authentication while the RTSP exchange still returns 200 with
	// a TLV error. The shared secret exists from M1 in either case, so keying
	// or promoting on it alone would treat a rejected controller as verified
	// (and would desynchronise a still-plaintext connection). Require the
	// signature proof before retaining the identity or switching framing.
	if state == tlv8.StateM3 && !s.verify.Verified() {
		s.m.logf("session from %s: pair-verify M3 was not authenticated", s.peer.Address)
	}
	if s.verify.Verified() {
		if shared := s.verify.SharedSecret(); len(shared) > 0 {
			s.shared = shared
		}
		if id := s.verify.ControllerID(); id != "" {
			s.controllerID = id
		}
	}
	var upgrade *channelKeys
	if state == tlv8.StateM3 && s.verify.Verified() && s.shared != nil && !s.encrypted {
		keys, err := s.channelUpgrade(s.shared)
		if err != nil {
			return nil, nil, err
		}
		upgrade = keys
	}
	resp, _, err := s.respondTLV(req, response)
	return resp, upgrade, err
}

// pairingAdmin checks that this session belongs to a paired controller with
// administrative permission. Upstream's pairing library performs no such check
// and does not persist permissions; this receiver treats the admin surface as
// real security rather than an unauthenticated endpoint.
func (s *Session) pairingAdmin() (bool, pair.Pairing) {
	if !s.encrypted || s.controllerID == "" {
		return false, pair.Pairing{}
	}
	record, ok := s.m.store.Controller(s.controllerID)
	if !ok || !record.Admin {
		return false, pair.Pairing{}
	}
	return true, record
}

// handlePairAdministration implements POST /pair-add and /pair-remove: the
// request is a TLV8 payload addressed by pairing identifier, matching the
// upstream wire shape (State 1, Method 3 or 4, Identifier, and for add the
// PublicKey and optional Permissions). The reply is State 2, or a TLV error for
// a refusal; a malformed request answers 451, as upstream's library error path
// does.
func (s *Session) handlePairAdministration(req *rtsp.Request, remove bool) (*rtsp.Response, *channelKeys, error) {
	body, err := s.readBody(req)
	if err != nil {
		return s.refuse(req, 450, err.Error())
	}
	items, err := tlv8.Decode(body)
	if err != nil {
		return s.refuse(req, 451, "malformed pairing TLV")
	}
	state, _ := tlv8.GetUint8(items, tlv8.TagState)
	method, _ := tlv8.GetUint8(items, tlv8.TagMethod)
	wantMethod := tlv8.MethodAddPairing
	if remove {
		wantMethod = tlv8.MethodRemovePairing
	}
	if state != 0x01 || method != wantMethod {
		return s.refuse(req, 451, "pairing request is not an M1 for that method")
	}
	id := tlv8.GetString(items, tlv8.TagIdentifier)
	if id == "" || len(id) > 64 {
		return s.refuse(req, 451, "pairing request has no identifier")
	}
	if admin, _ := s.pairingAdmin(); !admin {
		s.m.logf("session from %s: %s refused for an unpaired or non-administrator controller", s.peer.Address, req.Path())
		return s.respondTLV(req, pairErrorResponse(tlv8.StateM2, tlv8.ErrorAuthentication))
	}
	if remove {
		if !s.m.store.RemoveController(id) {
			// HAP defines kTLVError_Unknown for a target that is not paired.
			return s.respondTLV(req, pairErrorResponse(tlv8.StateM2, tlv8.ErrorUnknown))
		}
		if err := s.m.persistControllers(); err != nil {
			s.m.logf("session: persisting the pairing removal: %v", err)
			return s.refuse(req, 500, "the pairing store could not be updated")
		}
		s.m.logf("session from %s: removed pairing %q", s.peer.Address, id)
		if id == s.controllerID {
			// HAP requires the session with a removed controller to end.
			s.closeAfterResponse = true
		}
		return s.respondTLV(req, pairStateM2())
	}
	ltpk, ok := tlv8.Get(items, tlv8.TagPublicKey)
	if !ok || len(ltpk) != ed25519.PublicKeySize {
		return s.refuse(req, 451, "pairing request has no controller public key")
	}
	admin := true
	if permissions, ok := tlv8.GetUint8(items, tlv8.TagPermissions); ok {
		if permissions > 1 {
			return s.refuse(req, 451, "pairing permissions must be 0 or 1")
		}
		admin = permissions == 1
	}
	s.m.store.SetController(id, ltpk, admin)
	if err := s.m.persistControllers(); err != nil {
		s.m.logf("session: persisting the added pairing: %v", err)
		return s.refuse(req, 500, "the pairing store could not be updated")
	}
	s.m.logf("session from %s: added pairing %q (admin=%v)", s.peer.Address, id, admin)
	return s.respondTLV(req, pairStateM2())
}

// handlePairList answers POST /pair-list with every pairing, its public key and
// its permissions, separated as HAP defines. Upstream lists every pairing with
// a hard-coded admin permission and no authentication; this receiver requires
// an administrator session and reports the stored permission.
func (s *Session) handlePairList(req *rtsp.Request) (*rtsp.Response, *channelKeys, error) {
	body, err := s.readBody(req)
	if err != nil {
		return s.refuse(req, 450, err.Error())
	}
	if len(body) > 0 {
		items, err := tlv8.Decode(body)
		if err != nil {
			return s.refuse(req, 451, "malformed pairing TLV")
		}
		if state, _ := tlv8.GetUint8(items, tlv8.TagState); state != 0x01 {
			return s.refuse(req, 451, "pairing request is not an M1")
		}
		if method, _ := tlv8.GetUint8(items, tlv8.TagMethod); method != tlv8.MethodListPairings {
			return s.refuse(req, 451, "pairing request is not a list request")
		}
	}
	if admin, _ := s.pairingAdmin(); !admin {
		s.m.logf("session from %s: pair-list refused for an unpaired or non-administrator controller", s.peer.Address)
		return s.respondTLV(req, pairErrorResponse(tlv8.StateM2, tlv8.ErrorAuthentication))
	}
	response := []tlv8.Item{{Tag: tlv8.TagState, Value: []byte{tlv8.StateM2}}}
	for index, id := range s.m.store.ControllerIDs() {
		record, ok := s.m.store.Controller(id)
		if !ok {
			continue
		}
		if index > 0 {
			response = append(response, tlv8.Item{Tag: tlv8.TagSeparator})
		}
		permission := byte(0)
		if record.Admin {
			permission = 1
		}
		response = append(response,
			tlv8.Item{Tag: tlv8.TagIdentifier, Value: []byte(id)},
			tlv8.Item{Tag: tlv8.TagPublicKey, Value: record.LTPK},
			tlv8.Item{Tag: tlv8.TagPermissions, Value: []byte{permission}},
		)
	}
	return s.respondTLV(req, tlv8.Encode(response...))
}

// pairErrorResponse is the HAP TLV error reply ({state, error}).
func pairErrorResponse(state, code byte) []byte {
	return tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{state}},
		tlv8.Item{Tag: tlv8.TagError, Value: []byte{code}},
	)
}

// pairStateM2 is the bare success reply of the pairing administration methods.
func pairStateM2() []byte {
	return tlv8.Encode(tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM2}})
}

// respondOctet answers with an opaque body, which is what the pairing library's
// callers use for these endpoints.
func (s *Session) respondOctet(req *rtsp.Request, body []byte) (*rtsp.Response, *channelKeys, error) {
	response := rtsp.NewResponse(req.CSeq(), 200, "")
	response.Body = body
	response.Set("Content-Type", "application/octet-stream")
	return response, nil, nil
}

// handleFeedback answers the sender's liveness request with the live stream
// descriptors; a sender that gets no answer considers the session broken.
func (s *Session) handleFeedback(req *rtsp.Request) (*rtsp.Response, *channelKeys, error) {
	if body, err := s.readBody(req); err == nil && len(body) > 0 {
		s.noteSenderPlist(body)
	}
	streams := []any{}
	for _, stream := range s.activeStreams() {
		streams = append(streams, stream.descriptor())
	}
	body := map[string]any{}
	if len(streams) > 0 {
		body["streams"] = streams
	}
	return s.respondPlist(req, body)
}

// handleSenderCommand processes a command the sender pushes on the event
// channel. Playback state is taken from SETRATEANCHORTI; anything the sender
// sends here is treated as additional state (playback rate, now-playing info).
func (s *Session) handleSenderCommand(req *rtsp.Request) (*rtsp.Response, *channelKeys, error) {
	body, err := s.readBody(req)
	if err != nil {
		return s.refuse(req, 400, err.Error())
	}
	if len(body) > 0 {
		s.noteSenderPlist(body)
	}
	return rtsp.NewResponse(req.CSeq(), 200, ""), nil, nil
}

// noteSenderPlist folds the state a sender plist carries into the session.
func (s *Session) noteSenderPlist(body []byte) {
	value, err := plist.Decode(body)
	if err != nil {
		return
	}
	dict, ok := value.(map[string]any)
	if !ok {
		return
	}
	if name := plistString(dict["deviceName"]); name != "" {
		s.setDeviceName(name)
	}
	params := dict
	if nested, ok := plistDict(dict["params"]); ok {
		params = nested
	}
	if rate, ok := plistInt(params["kMRMediaRemoteNowPlayingInfoPlaybackRate"]); ok {
		if rate > 0 {
			s.setRate(1)
		} else {
			s.setRate(0)
		}
	}
	if title := plistString(params["kMRMediaRemoteNowPlayingInfoTitle"]); title != "" {
		artist := plistString(params["kMRMediaRemoteNowPlayingInfoArtist"])
		album := plistString(params["kMRMediaRemoteNowPlayingInfoAlbum"])
		s.setMetadata(dmap.Metadata{Title: title, Artist: artist, Album: album})
	}
}

// handleAnnounce parses the legacy SDP body and, when the sender encrypted the
// AES key with our RSA public key, decrypts it.
func (s *Session) handleAnnounce(req *rtsp.Request) (*rtsp.Response, *channelKeys, error) {
	body, err := s.readBody(req)
	if err != nil {
		return s.refuse(req, 400, err.Error())
	}
	stream, err := raop.ParseSDP(body)
	if err != nil {
		s.m.logf("session from %s: ANNOUNCE refused: %v", s.peer.Address, err)
		return s.refuse(req, 400, "unsupported SDP: "+err.Error())
	}
	var decryptedKey []byte
	if stream.HasRSA {
		key, err := s.m.raop.DecryptAESKey(stream.RSAAESKey)
		if err != nil {
			s.m.logf("session from %s: ANNOUNCE AES key could not be decrypted: %v", s.peer.Address, err)
			return s.refuse(req, 400, "the AES key could not be decrypted")
		}
		decryptedKey = key
	}
	if strings.EqualFold(stream.Codec, "aac") {
		s.m.logf("session from %s: ANNOUNCE refused: AAC audio requires a decoder this build does not include", s.peer.Address)
		return s.refuse(req, 415, "unsupported codec: aac")
	}
	s.mu.Lock()
	s.announce = stream
	s.announceKey = decryptedKey
	s.mu.Unlock()
	s.m.logf("session from %s: ANNOUNCE %s %d Hz %d ch (encryption: %v)", s.peer.Address, stream.Codec, stream.ALAC.SampleRate, stream.ALAC.Channels, stream.HasRSA)
	return rtsp.NewResponse(req.CSeq(), 200, ""), nil, nil
}

// handleSetup creates the audio streams. Two shapes exist:
//
//   - AirPlay 2: a binary-plist body with a "streams" array (type 96 realtime,
//     103 buffered); the receiver answers with the ports the daemon bound for
//     it. A plist body without "streams" is the sender asking for the event
//     port, which is how the reverse control channel is announced.
//   - Legacy RAOP: a "Transport:" header (UDP ports chosen by the receiver, or
//     interleaved RTP over the control connection).
func (s *Session) handleSetup(req *rtsp.Request) (*rtsp.Response, *channelKeys, error) {
	// A SETUP is media ownership only when it stages a real audio stream. The
	// event-only and remote-control-only forms configure channels; they must
	// not take the player or advertisements from the current sender.
	s.mu.Lock()
	s.setupStartsMedia = false
	s.mu.Unlock()
	transport := req.Get("Transport")
	if transport != "" {
		return s.handleLegacySetup(req, transport)
	}
	body, err := s.readBody(req)
	if err != nil {
		return s.refuse(req, 400, err.Error())
	}
	value, err := plist.Decode(body)
	if err != nil {
		return s.refuse(req, 400, "malformed SETUP body")
	}
	dict, ok := value.(map[string]any)
	if !ok {
		return s.refuse(req, 400, "malformed SETUP body")
	}
	rawGID, gidPresent := dict["gid"]
	gid := plistString(rawGID)
	gidSet := gidPresent && gid != ""

	rawStreams, streamsPresent := dict["streams"]
	streamList, listOK := rawStreams.([]any)
	if streamsPresent && !listOK {
		// A present but non-list "streams" is a malformed stream SETUP, not
		// the event-channel setup that omits the key. Refuse it before it can
		// reserve ownership.
		return s.refuse(req, 400, "malformed SETUP streams")
	}
	if !streamsPresent || len(streamList) == 0 {
		// Announce the event channel: the sender connects to this port and the
		// receiver writes its reverse commands there.
		if err := s.ensureEventListener(); err != nil {
			return s.refuse(req, 500, err.Error())
		}
		s.noteGroupID(gid, gidSet)
		_, port := s.eventListener.Addr()
		return s.respondPlist(req, map[string]any{"eventPort": int64(port)})
	}
	if len(streamList)+len(s.activeStreams())+len(s.stagedStreams()) > maxStreamsPerSession {
		s.m.logf("session from %s: SETUP refused: %d active and %d staged streams plus %d requested exceed the cap of %d",
			s.peer.Address, len(s.activeStreams()), len(s.stagedStreams()), len(streamList), maxStreamsPerSession)
		return s.refuse(req, 453, "too many audio streams in this session")
	}
	plans := make([]airPlay2StreamPlan, 0, len(streamList))
	for _, raw := range streamList {
		streamDict, ok := raw.(map[string]any)
		if !ok {
			return s.refuse(req, 400, "malformed stream description")
		}
		plan, err := s.planAirPlay2Stream(streamDict)
		if err != nil {
			s.m.logf("session from %s: SETUP refused: %v", s.peer.Address, err)
			return s.refuse(req, 400, err.Error())
		}
		plans = append(plans, plan)
	}
	// All descriptors are validated before the control socket or any audio
	// listener is opened, so a malformed or unsupported request has no side
	// effects. The listeners must exist before the reply can name their ports.
	if err := s.ensureAP2Control(); err != nil {
		return s.refuse(req, 500, err.Error())
	}
	descriptors := make([]any, 0, len(plans))
	mediaStaged := false
	for _, plan := range plans {
		if plan.controlOnly {
			descriptors = append(descriptors, map[string]any{
				"type":        int64(streamTypeRemoteControl),
				"controlPort": int64(s.ap2ControlPort()),
			})
			continue
		}
		stream, err := s.newAudioStream(plan.spec)
		if err != nil {
			s.discardStagedStreams()
			s.m.logf("session from %s: SETUP refused: %v", s.peer.Address, err)
			return s.refuse(req, 400, err.Error())
		}
		s.stageStream(stream)
		mediaStaged = true
		descriptors = append(descriptors, stream.descriptor())
	}
	s.mu.Lock()
	s.setupStartsMedia = mediaStaged
	s.mu.Unlock()
	// The group id is committed only with a successful SETUP: a candidate
	// stages it until it owns the receiver; a live sender applies it now.
	s.noteGroupID(gid, gidSet)
	return s.respondPlist(req, map[string]any{"streams": descriptors})
}

// airPlay2StreamPlan is one validated SETUP descriptor. It holds no listener;
// creation happens only after every descriptor in the request has passed
// validation.
type airPlay2StreamPlan struct {
	spec        audioStreamSpec
	controlOnly bool
}

// planAirPlay2Stream validates one type-96/130/103 descriptor. Type 103 is
// accepted only on the authenticated control channel with the fixed 32-byte
// session key and one of the buffered formats advertised in /info.
func (s *Session) planAirPlay2Stream(dict map[string]any) (airPlay2StreamPlan, error) {
	streamType := int64(streamTypeRealtime)
	if raw, present := dict["type"]; present {
		value, ok := plistUint64(raw)
		if !ok || value > 1<<31 {
			return airPlay2StreamPlan{}, fmt.Errorf("unsupported stream type value %v", raw)
		}
		if value != 0 {
			streamType = int64(value)
		}
	}
	if streamType == streamTypeRemoteControl {
		// A remote-control stream exists only inside an authenticated session;
		// the reference reaches this case on its PTP (encrypted) connection.
		if !s.encrypted {
			return airPlay2StreamPlan{}, errors.New("stream type 130 requires an authenticated session")
		}
		// The reference acknowledges it by echoing a descriptor with the
		// control port and creates nothing (rtsp.c case 130).
		return airPlay2StreamPlan{controlOnly: true}, nil
	}
	if streamType != streamTypeRealtime && streamType != streamTypeBuffered {
		return airPlay2StreamPlan{}, fmt.Errorf("unsupported stream type %d", streamType)
	}
	format, formatOK := plistUint64(dict["audioFormat"])
	codec, config, aacProfile := "", alac.Config{}, aac.Profile("")
	var err error
	if streamType == streamTypeBuffered {
		if !s.encrypted {
			return airPlay2StreamPlan{}, errors.New("buffered audio requires an authenticated control channel")
		}
		shk := plistData(dict["shk"])
		if len(shk) != 32 {
			return airPlay2StreamPlan{}, fmt.Errorf("buffered audio requires a 32-byte shk, got %d bytes", len(shk))
		}
		if !formatOK {
			return airPlay2StreamPlan{}, errors.New("buffered audio requires an audioFormat")
		}
		codec, config, aacProfile, err = bufferedCodecFromFormat(format)
	} else {
		codec, config, aacProfile, err = codecFromFormat(plistUint(dict["audioFormat"]))
	}
	if err != nil {
		return airPlay2StreamPlan{}, err
	}
	spf := int(config.FrameLength)
	if spf == 0 && codec == "aac" {
		spf = 1024
	}
	if rawSPF, present := dict["spf"]; present {
		value, ok := plistUint64(rawSPF)
		if !ok || value == 0 || value > 1<<20 {
			return airPlay2StreamPlan{}, fmt.Errorf("unsupported samples-per-frame value %v", rawSPF)
		}
		spf = int(value)
	}
	return airPlay2StreamPlan{spec: audioStreamSpec{
		streamType:      int(streamType),
		codec:           codec,
		config:          config,
		aacProfile:      aacProfile,
		sharedKey:       plistData(dict["shk"]),
		samplesPerFrame: spf,
	}}, nil
}

// handleLegacySetup honours the RAOP "Transport:" header.
func (s *Session) handleLegacySetup(req *rtsp.Request, transport string) (*rtsp.Response, *channelKeys, error) {
	if len(s.activeStreams()) >= maxStreamsPerSession {
		return s.refuse(req, 453, "too many audio streams in this session")
	}
	s.mu.Lock()
	announced := s.announce
	decryptedKey := s.announceKey
	s.mu.Unlock()
	if announced == nil {
		return s.refuse(req, 400, "SETUP before ANNOUNCE")
	}
	options := rtsp.Keys(transport)
	codec := strings.ToLower(announced.Codec)
	config := alac.Config{
		FrameLength: uint32(valueOr(announced.ALAC.FrameLength, 352)),
		BitDepth:    uint8(valueOr(announced.ALAC.BitDepth, 16)),
		Channels:    uint8(valueOr(announced.ALAC.Channels, 2)),
		SampleRate:  uint32(valueOr(announced.ALAC.SampleRate, 44100)),
	}
	if codec == "" {
		codec = "alac"
	}
	spec := audioStreamSpec{
		streamType:      streamTypeRealtime,
		codec:           codec,
		config:          config,
		aesKey:          decryptedKey,
		aesIV:           announced.AESIV,
		samplesPerFrame: announced.ALAC.FrameLength,
	}
	if channels, ok := options["interleaved"]; ok && !strings.Contains(strings.ToLower(transport), "udp") {
		data, control, err := parseInterleaved(channels)
		if err != nil {
			return s.refuse(req, 400, err.Error())
		}
		spec.interleavedData = data
		spec.interleavedControl = control
	} else {
		spec.useUDP = true
	}
	stream, err := s.newAudioStream(spec)
	if err != nil {
		return s.refuse(req, 400, err.Error())
	}
	s.stageStream(stream)
	s.mu.Lock()
	s.setupStartsMedia = true
	s.mu.Unlock()
	response := rtsp.NewResponse(req.CSeq(), 200, "")
	if spec.useUDP {
		dataPort, controlPort := stream.udpPorts()
		response.Set("Transport", fmt.Sprintf("RTP/AVP/UDP;unicast;mode=record;server_port=%d;control_port=%d;timing_port=0", dataPort, controlPort))
	} else {
		response.Set("Transport", fmt.Sprintf("RTP/AVP/TCP;unicast;interleaved=%d-%d;mode=record", spec.interleavedData, spec.interleavedControl))
	}
	response.Set("Session", "1")
	response.Body = s.m.raop.SetupResponseBody()
	response.Set("Content-Type", "application/x-apple-binary-plist")
	return response, nil, nil
}

func parseInterleaved(value string) (int, int, error) {
	parts := strings.SplitN(value, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("malformed interleaved channel pair %q", value)
	}
	data, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("malformed interleaved data channel %q", value)
	}
	control, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("malformed interleaved control channel %q", value)
	}
	return data, control, nil
}

func (s *Session) handleRecord(req *rtsp.Request) (*rtsp.Response, *channelKeys, error) {
	_ = s.readBodyOptional(req)
	realtime := false
	for _, stream := range s.activeStreams() {
		if stream.spec.streamType == streamTypeBuffered {
			// A buffered stream starts only at a valid SETRATEANCHORTI. RECORD
			// must not make its staged audio audible or report PLAYING before
			// PCM has reached the sink.
			continue
		}
		stream.begin()
		realtime = true
	}
	if realtime {
		s.setRate(1)
	}
	response := rtsp.NewResponse(req.CSeq(), 200, "")
	rate := 44100
	if streams := s.activeStreams(); len(streams) > 0 {
		rate = streams[0].sampleRate()
	}
	response.Set("Audio-Latency", strconv.Itoa(int(s.m.cfg.FrameTarget.Seconds()*float64(rate))))
	response.Set("Session", "1")
	return response, nil, nil
}

func (s *Session) handleSetParameter(req *rtsp.Request) (*rtsp.Response, *channelKeys, error) {
	body, err := s.readBody(req)
	if err != nil {
		return s.refuse(req, 400, err.Error())
	}
	contentType := strings.ToLower(req.Get("Content-Type"))
	switch {
	case strings.HasPrefix(contentType, "text/parameters"):
		for _, line := range strings.Split(string(body), "\n") {
			key, value, found := strings.Cut(strings.TrimSpace(line), ":")
			if !found {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "volume":
				// A malformed line is ignored, not interpreted as 0 dB:
				// Shairport's atof would turn garbage into full volume, which
				// is not a legitimate slider position.
				if dB, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
					s.applySenderVolume(dB)
				} else {
					s.m.logf("session from %s: volume change ignored: malformed value", s.peer.Address)
				}
			case "progress":
				start, current, end, ok := dmap.ParseProgress(strings.TrimSpace(value))
				if ok {
					s.mu.Lock()
					s.progress.start, s.progress.current, s.progress.end = start, current, end
					s.progress.has = true
					gen := s.boardGen
					s.mu.Unlock()
					if s.m.board != nil && gen != 0 {
						s.m.board.SetProgress(gen, uint64(current))
					}
				}
			case "paused":
				if strings.TrimSpace(value) == "true" {
					s.setRate(0)
				} else {
					s.setRate(1)
				}
			}
		}
	case strings.HasPrefix(contentType, "image/"):
		palette, err := artwork.Palette(body)
		if err != nil {
			s.m.logf("session: cover art rejected: %v", err)
			break
		}
		s.setPalette(palette)
	case strings.HasPrefix(contentType, "application/x-dmap-tagged"):
		metadata, err := dmap.Parse(body)
		if err != nil {
			s.m.logf("session: metadata rejected: %v", err)
			break
		}
		s.setMetadata(*metadata)
	case strings.HasPrefix(contentType, "application/x-apple-binary-plist"):
		s.noteSenderPlist(body)
		if value, err := plist.Decode(body); err == nil {
			if dict, ok := value.(map[string]any); ok {
				if dB, ok := plistFloat(dict["volume"]); ok {
					s.applySenderVolume(dB)
				}
			}
		}
	}
	return rtsp.NewResponse(req.CSeq(), 200, ""), nil, nil
}

func (s *Session) handleGetParameter(req *rtsp.Request) (*rtsp.Response, *channelKeys, error) {
	body, _ := s.readBody(req)
	wanted := strings.TrimSpace(string(body))
	response := rtsp.NewResponse(req.CSeq(), 200, "")
	if strings.Contains(wanted, "volume") || wanted == "" {
		// The reference's exact reply (rtsp.c:3313-3322): a CRLF, the
		// value with six decimals and a trailing CRLF. Unit 0 is reported as
		// the -144 dB mute value; an unreadable master reports that same safe
		// value rather than 0 dB/full scale.
		response.Body = []byte(fmt.Sprintf("\r\nvolume: %.6f\r\n", s.masterVolumeDB()))
		response.Set("Content-Type", "text/parameters")
	}
	return response, nil, nil
}

func (s *Session) handleFlush(req *rtsp.Request) (*rtsp.Response, *channelKeys, error) {
	_ = s.readBodyOptional(req)
	for _, stream := range s.activeStreams() {
		if stream.spec.streamType == streamTypeBuffered {
			// Type 103 uses FLUSHBUFFERED with a sequence/timestamp range.
			continue
		}
		stream.flush()
	}
	return rtsp.NewResponse(req.CSeq(), 200, ""), nil, nil
}

// handleFlushBuffered applies the sender's buffered-audio flush range. A
// missing flushFromSeq/flushFromTS pair is the immediate form: playback stops
// and the obsolete range is discarded up to but not including flushUntilSeq;
// an omitted until pair means the whole queue (the reference's zero endpoint).
// With the from pair the request is deferred: blocks before flushFromSeq play,
// the named range is discarded when the stream reaches it, and flushUntilSeq
// itself resumes playout. Both endpoints are half-open, matching the reference:
// the block at flushUntilSeq is decoded rather than discarded. The reference
// selects by the 23-bit sequence numbers; timestamps are carried for the
// protocol but do not override that selection. A present but malformed field,
// a partial pair or an out-of-range value is refused before any audio or player
// state changes.
func (s *Session) handleFlushBuffered(req *rtsp.Request) (*rtsp.Response, *channelKeys, error) {
	body, err := s.readBody(req)
	if err != nil {
		return s.refuse(req, 400, err.Error())
	}
	if len(body) == 0 {
		return s.refuse(req, 400, "FLUSHBUFFERED requires a plist body")
	}
	value, err := plist.Decode(body)
	if err != nil {
		return s.refuse(req, 400, "malformed FLUSHBUFFERED body")
	}
	dict, ok := value.(map[string]any)
	if !ok {
		return s.refuse(req, 400, "malformed FLUSHBUFFERED body")
	}
	fromSeq, fromSeqPresent, fromSeqOK := optionalPlistUint32(dict, "flushFromSeq")
	fromTS, fromTSPresent, fromTSOK := optionalPlistUint32(dict, "flushFromTS")
	untilSeq, untilSeqPresent, untilSeqOK := optionalPlistUint32(dict, "flushUntilSeq")
	untilTS, untilTSPresent, untilTSOK := optionalPlistUint32(dict, "flushUntilTS")
	if (fromSeqPresent && !fromSeqOK) || (fromTSPresent && !fromTSOK) ||
		(untilSeqPresent && !untilSeqOK) || (untilTSPresent && !untilTSOK) {
		return s.refuse(req, 400, "FLUSHBUFFERED fields must be 32-bit unsigned integers")
	}
	if fromSeqPresent != fromTSPresent {
		return s.refuse(req, 400, "FLUSHBUFFERED requires flushFromSeq and flushFromTS together")
	}
	if untilSeqPresent != untilTSPresent {
		return s.refuse(req, 400, "FLUSHBUFFERED requires flushUntilSeq and flushUntilTS together")
	}
	immediate := !fromSeqPresent
	if !immediate && !untilSeqPresent {
		return s.refuse(req, 400, "deferred FLUSHBUFFERED requires flushUntilSeq and flushUntilTS")
	}
	streams := s.activeStreams()
	hasBuffered := false
	for _, stream := range streams {
		if stream.spec.streamType == streamTypeBuffered {
			hasBuffered = true
			break
		}
	}
	if !hasBuffered {
		return rtsp.NewResponse(req.CSeq(), 200, ""), nil, nil
	}
	if immediate {
		// The reference stops playout and invalidates the PTP anchor for an
		// immediate flush. Keep the buffered range prefetch (untilSeq) intact
		// for the wire reader; it decides which blocks are obsolete.
		s.mu.Lock()
		s.rate = 0
		gen := s.boardGen
		s.mu.Unlock()
		if s.m.board != nil && gen != 0 {
			s.m.board.SetPlayback(gen, media.StatePaused)
		}
	} else {
		s.touch()
	}
	for _, stream := range streams {
		if stream.spec.streamType != streamTypeBuffered {
			continue
		}
		stream.bufferedFlush(
			uint32(fromSeq&0x7fffff), uint32(fromTS),
			uint32(untilSeq&0x7fffff), uint32(untilTS),
			immediate,
		)
	}
	return rtsp.NewResponse(req.CSeq(), 200, ""), nil, nil
}

func (s *Session) handleTeardown(req *rtsp.Request) (*rtsp.Response, *channelKeys, error) {
	_ = s.readBodyOptional(req)
	for _, stream := range s.activeStreams() {
		stream.stop()
	}
	return rtsp.NewResponse(req.CSeq(), 200, ""), nil, nil
}

// handleSetRateAnchorTime handles the buffered play anchor. The reference
// dispatch table names it SETRATEANCHORTI (rtsp.c method_handlers), while an
// iPhone field client sent SETRATEANCHORTIME; both spellings carry the same
// plist body and route here, so there is one parser and one playout path.
// Rate one names the RTP timestamp playing now and, for a buffered stream, the
// PTP network-time fields that make that timestamp a valid play anchor. Rate
// zero pauses. A malformed or incomplete anchor is refused before changing
// playout.
func (s *Session) handleSetRateAnchorTime(req *rtsp.Request) (*rtsp.Response, *channelKeys, error) {
	body, err := s.readBody(req)
	if err != nil {
		return s.refuse(req, 400, err.Error())
	}
	if len(body) == 0 {
		return s.refuse(req, 400, "anchor requires a plist body")
	}
	value, err := plist.Decode(body)
	if err != nil {
		return s.refuse(req, 400, "malformed anchor body")
	}
	dict, ok := value.(map[string]any)
	if !ok {
		return s.refuse(req, 400, "malformed anchor body")
	}
	rate, ok := plistUint64(dict["rate"])
	if !ok || rate > 1 {
		return s.refuse(req, 400, "anchor rate must be 0 or 1")
	}
	if rate == 0 {
		s.setRate(0)
		return rtsp.NewResponse(req.CSeq(), 200, ""), nil, nil
	}

	rawRTP, ok := plistUint32Bits(dict["rtpTime"])
	if !ok {
		return s.refuse(req, 400, "anchor requires a 32-bit rtpTime")
	}
	streams := s.activeStreams()
	hasBuffered, hasRealtime := false, false
	for _, stream := range streams {
		switch stream.spec.streamType {
		case streamTypeBuffered:
			hasBuffered = true
		case streamTypeRealtime:
			hasRealtime = true
		}
	}

	var networkNS, timelineID uint64
	if hasBuffered {
		var timelineOK bool
		timelineID, timelineOK = plistUint64Bits(dict["networkTimeTimelineID"])
		seconds, secondsOK := plistUint64(dict["networkTimeSecs"])
		frac, fracOK := plistUint64Bits(dict["networkTimeFrac"])
		if !timelineOK || timelineID == 0 || !secondsOK || !fracOK {
			return s.refuse(req, 400, "buffered anchor requires a non-zero networkTimeTimelineID, networkTimeSecs and networkTimeFrac")
		}
		const nanosPerSecond = uint64(1_000_000_000)
		if seconds > ^uint64(0)/nanosPerSecond {
			return s.refuse(req, 400, "networkTimeSecs is out of range")
		}
		// networkTimeFrac is a binary fraction of a second: the high 32 bits
		// scale to nanoseconds exactly as the reference does (rtsp.c
		// handle_setrateanchori).
		fracNS := ((frac >> 32) * nanosPerSecond) >> 32
		networkNS = seconds*nanosPerSecond + fracNS
		if networkNS < seconds*nanosPerSecond {
			return s.refuse(req, 400, "network time is out of range")
		}
	}
	if hasRealtime {
		s.setRateAnchor(1, uint32(rawRTP))
	}
	if hasBuffered {
		s.setBufferedAnchor(streams, timelineID, uint32(rawRTP), networkNS)
	} else if !hasRealtime {
		// No audio stream yet: record the rate without starting a player.
		s.mu.Lock()
		s.rate = 1
		s.mu.Unlock()
	}
	return rtsp.NewResponse(req.CSeq(), 200, ""), nil, nil
}

// setBufferedAnchor applies one validated sender anchor to every type-103
// stream. The playout seam selects the PTP estimate by the sender's timeline
// identifier; it never falls back to an unrelated source's offset. Without a
// matching disciplined clock it uses its explicit local arrival fallback.
func (s *Session) setBufferedAnchor(streams []*audioStream, timelineID uint64, rtpTime uint32, networkNS uint64) {
	for _, stream := range streams {
		if stream.spec.streamType != streamTypeBuffered {
			continue
		}
		stream.bufferedAnchorPlay(timelineID, rtpTime, networkNS)
	}
}

// setRate changes the play/pause state and reports it on the media contract.
func (s *Session) setRate(rate float64) { s.setRateAnchor(rate, 0) }

func (s *Session) setRateAnchor(rate float64, rtpTime uint32) {
	s.mu.Lock()
	previous := s.rate
	s.rate = rate
	gen := s.boardGen
	s.mu.Unlock()
	if previous == rate && rtpTime == 0 && rate != 0 {
		return
	}
	if rate == 0 {
		for _, stream := range s.activeStreams() {
			if stream.spec.streamType == streamTypeBuffered {
				// Buffered audio stops and discards obsolete queued sound; it
				// resumes only through a fresh SETRATEANCHORTI.
				stream.bufferedPause()
				continue
			}
			stream.pause()
		}
		if s.m.board != nil && gen != 0 {
			s.m.board.SetPlayback(gen, media.StatePaused)
		}
		return
	}
	realtime := false
	for _, stream := range s.activeStreams() {
		if stream.spec.streamType == streamTypeBuffered {
			// The RTP timestamp alone is not a play anchor for buffered audio:
			// only handleSetRateAnchorTime, after validating network time,
			// resumes it.
			continue
		}
		stream.resume(rtpTime)
		realtime = true
	}
	if realtime && s.m.board != nil && gen != 0 {
		s.m.board.SetPlayback(gen, media.StatePlaying)
	}
}

func (s *Session) setMetadata(metadata dmap.Metadata) {
	s.mu.Lock()
	s.metadata = metadata
	position := uint64(0)
	duration := uint64(0)
	if s.progress.has {
		position = uint64(s.progress.current)
		if s.progress.end > s.progress.start {
			duration = uint64(s.progress.end - s.progress.start)
		}
	}
	gen := s.boardGen
	s.mu.Unlock()
	if s.m.board != nil && gen != 0 {
		s.m.board.SetMetadata(gen, metadata.Title, metadata.Artist, metadata.Album, duration, position)
	}
	if metadata.Title != "" {
		s.m.logf("session %s: %s — %s", s.peer.Address, metadata.Title, metadata.Artist)
	}
}

func (s *Session) setPalette(palette []artwork.RGB) {
	s.mu.Lock()
	s.palette = palette
	s.mu.Unlock()
	if length := len(palette); length > 0 {
		s.m.logf("session %s: cover art palette %d colour(s)", s.peer.Address, length)
	}
}

// --- helpers --------------------------------------------------------------

// readBody returns the request body. The RTSP reader has already consumed it
// from the connection and enforced the size cap; reading the stream again here
// would consume the next request instead.
func (s *Session) readBody(req *rtsp.Request) ([]byte, error) {
	if len(req.Body) > maxRequestBody {
		return nil, fmt.Errorf("body of %d bytes is too large", len(req.Body))
	}
	return req.Body, nil
}

func (s *Session) readBodyOptional(req *rtsp.Request) []byte { return req.Body }

func (s *Session) respondPlist(req *rtsp.Request, value any) (*rtsp.Response, *channelKeys, error) {
	body, err := plist.EncodeBinary(value)
	if err != nil {
		return nil, nil, fmt.Errorf("encode plist response: %w", err)
	}
	response := rtsp.NewResponse(req.CSeq(), 200, "")
	response.Body = body
	response.Set("Content-Type", "application/x-apple-binary-plist")
	return response, nil, nil
}

func (s *Session) respondTLV(req *rtsp.Request, body []byte) (*rtsp.Response, *channelKeys, error) {
	response := rtsp.NewResponse(req.CSeq(), 200, "")
	response.Body = body
	response.Set("Content-Type", "application/pairing+tlv8")
	return response, nil, nil
}

// respondPairSetup uses the reference receiver's content type for /pair-setup
// replies. Shairport Sync's handle_pair_setup sends application/octet-stream
// whenever the reply has a body; the other pairing endpoints keep
// application/pairing+tlv8.
func (s *Session) respondPairSetup(req *rtsp.Request, body []byte) (*rtsp.Response, *channelKeys, error) {
	response := rtsp.NewResponse(req.CSeq(), 200, "")
	response.Body = body
	response.Set("Content-Type", "application/octet-stream")
	return response, nil, nil
}

// ensureEventListener opens the daemon-allocated listener that carries the
// sender-initiated event channel (the reverse command path).
func (s *Session) ensureEventListener() error {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if s.eventListener != nil {
		return nil
	}
	listener, err := s.m.transport.OpenTCPListener("0.0.0.0", 0)
	if err != nil {
		return fmt.Errorf("open the event listener: %w", err)
	}
	s.eventListener = listener
	go s.acceptEventConnections(listener)
	return nil
}

// codecFromFormat maps an AirPlay 2 audioFormat bit mask to a decoder setup.
// Only the codecs this build decodes are accepted; the rest are refused with a
// precise reason instead of being played as noise.
func codecFromFormat(format uint64) (string, alac.Config, aac.Profile, error) {
	config := alac.Config{FrameLength: 352}
	lc := aac.Config{Profile: aac.ProfileLC}
	switch format {
	case 1 << 11:
		return "pcm", alac.Config{SampleRate: 44100, Channels: 2, BitDepth: 16}, "", nil
	case 1 << 15:
		return "pcm", alac.Config{SampleRate: 48000, Channels: 2, BitDepth: 16}, "", nil
	case 1 << 10:
		return "pcm", alac.Config{SampleRate: 44100, Channels: 1, BitDepth: 16}, "", nil
	case 1 << 18:
		return "alac", alac.Config{FrameLength: 352, SampleRate: 44100, Channels: 2, BitDepth: 16}, "", nil
	case 1 << 19:
		return "alac", alac.Config{FrameLength: 352, SampleRate: 44100, Channels: 2, BitDepth: 24}, "", nil
	case 1 << 20:
		return "alac", alac.Config{FrameLength: 352, SampleRate: 48000, Channels: 2, BitDepth: 16}, "", nil
	case 1 << 21:
		return "alac", alac.Config{FrameLength: 352, SampleRate: 48000, Channels: 2, BitDepth: 24}, "", nil
	case 0:
		return "alac", config, "", nil
	case 1 << 22: // AAC-LC/44.1K/F24/2, one of the formats upstream advertises
		lc.SampleRate, lc.Channels = 44100, 2
		return aacCodec(lc)
	case 1 << 23: // AAC-LC/48K/F24/2
		lc.SampleRate, lc.Channels = 48000, 2
		return aacCodec(lc)
	case 1 << 24, 1 << 31: // AAC-ELD, stereo and mono 44.1K
		eld := aac.Config{Profile: aac.ProfileELD, SampleRate: 44100, Channels: 2}
		if format == 1<<31 {
			eld.Channels = 1
		}
		return aacCodec(eld)
	case 1 << 25, 1 << 32: // AAC-ELD, stereo and mono 48K
		eld := aac.Config{Profile: aac.ProfileELD, SampleRate: 48000, Channels: 2}
		if format == 1<<32 {
			eld.Channels = 1
		}
		return aacCodec(eld)
	case 1 << 26:
		return aacCodec(aac.Config{Profile: aac.ProfileELD, SampleRate: 16000, Channels: 1})
	case 1 << 27:
		return aacCodec(aac.Config{Profile: aac.ProfileELD, SampleRate: 24000, Channels: 1})
	case 1 << 28, 1 << 29, 1 << 30:
		return "", config, "", errors.New("Opus audio is not decodable by this build")
	default:
		return "", config, "", fmt.Errorf("unsupported audio format mask 0x%x", format)
	}
}

// bufferedCodecFromFormat configures the initial decoder for a type-103
// descriptor. Only formats derived in bufferedStreamFormats are accepted:
// ALAC 48 kHz/24-bit stereo (the observed 0x15000000 SSRC) and the two
// AAC-LC stereo masks. A buffered packet may switch among those formats; the
// wire reader owns that switch, while this initial decoder gives the stream a
// concrete rate and channel count before the first packet arrives.
func bufferedCodecFromFormat(format uint64) (string, alac.Config, aac.Profile, error) {
	switch format {
	case bufferedFormatALAC48S24Stereo:
		return "alac", alac.Config{
			FrameLength: 352, SampleRate: 48000, Channels: 2, BitDepth: 24,
		}, "", nil
	case bufferedFormatAAC44F24Stereo:
		return aacCodec(aac.Config{Profile: aac.ProfileLC, SampleRate: 44100, Channels: 2})
	case bufferedFormatAAC48F24Stereo:
		return aacCodec(aac.Config{Profile: aac.ProfileLC, SampleRate: 48000, Channels: 2})
	default:
		return "", alac.Config{}, "", fmt.Errorf("unsupported buffered audio format mask 0x%x", format)
	}
}

// aacCodec reports the decoder configuration for an AAC stream, or the reason
// it cannot be played. The reason is only ever produced by a build without the
// FFmpeg decoder.
func aacCodec(cfg aac.Config) (string, alac.Config, aac.Profile, error) {
	format := alac.Config{SampleRate: uint32(cfg.SampleRate), Channels: uint8(cfg.Channels), BitDepth: 16}
	if !aac.Available() {
		return "", format, "", errors.New("AAC audio requires the FFmpeg decoder; this build has none")
	}
	return "aac", format, cfg.Profile, nil
}

func plistDict(value any) (map[string]any, bool) {
	dict, ok := value.(map[string]any)
	return dict, ok
}

func plistInt(value any) (int64, bool) {
	switch v := value.(type) {
	case int64:
		return v, true
	case uint64:
		if v > uint64(1<<63-1) {
			return 0, false
		}
		return int64(v), true
	case int:
		return int64(v), true
	case float64:
		return int64(v), true
	}
	return 0, false
}

func plistUint(value any) uint64 {
	switch v := value.(type) {
	case int64:
		if v < 0 {
			return 0
		}
		return uint64(v)
	case uint64:
		return v
	case int:
		if v < 0 {
			return 0
		}
		return uint64(v)
	case float64:
		if v < 0 {
			return 0
		}
		return uint64(v)
	}
	return 0
}

// plistUint64 accepts only non-negative plist integers. Protocol fields whose
// exact width or presence matters (audio formats, sequence numbers and clock
// anchors) use this rather than plistUint, which treats reals as integers and
// loses the difference between an absent value and zero.
func plistUint64(value any) (uint64, bool) {
	switch v := value.(type) {
	case int64:
		if v < 0 {
			return 0, false
		}
		return uint64(v), true
	case uint64:
		return v, true
	case int:
		if v < 0 {
			return 0, false
		}
		return uint64(v), true
	}
	return 0, false
}

// plistUint64Bits reads an unsigned 64-bit plist integer by bit pattern. The
// decoder returns an 8-byte integer as int64, so PTP clock IDs and binary
// network-time fractions with the high bit set otherwise look negative.
func plistUint64Bits(value any) (uint64, bool) {
	switch v := value.(type) {
	case int64:
		return uint64(v), true
	case uint64:
		return v, true
	case int:
		return uint64(v), true
	}
	return 0, false
}

// plistUint32Bits reads an unsigned 32-bit protocol field. Compact binary
// values are unsigned; negative 8-byte signed integers are not valid for these
// fields. Only the 64-bit PTP fields use signed bit-pattern reinterpretation.
func plistUint32Bits(value any) (uint32, bool) {
	v, ok := plistUint64(value)
	if !ok || v > uint64(^uint32(0)) {
		return 0, false
	}
	return uint32(v), true
}

// optionalPlistUint32 distinguishes an absent key from a present but malformed
// value, which matters for FLUSHBUFFERED's optional immediate-flush pair. A
// present value must be an integer in the protocol's 32-bit field range.
func optionalPlistUint32(dict map[string]any, key string) (value uint32, present, valid bool) {
	raw, present := dict[key]
	if !present {
		return 0, false, true
	}
	v, ok := plistUint32Bits(raw)
	if !ok {
		return 0, true, false
	}
	return v, true, true
}

func plistFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int64:
		return float64(v), true
	case uint64:
		return float64(v), true
	}
	return 0, false
}

func plistString(value any) string {
	s, _ := value.(string)
	return s
}

func plistData(value any) []byte {
	data, _ := value.([]byte)
	return data
}

func valueOr(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}
