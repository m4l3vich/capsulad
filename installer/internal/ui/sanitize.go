package ui

import (
	"fmt"
	"strings"
)

// cleanText removes C0/C1 control characters except newline and tab. It is for
// multi-line terminal text such as logs.
func cleanText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			// drop
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// cleanLine removes C0/C1 controls and folds newlines/tabs to spaces.
func cleanLine(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteByte(' ')
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			// drop
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escapeControls renders control characters visibly (for a password that may
// contain pasted bytes), so no escape sequence reaches the terminal.
func escapeControls(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20, r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r >= 0x80 && r <= 0x9f:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return true
		}
	}
	return false
}
