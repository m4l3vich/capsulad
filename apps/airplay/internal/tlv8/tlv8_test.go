package tlv8

import (
	"bytes"
	"testing"
)

func TestFragmentReassembly(t *testing.T) {
	val := make([]byte, 300)
	for i := range val {
		val[i] = byte(i)
	}
	enc := Encode(Item{Tag: TagEncryptedData, Value: val})
	// A 300-byte value must split into a 255-byte and a 45-byte fragment.
	if len(enc) != 2+255+2+45 {
		t.Fatalf("unexpected encoded length %d", len(enc))
	}
	items, err := Decode(enc)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 reassembled item, got %d", len(items))
	}
	if items[0].Tag != TagEncryptedData || !bytes.Equal(items[0].Value, val) {
		t.Fatalf("reassembled value mismatch")
	}
	// Re-encode must be byte-identical.
	if got := Encode(items...); !bytes.Equal(got, enc) {
		t.Fatalf("re-encode mismatch")
	}
}

func TestDecodeMergesSameTagFragments(t *testing.T) {
	raw := []byte{TagPublicKey, 2, 0xaa, 0xbb, TagPublicKey, 1, 0xcc}
	items, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || len(items[0].Value) != 3 {
		t.Fatalf("expected merged 3-byte item, got %+v", items)
	}
	if !bytes.Equal(items[0].Value, []byte{0xaa, 0xbb, 0xcc}) {
		t.Fatal("merged value mismatch")
	}
}

func TestEmptyInput(t *testing.T) {
	if _, err := Decode(nil); err == nil {
		t.Fatal("expected error for empty input")
	}
	if _, err := Decode([]byte{}); err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestTruncated(t *testing.T) {
	if _, err := Decode([]byte{TagState}); err == nil {
		t.Fatal("expected error for truncated header")
	}
	if _, err := Decode([]byte{TagState, 4, 1, 2}); err == nil {
		t.Fatal("expected error for truncated value")
	}
}

func TestAccessors(t *testing.T) {
	items := []Item{
		{Tag: TagIdentifier, Value: []byte("abc")},
		{Tag: TagState, Value: []byte{0x03}},
	}
	if got := GetString(items, TagIdentifier); got != "abc" {
		t.Fatalf("GetString = %q", got)
	}
	if got := GetString(items, TagProof); got != "" {
		t.Fatalf("GetString absent = %q", got)
	}
	if v, ok := GetUint8(items, TagState); !ok || v != 3 {
		t.Fatalf("GetUint8 = %d,%v", v, ok)
	}
	if _, ok := GetUint8(items, TagError); ok {
		t.Fatal("GetUint8 absent should be false")
	}
	if _, ok := Get(items, TagMethod); ok {
		t.Fatal("Get absent should be false")
	}
}

func TestConstants(t *testing.T) {
	// Confirmed against pair_ap/pair-tlv.h:10-32.
	pairs := map[byte]byte{
		TagMethod: 0, TagIdentifier: 1, TagSalt: 2, TagPublicKey: 3,
		TagProof: 4, TagEncryptedData: 5, TagState: 6, TagError: 7,
		TagRetryDelay: 8, TagCertificate: 9, TagSignature: 10, TagPermissions: 11,
		TagFragmentData: 12, TagFragmentLast: 13, TagFlags: 19,
	}
	for k, v := range pairs {
		if k != v {
			t.Fatalf("tag constant mismatch: got %#x want %#x", k, v)
		}
	}
}
