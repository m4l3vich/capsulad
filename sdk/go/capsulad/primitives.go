package capsulad

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Base64 flags. OR Base64NoPadding with an alphabet; Base64Standard is padded.
const (
	Base64Standard  int32 = 0
	Base64URLSafe   int32 = 1
	Base64NoPadding int32 = 2
)

// Base64Encode encodes data with the selected alphabet and padding.
func Base64Encode(data []byte, flags int32) ([]byte, error) {
	if err := ensureLibrary(FromEnv().Library); err != nil {
		return nil, err
	}
	out, status, detail := base64EncodeNative(data, flags)
	if status != statusOK {
		return nil, nativeStatusError("base64 encode", status, detail)
	}
	return out, nil
}

// Base64Decode decodes strictly: non-canonical padding and alphabet are
// rejected.
func Base64Decode(data []byte, flags int32) ([]byte, error) {
	if err := ensureLibrary(FromEnv().Library); err != nil {
		return nil, err
	}
	out, status, detail := base64DecodeNative(data, flags)
	if status != statusOK {
		return nil, nativeStatusError("base64 decode", status, detail)
	}
	return out, nil
}

// PercentEncode applies RFC 3986 percent-encoding to the unreserved set.
func PercentEncode(data []byte) ([]byte, error) {
	if err := ensureLibrary(FromEnv().Library); err != nil {
		return nil, err
	}
	out, status, detail := percentEncodeNative(data)
	if status != statusOK {
		return nil, nativeStatusError("percent encode", status, detail)
	}
	return out, nil
}

// PercentDecode reverses percent-encoding; malformed escapes are rejected.
func PercentDecode(data []byte) ([]byte, error) {
	if err := ensureLibrary(FromEnv().Library); err != nil {
		return nil, err
	}
	out, status, detail := percentDecodeNative(data)
	if status != statusOK {
		return nil, nativeStatusError("percent decode", status, detail)
	}
	return out, nil
}

// SHA256 returns the 32-byte SHA-256 digest of data.
func SHA256(data []byte) ([]byte, error) {
	if err := ensureLibrary(FromEnv().Library); err != nil {
		return nil, err
	}
	out, status, detail := sha256Native(data)
	if status != statusOK {
		return nil, nativeStatusError("sha256", status, detail)
	}
	return out, nil
}

// RandomBytes returns n bytes from the platform random source.
func RandomBytes(n int) ([]byte, error) {
	if err := ensureLibrary(FromEnv().Library); err != nil {
		return nil, err
	}
	if n < 0 {
		return nil, errors.New("capsulad: RandomBytes length must not be negative")
	}
	out := make([]byte, n)
	if n == 0 {
		return out, nil
	}
	status, detail := randomBytesNative(out)
	if status != statusOK {
		return nil, nativeStatusError("random bytes", status, detail)
	}
	return out, nil
}

// UUIDv4 returns a lowercase RFC 4122 version 4 UUID.
func UUIDv4() (string, error) {
	if err := ensureLibrary(FromEnv().Library); err != nil {
		return "", err
	}
	out, status, detail := uuidV4Native()
	if status != statusOK {
		return "", nativeStatusError("uuid v4", status, detail)
	}
	return string(out), nil
}

// URL is one parsed absolute http(s) URL. Port is the effective TCP port
// (explicit, or 80/443).
type URL struct {
	Scheme string `json:"scheme"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Path   string `json:"path"`
	Query  string `json:"query"`
}

// ParseURL parses an absolute http(s) URL without userinfo or fragment.
func ParseURL(raw string) (URL, error) {
	if err := ensureLibrary(FromEnv().Library); err != nil {
		return URL{}, err
	}
	out, status, detail := urlParseNative(raw)
	if status != statusOK {
		return URL{}, nativeStatusError("url parse", status, detail)
	}
	var parsed URL
	if err := json.Unmarshal(out, &parsed); err != nil {
		return URL{}, fmt.Errorf("capsulad: decode parsed URL: %w", err)
	}
	return parsed, nil
}
