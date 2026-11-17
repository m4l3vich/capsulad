package session

// Independent interop proof against pyatv, the third-party Python AirPlay
// client. It runs pyatv's own SRP implementation and HKDF derivation against
// this receiver's pair-setup server, so A1's key source and A2's channel
// direction are checked against an implementation we did not write.
//
// The test is gated: it runs only when PYATV_PYTHON points at a Python
// interpreter that can `import pyatv` (for example a venv with pyatv
// installed). Without it the test skips rather than faking a result.

import (
	"bytes"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"capsulad/apps/airplay/internal/hapcrypt"
	"capsulad/apps/airplay/internal/pair"
	"capsulad/apps/airplay/internal/tlv8"
)

const pyatvSRPClient = `
import sys, json, binascii
from pyatv.auth.hap_srp import SRPAuthHandler, hkdf_expand

salt = binascii.unhexlify(sys.argv[1])
server_public = binascii.unhexlify(sys.argv[2])
pin = int(sys.argv[3])

handler = SRPAuthHandler()
handler.initialize()
handler.step1(pin)
public, proof = handler.step2(server_public, salt)
shared = binascii.unhexlify(handler.shared_key)

print(json.dumps({
    "A": public.hex(),
    "proof": proof.hex(),
    "K": shared.hex(),
    # pyatv's accessory-side names in the reference: control outbound is
    # Control-Read, inbound is Control-Write.
    "control_out": hkdf_expand("Control-Salt", "Control-Read-Encryption-Key", shared).hex(),
    "control_in": hkdf_expand("Control-Salt", "Control-Write-Encryption-Key", shared).hex(),
}))
`

func TestPyatvTransientPairingKeys(t *testing.T) {
	python := os.Getenv("PYATV_PYTHON")
	if python == "" {
		t.Skip("set PYATV_PYTHON to a python with pyatv installed to run the independent interop proof")
	}
	store, err := pair.NewStore("pyatv-interop")
	if err != nil {
		t.Fatal(err)
	}
	server := pair.NewSetupServer(store)
	server.SetPIN(pair.DefaultPIN)

	// M1 with the transient flag, as pyatv's hap_transient sends it.
	m2, err := server.Handle(tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM1}},
		tlv8.Item{Tag: tlv8.TagMethod, Value: []byte{tlv8.MethodPairSetup}},
		tlv8.Item{Tag: tlv8.TagFlags, Value: []byte{tlv8.FlagTransient}},
	))
	if err != nil {
		t.Fatal(err)
	}
	items, err := tlv8.Decode(m2)
	if err != nil {
		t.Fatal(err)
	}
	salt, _ := tlv8.Get(items, tlv8.TagSalt)
	publicB, _ := tlv8.Get(items, tlv8.TagPublicKey)
	if len(salt) == 0 || len(publicB) == 0 {
		t.Fatalf("M2 lacks salt or public key")
	}

	script := t.TempDir() + "/client.py"
	if err := os.WriteFile(script, []byte(pyatvSRPClient), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(python, script, hex.EncodeToString(salt), hex.EncodeToString(publicB), pair.DefaultPIN)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("pyatv client: %v: %s", err, stderr.String())
	}
	var result struct {
		A, Proof, K, ControlOut, ControlIn string `json:"-"`
	}
	var raw map[string]string
	if err := json.Unmarshal(output, &raw); err != nil {
		t.Fatalf("pyatv output %q: %v", output, err)
	}
	result.A, result.Proof, result.K = raw["A"], raw["proof"], raw["K"]
	result.ControlOut, result.ControlIn = raw["control_out"], raw["control_in"]
	publicA, err := hex.DecodeString(result.A)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := hex.DecodeString(result.Proof)
	if err != nil {
		t.Fatal(err)
	}
	pyatvKey, err := hex.DecodeString(result.K)
	if err != nil {
		t.Fatal(err)
	}

	// M3 with pyatv's public key and proof: the receiver must accept it.
	m4, err := server.Handle(tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM3}},
		tlv8.Item{Tag: tlv8.TagPublicKey, Value: publicA},
		tlv8.Item{Tag: tlv8.TagProof, Value: proof},
	))
	if err != nil {
		t.Fatal(err)
	}
	items, err = tlv8.Decode(m4)
	if err != nil {
		t.Fatal(err)
	}
	if code, ok := tlv8.GetUint8(items, tlv8.TagError); ok {
		t.Fatalf("the receiver refused pyatv's SRP proof with TLV error %d — A1/SRP byte conventions disagree with pyatv", code)
	}
	if state, _ := tlv8.GetUint8(items, tlv8.TagState); state != tlv8.StateM4 {
		t.Fatalf("M4 state = %d", state)
	}
	if !server.TransientComplete() {
		t.Fatal("a transient pair-setup did not complete at M4")
	}

	// A1: the SRP session key must be the same 64-byte SHA-512(S) pyatv holds.
	if got := server.SessionKey(); !bytes.Equal(got, pyatvKey) {
		t.Fatalf("session keys differ: receiver %x, pyatv %x", hashPrefix(got), hashPrefix(pyatvKey))
	}
	if len(pyatvKey) != sha512.Size {
		t.Fatalf("pyatv session key is %d bytes, want %d", len(pyatvKey), sha512.Size)
	}

	// A2: the accessory's outbound/inbound control keys must be the mirror of
	// pyatv's own derivation for that channel.
	control := hapcrypt.ControlChannelKeys(pyatvKey)
	wantOut, _ := hex.DecodeString(result.ControlOut)
	wantIn, _ := hex.DecodeString(result.ControlIn)
	if !bytes.Equal(control.Outbound, wantOut) || !bytes.Equal(control.Inbound, wantIn) {
		t.Fatalf("control channel keys disagree with pyatv:\n receiver out %x in %x\n pyatv    out %x in %x",
			control.Outbound, control.Inbound, wantOut, wantIn)
	}
	if _, err := base64.StdEncoding.DecodeString(result.A); err != nil {
		_ = err // A is hex, decoded above; keeps base64 out of the assertions
	}
}

func hashPrefix(key []byte) []byte {
	if len(key) > 8 {
		return key[:8]
	}
	return key
}
