package raop

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"testing"
)

// The OpenSSL fixtures in testdata were produced with OpenSSL 4.1.0 from a
// freshly generated 2048-bit key. These are the exact commands:
//
//	openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out fixture.pem
//	openssl rsa -in fixture.pem -traditional -outform DER -out testdata/openssl_rsa_priv.der
//	openssl rsa -in fixture.pem -RSAPublicKey_out -outform DER -out testdata/openssl_rsa_pub.der
//	printf '0123456789abcdef' > aes.bin
//	openssl pkeyutl -encrypt -in aes.bin -out testdata/openssl_oaep_sha1_aes.enc \
//	    -inkey fixture.pem -pkeyopt rsa_padding_mode:oaep \
//	    -pkeyopt rsa_oaep_md:sha1 -pkeyopt rsa_mgf1_md:sha1
//	openssl pkeyutl -decrypt -in testdata/openssl_oaep_sha1_aes.enc -inkey fixture.pem \
//	    -pkeyopt rsa_padding_mode:oaep \
//	    -pkeyopt rsa_oaep_md:sha1 -pkeyopt rsa_mgf1_md:sha1
//	# prints: 0123456789abcdef
//
// The explicit sha1 OAEP parameters name the OpenSSL defaults that
// shairport-sync relies on in common.c rsa_apply(), RSA_MODE_KEY.

func TestGenerateKeyPairRoundTrip(t *testing.T) {
	kp, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}

	priv, err := x509.ParsePKCS1PrivateKey(kp.PrivateKey)
	if err != nil {
		t.Fatalf("PrivateKey is not PKCS#1 DER: %v", err)
	}
	if priv.N.BitLen() != 2048 {
		t.Errorf("key size = %d bits, want 2048", priv.N.BitLen())
	}

	pubDER, err := base64.StdEncoding.DecodeString(kp.PublicTXT)
	if err != nil {
		t.Fatalf("PublicTXT is not base64: %v", err)
	}
	pub, err := x509.ParsePKCS1PublicKey(pubDER)
	if err != nil {
		t.Fatalf("PublicTXT is not a PKCS#1 public key: %v", err)
	}
	if pub.N.Cmp(priv.N) != 0 || pub.E != priv.E {
		t.Error("PublicTXT does not match the private key")
	}

	// Encrypt with the published key using the reference padding mode, and
	// recover the AES key through the frozen API.
	want := []byte("0123456789abcdef")
	blob, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, pub, want, nil)
	if err != nil {
		t.Fatalf("EncryptOAEP: %v", err)
	}
	got, err := kp.DecryptAESKey(blob)
	if err != nil {
		t.Fatalf("DecryptAESKey: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("DecryptAESKey = %q, want %q", got, want)
	}
}

func TestOpenSSLFixture(t *testing.T) {
	privDER := readTestFile(t, "openssl_rsa_priv.der")
	pubDER := readTestFile(t, "openssl_rsa_pub.der")
	ciphertext := readTestFile(t, "openssl_oaep_sha1_aes.enc")

	kp, err := LoadKeyPair(privDER)
	if err != nil {
		t.Fatalf("LoadKeyPair: %v", err)
	}
	if kp.PublicTXT != base64.StdEncoding.EncodeToString(pubDER) {
		t.Error("PublicTXT does not match the independently generated OpenSSL public key")
	}

	got, err := kp.DecryptAESKey(ciphertext)
	if err != nil {
		t.Fatalf("DecryptAESKey: %v", err)
	}
	if want := []byte("0123456789abcdef"); !bytes.Equal(got, want) {
		t.Errorf("DecryptAESKey = %q, want %q", got, want)
	}
}

// TestDecryptAESKeyUsesSHA1OAEP proves the fixture is SHA-1 OAEP: the same
// ciphertext must not decrypt with SHA-256 OAEP.
func TestDecryptAESKeyUsesSHA1OAEP(t *testing.T) {
	priv, err := x509.ParsePKCS1PrivateKey(readTestFile(t, "openssl_rsa_priv.der"))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	ciphertext := readTestFile(t, "openssl_oaep_sha1_aes.enc")
	if _, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, priv, ciphertext, nil); err == nil {
		t.Fatal("SHA-256 OAEP decrypted a SHA-1 OAEP ciphertext")
	}
}

func TestDecryptAESKeyErrors(t *testing.T) {
	kp, err := LoadKeyPair(readTestFile(t, "openssl_rsa_priv.der"))
	if err != nil {
		t.Fatalf("LoadKeyPair: %v", err)
	}
	ciphertext := readTestFile(t, "openssl_oaep_sha1_aes.enc")

	if _, err := kp.DecryptAESKey(nil); err == nil {
		t.Error("DecryptAESKey(nil) = nil error, want error")
	}
	if _, err := kp.DecryptAESKey(ciphertext[:len(ciphertext)-1]); err == nil {
		t.Error("short ciphertext accepted")
	}
	if _, err := kp.DecryptAESKey(append(append([]byte(nil), ciphertext...), 0)); err == nil {
		t.Error("long ciphertext accepted")
	}
	corrupt := append([]byte(nil), ciphertext...)
	corrupt[0] ^= 0xff
	if _, err := kp.DecryptAESKey(corrupt); err == nil {
		t.Error("corrupted ciphertext accepted")
	}

	var nilKP *KeyPair
	if _, err := nilKP.DecryptAESKey(ciphertext); err == nil {
		t.Error("nil KeyPair accepted")
	}
	if _, err := (&KeyPair{}).DecryptAESKey(ciphertext); err == nil {
		t.Error("KeyPair without a key accepted")
	}
	if _, err := LoadKeyPair(nil); err == nil {
		t.Error("LoadKeyPair(nil) = nil error, want error")
	}
	if _, err := LoadKeyPair([]byte("not a key")); err == nil {
		t.Error("LoadKeyPair(garbage) = nil error, want error")
	}
}

// TestAnnounceDecryptRoundTrip exercises the full legacy path: the sender
// encrypts its AES key with the receiver key from PublicTXT, the ANNOUNCE SDP
// carries it in a=rsaaeskey, and ParseSDP plus DecryptAESKey recover it.
func TestAnnounceDecryptRoundTrip(t *testing.T) {
	kp, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	pubDER, err := base64.StdEncoding.DecodeString(kp.PublicTXT)
	if err != nil {
		t.Fatalf("PublicTXT: %v", err)
	}
	pub, err := x509.ParsePKCS1PublicKey(pubDER)
	if err != nil {
		t.Fatalf("parse public key: %v", err)
	}

	wantKey := []byte("abcdefghijklmnop")
	wantIV := bytes.Repeat([]byte{0x42}, 16)
	blob, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, pub, wantKey, nil)
	if err != nil {
		t.Fatalf("EncryptOAEP: %v", err)
	}

	body := fmt.Sprintf("v=0\r\nm=audio 0 RTP/AVP 96\r\n"+
		"a=rtpmap:96 AppleLossless\r\n"+
		"a=fmtp:96 352 0 16 40 10 14 2 255 0 0 44100\r\n"+
		"a=rsaaeskey:%s\r\na=aesiv:%s\r\n",
		base64.StdEncoding.EncodeToString(blob), base64.StdEncoding.EncodeToString(wantIV))

	st, err := ParseSDP([]byte(body))
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}
	if !bytes.Equal(st.AESIV, wantIV) {
		t.Errorf("AESIV = %x, want %x", st.AESIV, wantIV)
	}
	gotKey, err := kp.DecryptAESKey(st.RSAAESKey)
	if err != nil {
		t.Fatalf("DecryptAESKey: %v", err)
	}
	if !bytes.Equal(gotKey, wantKey) {
		t.Errorf("AES key = %q, want %q", gotKey, wantKey)
	}
}

func TestSetupResponseBody(t *testing.T) {
	kp, err := LoadKeyPair(readTestFile(t, "openssl_rsa_priv.der"))
	if err != nil {
		t.Fatalf("LoadKeyPair: %v", err)
	}
	pubDER := readTestFile(t, "openssl_rsa_pub.der")
	body := kp.SetupResponseBody()

	key, value := decodeBplistDataDict(t, body)
	if key != "pk" {
		t.Errorf("plist key = %q, want pk", key)
	}
	if !bytes.Equal(value, pubDER) {
		t.Errorf("plist public key is %d bytes, want the %d-byte PKCS#1 DER", len(value), len(pubDER))
	}

	var nilKP *KeyPair
	if body := nilKP.SetupResponseBody(); body != nil {
		t.Errorf("nil KeyPair body = %x, want nil", body)
	}
}

// decodeBplistDataDict decodes the single-entry binary plist produced by
// binaryPlistDataDict. It is deliberately an independent reader of the format
// (it does not share the encoder's helpers) so it can catch layout mistakes.
func decodeBplistDataDict(t *testing.T, body []byte) (string, []byte) {
	t.Helper()
	if len(body) < 8+32 || string(body[:8]) != "bplist00" {
		t.Fatalf("body does not start with bplist00 (len %d)", len(body))
	}
	trailer := body[len(body)-32:]
	offsetIntSize := int(trailer[6])
	objRefSize := int(trailer[7])
	numObjects := binary.BigEndian.Uint64(trailer[8:16])
	topObject := binary.BigEndian.Uint64(trailer[16:24])
	offsetTableOffset := int(binary.BigEndian.Uint64(trailer[24:32]))

	if numObjects != 3 {
		t.Fatalf("numObjects = %d, want 3", numObjects)
	}
	if topObject != 0 {
		t.Fatalf("topObject = %d, want 0", topObject)
	}
	if objRefSize != 1 {
		t.Fatalf("object ref size = %d, want 1", objRefSize)
	}
	if offsetIntSize < 1 || offsetIntSize > 8 {
		t.Fatalf("offset int size = %d", offsetIntSize)
	}
	if offsetTableOffset+3*offsetIntSize > len(body)-32 {
		t.Fatalf("offset table runs past the trailer")
	}
	offsets := make([]int, 3)
	for i := range offsets {
		offsets[i] = int(readBE(body[offsetTableOffset+i*offsetIntSize:], offsetIntSize))
	}
	for i, off := range offsets {
		if off < 8 || off >= offsetTableOffset {
			t.Fatalf("object %d offset %d is outside the object table", i, off)
		}
	}

	if body[offsets[0]] != 0xD1 {
		t.Fatalf("object 0 marker = %#x, want dict with one entry", body[offsets[0]])
	}
	keyRef := int(body[offsets[0]+1])
	valueRef := int(body[offsets[0]+2])
	if keyRef != 1 || valueRef != 2 {
		t.Fatalf("dict refs = %d/%d, want 1/2", keyRef, valueRef)
	}

	strLen, strStart := plistLength(body[offsets[1]:])
	if body[offsets[1]]&0xF0 != 0x50 {
		t.Fatalf("object 1 is not an ASCII string: %#x", body[offsets[1]])
	}
	key := string(body[offsets[1]+strStart : offsets[1]+strStart+strLen])

	if body[offsets[2]]&0xF0 != 0x40 {
		t.Fatalf("object 2 is not data: %#x", body[offsets[2]])
	}
	dataLen, dataStart := plistLength(body[offsets[2]:])
	data := body[offsets[2]+dataStart : offsets[2]+dataStart+dataLen]

	return key, data
}

// plistLength decodes the length of a variable-length plist object and
// returns it together with the number of marker bytes it occupied.
func plistLength(b []byte) (int, int) {
	if b[0]&0x0F != 0x0F {
		return int(b[0] & 0x0F), 1
	}
	intMarker := b[1]
	n := 1 << (intMarker & 0x0F)
	start := 2
	return int(readBE(b[start:start+n], n)), start + n
}

func readBE(b []byte, n int) uint64 {
	var v uint64
	for i := range min(n, len(b)) {
		v = v<<8 | uint64(b[i])
	}
	return v
}
