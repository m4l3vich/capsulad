package pair

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"

	"capsulad/apps/airplay/internal/tlv8"
)

// VerifyServer is the accessory side of an AirPlay 2 pair-verify exchange.
type VerifyServer struct {
	store        *Store
	priv         *ecdh.PrivateKey
	curvePub     []byte
	clientPub    []byte
	shared       []byte
	encKey       []byte
	ltpk         []byte
	controllerID string
	step         int
}

// NewVerifyServer creates a pair-verify server bound to a store.
func NewVerifyServer(s *Store) *VerifyServer {
	return &VerifyServer{store: s}
}

// Handle processes one pair-verify request (M1 or M3) and returns the
// response payload.
func (v *VerifyServer) Handle(req []byte) ([]byte, error) {
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
		return v.handleM1(items)
	case tlv8.StateM3:
		if v.step != 1 {
			return errorResponse(tlv8.StateM4, tlv8.ErrorAuthentication), nil
		}
		return v.handleM3(items)
	}
	return errorResponse(state, tlv8.ErrorUnknown), nil
}

func (v *VerifyServer) handleM1(items []tlv8.Item) ([]byte, error) {
	clientPub, ok := tlv8.Get(items, tlv8.TagPublicKey)
	if !ok || len(clientPub) != 32 {
		return errorResponse(tlv8.StateM2, tlv8.ErrorAuthentication), nil
	}
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	peer, err := ecdh.X25519().NewPublicKey(clientPub)
	if err != nil {
		return errorResponse(tlv8.StateM2, tlv8.ErrorAuthentication), nil
	}
	shared, err := priv.ECDH(peer)
	if err != nil {
		return errorResponse(tlv8.StateM2, tlv8.ErrorAuthentication), nil
	}
	v.priv = priv
	v.curvePub = priv.PublicKey().Bytes()
	v.clientPub = append([]byte(nil), clientPub...)
	v.shared = shared
	v.encKey = hkdf32(shared, "Pair-Verify-Encrypt-Salt", "Pair-Verify-Encrypt-Info")
	v.step = 1

	sig := v.store.Device.sign(v.curvePub, []byte(v.store.Device.PairingID), v.clientPub)
	sub := tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagIdentifier, Value: []byte(v.store.Device.PairingID)},
		tlv8.Item{Tag: tlv8.TagSignature, Value: sig},
	)
	enc, err := seal(v.encKey, "PV-Msg02", sub)
	if err != nil {
		return nil, err
	}
	return tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM2}},
		tlv8.Item{Tag: tlv8.TagPublicKey, Value: v.curvePub},
		tlv8.Item{Tag: tlv8.TagEncryptedData, Value: enc},
	), nil
}

func (v *VerifyServer) handleM3(items []tlv8.Item) ([]byte, error) {
	enc, ok := tlv8.Get(items, tlv8.TagEncryptedData)
	if !ok {
		return errorResponse(tlv8.StateM4, tlv8.ErrorAuthentication), nil
	}
	plain, err := open(v.encKey, "PV-Msg03", enc)
	if err != nil {
		return errorResponse(tlv8.StateM4, tlv8.ErrorAuthentication), nil
	}
	sub, err := tlv8.Decode(plain)
	if err != nil {
		return errorResponse(tlv8.StateM4, tlv8.ErrorAuthentication), nil
	}
	id := tlv8.GetString(sub, tlv8.TagIdentifier)
	v.controllerID = id
	sig, ok := tlv8.Get(sub, tlv8.TagSignature)
	if !ok {
		return errorResponse(tlv8.StateM4, tlv8.ErrorAuthentication), nil
	}
	ltpk, found := v.store.ControllerLTPK(id)
	if !found || len(ltpk) != ed25519.PublicKeySize {
		return errorResponse(tlv8.StateM4, tlv8.ErrorAuthentication), nil
	}
	if !ed25519.Verify(ed25519.PublicKey(ltpk), newHasher(v.clientPub, []byte(id), v.curvePub), sig) {
		return errorResponse(tlv8.StateM4, tlv8.ErrorAuthentication), nil
	}
	v.ltpk = append([]byte(nil), ltpk...)
	v.step = 2
	return tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM4}},
	), nil
}

// SharedSecret returns the X25519 shared secret established during M1/M3.
func (v *VerifyServer) SharedSecret() []byte { return v.shared }

// ControllerLTPK returns the verified controller LTPK after M3 succeeded.
func (v *VerifyServer) ControllerLTPK() []byte { return v.ltpk }

// ControllerID returns the pairing identifier the controller proved in M3.
func (v *VerifyServer) ControllerID() string { return v.controllerID }
