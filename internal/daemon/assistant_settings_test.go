package daemon

import "testing"

func TestParseAssistantDelay(t *testing.T) {
	valid := []struct {
		raw      string
		min, max int64
	}{
		{"", 0, 0},
		{"   ", 0, 0},
		{"100", 100, 100},
		{"0", 0, 0},
		{"60000", 60000, 60000},
		{"200-1500", 200, 1500},
		{"  200 - 1500  ", 200, 1500},
		{"1500-1500", 1500, 1500},
	}
	for _, tc := range valid {
		min, max, err := parseAssistantDelay(tc.raw)
		if err != nil || min != tc.min || max != tc.max {
			t.Fatalf("parseAssistantDelay(%q) = %d,%d,%v; want %d,%d", tc.raw, min, max, err, tc.min, tc.max)
		}
	}
	for _, raw := range []string{
		"-1", "+100", "1.5", "1e3", "0x10", "60 000", "100-", "-100-200",
		"200-100", "60001", "1-2-3", "10-60001", "abc",
	} {
		if _, _, err := parseAssistantDelay(raw); err == nil {
			t.Fatalf("parseAssistantDelay(%q) was accepted", raw)
		}
	}
}
