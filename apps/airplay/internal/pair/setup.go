package pair

import (
	"crypto/ed25519"

	"capsulad/apps/airplay/internal/srp"
	"capsulad/apps/airplay/internal/tlv8"
)

// SetupServer is the accessory side of an AirPlay 2 pair-setup exchange.
// DefaultPIN is the HomeKit setup code this accessory answers. Upstream's
// receiver uses the same fixed code ("3939") for both transient and normal
// pair-setup when no AirPlay password is configured (Shairport Sync
// pair_ap/pair_homekit.c, server_setup_new).
const DefaultPIN = "3939"

type SetupServer struct {
	store     *Store
	srp       *srp.Server
	step      int
	transient bool
	complete  bool
	pin       string
}

// NewSetupServer creates a pair-setup server bound to a store. It answers with
// DefaultPIN until SetPIN is called.
func NewSetupServer(s *Store) *SetupServer {
	return &SetupServer{store: s, pin: DefaultPIN}
}

// SetPIN selects the setup code used as the SRP password. An empty value means
// DefaultPIN: a transient/normal pair-setup with no code configured cannot
// succeed with an empty password.
func (ss *SetupServer) SetPIN(pin string) {
	if pin == "" {
		pin = DefaultPIN
	}
	ss.pin = pin
}

// PIN reports the setup code in use.
func (ss *SetupServer) PIN() string { return ss.pin }

// Handle processes one pair-setup request TLV8 payload and returns the
// response payload. Protocol-level refusals are reported as a TLV8 response
// with State + Error, never as a Go error.
func (ss *SetupServer) Handle(req []byte) ([]byte, error) {
	items, err := tlv8.Decode(req)
	if err != nil {
		return nil, err
	}
	state, ok := tlv8.GetUint8(items, tlv8.TagState)
	if !ok {
		return errorResponse(tlv8.StateM2, tlv8.ErrorUnknown), nil
	}
	switch state {
	case tlv8.StateM1:
		return ss.handleM1(items)
	case tlv8.StateM3:
		if ss.step != 1 {
			return errorResponse(tlv8.StateM4, tlv8.ErrorUnknown), nil
		}
		return ss.handleM3(items)
	case tlv8.StateM5:
		if ss.step != 2 {
			return errorResponse(tlv8.StateM6, tlv8.ErrorAuthentication), nil
		}
		return ss.handleM5(items)
	}
	return errorResponse(state, tlv8.ErrorUnknown), nil
}

func (ss *SetupServer) handleM1(items []tlv8.Item) ([]byte, error) {
	method, _ := tlv8.GetUint8(items, tlv8.TagMethod)
	if method != tlv8.MethodPairSetup {
		return errorResponse(tlv8.StateM2, tlv8.ErrorUnavailable), nil
	}
	if flags, ok := tlv8.GetUint8(items, tlv8.TagFlags); ok {
		ss.transient = flags&tlv8.FlagTransient != 0
	}
	s, err := srp.NewServer("Pair-Setup", ss.pin)
	if err != nil {
		return nil, err
	}
	ss.srp = s
	ss.step = 1
	return tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM2}},
		tlv8.Item{Tag: tlv8.TagSalt, Value: s.Salt()},
		tlv8.Item{Tag: tlv8.TagPublicKey, Value: s.PublicKey()},
	), nil
}

func (ss *SetupServer) handleM3(items []tlv8.Item) ([]byte, error) {
	a, ok := tlv8.Get(items, tlv8.TagPublicKey)
	if !ok {
		return errorResponse(tlv8.StateM4, tlv8.ErrorAuthentication), nil
	}
	if err := ss.srp.SetClientPublicKey(a); err != nil {
		return errorResponse(tlv8.StateM4, tlv8.ErrorAuthentication), nil
	}
	proof, ok := tlv8.Get(items, tlv8.TagProof)
	if !ok {
		return errorResponse(tlv8.StateM4, tlv8.ErrorAuthentication), nil
	}
	if err := ss.srp.VerifyClientProof(proof); err != nil {
		return errorResponse(tlv8.StateM4, tlv8.ErrorAuthentication), nil
	}
	ss.step = 2
	if ss.transient {
		// HAP transient pairing covers M1..M4 only: the SRP session key is the
		// shared secret, and the controller never sends M5/M6 (pyatv
		// `hap_transient.py`; Shairport Sync `server_setup_response2`).
		ss.complete = true
	}
	return tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM4}},
		tlv8.Item{Tag: tlv8.TagProof, Value: ss.srp.Proof()},
	), nil
}

// TransientComplete reports that a transient pair-setup finished at M4 and no
// further message is expected. The caller derives the channel keys from
// SessionKey at this point.
func (ss *SetupServer) TransientComplete() bool { return ss.complete }

// SessionKey is the SRP session key (SHA-512 of S). For a transient setup it
// is the shared secret for the control and event channels.
func (ss *SetupServer) SessionKey() []byte {
	if ss.srp == nil {
		return nil
	}
	return ss.srp.SessionKey()
}

func (ss *SetupServer) handleM5(items []tlv8.Item) ([]byte, error) {
	enc, ok := tlv8.Get(items, tlv8.TagEncryptedData)
	if !ok {
		return errorResponse(tlv8.StateM6, tlv8.ErrorAuthentication), nil
	}
	key := hkdf32(ss.srp.SessionKey(), "Pair-Setup-Encrypt-Salt", "Pair-Setup-Encrypt-Info")
	plain, err := open(key, "PS-Msg05", enc)
	if err != nil {
		return errorResponse(tlv8.StateM6, tlv8.ErrorAuthentication), nil
	}
	sub, err := tlv8.Decode(plain)
	if err != nil {
		return errorResponse(tlv8.StateM6, tlv8.ErrorAuthentication), nil
	}
	id := tlv8.GetString(sub, tlv8.TagIdentifier)
	ltpk, ok1 := tlv8.Get(sub, tlv8.TagPublicKey)
	sig, ok2 := tlv8.Get(sub, tlv8.TagSignature)
	if !ok1 || !ok2 || len(ltpk) != ed25519.PublicKeySize {
		return errorResponse(tlv8.StateM6, tlv8.ErrorAuthentication), nil
	}
	controllerX := hkdf32(ss.srp.SessionKey(), "Pair-Setup-Controller-Sign-Salt", "Pair-Setup-Controller-Sign-Info")
	if !ed25519.Verify(ed25519.PublicKey(ltpk), newHasher(controllerX, []byte(id), ltpk), sig) {
		return errorResponse(tlv8.StateM6, tlv8.ErrorAuthentication), nil
	}

	// A controller that completed pair-setup is an administrator: it verified
	// the setup code, and HAP's first/again paired controllers gain admin. A
	// transient setup is not a persistent pairing: answer M6 (some senders
	// follow the state machine past M4) but store nothing.
	if !ss.transient {
		ss.store.SetController(id, ltpk, true)
	}

	accessoryX := hkdf32(ss.srp.SessionKey(), "Pair-Setup-Accessory-Sign-Salt", "Pair-Setup-Accessory-Sign-Info")
	ltpkAccessory := ss.store.Device.LTPK[:]
	sig6 := ss.store.Device.sign(accessoryX, []byte(ss.store.Device.PairingID), ltpkAccessory)
	out := tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagIdentifier, Value: []byte(ss.store.Device.PairingID)},
		tlv8.Item{Tag: tlv8.TagPublicKey, Value: ltpkAccessory},
		tlv8.Item{Tag: tlv8.TagSignature, Value: sig6},
	)
	encOut, err := seal(key, "PS-Msg06", out)
	if err != nil {
		return nil, err
	}
	ss.step = 3
	return tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM6}},
		tlv8.Item{Tag: tlv8.TagEncryptedData, Value: encOut},
	), nil
}
