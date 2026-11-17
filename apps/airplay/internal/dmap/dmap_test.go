package dmap

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// item builds one DMAP item: tag, big-endian length, payload.
func item(tag string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	copy(b[:4], tag)
	binary.BigEndian.PutUint32(b[4:8], uint32(len(payload)))
	copy(b[8:], payload)
	return b
}

// container builds a DMAP item whose payload is the concatenated children.
func container(tag string, children ...[]byte) []byte {
	return item(tag, bytes.Join(children, nil))
}

func TestParseTopLevelWithNestedAndUnknownTags(t *testing.T) {
	blob := container("mlit",
		item("minm", []byte("Title")),
		item("asar", []byte("Artist")),
		item("asal", []byte("Album")),
		item("ascp", []byte("Composer")),
		item("asgn", []byte("Genre")),
		// A nested track listing must not overwrite top-level item metadata.
		container("mhit",
			item("minm", []byte("Nested Title")),
			item("asar", []byte("Nested Artist")),
		),
		item("zzzz", []byte("ignored")),
	)

	got, err := Parse(blob)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := &Metadata{
		Title:    "Title",
		Artist:   "Artist",
		Album:    "Album",
		Composer: "Composer",
		Genre:    "Genre",
	}
	if *got != *want {
		t.Fatalf("metadata = %+v, want %+v", *got, *want)
	}
}

func TestParseDescendsIntoNestedPlaylist(t *testing.T) {
	blob := container("mlit",
		container("mlog",
			container("mhit",
				item("minm", []byte("Track\x00\x00")),
				item("asar", []byte("Nested Artist")),
				item("asal", []byte("Nested Album")),
			),
		),
	)

	got, err := Parse(blob)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := &Metadata{
		Title:  "Track",
		Artist: "Nested Artist",
		Album:  "Nested Album",
	}
	if *got != *want {
		t.Fatalf("metadata = %+v, want %+v", *got, *want)
	}
}

func TestParseTruncatedInputErrors(t *testing.T) {
	valid := container("mlit",
		item("minm", []byte("Title")),
		item("asar", []byte("Artist")),
	)

	t.Run("missing payload byte", func(t *testing.T) {
		if _, err := Parse(valid[:len(valid)-1]); err == nil {
			t.Fatal("truncated blob accepted, want error")
		}
	})

	t.Run("short header", func(t *testing.T) {
		if _, err := Parse([]byte("mli")); err == nil {
			t.Fatal("short blob accepted, want error")
		}
	})

	t.Run("item overruns container", func(t *testing.T) {
		// mlit payload declares 8 bytes but the item claims 100.
		overrun := make([]byte, 16+8)
		copy(overrun, "mlit")
		binary.BigEndian.PutUint32(overrun[4:8], 16)
		copy(overrun[8:], "minm")
		binary.BigEndian.PutUint32(overrun[12:16], 100)
		if _, err := Parse(overrun); err == nil {
			t.Fatal("overrunning item accepted, want error")
		}
	})

	t.Run("wrong container tag", func(t *testing.T) {
		if _, err := Parse(item("abcd", nil)); err == nil {
			t.Fatal("non-mlit blob accepted, want error")
		}
	})
}

func TestParseProgress(t *testing.T) {
	start, current, end, ok := ParseProgress("0/1200/3000")
	if !ok || start != 0 || current != 1200 || end != 3000 {
		t.Fatalf("ParseProgress = %d/%d/%d ok=%v", start, current, end, ok)
	}

	start, current, end, ok = ParseProgress(" 10 / 20 / 30 ")
	if !ok || start != 10 || current != 20 || end != 30 {
		t.Fatalf("ParseProgress with spaces = %d/%d/%d ok=%v", start, current, end, ok)
	}

	for _, text := range []string{"", "1/2", "1/2/3/4", "a/b/c", "1//3", "-1/2/3"} {
		if _, _, _, ok := ParseProgress(text); ok {
			t.Fatalf("ParseProgress(%q) reported ok", text)
		}
	}
}
