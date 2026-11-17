package pair

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"math/big"
	"testing"

	"capsulad/apps/airplay/internal/hapcrypt"
	"capsulad/apps/airplay/internal/tlv8"
)

// RFC 5054 group 3072 prime (see srp package). Duplicated here so the client
// side of pair-setup can be driven independently of the server package.
const clientNHex = "FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B" +
	"139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485" +
	"B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7EDEE386BFB5A899FA5AE9F24117C4B1F" +
	"E649286651ECE45B3DC2007CB8A163BF0598DA48361C55D39A69163FA8FD24CF5F83655D23" +
	"DCA3AD961C62F356208552BB9ED529077096966D670C354E4ABC9804F1746C08CA18217C32" +
	"905E462E36CE3BE39E772C180E86039B2783A2EC07A28FB5C55DF06F4C52C9DE2BCBF69558" +
	"17183995497CEA956AE515D2261898FA051015728E5A8AAAC42DAD33170D04507A33A85521" +
	"ABDF1CBA64ECFB850458DBEF0A8AEA71575D060C7DB3970F85A6E1E4C7ABF5AE8CDB0933D7" +
	"1E8C94E04A25619DCEE3D2261AD2EE6BF12FFA06D98A0864D87602733EC86A64521F2B1817" +
	"7B200CBBE117577A615D6C770988C0BAD946E208E24FA074E5AB3143DB5BFCE0FD108E4B82" +
	"D120A93AD2CAFFFFFFFFFFFFFFFF"

func hsum(chunks ...[]byte) []byte {
	h := sha512.New()
	for _, c := range chunks {
		h.Write(c)
	}
	return h.Sum(nil)
}

// srpClient drives the client side of SRP-6a (RFC 5054) with the same byte
// conventions as the server package.
func srpClient(t *testing.T, username, password string, salt, serverB []byte) (A, m1, m2, K []byte) {
	t.Helper()
	const padded = 384
	bigN, ok := new(big.Int).SetString(clientNHex, 16)
	if !ok {
		t.Fatal("bad modulus")
	}
	g := big.NewInt(5)
	pad := func(x *big.Int) []byte {
		b := x.Bytes()
		out := make([]byte, padded)
		copy(out[padded-len(b):], b)
		return out
	}

	aBytes := make([]byte, 64)
	if _, err := rand.Read(aBytes); err != nil {
		t.Fatal(err)
	}
	a := new(big.Int).SetBytes(aBytes)
	A = new(big.Int).Exp(g, a, bigN).Bytes()

	B := new(big.Int).SetBytes(serverB)
	k := new(big.Int).SetBytes(hsum(pad(bigN), pad(g)))
	x := new(big.Int).SetBytes(hsum(salt, hsum([]byte(username), []byte(":"), []byte(password))))
	u := new(big.Int).SetBytes(hsum(pad(new(big.Int).SetBytes(A)), pad(B)))

	gx := new(big.Int).Exp(g, x, bigN)
	kgx := new(big.Int).Mul(k, gx)
	base := new(big.Int).Sub(B, kgx)
	base.Mod(base, bigN)
	exp := new(big.Int).Add(a, new(big.Int).Mul(u, x))
	S := new(big.Int).Exp(base, exp, bigN)

	K = hsum(S.Bytes())

	hN := hsum(bigN.Bytes())
	hg := hsum(g.Bytes())
	hxor := make([]byte, sha512.Size)
	for i := range hxor {
		hxor[i] = hN[i] ^ hg[i]
	}
	m1 = hsum(hxor, hsum([]byte(username)), salt, A, B.Bytes(), K)
	m2 = hsum(A, m1, K)
	return
}

type testClient struct {
	id   string
	priv ed25519.PrivateKey
	pub  []byte
}

func newTestClient(t *testing.T, id string) *testClient {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &testClient{id: id, priv: priv, pub: pub}
}

// pairSetup runs a pair-setup against ss. With transient set the helper stops
// after M4, as a transient controller does; otherwise it performs M5/M6 and the
// controller is recorded.
func pairSetup(t *testing.T, ss *SetupServer, c *testClient, transient bool) (K []byte, accessoryID string, accessoryLTPK []byte) {
	t.Helper()

	// M1 -> M2
	m1 := []tlv8.Item{
		{Tag: tlv8.TagState, Value: []byte{tlv8.StateM1}},
		{Tag: tlv8.TagMethod, Value: []byte{tlv8.MethodPairSetup}},
	}
	if transient {
		m1 = append(m1, tlv8.Item{Tag: tlv8.TagFlags, Value: []byte{tlv8.FlagTransient}})
	}
	req := tlv8.Encode(m1...)
	resp, err := ss.Handle(req)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := tlv8.Decode(resp)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := tlv8.GetUint8(m2, tlv8.TagState); st != tlv8.StateM2 {
		t.Fatalf("expected state 2, got %d", st)
	}
	salt, _ := tlv8.Get(m2, tlv8.TagSalt)
	B, _ := tlv8.Get(m2, tlv8.TagPublicKey)

	// M3 -> M4
	A, proof, expectedM2, K := srpClient(t, "Pair-Setup", DefaultPIN, salt, B)
	req = tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM3}},
		tlv8.Item{Tag: tlv8.TagPublicKey, Value: A},
		tlv8.Item{Tag: tlv8.TagProof, Value: proof},
	)
	resp, err = ss.Handle(req)
	if err != nil {
		t.Fatal(err)
	}
	m4, err := tlv8.Decode(resp)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := tlv8.GetUint8(m4, tlv8.TagState); st != tlv8.StateM4 {
		t.Fatalf("expected state 4, got %d", st)
	}
	gotM2, _ := tlv8.Get(m4, tlv8.TagProof)
	if !bytes.Equal(gotM2, expectedM2) {
		t.Fatal("server M2 proof mismatch")
	}

	if transient {
		if !ss.TransientComplete() {
			t.Fatal("transient pair-setup did not complete at M4")
		}
		if !bytes.Equal(ss.SessionKey(), K) {
			t.Fatal("transient session key does not match the SRP session key")
		}
		if len(storeControllers(ss)) != 0 {
			t.Fatal("transient pair-setup recorded a controller")
		}
		return K, "", nil
	}

	// M5 -> M6
	controllerX := hkdf32(K, "Pair-Setup-Controller-Sign-Salt", "Pair-Setup-Controller-Sign-Info")
	sig := ed25519.Sign(c.priv, newHasher(controllerX, []byte(c.id), c.pub))
	sub := tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagIdentifier, Value: []byte(c.id)},
		tlv8.Item{Tag: tlv8.TagPublicKey, Value: c.pub},
		tlv8.Item{Tag: tlv8.TagSignature, Value: sig},
	)
	encKey := hkdf32(K, "Pair-Setup-Encrypt-Salt", "Pair-Setup-Encrypt-Info")
	enc, err := seal(encKey, "PS-Msg05", sub)
	if err != nil {
		t.Fatal(err)
	}
	req = tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM5}},
		tlv8.Item{Tag: tlv8.TagEncryptedData, Value: enc},
	)
	resp, err = ss.Handle(req)
	if err != nil {
		t.Fatal(err)
	}
	m6, err := tlv8.Decode(resp)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := tlv8.GetUint8(m6, tlv8.TagState); st != tlv8.StateM6 {
		t.Fatalf("expected state 6, got %d", st)
	}
	encOut, _ := tlv8.Get(m6, tlv8.TagEncryptedData)
	plain, err := open(encKey, "PS-Msg06", encOut)
	if err != nil {
		t.Fatalf("decrypting M6: %v", err)
	}
	out, err := tlv8.Decode(plain)
	if err != nil {
		t.Fatal(err)
	}
	accessoryID = tlv8.GetString(out, tlv8.TagIdentifier)
	accessoryLTPK, _ = tlv8.Get(out, tlv8.TagPublicKey)
	sig6, _ := tlv8.Get(out, tlv8.TagSignature)
	accessoryX := hkdf32(K, "Pair-Setup-Accessory-Sign-Salt", "Pair-Setup-Accessory-Sign-Info")
	if !ed25519.Verify(ed25519.PublicKey(accessoryLTPK), newHasher(accessoryX, []byte(accessoryID), accessoryLTPK), sig6) {
		t.Fatal("accessory M6 signature did not verify")
	}
	return K, accessoryID, accessoryLTPK
}

// pairVerify runs pair-verify against vs and returns the client's channel keys
// and shared secret.
func pairVerify(t *testing.T, vs *VerifyServer, c *testClient, accessoryID string) (shared, cw, cr []byte) {
	t.Helper()
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientPub := eph.PublicKey().Bytes()

	// M1 -> M2
	req := tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM1}},
		tlv8.Item{Tag: tlv8.TagPublicKey, Value: clientPub},
	)
	resp, err := vs.Handle(req)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := tlv8.Decode(resp)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := tlv8.GetUint8(m2, tlv8.TagState); st != tlv8.StateM2 {
		t.Fatalf("expected state 2, got %d", st)
	}
	accessoryPub, _ := tlv8.Get(m2, tlv8.TagPublicKey)
	encOut, _ := tlv8.Get(m2, tlv8.TagEncryptedData)

	peer, err := ecdh.X25519().NewPublicKey(accessoryPub)
	if err != nil {
		t.Fatal(err)
	}
	shared, err = eph.ECDH(peer)
	if err != nil {
		t.Fatal(err)
	}
	key := hkdf32(shared, "Pair-Verify-Encrypt-Salt", "Pair-Verify-Encrypt-Info")
	plain, err := open(key, "PV-Msg02", encOut)
	if err != nil {
		t.Fatalf("decrypting PV-Msg02: %v", err)
	}
	sub, err := tlv8.Decode(plain)
	if err != nil {
		t.Fatal(err)
	}
	id := tlv8.GetString(sub, tlv8.TagIdentifier)
	sg, _ := tlv8.Get(sub, tlv8.TagSignature)
	if id != accessoryID {
		t.Fatalf("accessory id %q != %q", id, accessoryID)
	}
	if !ed25519.Verify(ed25519.PublicKey(vs.store.Device.LTPK[:]), newHasher(accessoryPub, []byte(accessoryID), clientPub), sg) {
		t.Fatal("accessory PV-Msg02 signature did not verify")
	}

	// M3 -> M4
	sig := ed25519.Sign(c.priv, newHasher(clientPub, []byte(c.id), accessoryPub))
	sub3 := tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagIdentifier, Value: []byte(c.id)},
		tlv8.Item{Tag: tlv8.TagSignature, Value: sig},
	)
	enc3, err := seal(key, "PV-Msg03", sub3)
	if err != nil {
		t.Fatal(err)
	}
	req = tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM3}},
		tlv8.Item{Tag: tlv8.TagEncryptedData, Value: enc3},
	)
	resp, err = vs.Handle(req)
	if err != nil {
		t.Fatal(err)
	}
	m4, err := tlv8.Decode(resp)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := tlv8.GetUint8(m4, tlv8.TagState); st != tlv8.StateM4 {
		t.Fatalf("expected state 4, got %d", st)
	}
	if _, ok := tlv8.Get(m4, tlv8.TagError); ok {
		t.Fatal("unexpected error in M4")
	}
	keys := hapcrypt.ControlChannelKeys(shared)
	cw, cr = keys.Inbound, keys.Outbound
	return shared, cw, cr
}

func TestFullPairSetupAndVerify(t *testing.T) {
	store, err := NewStore("accessory-1")
	if err != nil {
		t.Fatal(err)
	}
	c := newTestClient(t, "controller-1")

	ss := NewSetupServer(store)
	pairSetup(t, ss, c, false)
	ltpk, ok := store.ControllerLTPK(c.id)
	if !ok || !bytes.Equal(ltpk, c.pub) {
		t.Fatal("controller LTPK not stored")
	}

	vs := NewVerifyServer(store)
	shared, cw, cr := pairVerify(t, vs, c, store.Device.PairingID)
	if !bytes.Equal(vs.SharedSecret(), shared) {
		t.Fatal("shared secret mismatch")
	}
	if !bytes.Equal(vs.ControllerLTPK(), c.pub) {
		t.Fatal("controller LTPK mismatch after verify")
	}
	control := hapcrypt.ControlChannelKeys(shared)
	if !bytes.Equal(cw, control.Inbound) || !bytes.Equal(cr, control.Outbound) {
		t.Fatal("channel keys differ between client and accessory")
	}
	events := hapcrypt.EventChannelKeys(shared)
	if len(events.Outbound) != 32 || len(events.Inbound) != 32 {
		t.Fatal("event keys bad length")
	}
}

func TestWrongProofYieldsStateAndError(t *testing.T) {
	store, _ := NewStore("accessory-1")
	ss := NewSetupServer(store)

	req := tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM1}},
		tlv8.Item{Tag: tlv8.TagMethod, Value: []byte{tlv8.MethodPairSetup}},
		tlv8.Item{Tag: tlv8.TagFlags, Value: []byte{tlv8.FlagTransient}},
	)
	resp, err := ss.Handle(req)
	if err != nil {
		t.Fatal(err)
	}
	m2, _ := tlv8.Decode(resp)
	salt, _ := tlv8.Get(m2, tlv8.TagSalt)
	B, _ := tlv8.Get(m2, tlv8.TagPublicKey)
	A, proof, _, _ := srpClient(t, "Pair-Setup", DefaultPIN, salt, B)
	proof[0] ^= 0xff // corrupt the client proof

	req = tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM3}},
		tlv8.Item{Tag: tlv8.TagPublicKey, Value: A},
		tlv8.Item{Tag: tlv8.TagProof, Value: proof},
	)
	resp, err = ss.Handle(req)
	if err != nil {
		t.Fatal(err)
	}
	items, _ := tlv8.Decode(resp)
	if _, ok := tlv8.Get(items, tlv8.TagState); !ok {
		t.Fatal("response missing State")
	}
	if code, ok := tlv8.GetUint8(items, tlv8.TagError); !ok || code != tlv8.ErrorAuthentication {
		t.Fatalf("expected authentication error, got %v,%v", code, ok)
	}
}

func TestVerifyUnknownControllerRefused(t *testing.T) {
	store, _ := NewStore("accessory-2")
	// A controller id that was never paired.
	c := newTestClient(t, "unknown-controller")

	vs := NewVerifyServer(store)
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientPub := eph.PublicKey().Bytes()

	req := tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM1}},
		tlv8.Item{Tag: tlv8.TagPublicKey, Value: clientPub},
	)
	resp, err := vs.Handle(req)
	if err != nil {
		t.Fatal(err)
	}
	m2, _ := tlv8.Decode(resp)
	accessoryPub, _ := tlv8.Get(m2, tlv8.TagPublicKey)
	encOut, _ := tlv8.Get(m2, tlv8.TagEncryptedData)
	peer, _ := ecdh.X25519().NewPublicKey(accessoryPub)
	shared, _ := eph.ECDH(peer)
	key := hkdf32(shared, "Pair-Verify-Encrypt-Salt", "Pair-Verify-Encrypt-Info")
	_, _ = open(key, "PV-Msg02", encOut)

	sig := ed25519.Sign(c.priv, newHasher(clientPub, []byte(c.id), accessoryPub))
	sub3 := tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagIdentifier, Value: []byte(c.id)},
		tlv8.Item{Tag: tlv8.TagSignature, Value: sig},
	)
	enc3, _ := seal(key, "PV-Msg03", sub3)
	req = tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM3}},
		tlv8.Item{Tag: tlv8.TagEncryptedData, Value: enc3},
	)
	resp, err = vs.Handle(req)
	if err != nil {
		t.Fatal(err)
	}
	items, _ := tlv8.Decode(resp)
	if _, ok := tlv8.Get(items, tlv8.TagState); !ok {
		t.Fatal("response missing State")
	}
	if code, ok := tlv8.GetUint8(items, tlv8.TagError); !ok || code != tlv8.ErrorAuthentication {
		t.Fatalf("expected authentication error, got %v,%v", code, ok)
	}
	if vs.ControllerLTPK() != nil {
		t.Fatal("controller LTPK should not be set for an unknown controller")
	}
}

func TestStoreMarshalRoundTrip(t *testing.T) {
	store, _ := NewStore("accessory-3")
	store.SetControllerLTPK("c1", bytes.Repeat([]byte{0x7a}, 32))
	data, err := store.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadStore(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Device.PairingID != store.Device.PairingID || got.Device.LTSK != store.Device.LTSK || got.Device.LTPK != store.Device.LTPK {
		t.Fatal("device identity not preserved")
	}
	if ltpk, ok := got.ControllerLTPK("c1"); !ok || !bytes.Equal(ltpk, bytes.Repeat([]byte{0x7a}, 32)) {
		t.Fatal("controller not preserved")
	}
}

// storeControllers exposes the server's store for assertions.
func storeControllers(ss *SetupServer) []string {
	return ss.store.ControllerIDs()
}

func TestTransientPairSetupCompletesAtM4(t *testing.T) {
	store, err := NewStore("accessory-1")
	if err != nil {
		t.Fatal(err)
	}
	c := newTestClient(t, "controller-transient")
	ss := NewSetupServer(store)
	K, _, _ := pairSetup(t, ss, c, true)
	if len(K) != sha512.Size {
		t.Fatalf("session key length = %d", len(K))
	}
	// Some senders follow the HAP state machine past M4. An M5 after a
	// transient setup must be answered (M6) without persisting a pairing: the
	// M5 payload here is junk, so an authentication error is also acceptable,
	// but the controller must never be stored.
	resp, err := ss.Handle(tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM5}},
		tlv8.Item{Tag: tlv8.TagEncryptedData, Value: []byte{0x00}},
	))
	if err != nil {
		t.Fatal(err)
	}
	items, err := tlv8.Decode(resp)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tlv8.Get(items, tlv8.TagError); !ok {
		if state, _ := tlv8.GetUint8(items, tlv8.TagState); state != tlv8.StateM6 {
			t.Fatalf("M5 after a transient setup answered state %d without an error", state)
		}
	}
	if len(storeControllers(ss)) != 0 {
		t.Fatal("a transient setup persisted a controller through M5")
	}
}
