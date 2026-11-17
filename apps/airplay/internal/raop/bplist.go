package raop

import "bytes"

// binaryPlistDataDict encodes the one-entry dictionary {key: <data>} as a
// binary property list ("bplist00"). This is the format shairport-sync emits
// with plist_to_bin() and labels application/x-apple-binary-plist (rtsp.c).
//
// The encoding uses one-byte object references, which is sufficient for the
// three objects in this dictionary.
func binaryPlistDataDict(key string, value []byte) []byte {
	objects := [][]byte{
		{0xD1, 0x01, 0x02}, // dict, 1 entry: key ref 1, value ref 2
		plistASCIIString(key),
		plistDataObject(value),
	}

	var buf bytes.Buffer
	buf.WriteString("bplist00")

	offsets := make([]int, len(objects))
	for i, obj := range objects {
		offsets[i] = buf.Len()
		buf.Write(obj)
	}

	offsetTableOffset := buf.Len()
	offsetIntSize := 1
	if offsetTableOffset > 0xFF {
		offsetIntSize = 2
	}
	for _, off := range offsets {
		writeBEInt(&buf, uint64(off), offsetIntSize)
	}

	buf.Write(make([]byte, 6)) // unused trailer bytes
	buf.WriteByte(byte(offsetIntSize))
	buf.WriteByte(1) // object reference size
	writeBEInt(&buf, uint64(len(objects)), 8)
	writeBEInt(&buf, 0, 8) // top object
	writeBEInt(&buf, uint64(offsetTableOffset), 8)

	return buf.Bytes()
}

// plistASCIIString encodes an ASCII string object.
func plistASCIIString(s string) []byte {
	out := plistMarker(0x50, len(s))
	return append(out, s...)
}

// plistDataObject encodes a data object.
func plistDataObject(b []byte) []byte {
	out := plistMarker(0x40, len(b))
	return append(out, b...)
}

// plistMarker emits the object marker for a variable-length type. Lengths of
// 15 or more use the escape marker followed by a length integer object, per
// the binary plist format.
func plistMarker(base byte, n int) []byte {
	if n < 15 {
		return []byte{base | byte(n)}
	}
	return append([]byte{base | 0x0F}, plistInteger(uint64(n))...)
}

// plistInteger encodes an unsigned integer as a binary plist integer object.
func plistInteger(v uint64) []byte {
	switch {
	case v <= 0xFF:
		return []byte{0x10, byte(v)}
	case v <= 0xFFFF:
		return []byte{0x11, byte(v >> 8), byte(v)}
	case v <= 0xFFFFFFFF:
		return []byte{0x12, byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
	default:
		return []byte{0x13,
			byte(v >> 56), byte(v >> 48), byte(v >> 40), byte(v >> 32),
			byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
	}
}

func writeBEInt(buf *bytes.Buffer, v uint64, n int) {
	for i := n - 1; i >= 0; i-- {
		buf.WriteByte(byte(v >> (8 * i)))
	}
}
