// Package dmap parses the DMAP metadata blobs carried by AirPlay
// SET_PARAMETER requests.
//
// A blob starts with an 8-byte item header: a 4-byte ASCII tag (normally
// "mlit") followed by a big-endian uint32 payload length. The payload is a
// sequence of items in the same format. Container tags are descended into;
// string tags populate Metadata; all other tags are skipped.
package dmap

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// maxDepth bounds container nesting so that a hostile blob cannot exhaust the
// stack.
const maxDepth = 16

// Metadata is the subset of DMAP metadata a receiver displays.
type Metadata struct {
	Title    string
	Artist   string
	Album    string
	Composer string
	Genre    string
}

var errTruncated = errors.New("dmap: truncated data")

// Parse decodes a DMAP metadata blob. The outermost tag must be "mlit".
// Nested containers such as mhit and mlog are descended into. The first
// non-empty value of each string tag wins, so a nested track listing cannot
// overwrite the top-level item metadata that accompanies it.
func Parse(b []byte) (*Metadata, error) {
	if len(b) < 8 {
		return nil, fmt.Errorf("dmap: blob of %d bytes is shorter than an item header: %w", len(b), errTruncated)
	}
	if tag := string(b[:4]); tag != "mlit" {
		return nil, fmt.Errorf("dmap: expected mlit container, got %q", tag)
	}
	size := binary.BigEndian.Uint32(b[4:8])
	payload := b[8:]
	if uint64(size) > uint64(len(payload)) {
		return nil, fmt.Errorf("dmap: mlit declares %d payload bytes, have %d: %w", size, len(payload), errTruncated)
	}
	m := &Metadata{}
	if err := m.parseItems(payload[:size], 0); err != nil {
		return nil, err
	}
	return m, nil
}

// parseItems walks the item sequence in b.
func (m *Metadata) parseItems(b []byte, depth int) error {
	if depth > maxDepth {
		return fmt.Errorf("dmap: container nesting deeper than %d", maxDepth)
	}
	for len(b) > 0 {
		if len(b) < 8 {
			return fmt.Errorf("dmap: %d trailing bytes cannot hold an item header: %w", len(b), errTruncated)
		}
		tag := string(b[:4])
		size := binary.BigEndian.Uint32(b[4:8])
		rest := b[8:]
		if uint64(size) > uint64(len(rest)) {
			return fmt.Errorf("dmap: item %q declares %d bytes, have %d: %w", tag, size, len(rest), errTruncated)
		}
		payload := rest[:size]
		b = rest[size:]

		switch tag {
		case "minm":
			setString(&m.Title, payload)
		case "asar":
			setString(&m.Artist, payload)
		case "asal":
			setString(&m.Album, payload)
		case "ascp":
			setString(&m.Composer, payload)
		case "asgn":
			setString(&m.Genre, payload)
		case "mlit", "mhit", "mlog", "mlcl", "msrv", "mdcl":
			if err := m.parseItems(payload, depth+1); err != nil {
				return err
			}
		default:
			// Unknown tags, including unknown containers, are skipped.
		}
	}
	return nil
}

// setString stores the first non-empty value seen for a field. Trailing NUL
// padding that some senders append to DMAP strings is discarded.
func setString(dst *string, payload []byte) {
	if *dst != "" {
		return
	}
	value := strings.TrimRight(string(payload), "\x00")
	if value != "" {
		*dst = value
	}
}

// ParseProgress parses the body of a `progress` text parameter,
// "start/current/end" in RTP timestamp milliseconds. It reports ok=false when
// the text is not exactly three unsigned 32-bit integers.
func ParseProgress(text string) (startMS, currentMS, endMS uint32, ok bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, 0, 0, false
	}
	parts := strings.Split(text, "/")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var values [3]uint32
	for i, part := range parts {
		n, err := strconv.ParseUint(strings.TrimSpace(part), 10, 32)
		if err != nil {
			return 0, 0, 0, false
		}
		values[i] = uint32(n)
	}
	return values[0], values[1], values[2], true
}
