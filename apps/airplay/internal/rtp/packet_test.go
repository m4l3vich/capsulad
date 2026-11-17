package rtp

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"golang.org/x/crypto/chacha20poly1305"
)

// buildPacket assembles a datagram the way an AirPlay sender does: a 12-byte
// RTP header, the body, and the 8-byte nonce at the very end.
func buildPacket(marker bool, payloadType uint8, seq uint16, ts, ssrc uint32, body, nonce []byte) []byte {
	b := make([]byte, headerSize, headerSize+len(body)+len(nonce))
	b[0] = 0x80
	if marker {
		b[1] |= 0x80
	}
	b[1] |= payloadType
	binary.BigEndian.PutUint16(b[2:4], seq)
	binary.BigEndian.PutUint32(b[4:8], ts)
	binary.BigEndian.PutUint32(b[8:12], ssrc)
	b = append(b, body...)
	b = append(b, nonce...)
	return b
}

// encryptRealtime encrypts a frame the way the sender does: ChaCha20-Poly1305
// with the 8-byte nonce front-padded to the IETF 12-byte nonce, and the RTP
// timestamp and SSRC as additional data. The result is the ciphertext followed
// by its tag.
func encryptRealtime(t *testing.T, key, nonce, aad, frame []byte) []byte {
	t.Helper()
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		t.Fatal(err)
	}
	full := make([]byte, chacha20poly1305.NonceSize)
	copy(full[chacha20poly1305.NonceSize-len(nonce):], nonce)
	header := make([]byte, 12)
	copy(header[4:12], aad)
	return aead.Seal(nil, full, frame, header[4:12])
}

func testKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i * 7)
	}
	return key
}

func testNonce() []byte { return []byte{1, 2, 3, 4, 5, 6, 7, 8} }

func TestParseAndDecryptRealtime(t *testing.T) {
	key, nonce := testKey(), testNonce()
	frame := []byte("an ALAC frame, which happens to be plain text here")
	ts, ssrc := uint32(0x11223344), uint32(0xaabbccdd)
	aad := make([]byte, 8)
	binary.BigEndian.PutUint32(aad[0:4], ts)
	binary.BigEndian.PutUint32(aad[4:8], ssrc)

	body := encryptRealtime(t, key, nonce, aad, frame)
	raw := buildPacket(true, PayloadALAC, 7, ts, ssrc, body, nonce)

	pkt, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !pkt.Marker || pkt.PayloadType != PayloadALAC || pkt.Sequence != 7 || pkt.Timestamp != ts || pkt.SSRC != ssrc {
		t.Fatalf("unexpected header: %+v", pkt)
	}
	if len(pkt.Header) != 12 || len(pkt.Nonce) != 8 {
		t.Fatalf("header %d bytes, nonce %d bytes", len(pkt.Header), len(pkt.Nonce))
	}
	if !bytes.Equal(pkt.Payload, body) {
		t.Fatal("payload is not the ciphertext including the tag")
	}

	got, err := pkt.DecryptRealtime(key)
	if err != nil {
		t.Fatalf("DecryptRealtime: %v", err)
	}
	if !bytes.Equal(got, frame) {
		t.Fatalf("decrypted %q, want %q", got, frame)
	}

	// A tampered packet must fail authentication, as must the wrong key.
	tampered := append([]byte{}, raw...)
	tampered[len(tampered)-nonceSize-1] ^= 0x01
	tp, err := Parse(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tp.DecryptRealtime(key); err == nil {
		t.Error("tampered packet authenticated")
	}
	wrong := testKey()
	wrong[0] ^= 0xff
	if _, err := pkt.DecryptRealtime(wrong); err == nil {
		t.Error("wrong key authenticated")
	}
	if _, err := pkt.DecryptRealtime(key[:16]); err == nil {
		t.Error("short key accepted")
	}
}

func TestDecryptRealtimeRedundancy(t *testing.T) {
	key, nonce := testKey(), testNonce()
	ts, ssrc := uint32(100000), uint32(0xdeadbeef)
	aad := make([]byte, 8)
	binary.BigEndian.PutUint32(aad[0:4], ts)
	binary.BigEndian.PutUint32(aad[4:8], ssrc)

	// Two redundant blocks (one and two packets old) in front of the current
	// frame, as RFC2198 describes: F=1 descriptors, then the F=0 descriptor
	// of the primary block.
	old1 := []byte("frame from one packet ago")
	old2 := []byte("frame from two packets ago")
	current := []byte("current frame")
	// The redundant copies precede the current frame, newest first, which is
	// the order the descriptors list them in as well.
	plain := append(append(append([]byte{}, old1...), old2...), current...)

	descriptor := func(payloadType uint8, offset14, length10 uint16) []byte {
		// F|block PT, then timestamp offset (14 bits) and block length
		// (10 bits) spanning bytes 1 to 3.
		d := []byte{0x80 | payloadType, 0, 0, 0}
		word := offset14<<2 | length10>>8
		binary.BigEndian.PutUint16(d[1:3], word)
		d[3] = byte(length10)
		return d
	}
	header := append(append(append([]byte{},
		descriptor(PayloadALAC, 352, uint16(len(old1)))...),
		descriptor(PayloadALAC, 704, uint16(len(old2)))...), PayloadALAC)

	ciphertext := encryptRealtime(t, key, nonce, aad, plain)

	raw := buildPacket(false, PayloadRedundant, 11, ts, ssrc, append(header, ciphertext...), nonce)
	pkt, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	blocks := pkt.RedundantBlocks()
	if len(blocks) != 2 {
		t.Fatalf("got %d redundant blocks, want 2", len(blocks))
	}
	if blocks[0].Length != len(old1) || blocks[0].Timestamp != ts-352 {
		t.Errorf("block 0 = %+v, want timestamp %d length %d", blocks[0], ts-352, len(old1))
	}
	if blocks[1].Length != len(old2) || blocks[1].Timestamp != ts-704 {
		t.Errorf("block 1 = %+v, want timestamp %d length %d", blocks[1], ts-704, len(old2))
	}

	got, err := pkt.DecryptRealtime(key)
	if err != nil {
		t.Fatalf("DecryptRealtime: %v", err)
	}
	if !bytes.Equal(got, current) {
		t.Fatalf("got %q, want the current frame %q", got, current)
	}
	// The frame is exactly the current one, with both redundant copies already
	// removed (review B1). A caller that strips the descriptors again would
	// lose len(old1)+len(old2) bytes here: that is what the second, redundant
	// slice in the old deliver path did.
	if len(got) != len(current) {
		t.Fatalf("frame is %d bytes, want the %d-byte current frame", len(got), len(current))
	}
	redundant := 0
	for _, b := range blocks {
		redundant += b.Length
	}
	if redundant == 0 {
		t.Fatal("the descriptors reported no redundancy: the test cannot see a double strip")
	}
	if bytes.HasPrefix(got, old2) || bytes.HasPrefix(got, old1) {
		t.Fatal("the frame still starts with a redundant copy")
	}

	// The packet-by-packet decoder returns the same current frame, appended to
	// the caller's buffer.
	dec, err := NewRealtimeDecoder(key)
	if err != nil {
		t.Fatal(err)
	}
	scratch := make([]byte, 0, 256)
	reused, err := dec.DecodeInto(scratch, pkt)
	if err != nil {
		t.Fatalf("DecodeInto: %v", err)
	}
	if !bytes.Equal(reused, current) {
		t.Fatalf("DecodeInto returned %q, want the current frame %q", reused, current)
	}
}

// TestDecodeIntoAppends checks that the Into methods append to the caller's
// buffer instead of replacing it, which is what makes a decrypt loop allocation
// free (review C8).
func TestDecodeIntoAppends(t *testing.T) {
	key, nonce := testKey(), testNonce()
	frame := []byte("a frame that is decrypted into a reused buffer")
	ts, ssrc := uint32(77), uint32(88)
	aad := make([]byte, 8)
	binary.BigEndian.PutUint32(aad[0:4], ts)
	binary.BigEndian.PutUint32(aad[4:8], ssrc)
	raw := buildPacket(false, PayloadALAC, 9, ts, ssrc, encryptRealtime(t, key, nonce, aad, frame), nonce)
	pkt, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}

	prefix := []byte("keep me")
	dst := append(make([]byte, 0, 512), prefix...)
	dec, err := NewRealtimeDecoder(key)
	if err != nil {
		t.Fatal(err)
	}
	out, err := dec.DecodeInto(dst, pkt)
	if err != nil {
		t.Fatalf("DecodeInto: %v", err)
	}
	if !bytes.Equal(out, frame) {
		t.Fatalf("out = %q, want the frame %q", out, frame)
	}
	// The frame was appended into the caller's array, not into a fresh one:
	// re-slicing dst shows the prefix followed by the plaintext.
	grown := dst[:len(prefix)+len(frame)]
	if !bytes.Equal(grown, append(append([]byte{}, prefix...), frame...)) {
		t.Fatalf("the caller's buffer holds %q, want the prefix followed by the frame", grown)
	}
	// A failed authentication leaves the destination untouched.
	tampered := append([]byte{}, raw...)
	tampered[len(tampered)-1] ^= 0xff
	tp, err := Parse(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dec.DecodeInto(dst, tp); err != ErrAuth {
		t.Fatalf("tampered packet: err = %v, want ErrAuth", err)
	}
	if !bytes.HasPrefix(dst, prefix) {
		t.Errorf("the destination's prefix changed on a failed decrypt: %q", dst)
	}

	// The legacy path appends too.
	aesKey, iv := make([]byte, 16), make([]byte, 16)
	for i := range aesKey {
		aesKey[i] = byte(i)
	}
	legacy := &Packet{Payload: []byte{1, 2, 3, 4}, Nonce: nonce, Header: make([]byte, headerSize)}
	legacyDec, err := NewLegacyDecoder(aesKey, iv)
	if err != nil {
		t.Fatal(err)
	}
	legacyDst := append(make([]byte, 0, 64), prefix...)
	legacyOut, err := legacyDec.DecodeInto(legacyDst, legacy)
	if err != nil {
		t.Fatalf("legacy DecodeInto: %v", err)
	}
	want := append(append([]byte{}, prefix...), append(append([]byte{}, legacy.Payload...), nonce...)...)
	if !bytes.Equal(legacyOut, want) {
		t.Fatalf("legacy out = %x, want %x", legacyOut, want)
	}
}

// TestDecryptAllocsPerRun pins the allocation behaviour of a steady-state
// decrypt loop: a stream that holds its decoder and reuses one plaintext buffer
// must not allocate per packet (review C8).
func TestDecryptAllocsPerRun(t *testing.T) {
	key, nonce := testKey(), testNonce()
	frame := bytes.Repeat([]byte{0x5a}, 4096)
	ts, ssrc := uint32(1234), uint32(0xfeed)
	aad := make([]byte, 8)
	binary.BigEndian.PutUint32(aad[0:4], ts)
	binary.BigEndian.PutUint32(aad[4:8], ssrc)
	raw := buildPacket(false, PayloadALAC, 3, ts, ssrc, encryptRealtime(t, key, nonce, aad, frame), nonce)
	pkt, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := NewRealtimeDecoder(key)
	if err != nil {
		t.Fatal(err)
	}
	// One 4 KiB scratch buffer, reused: the same shape as the receive loop.
	scratch := make([]byte, 0, len(frame)+64)
	allocs := testing.AllocsPerRun(1000, func() {
		out, err := dec.DecodeInto(scratch[:0], pkt)
		if err != nil {
			t.Fatalf("DecodeInto: %v", err)
		}
		if len(out) != len(frame) {
			t.Fatalf("decrypted %d bytes, want %d", len(out), len(frame))
		}
	})
	if allocs != 0 {
		t.Errorf("realtime decrypt allocates %.1f times per packet, want 0", allocs)
	}

	// The legacy path reuses the caller's plaintext buffer and the AES key
	// schedule, which the old implementation did not: it copied Payload||Nonce
	// into a fresh 4 KiB slice and decrypted into a second one, once per
	// packet. What remains is the small fixed cost of cipher.NewCBCDecrypter:
	// every RAOP packet restarts the CBC chain from the same ANNOUNCE IV and
	// cipher.BlockMode offers no way to reset it, so the mode cannot be carried
	// over from the previous packet. Allocations must therefore not grow with
	// the frame, which is what this measures.
	aesKey, iv := make([]byte, 16), make([]byte, 16)
	legacyDec, err := NewLegacyDecoder(aesKey, iv)
	if err != nil {
		t.Fatal(err)
	}
	legacyAllocs := func(payload int) float64 {
		pkt := &Packet{
			Payload: bytes.Repeat([]byte{7}, payload),
			Nonce:   nonce,
			Header:  make([]byte, headerSize),
		}
		scratch := make([]byte, 0, payload+nonceSize)
		return testing.AllocsPerRun(500, func() {
			if _, err := legacyDec.DecodeInto(scratch[:0], pkt); err != nil {
				t.Fatalf("legacy DecodeInto: %v", err)
			}
		})
	}
	small, large := legacyAllocs(512), legacyAllocs(8192)
	if large != small {
		t.Errorf("legacy decrypt allocates %.1f times for a 512-byte frame and %.1f times for an "+
			"8192-byte one: the plaintext must go into the caller's buffer", small, large)
	}
}

func TestParseRedundancyMalformed(t *testing.T) {
	cases := map[string][]byte{
		"empty":                {},
		"truncated descriptor": {0x80 | PayloadALAC, 0x00},
		"no primary block":     {0x80 | PayloadALAC, 0, 0, 0},
	}
	for name, payload := range cases {
		if _, _, ok := parseRedundancy(payload, 0); ok {
			t.Errorf("%s: accepted", name)
		}
	}
	// Well formed: one redundant block and the primary descriptor.
	payload := []byte{0x80 | PayloadALAC, 0x01, 0x00, 0x40, PayloadALAC}
	blocks, headerLen, ok := parseRedundancy(payload, 1000)
	if !ok || len(blocks) != 1 || headerLen != 5 {
		t.Fatalf("blocks=%v headerLen=%d ok=%v", blocks, headerLen, ok)
	}
	if blocks[0].Timestamp != 1000-64 || blocks[0].Length != 64 {
		t.Fatalf("block = %+v, want timestamp %d length 64", blocks[0], 1000-64)
	}
}

func TestRedundantBlocksNilForOtherPayloadTypes(t *testing.T) {
	raw := buildPacket(false, PayloadALAC, 1, 0, 1, bytes.Repeat([]byte{1}, 32), testNonce())
	pkt, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if blocks := pkt.RedundantBlocks(); blocks != nil {
		t.Errorf("got %v, want nil for payload type 96", blocks)
	}
}

// TestParseUnencrypted covers the short datagrams of an unencrypted stream,
// which Parse deliberately rejects because it reserves the trailing tag and
// nonce (review B8).
func TestParseUnencrypted(t *testing.T) {
	header := make([]byte, MinUnencryptedPacketSize)
	header[0] = 0x80
	header[1] = 0x80 | PayloadALAC
	binary.BigEndian.PutUint16(header[2:4], 4242)
	binary.BigEndian.PutUint32(header[4:8], 0x0a0b0c0d)
	binary.BigEndian.PutUint32(header[8:12], 0x11223344)

	// A datagram of exactly the header size is legal on an unencrypted stream:
	// it carries an empty frame.
	pkt, err := ParseUnencrypted(header)
	if err != nil {
		t.Fatalf("ParseUnencrypted(header only): %v", err)
	}
	if !pkt.Marker || pkt.PayloadType != PayloadALAC || pkt.Sequence != 4242 ||
		pkt.Timestamp != 0x0a0b0c0d || pkt.SSRC != 0x11223344 {
		t.Fatalf("header = %+v", pkt)
	}
	if len(pkt.Header) != MinUnencryptedPacketSize {
		t.Errorf("header is %d bytes, want %d", len(pkt.Header), MinUnencryptedPacketSize)
	}
	if len(pkt.Payload) != 0 || pkt.Nonce != nil {
		t.Fatalf("payload %d bytes, nonce %v: want an empty payload and no nonce",
			len(pkt.Payload), pkt.Nonce)
	}

	// Every byte after the header is payload.
	body := []byte{1, 2, 3, 4, 5}
	raw := append(append([]byte{}, header...), body...)
	pkt, err = ParseUnencrypted(raw)
	if err != nil {
		t.Fatalf("ParseUnencrypted(payload): %v", err)
	}
	if !bytes.Equal(pkt.Payload, body) {
		t.Fatalf("payload = %v, want %v", pkt.Payload, body)
	}
	// This is the datagram Parse drops: 17 bytes cannot hold a tag and a nonce.
	if _, err := Parse(raw); err == nil {
		t.Error("Parse accepted a 17-byte unencrypted datagram")
	}

	// One byte short of a header is not a datagram at all.
	if _, err := ParseUnencrypted(header[:MinUnencryptedPacketSize-1]); err != ErrShortPacket {
		t.Errorf("short datagram: err = %v, want ErrShortPacket", err)
	}

	// The header rules are the same as for encrypted streams. The datagram
	// below is long enough that a version/CC problem, not its length, is what
	// has to make it fail.
	long := append(append([]byte{}, header...), bytes.Repeat([]byte{0xaa}, 32)...)
	bad := map[string][]byte{
		"version 1":        append([]byte{0x40}, long[1:]...),
		"version 3":        append([]byte{0xc0}, long[1:]...),
		"csrc count":       append([]byte{0x82}, long[1:]...),
		"header extension": append([]byte{0x90}, long[1:]...),
	}
	for name, b := range bad {
		if _, err := ParseUnencrypted(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// The two parsers read the same bytes differently on purpose: Parse keeps
	// the last 8 bytes as the nonce and the 16 before them as the tag.
	pkt, err = Parse(long)
	if err != nil {
		t.Fatalf("Parse(long): %v", err)
	}
	if len(pkt.Payload) != len(long)-headerSize-nonceSize || len(pkt.Nonce) != nonceSize {
		t.Fatalf("Parse split the datagram into %d payload and %d nonce bytes",
			len(pkt.Payload), len(pkt.Nonce))
	}

	// An unencrypted packet carries no nonce, so the realtime path cannot read
	// it: it must say so instead of decrypting with a truncated nonce.
	short, err := ParseUnencrypted(append(append([]byte{}, header...), 0xaa))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := short.DecryptRealtime(testKey()); err == nil {
		t.Error("DecryptRealtime accepted a packet without a nonce")
	}
}

func TestParseRejectsMalformedPackets(t *testing.T) {
	key, nonce := testKey(), testNonce()
	frame := []byte("frame")
	ts, ssrc := uint32(1), uint32(2)
	aad := make([]byte, 8)
	binary.BigEndian.PutUint32(aad[0:4], ts)
	binary.BigEndian.PutUint32(aad[4:8], ssrc)
	body := encryptRealtime(t, key, nonce, aad, frame)
	raw := buildPacket(false, PayloadALAC, 1, ts, ssrc, body, nonce)

	cases := map[string][]byte{
		"nil":              nil,
		"short":            raw[:35],
		"version 1":        append([]byte{0x40}, raw[1:]...),
		"version 3":        append([]byte{0xc0}, raw[1:]...),
		"csrc count":       append([]byte{0x82}, raw[1:]...),
		"header extension": append([]byte{0x90}, raw[1:]...),
	}
	for name, b := range cases {
		if _, err := Parse(b); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := Parse(raw); err != nil {
		t.Fatalf("valid packet rejected: %v", err)
	}
}

// TestDecryptLegacyAESCBCVector uses the NIST SP 800-38A F.2.1 CBC-AES128
// vector, whose plaintext is four blocks followed by nothing, plus a run of
// clear trailing bytes to exercise the RAOP boundary rule.
func TestDecryptLegacyAESCBCVector(t *testing.T) {
	key, _ := hex.DecodeString("2b7e151628aed2a6abf7158809cf4f3c")
	iv, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f")
	ciphertext, _ := hex.DecodeString(
		"7649abac8119b246cee98e9b12e9197d" +
			"5086cb9b507219ee95db113a917678b2" +
			"73bed6b8e3c1743b7116e69e22229516" +
			"3ff1caa1681fac09120eca307586e1a7")
	want, _ := hex.DecodeString(
		"6bc1bee22e409f96e93d7e117393172a" +
			"ae2d8a571e03ac9c9eb76fac45af8e51" +
			"30c81c46a35ce411e5fbc1191a0a52ef" +
			"f69f2445df4f9b17ad2b417be66c3710")

	// The RAOP sender encrypts only the 16-byte multiple prefix and appends
	// the leftover bytes in clear; here six of them, before the nonce.
	clear := []byte{0xde, 0xad, 0xbe, 0xef, 0x01, 0x02}
	nonce := []byte{9, 9, 9, 9, 9, 9, 9, 9}
	body := append(append([]byte{}, ciphertext...), clear...)
	raw := buildPacket(false, PayloadALAC, 1, 100, 200, body, nonce)

	pkt, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pkt.DecryptLegacyAES(key, iv)
	if err != nil {
		t.Fatalf("DecryptLegacyAES: %v", err)
	}
	wantAll := append(append(append([]byte{}, want...), clear...), nonce...)
	if !bytes.Equal(got, wantAll) {
		t.Fatalf("got %x\nwant %x", got, wantAll)
	}

	if _, err := pkt.DecryptLegacyAES(key[:8], iv); err == nil {
		t.Error("short key accepted")
	}
	if _, err := pkt.DecryptLegacyAES(key, iv[:8]); err == nil {
		t.Error("short IV accepted")
	}
}

// TestDecryptLegacyAESPerPacketIV checks that every RAOP packet starts its CBC
// chain from the ANNOUNCE IV, so one decoder can decrypt a stream packet by
// packet: the standard library's CBC mode carries the last ciphertext block
// over, and a decoder that reused it would corrupt the first block of every
// packet after the first.
func TestDecryptLegacyAESPerPacketIV(t *testing.T) {
	key, iv := make([]byte, 16), make([]byte, 16)
	for i := range key {
		key[i] = byte(i)
		iv[i] = byte(0xf0 + i)
	}
	// Encrypt two frames with the same key and IV, the way a RAOP sender does.
	encrypt := func(frame []byte) []byte {
		block, err := aes.NewCipher(key)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]byte, len(frame))
		cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, frame)
		return out
	}
	frames := [][]byte{
		bytes.Repeat([]byte{0x11}, 32),
		bytes.Repeat([]byte{0x22}, 48),
	}
	dec, err := NewLegacyDecoder(key, iv)
	if err != nil {
		t.Fatal(err)
	}
	scratch := make([]byte, 0, 128)
	for i, frame := range frames {
		pkt := &Packet{Payload: encrypt(frame), Header: make([]byte, headerSize)}
		got, err := dec.DecodeInto(scratch[:0], pkt)
		if err != nil {
			t.Fatalf("packet %d: DecodeInto: %v", i, err)
		}
		if !bytes.Equal(got, frame) {
			t.Fatalf("packet %d decrypted to %x, want %x", i, got, frame)
		}
	}
	// Decrypting the same packet twice must give the same bytes: the IV is
	// reset, not left at the previous packet's chain end.
	pkt := &Packet{Payload: encrypt(frames[0]), Header: make([]byte, headerSize)}
	first, err := dec.DecodeInto(scratch[:0], pkt)
	if err != nil {
		t.Fatal(err)
	}
	second, err := dec.DecodeInto(scratch[:0], pkt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("the same packet decrypted to %x and then %x", first, second)
	}
}

// TestDecryptLegacyAESShortPayload covers less than one cipher block: the bytes
// are passed through untouched.
func TestDecryptLegacyAESShortPayload(t *testing.T) {
	// Such a datagram cannot come from Parse, which always requires room for
	// the tag and nonce, so the packet is built by hand.
	pkt := &Packet{Payload: []byte{1, 2, 3, 4}, Nonce: testNonce()}
	got, err := pkt.DecryptLegacyAES(make([]byte, 16), make([]byte, 16))
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte{}, pkt.Payload...), pkt.Nonce...)
	if !bytes.Equal(got, want) {
		t.Fatalf("got %x, want %x", got, want)
	}
}

func TestReceiverReport(t *testing.T) {
	s := Stats{Received: 1000, Lost: 24, HighestSeq: 60000, Cycles: 3, Jitter: 1234}
	rr := ReceiverReport(0x11223344, 0x55667788, s)
	if len(rr) != 32 {
		t.Fatalf("receiver report is %d bytes, want 32", len(rr))
	}
	// RFC 3550 6.4.2: the low five bits of the first byte are the number of
	// report blocks that follow the sender SSRC, and the packet has exactly one
	// (review B7). The header said zero while carrying one block, so a
	// conforming parser read 32 bytes of garbage after the SSRC.
	if rr[0] != 0x81 || rr[1] != 201 {
		t.Fatalf("header %x %x, want 81 c9", rr[0], rr[1])
	}
	if got := int(rr[0] & 0x1f); got != receiverReportBlocks {
		t.Errorf("report count %d, want %d (the blocks the packet carries)", got, receiverReportBlocks)
	}
	// Version 2, no padding, no reports-are-compressed flag.
	if rr[0]>>6 != 2 || rr[0]&0x20 != 0 {
		t.Errorf("version/padding bits in %#x", rr[0])
	}
	// One report block is 24 bytes: 32 total minus the 8-byte header.
	blockBytes := len(rr) - 8
	if want := 24 * receiverReportBlocks; blockBytes != want {
		t.Errorf("the report blocks are %d bytes, want %d", blockBytes, want)
	}
	if n := binary.BigEndian.Uint16(rr[2:4]); n != 7 {
		t.Errorf("length field %d, want 7", n)
	}
	if got := binary.BigEndian.Uint32(rr[4:8]); got != 0x11223344 {
		t.Errorf("sender ssrc %#x", got)
	}
	if got := binary.BigEndian.Uint32(rr[8:12]); got != 0x55667788 {
		t.Errorf("source ssrc %#x", got)
	}
	if want := byte(24 * 256 / 1024); rr[12] != want {
		t.Errorf("fraction lost %d, want %d", rr[12], want)
	}
	if got := uint32(rr[13])<<16 | uint32(rr[14])<<8 | uint32(rr[15]); got != 24 {
		t.Errorf("cumulative lost %d, want 24", got)
	}
	if got := binary.BigEndian.Uint32(rr[16:20]); got != 3<<16|60000 {
		t.Errorf("extended highest sequence %d, want %d", got, 3<<16|60000)
	}
	if got := binary.BigEndian.Uint32(rr[20:24]); got != 1234 {
		t.Errorf("jitter %d, want 1234", got)
	}
	if !bytes.Equal(rr[24:32], make([]byte, 8)) {
		t.Error("LSR/DLSR are not zero")
	}
	// No losses: no fraction.
	if rr := ReceiverReport(1, 2, Stats{Received: 10, HighestSeq: 9}); rr[12] != 0 {
		t.Errorf("fraction lost %d, want 0", rr[12])
	}
}

func TestNACKPacket(t *testing.T) {
	if pkts := NACKPacket(1, 2, nil); pkts != nil {
		t.Errorf("got %v for no missing packets", pkts)
	}

	missing := []uint16{100, 101, 102, 105}
	pkts := NACKPacket(0xaaaa, 0xbbbb, missing)
	if len(pkts) != 2 {
		t.Fatalf("got %d packets, want 2", len(pkts))
	}
	p := pkts[0]
	if len(p) != 16 {
		t.Fatalf("packet is %d bytes, want 16", len(p))
	}
	if p[0] != 0x81 || p[1] != 205 {
		t.Fatalf("header %x %x, want 81 cd", p[0], p[1])
	}
	if got := binary.BigEndian.Uint16(p[2:4]); got != 3 {
		t.Errorf("length field %d, want 3", got)
	}
	if got := binary.BigEndian.Uint32(p[4:8]); got != 0xaaaa {
		t.Errorf("sender ssrc %#x", got)
	}
	if got := binary.BigEndian.Uint32(p[8:12]); got != 0xbbbb {
		t.Errorf("media ssrc %#x", got)
	}
	if got := binary.BigEndian.Uint16(p[12:14]); got != 100 {
		t.Errorf("PID %d, want 100", got)
	}
	if got := binary.BigEndian.Uint16(p[14:16]); got != 0x3 {
		t.Errorf("BLP %#x, want 0x3", got)
	}
	if got := binary.BigEndian.Uint16(pkts[1][12:14]); got != 105 {
		t.Errorf("second PID %d, want 105", got)
	}
	if got := binary.BigEndian.Uint16(pkts[1][14:16]); got != 0 {
		t.Errorf("second BLP %#x, want 0", got)
	}

	// Runs are broken up at 17 sequence numbers, and wrap around.
	long := make([]uint16, 0, 40)
	for i := range 40 {
		long = append(long, uint16(65500+i))
	}
	wrap := NACKPacket(1, 2, long)
	if len(wrap) != 3 {
		t.Fatalf("got %d packets for 40 missing, want 3", len(wrap))
	}
	if got := binary.BigEndian.Uint16(wrap[1][12:14]); got != 65500+17 {
		t.Errorf("second PID %d, want %d", got, 65500+17)
	}
	if got := binary.BigEndian.Uint16(wrap[0][14:16]); got != 0xffff {
		t.Errorf("first BLP %#x, want 0xffff", got)
	}
}

func TestParseSenderReport(t *testing.T) {
	sr := make([]byte, 28)
	sr[0] = 0x80
	sr[1] = 200
	binary.BigEndian.PutUint16(sr[2:4], 6)
	binary.BigEndian.PutUint32(sr[4:8], 0x1234)
	binary.BigEndian.PutUint32(sr[8:12], 0xe1234567)  // NTP seconds
	binary.BigEndian.PutUint32(sr[12:16], 0x80000000) // NTP fraction
	binary.BigEndian.PutUint32(sr[16:20], 44100*3)

	secs, frac, ts, ok := ParseSenderReport(sr)
	if !ok || secs != 0xe1234567 || frac != 0x80000000 || ts != 132300 {
		t.Fatalf("got (%#x, %#x, %d, %v)", secs, frac, ts, ok)
	}

	if _, _, _, ok := ParseSenderReport(sr[:27]); ok {
		t.Error("short sender report accepted")
	}
	rr := ReceiverReport(1, 2, Stats{})
	if _, _, _, ok := ParseSenderReport(rr); ok {
		t.Error("receiver report parsed as a sender report")
	}
	sr[1] = 201
	if _, _, _, ok := ParseSenderReport(sr); ok {
		t.Error("wrong payload type accepted")
	}
}
