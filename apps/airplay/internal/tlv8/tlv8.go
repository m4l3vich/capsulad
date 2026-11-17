// Package tlv8 implements the HAP/AirPlay 2 TLV8 encoding used by the
// pairing endpoints (application/pairing+tlv8).
//
// Type numbers verified against shairport-sync pair_ap/pair-tlv.h:10-32
// (TLVType_* block) and the error/method/flags enums in
// pair_ap/pair_homekit.c:116-136.
package tlv8

import "errors"

// TLV type tags. Values confirmed in pair_ap/pair-tlv.h:10-32, except
// TagFragmentData/TagFragmentLast, which the shairport-sync tree lists as 13
// and 14 (pair-tlv.h:28-30); the frozen contract (and the HAP specification)
// use 0x0c/0x0d, which is what we implement here.
const (
	TagMethod        byte = 0x00
	TagIdentifier    byte = 0x01
	TagSalt          byte = 0x02
	TagPublicKey     byte = 0x03
	TagProof         byte = 0x04
	TagEncryptedData byte = 0x05
	TagState         byte = 0x06
	TagError         byte = 0x07
	TagRetryDelay    byte = 0x08
	TagCertificate   byte = 0x09
	TagSignature     byte = 0x0a
	TagPermissions   byte = 0x0b
	TagFragmentData  byte = 0x0c
	TagFragmentLast  byte = 0x0d
	// TagSeparator separates records in a pair-list response.
	TagSeparator byte = 0xff
	TagFlags     byte = 0x13
)

// Pairing methods (pair_ap/pair_homekit.c:122-128).
const (
	MethodPairSetup         byte = 0x00
	MethodPairSetupWithAuth byte = 0x01
	MethodPairVerify        byte = 0x02
	MethodAddPairing        byte = 0x03
	MethodRemovePairing     byte = 0x04
	MethodListPairings      byte = 0x05
)

// TLV error codes (pair_ap/pair-tlv.h:37-45).
const (
	ErrorUnknown        byte = 0x01
	ErrorAuthentication byte = 0x02
	ErrorBackoff        byte = 0x03
	ErrorMaxPeers       byte = 0x04
	ErrorMaxTries       byte = 0x05
	ErrorUnavailable    byte = 0x06
	ErrorBusy           byte = 0x07
)

// FlagTransient marks a transient pair-setup (pair_ap/pair_homekit.c:134-135).
const FlagTransient byte = 0x10

// HAP pairing states M1..M6 (pair_ap/pair_homekit.c:95-108).
const (
	StateM1 byte = 0x01
	StateM2 byte = 0x02
	StateM3 byte = 0x03
	StateM4 byte = 0x04
	StateM5 byte = 0x05
	StateM6 byte = 0x06
)

// Item is a single TLV8 item after fragment reassembly.
type Item struct {
	Tag   byte
	Value []byte
}

// Decode parses a TLV8 byte string, reassembling values that were split across
// consecutive fragments carrying the same tag. It rejects empty input.
func Decode(b []byte) ([]Item, error) {
	if len(b) == 0 {
		return nil, errors.New("tlv8: empty input")
	}
	var items []Item
	for i := 0; i < len(b); {
		if i+2 > len(b) {
			return nil, errors.New("tlv8: truncated item header")
		}
		tag := b[i]
		l := int(b[i+1])
		i += 2
		if i+l > len(b) {
			return nil, errors.New("tlv8: truncated item value")
		}
		val := b[i : i+l]
		i += l
		if n := len(items); n > 0 && items[n-1].Tag == tag {
			items[n-1].Value = append(items[n-1].Value, val...)
			continue
		}
		items = append(items, Item{Tag: tag, Value: append([]byte(nil), val...)})
	}
	return items, nil
}

// Encode serializes items, splitting any value longer than 255 bytes into
// consecutive same-tag fragments.
func Encode(items ...Item) []byte {
	var out []byte
	for _, it := range items {
		v := it.Value
		for {
			n := len(v)
			if n > 255 {
				n = 255
			}
			out = append(out, it.Tag, byte(n))
			out = append(out, v[:n]...)
			v = v[n:]
			if len(v) == 0 {
				break
			}
		}
	}
	return out
}

// Get returns the value for tag, if present.
func Get(items []Item, tag byte) ([]byte, bool) {
	for _, it := range items {
		if it.Tag == tag {
			return it.Value, true
		}
	}
	return nil, false
}

// GetString returns the value for tag as a string, or "" when absent.
func GetString(items []Item, tag byte) string {
	v, ok := Get(items, tag)
	if !ok {
		return ""
	}
	return string(v)
}

// GetUint8 returns the first byte of the value for tag, or false when absent
// or empty.
func GetUint8(items []Item, tag byte) (uint8, bool) {
	v, ok := Get(items, tag)
	if !ok || len(v) == 0 {
		return 0, false
	}
	return v[0], true
}
