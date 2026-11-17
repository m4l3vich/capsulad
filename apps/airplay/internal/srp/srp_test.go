package srp

import (
	"bytes"
	"crypto/rand"
	"crypto/sha512"
	"math/big"
	"testing"
)

// client computes the RFC 5054 client side with the same byte conventions the
// server uses, so the two implementations can be validated against each other.
func client(t *testing.T, username, password string, salt, serverB []byte) (A, m1, m2, K []byte) {
	t.Helper()
	aBytes := make([]byte, 64)
	if _, err := rand.Read(aBytes); err != nil {
		t.Fatal(err)
	}
	a := new(big.Int).SetBytes(aBytes)
	A = new(big.Int).Exp(bigG, a, bigN).Bytes()

	B := new(big.Int).SetBytes(serverB)
	k := hashPaddedPair(bigN, bigG)
	x := computeX(salt, username, password)
	u := hashPaddedPair(new(big.Int).SetBytes(A), B)

	// S = (B - k*g^x)^(a + u*x) mod N
	gx := new(big.Int).Exp(bigG, x, bigN)
	kgx := new(big.Int).Mul(k, gx)
	base := new(big.Int).Sub(B, kgx)
	base.Mod(base, bigN)
	exp := new(big.Int).Add(a, new(big.Int).Mul(u, x))
	S := new(big.Int).Exp(base, exp, bigN)

	K = sha512Concat(S.Bytes())

	hN := sha512Concat(bigN.Bytes())
	hg := sha512Concat(bigG.Bytes())
	hxor := make([]byte, sha512.Size)
	for i := range hxor {
		hxor[i] = hN[i] ^ hg[i]
	}
	hI := sha512Concat([]byte(username))
	m1 = sha512Concat(hxor, hI, salt, A, B.Bytes(), K)
	m2 = sha512Concat(A, m1, K)
	return
}

func TestServerAcceptsCorrectProof(t *testing.T) {
	s, err := NewServer("Pair-Setup", "")
	if err != nil {
		t.Fatal(err)
	}
	A, m1, m2, K := client(t, "Pair-Setup", "", s.Salt(), s.PublicKey())
	if err := s.SetClientPublicKey(A); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyClientProof(m1); err != nil {
		t.Fatalf("correct proof rejected: %v", err)
	}
	if !bytes.Equal(s.Proof(), m2) {
		t.Fatalf("M2 mismatch:\n got %x\nwant %x", s.Proof(), m2)
	}
	if !bytes.Equal(s.SessionKey(), K) {
		t.Fatal("session key mismatch between client and server")
	}
	if len(s.SessionKey()) != 64 {
		t.Fatalf("session key length = %d", len(s.SessionKey()))
	}
}

func TestServerRejectsWrongProof(t *testing.T) {
	s, err := NewServer("Pair-Setup", "")
	if err != nil {
		t.Fatal(err)
	}
	A, m1, _, _ := client(t, "Pair-Setup", "", s.Salt(), s.PublicKey())
	if err := s.SetClientPublicKey(A); err != nil {
		t.Fatal(err)
	}
	wrong := append([]byte(nil), m1...)
	wrong[0] ^= 0xff
	if err := s.VerifyClientProof(wrong); err != ErrAuthentication {
		t.Fatalf("expected ErrAuthentication, got %v", err)
	}
}

func TestSetClientPublicKeyValidation(t *testing.T) {
	s, err := NewServer("Pair-Setup", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetClientPublicKey([]byte{0}); err == nil {
		t.Fatal("expected rejection of zero A")
	}
	if err := s.SetClientPublicKey(bigN.Bytes()); err == nil {
		t.Fatal("expected rejection of A = 0 mod N")
	}
}

func TestPublicKeyMinimal(t *testing.T) {
	s, err := NewServer("Pair-Setup", "")
	if err != nil {
		t.Fatal(err)
	}
	B := s.PublicKey()
	if len(B) == 0 || B[0] == 0 {
		t.Fatalf("public key is not minimal-length: %x", B[:min(4, len(B))])
	}
	if len(s.Salt()) != 16 {
		t.Fatalf("salt length = %d", len(s.Salt()))
	}
}
