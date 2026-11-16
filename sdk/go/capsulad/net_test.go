package capsulad

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestHTTPOptionsEncode(t *testing.T) {
	options, err := HTTPOptions{
		Roots:           "webpki",
		CAPEM:           []byte("pem"),
		UserAgent:       "agent",
		MaxBody:         1024,
		FollowRedirects: 4,
		ConnectTimeout:  2 * time.Second,
		ReadTimeout:     3 * time.Second,
		TotalTimeout:    4 * time.Second,
	}.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(options, &decoded); err != nil {
		t.Fatalf("json: %v", err)
	}
	for key, want := range map[string]any{
		"roots":              "webpki",
		"ca_pem":             "pem",
		"user_agent":         "agent",
		"max_body":           float64(1024),
		"follow_redirects":   float64(4),
		"connect_timeout_ms": float64(2000),
		"read_timeout_ms":    float64(3000),
		"total_timeout_ms":   float64(4000),
	} {
		if decoded[key] != want {
			t.Errorf("%s = %#v, want %#v", key, decoded[key], want)
		}
	}
	// Sub-millisecond timeouts round up to 1 ms; they must never encode as 0
	// (which selects the native default instead).
	sub, err := HTTPOptions{
		ConnectTimeout: time.Microsecond,
		ReadTimeout:    time.Microsecond,
		TotalTimeout:   time.Microsecond,
	}.encode()
	if err != nil {
		t.Fatalf("encode sub-ms: %v", err)
	}
	var small map[string]any
	if err := json.Unmarshal(sub, &small); err != nil {
		t.Fatalf("json sub-ms: %v", err)
	}
	for _, key := range []string{"connect_timeout_ms", "read_timeout_ms", "total_timeout_ms"} {
		if small[key] != float64(1) {
			t.Errorf("%s = %#v, want 1", key, small[key])
		}
	}
	if _, err := (HTTPOptions{MaxBody: -1}).encode(); err == nil {
		t.Fatal("a negative max_body must be rejected")
	}
	// FollowRedirects mirrors the C ABI: zero is always sent (disables).
	zero, err := HTTPOptions{}.encode()
	if err != nil {
		t.Fatalf("encode zero: %v", err)
	}
	if !strings.Contains(string(zero), `"follow_redirects":0`) {
		t.Fatalf("zero redirects must be explicit: %s", zero)
	}
}

func TestGRPCOptionsEncode(t *testing.T) {
	options, err := GRPCOptions{
		Roots:          "system",
		CAPEM:          []byte("pem"),
		ServerName:     "voice.test",
		ConnectTimeout: time.Second,
	}.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(options, &decoded); err != nil {
		t.Fatalf("json: %v", err)
	}
	if decoded["server_name"] != "voice.test" || decoded["connect_timeout_ms"] != float64(1000) {
		t.Fatalf("options = %s", options)
	}
	if _, err := (GRPCOptions{ConnectTimeout: -time.Second}).encode(); err == nil {
		t.Fatal("a negative timeout must be rejected")
	}
	// Zero is the Go zero value: omit the key so the native 10 s default
	// applies.
	zero, err := GRPCOptions{}.encode()
	if err != nil {
		t.Fatalf("encode zero: %v", err)
	}
	var zeroDecoded map[string]any
	if err := json.Unmarshal(zero, &zeroDecoded); err != nil {
		t.Fatalf("json zero: %v", err)
	}
	if _, ok := zeroDecoded["connect_timeout_ms"]; ok {
		t.Fatalf("zero connect timeout must be omitted: %s", zero)
	}
	// Positive sub-millisecond values round up to 1, never 0.
	sub, err := GRPCOptions{ConnectTimeout: time.Microsecond}.encode()
	if err != nil {
		t.Fatalf("encode sub-ms: %v", err)
	}
	var subDecoded map[string]any
	if err := json.Unmarshal(sub, &subDecoded); err != nil {
		t.Fatalf("json sub-ms: %v", err)
	}
	if subDecoded["connect_timeout_ms"] != float64(1) {
		t.Fatalf("sub-ms connect timeout = %#v, want 1", subDecoded["connect_timeout_ms"])
	}
}

func TestPrimitivesAgainstNativeLibrary(t *testing.T) {
	path := os.Getenv("CAPSULAD_TEST_LIBRARY")
	if path == "" {
		t.Skip("set CAPSULAD_TEST_LIBRARY to the staged libcapsulad artifact")
	}
	if err := ensureLibrary(path); err != nil {
		t.Fatalf("load %s: %v", path, err)
	}

	digest, err := SHA256([]byte("abc"))
	if err != nil {
		t.Fatalf("sha256: %v", err)
	}
	if hex.EncodeToString(digest) != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("sha256 = %x", digest)
	}

	encoded, err := Base64Encode([]byte{0xff, 0x00, 0xfe}, Base64URLSafe|Base64NoPadding)
	if err != nil {
		t.Fatalf("base64 encode: %v", err)
	}
	if string(encoded) != "_wD-" {
		t.Fatalf("base64 = %q", encoded)
	}
	decoded, err := Base64Decode(encoded, Base64URLSafe|Base64NoPadding)
	if err != nil || hex.EncodeToString(decoded) != "ff00fe" {
		t.Fatalf("base64 decode = %x, %v", decoded, err)
	}
	if _, err := Base64Decode([]byte("aGk"), Base64Standard); err == nil {
		t.Fatal("unpadded standard input must be rejected")
	}

	percent, err := PercentEncode([]byte("a b/c"))
	if err != nil || string(percent) != "a%20b%2Fc" {
		t.Fatalf("percent = %q, %v", percent, err)
	}
	if back, err := PercentDecode(percent); err != nil || string(back) != "a b/c" {
		t.Fatalf("percent decode = %q, %v", back, err)
	}
	if _, err := PercentDecode([]byte("%zz")); err == nil {
		t.Fatal("malformed percent escape must be rejected")
	}

	first, err := RandomBytes(16)
	if err != nil || len(first) != 16 {
		t.Fatalf("random = %x, %v", first, err)
	}
	second, err := RandomBytes(16)
	if err != nil || string(first) == string(second) {
		t.Fatal("random bytes must differ")
	}

	uuid, err := UUIDv4()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	if len(uuid) != 36 || uuid[14] != '4' || (uuid[19] != '8' && uuid[19] != '9' &&
		uuid[19] != 'a' && uuid[19] != 'b') {
		t.Fatalf("uuid = %q", uuid)
	}

	parsed, err := ParseURL("https://Example.com/a?b=1")
	if err != nil {
		t.Fatalf("url: %v", err)
	}
	if parsed.Scheme != "https" || parsed.Host != "example.com" || parsed.Port != 443 ||
		parsed.Path != "/a" || parsed.Query != "b=1" {
		t.Fatalf("parsed = %+v", parsed)
	}
	if _, err := ParseURL("ftp://example.com"); err == nil {
		t.Fatal("non-http URL must be rejected")
	}

	client, err := OpenHTTP(Env{Library: path}, HTTPOptions{
		Roots:           "webpki",
		FollowRedirects: 4,
	})
	if err != nil {
		t.Fatalf("open http: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close http: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}
