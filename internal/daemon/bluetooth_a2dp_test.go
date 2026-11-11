package daemon

import "testing"

func TestSBCFrameSamples(t *testing.T) {
	tests := []struct {
		name   string
		config []byte
		want   int
	}{
		{"negotiated 16 blocks / 8 subbands", []byte{0x21, 0x15, 0x02, 0x35}, 128},
		{"7-byte capabilities", []byte{0xF0, 0x0F, 0x10, 0x04, 0x01, 0x02, 0x35}, 128},
		{"12 blocks / 4 subbands", []byte{0x21, 0x2A, 0x02, 0x35}, 48},
		{"empty defaults to 128", nil, 128},
	}
	for _, tt := range tests {
		if got := sbcFrameSamples(tt.config); got != tt.want {
			t.Errorf("%s: got %d, want %d", tt.name, got, tt.want)
		}
	}
}
