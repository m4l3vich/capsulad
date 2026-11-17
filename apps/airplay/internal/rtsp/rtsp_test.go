package rtsp

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
)

func readAllRequests(t *testing.T, raw string) []*Request {
	t.Helper()
	reader := bufio.NewReader(strings.NewReader(raw))
	var requests []*Request
	for {
		req, err := ReadRequest(reader)
		if errors.Is(err, io.EOF) {
			return requests
		}
		if err != nil {
			t.Fatalf("ReadRequest: %v", err)
		}
		requests = append(requests, req)
	}
}

func TestReadPipelinedRequests(t *testing.T) {
	raw := "OPTIONS * RTSP/1.0\r\nCSeq: 1\r\n\r\n" +
		"ANNOUNCE rtsp://192.168.1.10:7000/1234 RTSP/1.0\r\n" +
		"CSeq: 2\r\nContent-Type: application/sdp\r\nContent-Length: 5\r\n\r\nhello"

	requests := readAllRequests(t, raw)
	if len(requests) != 2 {
		t.Fatalf("got %d requests, want 2", len(requests))
	}
	if requests[0].Method != "OPTIONS" || requests[0].URI != "*" || requests[0].CSeq() != "1" {
		t.Fatalf("first request = %+v", requests[0])
	}
	if requests[1].Method != "ANNOUNCE" || requests[1].CSeq() != "2" {
		t.Fatalf("second request = %+v", requests[1])
	}
	if string(requests[1].Body) != "hello" {
		t.Fatalf("second body = %q, want %q", requests[1].Body, "hello")
	}
}

func TestReadLFOnlyLineEndings(t *testing.T) {
	raw := "SETUP rtsp://host/1 RTSP/1.0\n" +
		"CSeq: 9\n" +
		"Transport: RTP/AVP/TCP;unicast;interleaved=0-1;mode=record\n" +
		"\n"
	requests := readAllRequests(t, raw)
	if len(requests) != 1 {
		t.Fatalf("got %d requests, want 1", len(requests))
	}
	if got := requests[0].Get("cseq"); got != "9" {
		t.Fatalf("cseq = %q, want 9", got)
	}
	if got := requests[0].Get("TRANSPORT"); got != "RTP/AVP/TCP;unicast;interleaved=0-1;mode=record" {
		t.Fatalf("transport = %q", got)
	}
}

func TestHeaderLookupIsCaseInsensitive(t *testing.T) {
	raw := "SET_PARAMETER rtsp://host/1 RTSP/1.0\r\n" +
		"cseq: 4\r\n" +
		"content-TYPE: text/parameters\r\n" +
		"X-Apple-ProtocolVersion: 1\r\n" +
		"X-APPLE-PROTOCOLVERSION: 2\r\n" +
		"\r\n"
	requests := readAllRequests(t, raw)
	if len(requests) != 1 {
		t.Fatalf("got %d requests, want 1", len(requests))
	}
	req := requests[0]
	if req.CSeq() != "4" {
		t.Fatalf("CSeq = %q", req.CSeq())
	}
	if got := req.Get("Content-Type"); got != "text/parameters" {
		t.Fatalf("Content-Type = %q", got)
	}
	// Get returns the first matching header.
	if got := req.Get("x-apple-protocolversion"); got != "1" {
		t.Fatalf("X-Apple-ProtocolVersion = %q", got)
	}
	if got := req.Get("Missing"); got != "" {
		t.Fatalf("missing header = %q, want empty", got)
	}
}

func TestFoldedHeaderIsRejected(t *testing.T) {
	raw := "SET_PARAMETER rtsp://host/1 RTSP/1.0\r\n" +
		"X-Folded: one\r\n" +
		" two\r\n" +
		"\r\n"
	_, err := ReadRequest(bufio.NewReader(strings.NewReader(raw)))
	if err == nil {
		t.Fatal("folded header accepted, want error")
	}
}

func TestRequestLineMustHaveThreeTokens(t *testing.T) {
	for _, raw := range []string{
		"OPTIONS\r\n\r\n",
		"OPTIONS *\r\n\r\n",
		"OPTIONS * RTSP/1.0 extra\r\n\r\n",
	} {
		if _, err := ReadRequest(bufio.NewReader(strings.NewReader(raw))); err == nil {
			t.Fatalf("request line %q accepted, want error", raw)
		}
	}
}

func TestKeysTransport(t *testing.T) {
	keys := Keys("RTP/AVP/TCP;unicast;interleaved=0-1;mode=record")
	if keys["interleaved"] != "0-1" {
		t.Fatalf("interleaved = %q", keys["interleaved"])
	}
	if keys["mode"] != "record" {
		t.Fatalf("mode = %q", keys["mode"])
	}
	if value, ok := keys["unicast"]; !ok || value != "" {
		t.Fatalf("unicast = %q, present = %v", value, ok)
	}
}

func TestKeysRTPInfo(t *testing.T) {
	keys := Keys("seq=1234;rtptime=5678")
	if keys["seq"] != "1234" || keys["rtptime"] != "5678" {
		t.Fatalf("keys = %v", keys)
	}
	quoted := Keys(`mode="record";latency: 100`)
	if quoted["mode"] != "record" {
		t.Fatalf("quoted mode = %q", quoted["mode"])
	}
	if quoted["latency"] != "100" {
		t.Fatalf("latency = %q", quoted["latency"])
	}
}

func TestMissingCSeq(t *testing.T) {
	raw := "ANNOUNCE rtsp://host/1 RTSP/1.0\r\nContent-Type: application/sdp\r\n\r\n"
	req, err := ReadRequest(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if req.CSeq() != "" {
		t.Fatalf("CSeq = %q, want empty", req.CSeq())
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	raw := "ANNOUNCE rtsp://host/1 RTSP/1.0\r\n" +
		"CSeq: 1\r\n" +
		"Content-Length: " + strconv.Itoa(maxBody+1) + "\r\n" +
		"\r\n"
	if _, err := ReadRequest(bufio.NewReader(strings.NewReader(raw))); err == nil {
		t.Fatal("oversized body accepted, want error")
	}
}

func TestInvalidContentLengthIsRejected(t *testing.T) {
	for _, value := range []string{"-1", "abc", "1.5"} {
		raw := "ANNOUNCE rtsp://host/1 RTSP/1.0\r\nCSeq: 1\r\nContent-Length: " + value + "\r\n\r\n"
		if _, err := ReadRequest(bufio.NewReader(strings.NewReader(raw))); err == nil {
			t.Fatalf("Content-Length %q accepted, want error", value)
		}
	}
}

func TestOversizedHeaderLineIsRejected(t *testing.T) {
	raw := "SET_PARAMETER rtsp://host/1 RTSP/1.0\r\n" +
		"X-Big: " + strings.Repeat("a", maxHeaderLine) + "\r\n\r\n"
	if _, err := ReadRequest(bufio.NewReader(strings.NewReader(raw))); err == nil {
		t.Fatal("oversized header line accepted, want error")
	}
}

func TestTooManyHeadersIsRejected(t *testing.T) {
	var b strings.Builder
	b.WriteString("SET_PARAMETER rtsp://host/1 RTSP/1.0\r\n")
	for i := range maxHeaders + 1 {
		b.WriteString("X-Header-")
		b.WriteString(strconv.Itoa(i))
		b.WriteString(": value\r\n")
	}
	b.WriteString("\r\n")
	if _, err := ReadRequest(bufio.NewReader(strings.NewReader(b.String()))); err == nil {
		t.Fatal("more than 64 headers accepted, want error")
	}
}

func TestMalformedHeaderIsRejected(t *testing.T) {
	raw := "SET_PARAMETER rtsp://host/1 RTSP/1.0\r\nNoColonHere\r\n\r\n"
	if _, err := ReadRequest(bufio.NewReader(strings.NewReader(raw))); err == nil {
		t.Fatal("malformed header accepted, want error")
	}
}

func TestCleanCloseVersusTruncated(t *testing.T) {
	_, err := ReadRequest(bufio.NewReader(strings.NewReader("")))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("clean close error = %v, want io.EOF", err)
	}

	_, err = ReadRequest(bufio.NewReader(strings.NewReader("OPTIONS * RTSP/1.0")))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated line error = %v, want io.ErrUnexpectedEOF", err)
	}

	raw := "ANNOUNCE rtsp://host/1 RTSP/1.0\r\nCSeq: 1\r\nContent-Length: 10\r\n\r\nshort"
	_, err = ReadRequest(bufio.NewReader(strings.NewReader(raw)))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated body error = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestPathAndQuery(t *testing.T) {
	req := &Request{URI: "rtsp://192.168.1.2:7000/1234?x=1&y=2"}
	if got := req.Path(); got != "/1234" {
		t.Fatalf("Path = %q", got)
	}
	if got := req.Query(); got != "x=1&y=2" {
		t.Fatalf("Query = %q", got)
	}
	star := &Request{URI: "*"}
	if got := star.Path(); got != "*" {
		t.Fatalf("Path(*) = %q", got)
	}
	if got := star.Query(); got != "" {
		t.Fatalf("Query(*) = %q", got)
	}
}

func TestResponseSetReplacesCaseInsensitively(t *testing.T) {
	resp := NewResponse("1", 200, "")
	resp.Set("content-type", "a")
	resp.Set("Content-Type", "b")
	count := 0
	for _, h := range resp.Headers {
		if strings.EqualFold(h.Name, "Content-Type") {
			count++
			if h.Value != "b" {
				t.Fatalf("Content-Type = %q, want b", h.Value)
			}
		}
	}
	if count != 1 {
		t.Fatalf("Content-Type appears %d times, want 1", count)
	}
}

func TestResponseRoundTrip(t *testing.T) {
	body := []byte("volume: -12.34\r\n")
	resp := NewResponse("7", 200, "")
	resp.Set("Content-Type", "text/parameters")
	resp.Add("Content-Length", "999") // must be corrected to the real body length
	resp.Body = body

	var buf bytes.Buffer
	if err := resp.Write(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	parsed, err := ReadRequest(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("parsing serialised response: %v", err)
	}
	if parsed.Method != "RTSP/1.0" || parsed.URI != "200" || parsed.Proto != "OK" {
		t.Fatalf("status line parsed as %q %q %q", parsed.Method, parsed.URI, parsed.Proto)
	}
	if got := parsed.Get("CSeq"); got != "7" {
		t.Fatalf("CSeq = %q", got)
	}
	if got := parsed.Get("Content-Type"); got != "text/parameters" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := parsed.ContentLength(); got != len(body) {
		t.Fatalf("Content-Length = %d, want %d", got, len(body))
	}
	lengthHeaders := 0
	for _, h := range parsed.Headers {
		if strings.EqualFold(h.Name, "Content-Length") {
			lengthHeaders++
		}
	}
	if lengthHeaders != 1 {
		t.Fatalf("Content-Length appears %d times, want 1", lengthHeaders)
	}
	if !bytes.Equal(parsed.Body, body) {
		t.Fatalf("body = %q, want %q", parsed.Body, body)
	}
}

func TestResponseEmptyBodyHasNoContentLength(t *testing.T) {
	resp := NewResponse("8", 204, "")
	var buf bytes.Buffer
	if err := resp.Write(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	parsed, err := ReadRequest(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("parsing serialised response: %v", err)
	}
	if got := parsed.Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want absent", got)
	}
	if len(parsed.Body) != 0 {
		t.Fatalf("body = %q, want empty", parsed.Body)
	}
	if parsed.ContentLength() != 0 {
		t.Fatalf("ContentLength = %d, want 0", parsed.ContentLength())
	}
}

func TestReasonText(t *testing.T) {
	cases := map[int]string{
		200: "OK",
		400: "Bad Request",
		401: "Unauthorized",
		403: "Forbidden",
		404: "Not Found",
		405: "Method Not Allowed",
		453: "Not Enough Bandwidth",
		454: "Session Not Found",
		500: "Internal Server Error",
		501: "Not Implemented",
	}
	for status, want := range cases {
		if got := ReasonText(status); got != want {
			t.Errorf("ReasonText(%d) = %q, want %q", status, got, want)
		}
	}
}
