// Package plist implements just enough of the Apple property-list format for
// AirPlay 2: a binary plist (bplist00) decoder/encoder and an XML plist
// decoder. Only the value kinds used by the protocol are supported.
package plist

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// Value is the dynamic type produced by Decode and accepted by EncodeBinary.
// Plausible Go types: nil, bool, int64, uint64, UID, float64, string,
// []byte, time.Time, []any and map[string]any.
type Value = any

// UID is a binary-plist UID object. It is deliberately distinct from an
// integer so a UID cannot be mistaken for a numeric control field.
type UID uint64

const appleEpoch = 978307200 // seconds between 1970-01-01 and 2001-01-01 UTC

// Decode decodes either a binary plist or an XML plist document.
func Decode(data []byte) (any, error) {
	if len(data) >= 8 && string(data[:8]) == "bplist00" {
		return decodeBinary(data)
	}
	return decodeXML(data)
}

// ---------------------------------------------------------------------------
// Binary plist decoder
// ---------------------------------------------------------------------------

type decoder struct {
	data          []byte
	offsetIntSize int
	objectRefSize int
	numObjects    uint64
	topObject     uint64
	offsets       []uint64
}

func beUint64(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

func beBytes(b []byte) uint64 { return beUint64(b) }

func decodeBinary(data []byte) (any, error) {
	if len(data) < 40 {
		return nil, errors.New("plist: truncated binary plist")
	}
	if string(data[:8]) != "bplist00" {
		return nil, errors.New("plist: bad binary magic")
	}
	tr := data[len(data)-32:]
	d := &decoder{data: data}
	d.offsetIntSize = int(tr[6])
	d.objectRefSize = int(tr[7])
	d.numObjects = beUint64(tr[8:16])
	d.topObject = beUint64(tr[16:24])
	otOff := beUint64(tr[24:32])

	if d.offsetIntSize < 1 || d.offsetIntSize > 8 {
		return nil, fmt.Errorf("plist: bad offset int size %d", d.offsetIntSize)
	}
	if d.objectRefSize < 1 || d.objectRefSize > 8 {
		return nil, fmt.Errorf("plist: bad object ref size %d", d.objectRefSize)
	}
	if d.numObjects == 0 {
		return nil, errors.New("plist: no objects")
	}
	tableLen := d.numObjects * uint64(d.offsetIntSize)
	if otOff > uint64(len(data)-32) || tableLen > uint64(len(data)-32)-otOff {
		return nil, errors.New("plist: offset table out of range")
	}
	d.offsets = make([]uint64, d.numObjects)
	for i := uint64(0); i < d.numObjects; i++ {
		off := otOff + i*uint64(d.offsetIntSize)
		d.offsets[i] = beUint64(data[off : off+uint64(d.offsetIntSize)])
	}
	return d.parseObject(d.topObject, 0)
}

func (d *decoder) parseObject(index uint64, depth int) (any, error) {
	if depth > 512 {
		return nil, errors.New("plist: nesting too deep")
	}
	if index >= d.numObjects {
		return nil, fmt.Errorf("plist: object index %d out of range", index)
	}
	off := d.offsets[index]
	if off >= uint64(len(d.data)) {
		return nil, errors.New("plist: object offset out of range")
	}
	marker := d.data[off]
	hi := marker >> 4
	lo := marker & 0x0f

	switch hi {
	case 0x0:
		switch lo {
		case 0x0:
			return nil, nil
		case 0x8:
			return false, nil
		case 0x9:
			return true, nil
		}
		// 0x0f fill byte behaves as null.
		return nil, nil
	case 0x1: // integer
		n := 1 << lo
		raw, err := d.slice(off+1, uint64(n))
		if err != nil {
			return nil, err
		}
		v := beUint64(raw)
		switch n {
		case 1, 2, 4:
			// Binary plist 1/2/4-byte integers are unsigned in format 00;
			// do not sign-extend them (CFBinaryPList.c:1093-1109).
			return int64(v), nil
		case 8:
			// An 8-byte integer is a signed 64-bit two's-complement value.
			return int64(v), nil
		case 16:
			bi := new(big.Int).SetBytes(raw)
			if raw[0]&0x80 != 0 {
				bi.Sub(bi, new(big.Int).Lsh(big.NewInt(1), 128))
			}
			if bi.IsInt64() {
				return bi.Int64(), nil
			}
			if bi.Sign() > 0 && bi.IsUint64() {
				return bi.Uint64(), nil
			}
			return nil, fmt.Errorf("plist: integer outside signed/unsigned 64-bit range")
		}
		return nil, fmt.Errorf("plist: bad int width %d", n)
	case 0x2: // real
		n := 1 << lo
		raw, err := d.slice(off+1, uint64(n))
		if err != nil {
			return nil, err
		}
		switch n {
		case 4:
			return float64(math.Float32frombits(uint32(beUint64(raw)))), nil
		case 8:
			return math.Float64frombits(beUint64(raw)), nil
		}
		return nil, fmt.Errorf("plist: bad real width %d", n)
	case 0x3: // date
		if lo != 0x3 {
			return nil, fmt.Errorf("plist: bad date marker %#x", marker)
		}
		raw, err := d.slice(off+1, 8)
		if err != nil {
			return nil, err
		}
		secs := math.Float64frombits(beUint64(raw))
		return time.Unix(appleEpoch, 0).Add(time.Duration(secs * float64(time.Second))).UTC(), nil
	case 0x4: // data
		count, start, err := d.count(off, lo)
		if err != nil {
			return nil, err
		}
		raw, err := d.slice(start, count)
		if err != nil {
			return nil, err
		}
		return append([]byte(nil), raw...), nil
	case 0x5: // ASCII string
		count, start, err := d.count(off, lo)
		if err != nil {
			return nil, err
		}
		raw, err := d.slice(start, count)
		if err != nil {
			return nil, err
		}
		return string(raw), nil
	case 0x6: // UTF-16BE string
		count, start, err := d.count(off, lo)
		if err != nil {
			return nil, err
		}
		raw, err := d.slice(start, count*2)
		if err != nil {
			return nil, err
		}
		u := make([]uint16, count)
		for i := range u {
			u[i] = uint16(beUint64(raw[i*2 : i*2+2]))
		}
		return string(utf16.Decode(u)), nil
	case 0x8: // UID
		n := int(lo) + 1
		if n < 1 || n > 8 {
			return nil, fmt.Errorf("plist: bad UID width %d", n)
		}
		raw, err := d.slice(off+1, uint64(n))
		if err != nil {
			return nil, err
		}
		return UID(beUint64(raw)), nil
	case 0xa: // array
		count, start, err := d.count(off, lo)
		if err != nil {
			return nil, err
		}
		arr := make([]any, count)
		for i := uint64(0); i < count; i++ {
			ref, err := d.ref(start + i*uint64(d.objectRefSize))
			if err != nil {
				return nil, err
			}
			v, err := d.parseObject(ref, depth+1)
			if err != nil {
				return nil, err
			}
			arr[i] = v
		}
		return arr, nil
	case 0xd: // dict
		count, start, err := d.count(off, lo)
		if err != nil {
			return nil, err
		}
		m := make(map[string]any, count)
		for i := uint64(0); i < count; i++ {
			// All key references, then all value references.
			kref, err := d.ref(start + i*uint64(d.objectRefSize))
			if err != nil {
				return nil, err
			}
			vref, err := d.ref(start + (count+i)*uint64(d.objectRefSize))
			if err != nil {
				return nil, err
			}
			kv, err := d.parseObject(kref, depth+1)
			if err != nil {
				return nil, err
			}
			key, ok := kv.(string)
			if !ok {
				return nil, errors.New("plist: dict key is not a string")
			}
			vv, err := d.parseObject(vref, depth+1)
			if err != nil {
				return nil, err
			}
			m[key] = vv
		}
		return m, nil
	}
	return nil, fmt.Errorf("plist: unsupported marker %#x", marker)
}

// count resolves the element count for a container/scalar marker and returns
// the offset of the payload.
func (d *decoder) count(off uint64, lo byte) (uint64, uint64, error) {
	if lo != 0x0f {
		return uint64(lo), off + 1, nil
	}
	if off+2 > uint64(len(d.data)) {
		return 0, 0, errors.New("plist: truncated extended count")
	}
	m := d.data[off+1]
	if m>>4 != 0x1 {
		return 0, 0, errors.New("plist: extended count is not an integer")
	}
	n := 1 << (m & 0x0f)
	start := off + 2
	raw, err := d.slice(start, uint64(n))
	if err != nil {
		return 0, 0, err
	}
	return beUint64(raw), start + uint64(n), nil
}

func (d *decoder) ref(off uint64) (uint64, error) {
	raw, err := d.slice(off, uint64(d.objectRefSize))
	if err != nil {
		return 0, err
	}
	return beUint64(raw), nil
}

func (d *decoder) slice(off, n uint64) ([]byte, error) {
	if off > uint64(len(d.data)) || n > uint64(len(d.data))-off {
		return nil, errors.New("plist: truncated object")
	}
	return d.data[off : off+n], nil
}

// ---------------------------------------------------------------------------
// XML plist decoder
// ---------------------------------------------------------------------------

func decodeXML(data []byte) (any, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil, errors.New("plist: missing plist element")
		}
		if err != nil {
			return nil, fmt.Errorf("plist: xml: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok {
			if se.Name.Local != "plist" {
				return nil, fmt.Errorf("plist: unexpected root element <%s>", se.Name.Local)
			}
			return parseXMLValue(dec)
		}
	}
}

func parseXMLValue(dec *xml.Decoder) (any, error) {
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("plist: xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			return parseXMLElement(dec, t)
		case xml.CharData:
			// skip whitespace between elements
		case xml.EndElement:
			return nil, fmt.Errorf("plist: unexpected </%s>", t.Name.Local)
		}
	}
}

func parseXMLElement(dec *xml.Decoder, se xml.StartElement) (any, error) {
	switch se.Name.Local {
	case "dict":
		return parseXMLDict(dec)
	case "array":
		return parseXMLArray(dec)
	case "string":
		return readText(dec, "string")
	case "integer":
		s, err := readText(dec, "integer")
		if err != nil {
			return nil, err
		}
		s = strings.TrimSpace(s)
		if v, err := strconv.ParseInt(s, 0, 64); err == nil {
			return v, nil
		}
		v, err := strconv.ParseUint(s, 0, 64)
		if err != nil {
			return nil, fmt.Errorf("plist: bad integer %q", s)
		}
		return v, nil
	case "real":
		s, err := readText(dec, "real")
		if err != nil {
			return nil, err
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return nil, fmt.Errorf("plist: bad real %q", s)
		}
		return v, nil
	case "true":
		if _, err := readText(dec, "true"); err != nil {
			return nil, err
		}
		return true, nil
	case "false":
		if _, err := readText(dec, "false"); err != nil {
			return nil, err
		}
		return false, nil
	case "data":
		s, err := readText(dec, "data")
		if err != nil {
			return nil, err
		}
		raw, err := base64.StdEncoding.DecodeString(stripSpace(s))
		if err != nil {
			return nil, fmt.Errorf("plist: bad base64 data: %w", err)
		}
		return raw, nil
	case "date":
		s, err := readText(dec, "date")
		if err != nil {
			return nil, err
		}
		s = strings.TrimSpace(s)
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil, fmt.Errorf("plist: bad date %q", s)
		}
		return t.UTC(), nil
	}
	return nil, fmt.Errorf("plist: unsupported element <%s>", se.Name.Local)
}

func parseXMLDict(dec *xml.Decoder) (any, error) {
	m := map[string]any{}
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("plist: xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.CharData:
		case xml.EndElement:
			if t.Name.Local != "dict" {
				return nil, fmt.Errorf("plist: unexpected </%s> in dict", t.Name.Local)
			}
			return m, nil
		case xml.StartElement:
			if t.Name.Local != "key" {
				return nil, fmt.Errorf("plist: expected <key>, got <%s>", t.Name.Local)
			}
			key, err := readText(dec, "key")
			if err != nil {
				return nil, err
			}
			val, err := parseXMLValue(dec)
			if err != nil {
				return nil, err
			}
			m[key] = val
		}
	}
}

func parseXMLArray(dec *xml.Decoder) (any, error) {
	arr := []any{}
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("plist: xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.CharData:
		case xml.EndElement:
			if t.Name.Local != "array" {
				return nil, fmt.Errorf("plist: unexpected </%s> in array", t.Name.Local)
			}
			return arr, nil
		case xml.StartElement:
			v, err := parseXMLElement(dec, t)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
	}
}

func readText(dec *xml.Decoder, name string) (string, error) {
	var sb strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", fmt.Errorf("plist: xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.CharData:
			sb.Write(t)
		case xml.EndElement:
			if t.Name.Local != name {
				return "", fmt.Errorf("plist: unexpected </%s> in <%s>", t.Name.Local, name)
			}
			return sb.String(), nil
		case xml.StartElement:
			return "", fmt.Errorf("plist: unexpected <%s> in <%s>", t.Name.Local, name)
		}
	}
}

func stripSpace(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r':
			return -1
		}
		return r
	}, s)
}

// ---------------------------------------------------------------------------
// Binary plist encoder
// ---------------------------------------------------------------------------

type encObj struct {
	head []byte
	refs []int
}

type encoder struct {
	objs []encObj
}

func (e *encoder) addScalar(b []byte) int {
	e.objs = append(e.objs, encObj{head: b})
	return len(e.objs) - 1
}

func (e *encoder) addRefs(head []byte, refs []int) int {
	e.objs = append(e.objs, encObj{head: head, refs: refs})
	return len(e.objs) - 1
}

// EncodeBinary encodes v as a binary plist.
func EncodeBinary(v any) ([]byte, error) {
	e := &encoder{}
	top, err := e.addValue(v)
	if err != nil {
		return nil, err
	}
	return e.finalize(top)
}

func (e *encoder) addValue(v any) (int, error) {
	switch x := v.(type) {
	case nil:
		return e.addScalar([]byte{0x00}), nil
	case bool:
		if x {
			return e.addScalar([]byte{0x09}), nil
		}
		return e.addScalar([]byte{0x08}), nil
	case int:
		return e.addScalar(markerAndInt(big.NewInt(int64(x)))), nil
	case int8:
		return e.addScalar(markerAndInt(big.NewInt(int64(x)))), nil
	case int64:
		return e.addScalar(markerAndInt(big.NewInt(x))), nil
	case uint64:
		return e.addScalar(markerAndUint(x)), nil
	case UID:
		return e.addScalar(encodeUID(uint64(x))), nil
	case float64:
		return e.addScalar(markerAndReal(x)), nil
	case float32:
		return e.addScalar(markerAndReal(float64(x))), nil
	case string:
		return e.addScalar(encodeString(x)), nil
	case []byte:
		return e.addScalar(encodeData(x)), nil
	case time.Time:
		return e.addScalar(encodeDate(x)), nil
	case []any:
		refs := make([]int, len(x))
		for i, el := range x {
			r, err := e.addValue(el)
			if err != nil {
				return 0, err
			}
			refs[i] = r
		}
		return e.addRefs(countMarker(0xa, len(x)), refs), nil
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		// A dictionary stores every key reference first, then every value
		// reference (binary plist spec; Apple's and plistlib's parsers read the
		// two runs consecutively). Interleaving them makes a multi-key document
		// invalid.
		keyRefs := make([]int, 0, len(keys))
		valueRefs := make([]int, 0, len(keys))
		for _, k := range keys {
			kr, err := e.addValue(k)
			if err != nil {
				return 0, err
			}
			vr, err := e.addValue(x[k])
			if err != nil {
				return 0, err
			}
			keyRefs = append(keyRefs, kr)
			valueRefs = append(valueRefs, vr)
		}
		refs := append(keyRefs, valueRefs...)
		return e.addRefs(countMarker(0xd, len(keys)), refs), nil
	}
	return 0, fmt.Errorf("plist: cannot encode %T", v)
}

func bytesNeeded(x uint64) int {
	if x == 0 {
		return 1
	}
	n := 0
	for x > 0 {
		n++
		x >>= 8
	}
	return n
}

func putBE(x uint64, n int) []byte {
	b := make([]byte, n)
	for i := n - 1; i >= 0; i-- {
		b[i] = byte(x)
		x >>= 8
	}
	return b
}

// markerAndUint encodes a non-negative Go uint64. Values above MaxInt64 use
// the optional 16-byte unsigned representation so they do not alias a negative
// signed 8-byte integer.
func markerAndUint(x uint64) []byte {
	if x <= math.MaxInt64 {
		return markerAndInt(big.NewInt(int64(x)))
	}
	buf := make([]byte, 16)
	binary.BigEndian.PutUint64(buf[8:], x)
	return append([]byte{0x14}, buf...)
}

// markerAndInt encodes an integer in binary-plist format 00: positive values
// use the shortest unsigned width, while every negative value that fits int64
// is emitted as an 8-byte two's-complement integer (CFBinaryPList.c:1093-1109).
func markerAndInt(v *big.Int) []byte {
	if v.Sign() < 0 {
		if v.IsInt64() {
			return append([]byte{0x13}, putBE(uint64(v.Int64()), 8)...)
		}
	} else if v.IsUint64() {
		x := v.Uint64()
		switch {
		case x <= 0xff:
			return append([]byte{0x10}, byte(x))
		case x <= 0xffff:
			return append([]byte{0x11}, putBE(x, 2)...)
		case x <= 0xffffffff:
			return append([]byte{0x12}, putBE(x, 4)...)
		default:
			return append([]byte{0x13}, putBE(x, 8)...)
		}
	}
	// Values outside the signed/unsigned 64-bit range use the optional 16-byte
	// representation, as in the reference encoder.
	b := v.Bytes()
	buf := make([]byte, 16)
	if v.Sign() < 0 {
		for i := range buf {
			buf[i] = 0xff
		}
	}
	copy(buf[16-len(b):], b)
	return append([]byte{0x14}, buf...)
}

func encodeUID(v uint64) []byte {
	n := bytesNeeded(v)
	if n == 0 {
		n = 1
	}
	return append([]byte{0x80 | byte(n-1)}, putBE(v, n)...)
}

func markerAndReal(v float64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], math.Float64bits(v))
	return append([]byte{0x23}, b[:]...)
}

func encodeDate(t time.Time) []byte {
	secs := float64(t.UnixNano())/float64(time.Second) - float64(appleEpoch)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], math.Float64bits(secs))
	return append([]byte{0x33}, b[:]...)
}

func encodeData(d []byte) []byte {
	return append(countMarker(0x4, len(d)), d...)
}

func encodeString(s string) []byte {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return append(countMarker(0x5, len(s)), s...)
	}
	u := utf16.Encode([]rune(s))
	body := make([]byte, 0, len(u)*2)
	for _, c := range u {
		body = append(body, byte(c>>8), byte(c))
	}
	return append(countMarker(0x6, len(u)), body...)
}

// countMarker builds the object marker for a container/scalar whose payload
// length is count, using an inline count when it fits.
func countMarker(hi byte, count int) []byte {
	if count < 15 {
		return []byte{hi<<4 | byte(count)}
	}
	return append([]byte{hi<<4 | 0x0f}, markerAndInt(big.NewInt(int64(count)))...)
}

func (e *encoder) finalize(top int) ([]byte, error) {
	n := len(e.objs)
	if n == 0 {
		return nil, errors.New("plist: no objects")
	}
	refSize := bytesNeeded(uint64(n - 1))

	out := make([]byte, 0, 64)
	out = append(out, "bplist00"...)
	offsets := make([]uint64, n)
	for i, o := range e.objs {
		offsets[i] = uint64(len(out))
		out = append(out, o.head...)
		for _, r := range o.refs {
			out = append(out, putBE(uint64(r), refSize)...)
		}
	}
	otOff := uint64(len(out))
	offSize := bytesNeeded(otOff)
	for _, off := range offsets {
		out = append(out, putBE(off, offSize)...)
	}
	out = append(out, 0, 0, 0, 0, 0, 0) // trailer padding
	out = append(out, byte(offSize), byte(refSize))
	out = append(out, putBE(uint64(n), 8)...)
	out = append(out, putBE(uint64(top), 8)...)
	out = append(out, putBE(otOff, 8)...)
	return out, nil
}
