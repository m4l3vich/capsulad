// Package srp implements the SRP-6a server side used by AirPlay 2 / HAP
// pair-setup, with RFC 5054 group 3072 (SHA-512, g=5).
//
// The byte-encoding conventions (H(N), k = H(N,g) padded, u = H(A,B) padded,
// minimal big-endian A/B/salt inside M1) mirror shairport-sync
// pair_ap/pair.c:194-248 (H_nn_pad/H_ns/update_hash_n/hash_num) so the proofs
// interoperate with real Apple controllers.
package srp

import (
	"crypto/rand"
	"crypto/sha512"
	"crypto/subtle"
	"errors"
	"math/big"
)

// Modulus returns the RFC 5054 group's prime, N.
func Modulus() *big.Int { return new(big.Int).Set(bigN) }

// Generator returns the group's generator, g.
func Generator() *big.Int { return new(big.Int).Set(bigG) }

// ErrAuthentication is returned when a client proof does not verify.
var ErrAuthentication = errors.New("srp: authentication failed")

// RFC 5054 / RFC 3526 group 28 (3072-bit MODP) prime, from
// shairport-sync pair_ap/pair_homekit.c:213-229.
const nHex = "FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B" +
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

var (
	bigN   *big.Int
	bigG   = big.NewInt(5)
	padded = 384 // modulus length in bytes
)

func init() {
	N, ok := new(big.Int).SetString(nHex, 16)
	if !ok {
		panic("srp: bad modulus")
	}
	bigN = N
	if len(N.Bytes()) != padded {
		panic("srp: unexpected modulus length")
	}
}

func sha512Concat(chunks ...[]byte) []byte {
	h := sha512.New()
	for _, c := range chunks {
		h.Write(c)
	}
	return h.Sum(nil)
}

// pad returns x as exactly padded big-endian bytes.
func pad(x *big.Int) []byte {
	b := x.Bytes()
	if len(b) >= padded {
		return b[len(b)-padded:]
	}
	out := make([]byte, padded)
	copy(out[padded-len(b):], b)
	return out
}

func hashInt(x *big.Int) []byte { return sha512Concat(x.Bytes()) }

// hashPaddedPair computes H(pad(a) || pad(b)) (k and u).
func hashPaddedPair(a, b *big.Int) *big.Int {
	h := sha512Concat(pad(a), pad(b))
	return new(big.Int).SetBytes(h)
}

// computeX computes SHA-512(salt || SHA-512(username ":" password)).
func computeX(salt []byte, username, password string) *big.Int {
	h := sha512Concat([]byte(username), []byte(":"), []byte(password))
	x := sha512Concat(salt, h)
	return new(big.Int).SetBytes(x)
}

// Server is a single pair-setup SRP exchange.
type Server struct {
	username string
	password string
	salt     []byte
	b        *big.Int
	v        *big.Int
	B        *big.Int
	A        *big.Int
	S        *big.Int
	k        *big.Int
	key      []byte
	m1       []byte
	m2       []byte
}

// NewServer creates a server exchange for username/password with a fresh salt
// and ephemeral b.
func NewServer(username, password string) (*Server, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	bb := make([]byte, 64)
	if _, err := rand.Read(bb); err != nil {
		return nil, err
	}
	s := &Server{
		username: username,
		password: password,
		salt:     salt,
		b:        new(big.Int).SetBytes(bb),
	}
	s.v = new(big.Int).Exp(bigG, computeX(salt, username, password), bigN)
	s.k = hashPaddedPair(bigN, bigG)
	// B = k*v + g^b mod N
	gb := new(big.Int).Exp(bigG, s.b, bigN)
	kv := new(big.Int).Mul(s.k, s.v)
	s.B = kv.Add(kv, gb)
	s.B.Mod(s.B, bigN)
	return s, nil
}

// Salt returns the 16-byte salt.
func (s *Server) Salt() []byte { return s.salt }

// PublicKey returns B as minimal-length big-endian bytes.
func (s *Server) PublicKey() []byte { return s.B.Bytes() }

// SetClientPublicKey validates and stores the client public key A.
func (s *Server) SetClientPublicKey(a []byte) error {
	A := new(big.Int).SetBytes(a)
	if A.Sign() == 0 {
		return errors.New("srp: client public key is zero")
	}
	if new(big.Int).Mod(A, bigN).Sign() == 0 {
		return errors.New("srp: client public key is a multiple of N")
	}
	s.A = A
	return nil
}

// session computes the shared secret S and the session key K plus proofs.
func (s *Server) session() {
	u := hashPaddedPair(s.A, s.B)
	// S = (A * v^u)^b mod N
	vu := new(big.Int).Exp(s.v, u, bigN)
	base := new(big.Int).Mul(s.A, vu)
	base.Mod(base, bigN)
	S := new(big.Int).Exp(base, s.b, bigN)
	s.S = S
	s.key = sha512Concat(S.Bytes()) // K = SHA-512(minimal big-endian S)

	Hxor := make([]byte, sha512.Size)
	hn := hashInt(bigN)
	hg := hashInt(bigG)
	for i := range Hxor {
		Hxor[i] = hn[i] ^ hg[i]
	}
	hI := sha512Concat([]byte(s.username))
	s.m1 = sha512Concat(Hxor, hI, s.salt, s.A.Bytes(), s.B.Bytes(), s.key)
	s.m2 = sha512Concat(s.A.Bytes(), s.m1, s.key)
}

// VerifyClientProof checks the client proof M1 in constant time.
func (s *Server) VerifyClientProof(m1 []byte) error {
	if s.A == nil {
		return errors.New("srp: client public key not set")
	}
	s.session()
	if len(m1) != len(s.m1) || subtle.ConstantTimeCompare(m1, s.m1) != 1 {
		return ErrAuthentication
	}
	return nil
}

// Proof returns M2 as minimal-length big-endian bytes.
func (s *Server) Proof() []byte { return s.m2 }

// SessionKey returns K = SHA-512(S), 64 bytes.
func (s *Server) SessionKey() []byte { return s.key }
