package session

// HomeKit pairing beyond pair-verify: the setup code, the persisted controller
// permissions and the pair-add/pair-remove/pair-list administration endpoints.
// Upstream's receiver keeps pairings in memory, hard-codes admin on list and
// performs no session check; this test pins the equivalent wire behaviour with
// the stronger ownership model this receiver documents.

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"golang.org/x/crypto/chacha20poly1305"
	"math/big"
	"strings"
	"testing"
	"time"

	"capsulad/apps/airplay/internal/hapcrypt"
	"capsulad/apps/airplay/internal/pair"
	"capsulad/apps/airplay/internal/plist"
	"capsulad/apps/airplay/internal/srp"
	"capsulad/apps/airplay/internal/tlv8"
)

var (
	srpN, srpG = srp.Modulus(), srp.Generator()
	srpPadLen  = (srpN.BitLen() + 7) / 8
)

func srpHash(parts ...[]byte) *big.Int {
	hash := sha512.New()
	for _, part := range parts {
		hash.Write(part)
	}
	return new(big.Int).SetBytes(hash.Sum(nil))
}

func srpPadded(value *big.Int) []byte {
	out := make([]byte, srpPadLen)
	value.FillBytes(out)
	return out
}

func srpMinimal(value *big.Int) []byte {
	return value.Bytes()
}

// srpClientProof is the controller side of SRP-6a with the conventions the
// accessory uses (k = H(N,g) padded, u = H(A,B) padded, minimal H(N)/H(g) in
// M1).
func srpClientProof(username, password string, salt, serverPublic []byte) (publicA, proofM1, serverProof, sessionKey []byte, err error) {
	bPublic := new(big.Int).SetBytes(serverPublic)
	if bPublic.Sign() == 0 {
		return nil, nil, nil, nil, errors.New("server public key is zero")
	}
	secret := make([]byte, 64)
	if _, err := rand.Read(secret); err != nil {
		return nil, nil, nil, nil, err
	}
	a := new(big.Int).SetBytes(secret)
	publicAInt := new(big.Int).Exp(srpG, a, srpN)
	usernameHash := srpHash([]byte(username), []byte(":"), []byte(password))
	x := srpHash(salt, usernameHash.Bytes())
	verifier := new(big.Int).Exp(srpG, x, srpN)
	k := srpHash(srpPadded(srpN), srpPadded(srpG))
	u := srpHash(srpPadded(publicAInt), srpPadded(bPublic))
	base := new(big.Int).Sub(bPublic, new(big.Int).Mul(k, verifier))
	base.Mod(base, srpN)
	exponent := new(big.Int).Add(a, new(big.Int).Mul(u, x))
	sessionSecret := new(big.Int).Exp(base, exponent, srpN)
	keyHash := sha512.Sum512(srpMinimal(sessionSecret))
	sessionKey = keyHash[:]
	hN := sha512.Sum512(srpN.Bytes())
	hG := sha512.Sum512(srpG.Bytes())
	var xor [sha512.Size]byte
	for i := range xor {
		xor[i] = hN[i] ^ hG[i]
	}
	usernameDigest := srpHash([]byte(username))
	m1 := srpHash(xor[:], usernameDigest.Bytes(), salt, srpMinimal(publicAInt), srpMinimal(bPublic), sessionKey)
	m2 := srpHash(srpMinimal(publicAInt), m1.Bytes(), sessionKey)
	return srpMinimal(publicAInt), m1.Bytes(), m2.Bytes(), sessionKey, nil
}

// pairSetup runs the full controller side of pair-setup with the given setup
// code. It returns the server's TLV error code when the accessory refuses.
func pairSetup(t *testing.T, client *rtspClient, pairingID string, key ed25519.PrivateKey, password string) (byte, error) {
	t.Helper()
	response, err := client.do("POST", "/pair-setup", "application/pairing+tlv8", tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM1}},
		tlv8.Item{Tag: tlv8.TagMethod, Value: []byte{tlv8.MethodPairSetup}},
	))
	if err != nil {
		return 0, err
	}
	items, err := tlv8.Decode(response.body)
	if err != nil {
		return 0, err
	}
	if code, ok := tlv8.GetUint8(items, tlv8.TagError); ok {
		t.Logf("pair-setup M1->M2 refused with TLV error %d", code)
		return code, nil
	}
	if state, _ := tlv8.GetUint8(items, tlv8.TagState); state != tlv8.StateM2 {
		return 0, errors.New("pair-setup M2 state mismatch")
	}
	salt, _ := tlv8.Get(items, tlv8.TagSalt)
	serverPublic, _ := tlv8.Get(items, tlv8.TagPublicKey)
	publicA, proof, expectedServerProof, sessionKey, err := srpClientProof("Pair-Setup", password, salt, serverPublic)
	if err != nil {
		return 0, err
	}
	response, err = client.do("POST", "/pair-setup", "application/pairing+tlv8", tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM3}},
		tlv8.Item{Tag: tlv8.TagPublicKey, Value: publicA},
		tlv8.Item{Tag: tlv8.TagProof, Value: proof},
	))
	if err != nil {
		return 0, err
	}
	items, err = tlv8.Decode(response.body)
	if err != nil {
		return 0, err
	}
	if code, ok := tlv8.GetUint8(items, tlv8.TagError); ok {
		t.Logf("pair-setup M3->M4 refused with TLV error %d", code)
		return code, nil
	}
	if state, _ := tlv8.GetUint8(items, tlv8.TagState); state != tlv8.StateM4 {
		return 0, errors.New("pair-setup M4 state mismatch")
	}
	serverProof, _ := tlv8.Get(items, tlv8.TagProof)
	if len(expectedServerProof) == 0 || subtle.ConstantTimeCompare(serverProof, expectedServerProof) != 1 {
		return 0, errors.New("pair-setup accessory proof mismatch")
	}

	ltpk := key.Public().(ed25519.PublicKey)
	controllerX := hkdfKey(sessionKey, "Pair-Setup-Controller-Sign-Salt", "Pair-Setup-Controller-Sign-Info")
	signature := ed25519.Sign(key, append(append(append([]byte{}, controllerX...), []byte(pairingID)...), ltpk...))
	sub := tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagIdentifier, Value: []byte(pairingID)},
		tlv8.Item{Tag: tlv8.TagPublicKey, Value: ltpk},
		tlv8.Item{Tag: tlv8.TagSignature, Value: signature},
	)
	sealed, err := sealNonce(hkdfKey(sessionKey, "Pair-Setup-Encrypt-Salt", "Pair-Setup-Encrypt-Info"), "PS-Msg05", sub)
	if err != nil {
		return 0, err
	}
	response, err = client.do("POST", "/pair-setup", "application/pairing+tlv8", tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM5}},
		tlv8.Item{Tag: tlv8.TagEncryptedData, Value: sealed},
	))
	if err != nil {
		return 0, err
	}
	items, err = tlv8.Decode(response.body)
	if err != nil {
		return 0, err
	}
	if code, ok := tlv8.GetUint8(items, tlv8.TagError); ok {
		t.Logf("pair-setup M5->M6 refused with TLV error %d", code)
		return code, nil
	}
	if state, _ := tlv8.GetUint8(items, tlv8.TagState); state != tlv8.StateM6 {
		return 0, errors.New("pair-setup M6 state mismatch")
	}
	return 0, nil
}

// pairListEntry is one record of a pair-list answer.
type pairListEntry struct {
	ID          string
	LTPK        []byte
	Permissions byte
}

func pairList(t *testing.T, client *rtspClient) ([]pairListEntry, byte, error) {
	t.Helper()
	response, err := client.do("POST", "/pair-list", "application/pairing+tlv8", tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM1}},
		tlv8.Item{Tag: tlv8.TagMethod, Value: []byte{tlv8.MethodListPairings}},
	))
	if err != nil {
		return nil, 0, err
	}
	items, err := tlv8.Decode(response.body)
	if err != nil {
		return nil, 0, err
	}
	if code, ok := tlv8.GetUint8(items, tlv8.TagError); ok {
		return nil, code, nil
	}
	var entries []pairListEntry
	for _, item := range items {
		switch item.Tag {
		case tlv8.TagIdentifier:
			entries = append(entries, pairListEntry{ID: string(item.Value)})
		case tlv8.TagPublicKey:
			if n := len(entries); n > 0 {
				entries[n-1].LTPK = item.Value
			}
		case tlv8.TagPermissions:
			if n := len(entries); n > 0 && len(item.Value) == 1 {
				entries[n-1].Permissions = item.Value[0]
			}
		}
	}
	return entries, 0, nil
}

func pairBody(method byte, id string, ltpk []byte, permissions *byte) []byte {
	items := []tlv8.Item{
		{Tag: tlv8.TagState, Value: []byte{tlv8.StateM1}},
		{Tag: tlv8.TagMethod, Value: []byte{method}},
		{Tag: tlv8.TagIdentifier, Value: []byte(id)},
	}
	if ltpk != nil {
		items = append(items, tlv8.Item{Tag: tlv8.TagPublicKey, Value: ltpk})
	}
	if permissions != nil {
		items = append(items, tlv8.Item{Tag: tlv8.TagPermissions, Value: []byte{*permissions}})
	}
	return tlv8.Encode(items...)
}

func pairResult(t *testing.T, client *rtspClient, path string, body []byte) (byte, error) {
	t.Helper()
	response, err := client.do("POST", path, "application/pairing+tlv8", body)
	if err != nil {
		return 0, err
	}
	if response.status != 200 {
		return 0, errors.New("pairing request was refused")
	}
	items, err := tlv8.Decode(response.body)
	if err != nil {
		return 0, err
	}
	code, _ := tlv8.GetUint8(items, tlv8.TagError)
	return code, nil
}

func TestPairSetupAndAdministration(t *testing.T) {
	mgr, transport, _, _, _, _, _ := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("controller key: %v", err)
	}
	pairingID := "sender-pairing-id"
	mgr.Store().SetControllerLTPK(pairingID, public)
	mgr.Store().RemoveController("pre-registered-only") // no-op, keeps the store explicit

	conn, err := transport.dial(7000)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	client := newRTSPClient(conn)

	code, err := pairSetup(t, client, pairingID, private, pair.DefaultPIN)
	if err != nil {
		t.Fatalf("pair-setup: %v", err)
	}
	if code != 0 {
		t.Fatalf("pair-setup refused with TLV error %d", code)
	}
	record, ok := mgr.Store().Controller(pairingID)
	if !ok || !record.Admin {
		t.Fatalf("pair-setup did not record an administrator: %+v", record)
	}

	// Pair-verify with the key pair-setup registered, then switch the channel
	// to the encrypted framing the administration endpoints require.
	shared := pairVerify(t, client, pairingID, private)
	control := hapcrypt.ControlChannelKeys(shared)
	encrypted, err := hapcrypt.NewSession(conn, control.Inbound, control.Outbound)
	if err != nil {
		t.Fatalf("client channel: %v", err)
	}
	client.setStream(&rwStream{rw: encrypted})

	// Pairing administration needs an authenticated controller, not media
	// ownership: the pair-setup/pair-verify connection is still a candidate.
	if got := mgr.Active(); got != nil {
		t.Fatal("pairing administration took media ownership")
	}

	entries, code, err := pairList(t, client)
	if err != nil || code != 0 {
		t.Fatalf("pair-list: %v code=%d", err, code)
	}
	if len(entries) != 1 || entries[0].ID != pairingID || entries[0].Permissions != 1 {
		t.Fatalf("pair-list = %+v", entries)
	}
	if !strings.EqualFold(string(entries[0].LTPK), string(public)) {
		t.Fatalf("pair-list returned a different public key")
	}

	// Add a non-administrator controller.
	secondPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("second key: %v", err)
	}
	user := byte(0)
	if code, err := pairResult(t, client, "/pair-add", pairBody(tlv8.MethodAddPairing, "second", secondPublic, &user)); err != nil || code != 0 {
		t.Fatalf("pair-add: %v code=%d", err, code)
	}
	second, ok := mgr.Store().Controller("second")
	if !ok || second.Admin {
		t.Fatalf("added controller = %+v, ok=%v", second, ok)
	}
	entries, code, err = pairList(t, client)
	if err != nil || code != 0 || len(entries) != 2 {
		t.Fatalf("pair-list after add: %v code=%d entries=%+v", err, code, entries)
	}
	for _, entry := range entries {
		if entry.ID == "second" && entry.Permissions != 0 {
			t.Fatalf("user controller listed as admin: %+v", entry)
		}
	}

	// Remove it again.
	if code, err := pairResult(t, client, "/pair-remove", pairBody(tlv8.MethodRemovePairing, "second", nil, nil)); err != nil || code != 0 {
		t.Fatalf("pair-remove: %v code=%d", err, code)
	}
	if _, ok := mgr.Store().Controller("second"); ok {
		t.Fatal("pair-remove did not delete the controller")
	}
	// Removing an unknown pairing is the HAP "unknown" error.
	if code, err := pairResult(t, client, "/pair-remove", pairBody(tlv8.MethodRemovePairing, "missing", nil, nil)); err != nil || code != tlv8.ErrorUnknown {
		t.Fatalf("remove unknown: %v code=%d", err, code)
	}

	// A non-administrator session cannot use the administration surface.
	mgr.Store().SetController(pairingID, public, false)
	if _, code, err := pairList(t, client); err != nil || code != tlv8.ErrorAuthentication {
		t.Fatalf("non-admin pair-list: %v code=%d", err, code)
	}
	mgr.Store().SetController(pairingID, public, true)

	// Removing the controller that owns this session ends the session.
	if code, err := pairResult(t, client, "/pair-remove", pairBody(tlv8.MethodRemovePairing, pairingID, nil, nil)); err != nil || code != 0 {
		t.Fatalf("self remove: %v code=%d", err, code)
	}
	// The candidate must be closed by the server after its success response;
	// its board was never live, so board.Live alone would not prove this.
	readDone := make(chan error, 1)
	go func() {
		_, err := client.reader.ReadByte()
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("the self-removed candidate connection stayed open")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the self-removed candidate connection was not closed")
	}
	waitUntil(t, "the self-removed candidate to leave the pending set", func() bool { return pendingCount(mgr) == 0 })
	if mgr.Active() != nil {
		t.Fatal("self-removal promoted the candidate")
	}
	if mgr.Store().ControllerCount() != 0 {
		t.Fatalf("pairings left after self-removal: %d", mgr.Store().ControllerCount())
	}
}

func TestPairSetupWrongCodeIsRefused(t *testing.T) {
	mgr, transport, _, _, _, _, _ := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("controller key: %v", err)
	}
	conn, err := transport.dial(7000)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	client := newRTSPClient(conn)
	code, err := pairSetup(t, client, "wrong-code", private, "0000")
	if err != nil {
		t.Fatalf("pair-setup: %v", err)
	}
	if code != tlv8.ErrorAuthentication {
		t.Fatalf("wrong setup code answered TLV error %d, want %d", code, tlv8.ErrorAuthentication)
	}
	if _, ok := mgr.Store().ControllerLTPK("wrong-code"); ok {
		t.Fatal("a refused pair-setup stored a controller")
	}
}

// pairSetupTransient is a real transient sender: /pair-pin-start, then M1..M4
// with the transient flag, and no M5/M6 (pyatv hap_transient.py). It returns
// the SRP session key the controller uses for the channel. It also pins the
// reference /pair-setup response MIME on both plaintext replies; the callers
// continue on the encrypted channel, so the proof and keying stay covered end
// to end.
func pairSetupTransient(t *testing.T, client *rtspClient, password string) ([]byte, error) {
	t.Helper()
	if response, err := client.do("POST", "/pair-pin-start", "application/octet-stream", nil); err != nil {
		return nil, err
	} else if response.status != 200 {
		t.Fatalf("pair-pin-start answered %d", response.status)
	}
	response, err := client.do("POST", "/pair-setup", "application/octet-stream", tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM1}},
		tlv8.Item{Tag: tlv8.TagMethod, Value: []byte{tlv8.MethodPairSetup}},
		tlv8.Item{Tag: tlv8.TagFlags, Value: []byte{tlv8.FlagTransient}},
	))
	if err != nil {
		return nil, err
	}
	if response.status != 200 {
		t.Fatalf("pair-setup M2 answered %d", response.status)
	}
	if got := response.headers["content-type"]; got != "application/octet-stream" {
		t.Fatalf("pair-setup M2 content type = %q, want application/octet-stream", got)
	}
	items, err := tlv8.Decode(response.body)
	if err != nil {
		return nil, err
	}
	if _, ok := tlv8.GetUint8(items, tlv8.TagError); ok {
		return nil, errors.New("pair-setup was refused")
	}
	if state, _ := tlv8.GetUint8(items, tlv8.TagState); state != tlv8.StateM2 {
		return nil, errors.New("pair-setup M2 state mismatch")
	}
	salt, _ := tlv8.Get(items, tlv8.TagSalt)
	serverPublic, _ := tlv8.Get(items, tlv8.TagPublicKey)
	publicA, proof, expectedServerProof, sessionKey, err := srpClientProof("Pair-Setup", password, salt, serverPublic)
	if err != nil {
		return nil, err
	}
	response, err = client.do("POST", "/pair-setup", "application/octet-stream", tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM3}},
		tlv8.Item{Tag: tlv8.TagPublicKey, Value: publicA},
		tlv8.Item{Tag: tlv8.TagProof, Value: proof},
	))
	if err != nil {
		return nil, err
	}
	if response.status != 200 {
		t.Fatalf("pair-setup M4 answered %d", response.status)
	}
	if got := response.headers["content-type"]; got != "application/octet-stream" {
		t.Fatalf("pair-setup M4 content type = %q, want application/octet-stream", got)
	}
	items, err = tlv8.Decode(response.body)
	if err != nil {
		return nil, err
	}
	if _, ok := tlv8.GetUint8(items, tlv8.TagError); ok {
		return nil, errors.New("pair-setup was refused")
	}
	if state, _ := tlv8.GetUint8(items, tlv8.TagState); state != tlv8.StateM4 {
		return nil, errors.New("pair-setup M4 state mismatch")
	}
	serverProof, _ := tlv8.Get(items, tlv8.TagProof)
	if subtle.ConstantTimeCompare(serverProof, expectedServerProof) != 1 {
		return nil, errors.New("accessory proof mismatch")
	}
	return sessionKey, nil
}

// TestTransientPairingDrivesTheWholeSession is the sender flow pyatv performs:
// pair-pin-start, a four-state transient pair-setup (whose helper pins the
// reference M2/M4 response MIME), then every RTSP request and the audio channel
// keyed from the SRP session secret, with no pair-verify.
func TestTransientPairingDrivesTheWholeSession(t *testing.T) {
	mgr, transport, _, _, sink, _, _ := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	conn, err := transport.dial(7000)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	controllersBefore := mgr.Store().ControllerCount()
	client := newRTSPClient(conn)
	sessionKey, err := pairSetupTransient(t, client, pair.DefaultPIN)
	if err != nil {
		t.Fatalf("transient pair-setup: %v", err)
	}
	if len(sessionKey) != 64 {
		t.Fatalf("SRP session key length = %d", len(sessionKey))
	}
	if got := mgr.Store().ControllerCount(); got != controllersBefore {
		t.Fatalf("transient pairing changed the controller count from %d to %d", controllersBefore, got)
	}

	// The controller writes with Control-Write and reads with Control-Read; the
	// accessory's ControlChannelKeys helper names outbound/inbound explicitly.
	control := hapcrypt.ControlChannelKeys(sessionKey)
	encrypted, err := hapcrypt.NewSession(conn, control.Inbound, control.Outbound)
	if err != nil {
		t.Fatalf("client channel: %v", err)
	}
	client.setStream(&rwStream{rw: encrypted})

	// Every subsequent request is encrypted; if the direction were inverted the
	// accessory could not decrypt this request and would close the session.
	info, err := client.do("GET", "/info", "", nil)
	if err != nil {
		t.Fatalf("encrypted GET /info: %v", err)
	}
	if info.status != 200 {
		t.Fatalf("encrypted GET /info answered %d", info.status)
	}
	decoded, err := plist.Decode(info.body)
	if err != nil {
		t.Fatalf("decode /info: %v", err)
	}
	if dict, _ := decoded.(map[string]any); dict["deviceID"] != "aabbccddeeff" {
		t.Fatalf("/info deviceID = %v", dict["deviceID"])
	}

	// And a realtime PCM stream still plays in a transient session.
	shk := make([]byte, 32)
	if _, err := rand.Read(shk); err != nil {
		t.Fatalf("shk: %v", err)
	}
	body, err := plist.EncodeBinary(map[string]any{
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
	setup, err := client.do("SETUP", "/1", "application/x-apple-binary-plist", body)
	if err != nil || setup.status != 200 {
		t.Fatalf("SETUP = %v %v", setup, err)
	}
	decoded, err = plist.Decode(setup.body)
	if err != nil {
		t.Fatalf("decode SETUP: %v", err)
	}
	dict, _ := decoded.(map[string]any)
	streams, _ := dict["streams"].([]any)
	if len(streams) != 1 {
		t.Fatalf("SETUP streams = %v", dict["streams"])
	}
	streamDict, _ := streams[0].(map[string]any)
	dataPort := int(plistUint(streamDict["dataPort"]))
	if dataPort == 0 {
		t.Fatalf("SETUP dataPort = %v", streamDict["dataPort"])
	}
	if response, err := client.do("RECORD", "/1", "", nil); err != nil || response.status != 200 {
		t.Fatalf("RECORD = %v %v", response, err)
	}
	sendAudio, err := transport.sendUDP(dataPort)
	if err != nil {
		t.Fatalf("audio socket: %v", err)
	}
	samples := make([]byte, 352*2*2)
	aead, err := chacha20poly1305.New(shk)
	if err != nil {
		t.Fatalf("aead: %v", err)
	}
	var timestamp uint32 = 1
	for seq := 0; seq < 12; seq++ {
		header := make([]byte, 12)
		header[0] = 0x80
		header[1] = byte(streamTypeRealtime)
		binary.BigEndian.PutUint16(header[2:4], uint16(seq))
		binary.BigEndian.PutUint32(header[4:8], timestamp)
		binary.BigEndian.PutUint32(header[8:12], 0x99aabbcc)
		nonce := make([]byte, 12)
		binary.LittleEndian.PutUint64(nonce[4:], uint64(seq)+1)
		ciphertext := aead.Seal(nil, nonce, samples, header[4:12])
		packet := append(append(append([]byte{}, header...), ciphertext...), nonce[4:]...)
		if err := sendAudio(packet); err != nil {
			t.Fatalf("audio datagram: %v", err)
		}
		timestamp += 352
	}
	deadline := time.Now().Add(4 * time.Second)
	written := 0
	for time.Now().Before(deadline) {
		written, _ = sink.state()
		if written > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if written == 0 {
		t.Fatal("no PCM reached the sink in the transient session")
	}
	if response, err := client.do("TEARDOWN", "/1", "", nil); err != nil || response.status != 200 {
		t.Fatalf("TEARDOWN = %v %v", response, err)
	}
}

// TestFailedPairVerifyDoesNotEncryptOrPromote pins the authentication boundary
// of pair-verify. M3's proof is carried in a TLV inside an ordinary RTSP 200,
// so a rejected controller is not a transport error: the response is 200 with
// kTLVError_Authentication. The accessory computes the X25519 secret at M1,
// before it knows whether M3 will prove the pairing, so keying or promoting on
// the secret alone would treat a rejected controller as verified and switch a
// still-plaintext connection to HAP framing. The live sender must survive.
func TestFailedPairVerifyDoesNotEncryptOrPromote(t *testing.T) {
	mgr, transport, advertiser, _, sink, _, pairingID := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	// A first connection acquires media ownership with an accepted ANNOUNCE.
	// Transient pairing alone now protects and keys a connection but does not
	// promote it, so the live baseline must come from a media request.
	conn1, err := transport.dial(7000)
	if err != nil {
		t.Fatalf("dial live sender: %v", err)
	}
	client1 := newRTSPClient(conn1)
	announce(t, client1)
	waitUntil(t, "the announced live session", func() bool { return mgr.Active() != nil })
	live := mgr.Active()
	if live == nil {
		t.Fatal("the first session was not promoted")
	}
	// Wait for the promotion's active republish, then treat that as the
	// baseline: a rejected candidate must not add another one.
	for i := 0; i < 200 && advertiser.count("_airplay._tcp") < 2; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	before := advertiser.count("_airplay._tcp")
	writtenBefore, _ := sink.state()

	// A second connection completes M1/M2 and then signs M3 with the wrong key.
	conn2, err := transport.dial(7000)
	if err != nil {
		t.Fatalf("dial rejected sender: %v", err)
	}
	client2 := newRTSPClient(conn2)
	curveKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("curve key: %v", err)
	}
	m1 := tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM1}},
		tlv8.Item{Tag: tlv8.TagPublicKey, Value: curveKey.PublicKey().Bytes()},
	)
	response, err := client2.do("POST", "/pair-verify", "application/pairing+tlv8", m1)
	if err != nil {
		t.Fatalf("pair-verify M1: %v", err)
	}
	items, err := tlv8.Decode(response.body)
	if err != nil {
		t.Fatalf("pair-verify M2: %v", err)
	}
	accessoryPub, _ := tlv8.Get(items, tlv8.TagPublicKey)
	encryptedM2, _ := tlv8.Get(items, tlv8.TagEncryptedData)
	if len(accessoryPub) != 32 || len(encryptedM2) == 0 {
		t.Fatalf("M2 payload pub=%d enc=%d", len(accessoryPub), len(encryptedM2))
	}
	peer, err := ecdh.X25519().NewPublicKey(accessoryPub)
	if err != nil {
		t.Fatalf("accessory curve key: %v", err)
	}
	shared, err := curveKey.ECDH(peer)
	if err != nil {
		t.Fatalf("ECDH: %v", err)
	}
	verifyKey := hkdfKey(shared, "Pair-Verify-Encrypt-Salt", "Pair-Verify-Encrypt-Info")
	_, wrongKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("wrong controller key: %v", err)
	}
	deviceInfo := append(append(append([]byte{}, curveKey.PublicKey().Bytes()...), []byte(pairingID)...), accessoryPub...)
	sealed, err := sealNonce(verifyKey, "PV-Msg03", tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagIdentifier, Value: []byte(pairingID)},
		tlv8.Item{Tag: tlv8.TagSignature, Value: ed25519.Sign(wrongKey, deviceInfo)},
	))
	if err != nil {
		t.Fatalf("seal M3: %v", err)
	}
	m3 := tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM3}},
		tlv8.Item{Tag: tlv8.TagEncryptedData, Value: sealed},
	)
	response, err = client2.do("POST", "/pair-verify", "application/pairing+tlv8", m3)
	if err != nil {
		t.Fatalf("pair-verify M3: %v", err)
	}
	if response.status != 200 {
		t.Fatalf("rejected M3 RTSP status = %d, want 200 with a TLV error", response.status)
	}
	items, err = tlv8.Decode(response.body)
	if err != nil {
		t.Fatalf("pair-verify M4: %v", err)
	}
	if _, ok := tlv8.GetUint8(items, tlv8.TagError); !ok {
		t.Fatal("a bad M3 signature was not refused with a TLV error")
	}

	// The failed exchange must not switch framing: a plaintext request on the
	// same socket still reaches the dispatcher.
	if info, err := client2.do("GET", "/info", "", nil); err != nil || info.status != 200 {
		t.Fatalf("failed pair-verify switched the channel to encrypted framing: %v %v", info, err)
	}
	if got := mgr.Active(); got != live {
		t.Fatal("failed pair-verify promoted the rejected candidate over the live session")
	}
	if got := advertiser.count("_airplay._tcp"); got != before {
		t.Fatalf("failed pair-verify republished advertisements: %d, want %d", got, before)
	}
	if written, _ := sink.state(); written != writtenBefore {
		t.Fatalf("failed pair-verify changed the sink: %d, want %d", written, writtenBefore)
	}
	if info, err := client1.do("GET", "/info", "", nil); err != nil || info.status != 200 {
		t.Fatalf("live session lost its channel: %v %v", info, err)
	}
}
