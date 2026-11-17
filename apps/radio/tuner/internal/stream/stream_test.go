package stream

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
)

func TestChunkedReader(t *testing.T) {
	raw := "5\r\nhello\r\n6\r\n world\r\n0\r\nX-Trailer: value\r\n\r\n"
	got, err := io.ReadAll(newChunkedReader(bufio.NewReader(strings.NewReader(raw))))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello world" {
		t.Fatalf("body = %q", got)
	}
}

func TestChunkedReaderRejectsOversize(t *testing.T) {
	raw := "500000\r\n"
	_, err := io.ReadAll(newChunkedReader(bufio.NewReader(strings.NewReader(raw))))
	if err == nil {
		t.Fatal("oversize chunk accepted")
	}
}

func TestICYReaderStripsMetadata(t *testing.T) {
	meta := "StreamTitle='Artist - Song';"
	block := make([]byte, 32)
	copy(block, meta)
	raw := []byte("abcd")
	raw = append(raw, byte(len(block)/16))
	raw = append(raw, block...)
	raw = append(raw, []byte("efgh")...)

	r := newICYReader(bytes.NewReader(raw), 4)
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "abcdefgh" {
		t.Fatalf("audio = %q", got)
	}
	artist, title := r.metadata()
	if artist != "Artist" || title != "Song" {
		t.Fatalf("metadata = %q / %q", artist, title)
	}
}

func TestParseICYMetadataVariants(t *testing.T) {
	cases := []struct {
		block, artist, title string
	}{
		{"StreamTitle='Artist - Song';StreamUrl='x';", "Artist", "Song"},
		{"StreamTitle='Only a title';", "", "Only a title"},
		{"StreamTitle='';", "", ""},
		{"StreamTitle=\"Artist - Song\";", "Artist", "Song"},
		{"no metadata here", "", ""},
	}
	for _, c := range cases {
		artist, title := parseICYMetadata([]byte(c.block))
		if artist != c.artist || title != c.title {
			t.Fatalf("%q -> %q/%q, want %q/%q", c.block, artist, title, c.artist, c.title)
		}
	}
}

func TestReadResponseHead(t *testing.T) {
	raw := "ICY 200 OK\r\nicy-metaint: 8192\r\ncontent-type: audio/mpeg\r\n\r\n"
	status, _, headers, err := readResponseHead(bufio.NewReader(strings.NewReader(raw)), 64<<10)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if status != 200 || headers["icy-metaint"] != "8192" || headers["content-type"] != "audio/mpeg" {
		t.Fatalf("status=%d headers=%v", status, headers)
	}
}

func TestReadResponseHeadRejectsMalformed(t *testing.T) {
	raw := "HTTP/1.1 200 OK\r\nbad header line\r\n\r\n"
	if _, _, _, err := readResponseHead(bufio.NewReader(strings.NewReader(raw)), 4096); err == nil {
		t.Fatal("malformed header accepted")
	}
}

func TestParsePlaylistM3U(t *testing.T) {
	base, _ := url.Parse("https://example.test/listen.m3u")
	target, err := parsePlaylist([]byte("#EXTM3U\n#EXTINF:-1,Radio\n/live.mp3\n"), base)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if target.String() != "https://example.test/live.mp3" {
		t.Fatalf("target = %s", target)
	}
}

func TestParsePlaylistPLS(t *testing.T) {
	base, _ := url.Parse("https://example.test/listen.pls")
	target, err := parsePlaylist([]byte("[playlist]\nFile2=https://other.test/b.mp3\nFile1=https://other.test/a.mp3\n"), base)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if target.String() != "https://other.test/a.mp3" {
		t.Fatalf("target = %s", target)
	}
}

func TestParsePlaylistHLS(t *testing.T) {
	base, _ := url.Parse("https://example.test/listen.m3u")
	_, err := parsePlaylist([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:6\nseg1.aac\n"), base)
	if !errors.Is(err, ErrHLS) {
		t.Fatalf("err = %v, want ErrHLS", err)
	}
}

func TestGuardLeadingID3SkipsSmallTag(t *testing.T) {
	body := []byte("ID3\x03\x00\x00\x00\x00\x00\x04TAGD")
	body = append(body, []byte("FRAME")...)
	s := &Stream{body: bytes.NewReader(body)}
	if err := s.guardLeadingID3(); err != nil {
		t.Fatalf("guard: %v", err)
	}
	got, err := io.ReadAll(s.body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "FRAME" {
		t.Fatalf("body = %q, want FRAME", got)
	}
}

func TestGuardLeadingID3RejectsLargeTag(t *testing.T) {
	// Syncsafe 0x7F7F7F7F is 256 MiB; go-mp3 would allocate it directly.
	body := []byte("ID3\x03\x00\x00\x7f\x7f\x7f\x7f")
	s := &Stream{body: bytes.NewReader(body)}
	if err := s.guardLeadingID3(); err == nil {
		t.Fatal("256 MiB ID3v2 tag accepted")
	}
}

func TestGuardLeadingID3LeavesPlainAudio(t *testing.T) {
	body := []byte{0xFF, 0xFB, 0x90, 0x00, 'a', 'b', 'c', 'd'}
	s := &Stream{body: bytes.NewReader(body)}
	if err := s.guardLeadingID3(); err != nil {
		t.Fatalf("guard: %v", err)
	}
	got, err := io.ReadAll(s.body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("body = %v, want %v", got, body)
	}
}

func TestIsPlaylistResponse(t *testing.T) {
	cases := []struct {
		contentType, urlPath string
		want                 bool
	}{
		{"audio/x-scpls", "/listen", true},
		{"application/vnd.apple.mpegurl", "/listen", true},
		{"audio/mpeg", "/radio.m3u", false},
		{"audio/mpeg", "/radio.pls", false},
		{"", "/radio.pls", true},
		{"application/octet-stream", "/radio.m3u", true},
		{"text/plain", "/radio.pls", true},
		{"audio/mpeg", "/live.mp3", false},
	}
	for _, c := range cases {
		if got := isPlaylistResponse(c.contentType, c.urlPath); got != c.want {
			t.Fatalf("isPlaylistResponse(%q, %q) = %v, want %v", c.contentType, c.urlPath, got, c.want)
		}
	}
}

func TestBuildBodyRejectsChunkedWithAnotherEncoding(t *testing.T) {
	_, _, err := buildBody(bufio.NewReader(strings.NewReader("")), map[string]string{
		"transfer-encoding": "gzip, chunked",
	})
	if err == nil {
		t.Fatal("gzip, chunked accepted")
	}
	_, _, err = buildBody(bufio.NewReader(strings.NewReader("")), map[string]string{
		"transfer-encoding": "chunked",
	})
	if err != nil {
		t.Fatalf("plain chunked rejected: %v", err)
	}
}

func TestBuildRequestHeaders(t *testing.T) {
	u, err := url.Parse("https://user:secret@example.test/live.mp3?token=abc")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	request := string(buildRequest(Config{UserAgent: "capsulad-radio-tuner/1.0"}, u))
	for _, want := range []string{
		"GET /live.mp3?token=abc HTTP/1.1\r\n",
		"Host: example.test\r\n",
		"Accept-Encoding: identity\r\n",
		"Icy-MetaData: 1\r\n",
		"Connection: close\r\n",
	} {
		if !strings.Contains(request, want) {
			t.Fatalf("request missing %q:\n%s", want, request)
		}
	}
	if strings.Contains(request, "secret") {
		t.Fatalf("credentials leaked into the request:\n%s", request)
	}
	if !strings.HasSuffix(request, "\r\n\r\n") {
		t.Fatalf("request does not end with a blank line:\n%q", request)
	}
}

func TestCodecFromMagic(t *testing.T) {
	cases := []struct {
		head []byte
		want Codec
	}{
		{[]byte("ID3\x04"), CodecMP3},
		{[]byte{0xFF, 0xFB, 0x90, 0x00}, CodecMP3},
		{[]byte{0xFF, 0xF1, 0x50, 0x80}, CodecAAC},
		{[]byte("OggS"), CodecOgg},
		{[]byte("fLaC"), CodecFLAC},
		{[]byte{0, 0, 0, 0, 'f', 't', 'y', 'p'}, CodecMP4},
		{[]byte("RIFF"), CodecWAV},
		{[]byte("nope"), CodecUnknown},
	}
	for _, c := range cases {
		if got := codecFromMagic(c.head); got != c.want {
			t.Fatalf("codecFromMagic(%v) = %v, want %v", c.head, got, c.want)
		}
	}
}

func TestClassifyKeepsPrefix(t *testing.T) {
	body := bytes.NewReader([]byte{0xFF, 0xFB, 0x90, 0x00, 'a', 'b'})
	codec, reader, err := classify("application/octet-stream", body)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if codec != CodecMP3 {
		t.Fatalf("codec = %v", codec)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, []byte{0xFF, 0xFB, 0x90, 0x00, 'a', 'b'}) {
		t.Fatalf("prefix lost: %v", got)
	}
}
