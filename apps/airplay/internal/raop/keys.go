package raop

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
)

// KeyPair is the receiver's RSA identity for the legacy RAOP encrypted audio
// path.
type KeyPair struct {
	// PrivateKey is the PKCS#1 DER encoding of the private key.
	PrivateKey []byte
	// PublicTXT is the base64 of the PKCS#1 DER public key: the value an
	// AirPlay 1 receiver publishes in the `_raop._tcp` "pk" TXT record. This
	// receiver cannot advertise it there: the value is roughly 360 bytes and
	// Avahi rejects a single TXT item over 255 bytes, so `_raop._tcp` carries
	// the HomeKit ed25519 key instead (session/advertise.go). The RSA key
	// remains for an ANNOUNCE that carries rsaaeskey.
	PublicTXT string

	// priv caches the parsed key. It is always nil for a KeyPair loaded from
	// either constructor, so DecryptAESKey parses lazily and safely for
	// concurrent callers.
	priv *rsa.PrivateKey
}

// GenerateKeyPair creates a 2048-bit RSA key, the size used by AirPlay 1
// senders, and returns its PKCS#1 private key DER and the matching base64
// public key.
func GenerateKeyPair() (*KeyPair, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("raop: generate RSA key: %w", err)
	}
	return newKeyPair(priv)
}

// LoadKeyPair wraps an existing PKCS#1 DER private key.
func LoadKeyPair(pkcs1DER []byte) (*KeyPair, error) {
	if len(pkcs1DER) == 0 {
		return nil, errors.New("raop: empty PKCS#1 private key")
	}
	priv, err := x509.ParsePKCS1PrivateKey(pkcs1DER)
	if err != nil {
		return nil, fmt.Errorf("raop: parse PKCS#1 private key: %w", err)
	}
	if err := priv.Validate(); err != nil {
		return nil, fmt.Errorf("raop: invalid RSA private key: %w", err)
	}
	return newKeyPair(priv)
}

func newKeyPair(priv *rsa.PrivateKey) (*KeyPair, error) {
	pubDER := x509.MarshalPKCS1PublicKey(&priv.PublicKey)
	if len(pubDER) == 0 {
		return nil, errors.New("raop: marshalling RSA public key failed")
	}
	return &KeyPair{
		PrivateKey: x509.MarshalPKCS1PrivateKey(priv),
		PublicTXT:  base64.StdEncoding.EncodeToString(pubDER),
		priv:       priv,
	}, nil
}

// privateKey returns the parsed private key, parsing PrivateKey if the
// KeyPair was built by hand rather than by GenerateKeyPair/LoadKeyPair.
func (k *KeyPair) privateKey() (*rsa.PrivateKey, error) {
	if k == nil {
		return nil, errors.New("raop: nil KeyPair")
	}
	if k.priv != nil {
		return k.priv, nil
	}
	if len(k.PrivateKey) == 0 {
		return nil, errors.New("raop: KeyPair has no private key")
	}
	priv, err := x509.ParsePKCS1PrivateKey(k.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("raop: parse PKCS#1 private key: %w", err)
	}
	if err := priv.Validate(); err != nil {
		return nil, fmt.Errorf("raop: invalid RSA private key: %w", err)
	}
	return priv, nil
}

// DecryptAESKey decrypts the a=rsaaeskey value of an ANNOUNCE and returns the
// 16-byte AES-128 key.
//
// Padding: shairport-sync's rtsp.c handle_announce() calls
// rsa_apply(..., RSA_MODE_KEY); common.c rsa_apply() sets
// EVP_PKEY_CTX_set_rsa_padding(ctx, RSA_PKCS1_OAEP_PADDING). OpenSSL's
// RSA_PKCS1_OAEP_PADDING defaults to SHA-1 for both the OAEP digest and MGF1,
// so crypto/rsa's DecryptOAEP(sha1.New(), ...) is the matching primitive. The
// sender encrypts with the same mode using the receiver's RSA public key.
func (k *KeyPair) DecryptAESKey(blob []byte) ([]byte, error) {
	priv, err := k.privateKey()
	if err != nil {
		return nil, err
	}
	if len(blob) != priv.Size() {
		return nil, fmt.Errorf("raop: RSA ciphertext is %d bytes, want %d", len(blob), priv.Size())
	}
	key, err := rsa.DecryptOAEP(sha1.New(), rand.Reader, priv, blob, nil)
	if err != nil {
		return nil, fmt.Errorf("raop: decrypt AES key: %w", err)
	}
	if len(key) != 16 {
		return nil, fmt.Errorf("raop: decrypted AES key is %d bytes, want 16", len(key))
	}
	return key, nil
}

// SetupResponseBody returns the binary property list a legacy receiver sends
// in the body of its SETUP response.
//
// shairport-sync carries the receiver public key under the plist key "pk"
// (rtsp.c generateInfoPlist(): plist_dict_set_item(response_plist, "pk",
// plist_new_data(config.airplay_pk, ...))). The classic SETUP handler in
// 5.6.2 actually sends no body, so "pk" is the key the reference uses
// whenever it publishes the receiver key in a plist; the value is the raw key
// bytes, not base64. We emit the PKCS#1 DER public key, matching PublicTXT.
//
// The returned body is in bplist00 format, which is what
// application/x-apple-binary-plist (plist_to_bin() in rtsp.c) denotes. It
// returns nil if the KeyPair has no usable private key.
func (k *KeyPair) SetupResponseBody() []byte {
	priv, err := k.privateKey()
	if err != nil {
		return nil
	}
	return binaryPlistDataDict("pk", x509.MarshalPKCS1PublicKey(&priv.PublicKey))
}
