package plist

import (
	"bytes"
	"reflect"
	"testing"
	"time"
)

func TestRoundTripNested(t *testing.T) {
	when := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	doc := map[string]any{
		"name":  "Capsula \u00e9\u4e16\u754c", // non-ASCII -> UTF-16
		"tiny":  int64(42),                    // 1 byte
		"mid":   int64(1000),                  // 2 bytes
		"big":   int64(100000),                // 4 bytes
		"huge":  int64(1) << 40,               // 8 bytes
		"neg":   int64(-5),
		"ratio": 3.5,
		"blob":  []byte{0, 1, 2, 0xfe},
		"ok":    true,
		"no":    false,
		"none":  nil,
		"uid":   UID(9),
		"list":  []any{int64(1), "two", 3.0, []byte{9}},
		"nested": map[string]any{
			"k": "v",
			"n": int64(7),
		},
	}
	enc, err := EncodeBinary(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(enc, []byte("bplist00")) {
		t.Fatal("missing binary plist header")
	}
	got, err := Decode(enc)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, doc) {
		t.Fatalf("round-trip mismatch:\n got %#v\nwant %#v", got, doc)
	}

	// Date round-trips as its own value (compared with Equal to avoid
	// monotonic-clock/location differences).
	denc, err := EncodeBinary(when)
	if err != nil {
		t.Fatal(err)
	}
	dgot, err := Decode(denc)
	if err != nil {
		t.Fatal(err)
	}
	dt, ok := dgot.(time.Time)
	if !ok || !dt.Equal(when) {
		t.Fatalf("date round-trip = %v", dgot)
	}
}

func TestIntWidthsEmitted(t *testing.T) {
	cases := []struct {
		v      int64
		marker byte
	}{
		{42, 0x10},
		{200, 0x10},
		{1000, 0x11},
		{65535, 0x11},
		{65536, 0x12},
		{100000, 0x12},
		{4294967295, 0x12},
		{4294967296, 0x13},
		{int64(1) << 40, 0x13},
		{-1, 0x13},
		{-129, 0x13},
		{-32768, 0x13},
		{-32769, 0x13},
	}
	for _, c := range cases {
		enc, err := EncodeBinary(c.v)
		if err != nil {
			t.Fatal(err)
		}
		if enc[8] != c.marker {
			t.Fatalf("value %d: marker %#x, want %#x", c.v, enc[8], c.marker)
		}
		got, err := Decode(enc)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.v {
			t.Fatalf("value %d decoded as %v", c.v, got)
		}
	}
}

// singleObjectBinary wraps one hand-built object in a complete binary plist.
// The vectors below are independent of our encoder and cover the format rules
// documented in CFBinaryPList.c: compact unsigned values and signed 8-byte
// two's-complement values.
func singleObjectBinary(object []byte) []byte {
	b := append([]byte("bplist00"), object...)
	offsetTableOffset := len(b)
	b = append(b, 8)
	b = append(b, make([]byte, 6)...)
	b = append(b, 1, 1) // offsetIntSize, objectRefSize
	b = append(b, 0, 0, 0, 0, 0, 0, 0, 1)
	b = append(b, 0, 0, 0, 0, 0, 0, 0, 0)
	for i := 7; i >= 0; i-- {
		b = append(b, byte(offsetTableOffset>>uint(8*i)))
	}
	return b
}

func TestDecodeIndependentIntegerVectors(t *testing.T) {
	cases := []struct {
		name   string
		object []byte
		want   int64
	}{
		{"one byte unsigned", []byte{0x10, 0xc8}, 200},
		{"two byte unsigned", []byte{0x11, 0xff, 0xff}, 65535},
		{"four byte unsigned high bit", []byte{0x12, 0xff, 0xff, 0xff, 0xff}, 4294967295},
		{"eight byte signed negative", []byte{0x13, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, -1},
		{"eight byte signed negative five", []byte{0x13, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xfb}, -5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Decode(singleObjectBinary(tc.object))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got != tc.want {
				t.Fatalf("decoded %v, want %d", got, tc.want)
			}
		})
	}
}

func TestUIDAndWideIntegers(t *testing.T) {
	enc, err := EncodeBinary(UID(200))
	if err != nil {
		t.Fatal(err)
	}
	if enc[8] != 0x80 {
		t.Fatalf("UID marker = %#x, want 0x80", enc[8])
	}
	got, err := Decode(enc)
	if err != nil {
		t.Fatal(err)
	}
	if uid, ok := got.(UID); !ok || uid != 200 {
		t.Fatalf("UID round trip = %#v, want UID(200)", got)
	}
	enc, err = EncodeBinary(UID(0x1234))
	if err != nil {
		t.Fatal(err)
	}
	if enc[8] != 0x81 {
		t.Fatalf("two-byte UID marker = %#x, want 0x81", enc[8])
	}
	if got, err = Decode(enc); err != nil {
		t.Fatalf("two-byte UID decode: %v", err)
	}
	if uid, ok := got.(UID); !ok || uid != 0x1234 {
		t.Fatalf("two-byte UID round trip = %#v, want UID(0x1234)", got)
	}
	for _, vector := range []struct {
		object []byte
		want   UID
	}{
		{[]byte{0x80, 0xc8}, 200},
		{[]byte{0x81, 0x12, 0x34}, 0x1234},
	} {
		got, err := Decode(singleObjectBinary(vector.object))
		if err != nil {
			t.Fatalf("independent UID decode: %v", err)
		}
		if uid, ok := got.(UID); !ok || uid != vector.want {
			t.Fatalf("independent UID = %#v, want %#v", got, vector.want)
		}
	}

	// uint64 is an integer, not a UID.
	enc, err = EncodeBinary(uint64(200))
	if err != nil {
		t.Fatal(err)
	}
	if enc[8] != 0x10 {
		t.Fatalf("uint64 integer marker = %#x, want 0x10", enc[8])
	}
	if got, err = Decode(enc); err != nil || got != int64(200) {
		t.Fatalf("uint64 integer round trip = %#v, %v", got, err)
	}

	// A full-width unsigned value uses the 16-byte unsigned representation and
	// survives the round trip without becoming a negative signed 8-byte value.
	enc, err = EncodeBinary(^uint64(0))
	if err != nil {
		t.Fatal(err)
	}
	if enc[8] != 0x14 {
		t.Fatalf("wide unsigned marker = %#x, want 0x14", enc[8])
	}
	if got, err = Decode(enc); err != nil || got != ^uint64(0) {
		t.Fatalf("wide unsigned round trip = %#v, %v", got, err)
	}

	// 2^64+200 is outside the supported signed/unsigned 64-bit range and must
	// not silently truncate to 200.
	wide := make([]byte, 17)
	wide[0] = 0x14
	wide[8] = 1
	wide[16] = 0xc8
	if _, err := Decode(singleObjectBinary(wide)); err == nil {
		t.Fatal("2^64+200 decoded without an out-of-range error")
	}
}

// minimalBinary is a hand-written binary plist for {"a": 1, "b": "hi"}.
func minimalBinary() []byte {
	b := []byte("bplist00")
	b = append(b, 0xd2, 1, 3, 2, 4)        // 0: dict, 2 pairs (keys, then values)
	b = append(b, 0x51, 'a')               // 1: key "a"
	b = append(b, 0x10, 0x01)              // 2: int 1
	b = append(b, 0x51, 'b')               // 3: key "b"
	b = append(b, 0x52, 'h', 'i')          // 4: string "hi"
	b = append(b, 8, 13, 15, 17, 19)       // offset table
	b = append(b, 0, 0, 0, 0, 0, 0)        // trailer padding
	b = append(b, 1, 1)                    // offsetIntSize, objectRefSize
	b = append(b, 0, 0, 0, 0, 0, 0, 0, 5)  // numObjects
	b = append(b, 0, 0, 0, 0, 0, 0, 0, 0)  // topObject
	b = append(b, 0, 0, 0, 0, 0, 0, 0, 22) // offsetTableOffset
	return b
}

const minimalXML = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>a</key>
	<integer>1</integer>
	<key>b</key>
	<string>hi</string>
</dict>
</plist>`

func TestDecodeMinimalFixtures(t *testing.T) {
	want := map[string]any{"a": int64(1), "b": "hi"}

	got, err := Decode(minimalBinary())
	if err != nil {
		t.Fatalf("binary: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("binary decode = %#v", got)
	}

	got, err = Decode([]byte(minimalXML))
	if err != nil {
		t.Fatalf("xml: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("xml decode = %#v", got)
	}
}

func TestDecodeXMLVariants(t *testing.T) {
	xmlDoc := `<?xml version="1.0"?><plist version="1.0"><dict>
		<key>s</key><string>a &amp; b &lt;c&gt;</string>
		<key>i</key><integer>-7</integer>
		<key>r</key><real>2.5</real>
		<key>t</key><true/>
		<key>f</key><false/>
		<key>d</key><data>AQID</data>
		<key>when</key><date>2026-11-17T10:00:00Z</date>
		<key>arr</key><array><integer>1</integer><integer>2</integer></array>
	</dict></plist>`
	got, err := Decode([]byte(xmlDoc))
	if err != nil {
		t.Fatal(err)
	}
	m := got.(map[string]any)
	if m["s"] != "a & b <c>" {
		t.Fatalf("entities not decoded: %q", m["s"])
	}
	if m["i"] != int64(-7) || m["r"] != 2.5 || m["t"] != true || m["f"] != false {
		t.Fatalf("scalar mismatch: %#v", m)
	}
	if !bytes.Equal(m["d"].([]byte), []byte{1, 2, 3}) {
		t.Fatalf("data mismatch: %v", m["d"])
	}
	if tt := m["when"].(time.Time); !tt.Equal(time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("date mismatch: %v", tt)
	}
	if !reflect.DeepEqual(m["arr"], []any{int64(1), int64(2)}) {
		t.Fatalf("array mismatch: %#v", m["arr"])
	}
}

func TestTruncated(t *testing.T) {
	full := minimalBinary()
	if _, err := Decode(full[:20]); err == nil {
		t.Fatal("expected error for truncated binary plist")
	}
	if _, err := Decode([]byte("bplist00")); err == nil {
		t.Fatal("expected error for header-only input")
	}
	// Truncated object body (offset claims bytes past the data end).
	bad := append([]byte(nil), full[:22]...)
	bad[8] = 0x5f // string with extended count but no following int
	if _, err := Decode(bad); err == nil {
		t.Fatal("expected error for malformed object")
	}
}
