// Package rtsp implements the RTSP/1.0 request and response wire format used
// by AirPlay and RAOP receivers.
//
// The parser accepts both CRLF and bare LF line endings so that it can also be
// exercised with simply constructed test data. Header names are treated
// case-insensitively. Requests may be pipelined: ReadRequest consumes exactly
// one message (request line, headers and declared body) and leaves any
// following bytes untouched in the reader.
package rtsp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	// maxHeaderLine bounds a single request/status/header line, including the
	// line terminator.
	maxHeaderLine = 64 << 10
	// maxHeaders bounds the number of header fields in one message.
	maxHeaders = 64
	// maxBody bounds an accepted Content-Length. Larger bodies are rejected
	// before any allocation takes place.
	maxBody = 8 << 20
)

// Header is a single RTSP header field. Names and values are stored exactly as
// received (values are trimmed of surrounding whitespace).
type Header struct {
	Name  string
	Value string
}

// Request is a parsed RTSP request.
type Request struct {
	Method  string
	URI     string
	Proto   string
	Headers []Header
	Body    []byte
}

// ReadRequest reads and parses one RTSP request from r.
//
// io.EOF is returned only when the connection closes cleanly before the first
// byte of a new message. A connection that ends in the middle of a request
// line, a header or a body yields io.ErrUnexpectedEOF; malformed input yields
// a descriptive error.
func ReadRequest(r *bufio.Reader) (*Request, error) {
	line, err := readLine(r)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return nil, fmt.Errorf("rtsp: malformed request line %q: want 3 tokens, got %d", line, len(fields))
	}
	req := &Request{Method: fields[0], URI: fields[1], Proto: fields[2]}

	for {
		line, err := readLine(r)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				// Headers must be terminated by an empty line.
				return nil, fmt.Errorf("rtsp: unexpected end of headers: %w", io.ErrUnexpectedEOF)
			}
			return nil, err
		}
		if line == "" {
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			return nil, fmt.Errorf("rtsp: folded header line %q is not supported", line)
		}
		if len(req.Headers) >= maxHeaders {
			return nil, fmt.Errorf("rtsp: more than %d headers", maxHeaders)
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			return nil, fmt.Errorf("rtsp: malformed header line %q", line)
		}
		name := strings.TrimSpace(line[:colon])
		if name == "" {
			return nil, fmt.Errorf("rtsp: empty header name in %q", line)
		}
		req.Headers = append(req.Headers, Header{
			Name:  name,
			Value: strings.TrimSpace(line[colon+1:]),
		})
	}

	n, err := req.contentLengthStrict()
	if err != nil {
		return nil, err
	}
	if n > 0 {
		body := make([]byte, n)
		if read, err := io.ReadFull(r, body); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, fmt.Errorf("rtsp: truncated body (%d of %d bytes): %w", read, n, io.ErrUnexpectedEOF)
			}
			return nil, fmt.Errorf("rtsp: reading body: %w", err)
		}
		req.Body = body
	}
	return req, nil
}

// readLine reads one line terminated by '\n', tolerating a preceding '\r'.
// It bounds a single line and reports io.EOF only when the reader is at a
// clean close before any byte of the line.
func readLine(r *bufio.Reader) (string, error) {
	var line []byte
	for {
		frag, err := r.ReadSlice('\n')
		if len(line)+len(frag) > maxHeaderLine {
			return "", fmt.Errorf("rtsp: line exceeds %d bytes", maxHeaderLine)
		}
		line = append(line, frag...)
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(line) == 0 {
				return "", io.EOF
			}
			return "", io.ErrUnexpectedEOF
		}
		return "", err
	}
	line = line[:len(line)-1]
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	return string(line), nil
}

// Get returns the value of the first header with the given name, matched
// case-insensitively, or "" when the header is absent.
func (r *Request) Get(name string) string {
	for i := range r.Headers {
		if strings.EqualFold(r.Headers[i].Name, name) {
			return r.Headers[i].Value
		}
	}
	return ""
}

// CSeq returns the request sequence number header, or "" when absent.
func (r *Request) CSeq() string { return r.Get("CSeq") }

// Path returns the URI path with the scheme, authority and query removed.
func (r *Request) Path() string {
	path, _ := splitURI(r.URI)
	return path
}

// Query returns the URI query without the leading '?', or "" when absent.
func (r *Request) Query() string {
	_, query := splitURI(r.URI)
	return query
}

// ContentLength returns the declared Content-Length, or 0 when the header is
// absent or invalid.
func (r *Request) ContentLength() int {
	n, err := r.contentLengthStrict()
	if err != nil {
		return 0
	}
	return n
}

// contentLengthStrict parses and validates the Content-Length header.
func (r *Request) contentLengthStrict() (int, error) {
	value := r.Get("Content-Length")
	if value == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("rtsp: invalid Content-Length %q: %w", value, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("rtsp: negative Content-Length %d", n)
	}
	if n > maxBody {
		return 0, fmt.Errorf("rtsp: Content-Length %d exceeds the %d byte limit", n, maxBody)
	}
	return n, nil
}

// splitURI separates the path from the query of an RTSP request URI.
func splitURI(uri string) (path, query string) {
	rest := uri
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			rest = rest[j:]
		} else {
			rest = ""
		}
	}
	if i := strings.IndexByte(rest, '?'); i >= 0 {
		query = rest[i+1:]
		rest = rest[:i]
	}
	return rest, query
}

// Keys parses a semicolon-separated parameter header such as Transport or
// RTP-Info. Parameter names are folded to lower case; flags without a value
// are present with an empty value.
func Keys(header string) map[string]string {
	keys := make(map[string]string)
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value := part, ""
		if i := strings.IndexByte(part, '='); i >= 0 {
			key, value = part[:i], part[i+1:]
		} else if i := strings.IndexByte(part, ':'); i >= 0 {
			key, value = part[:i], part[i+1:]
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = value[1 : len(value)-1]
		}
		if key == "" {
			continue
		}
		keys[key] = value
	}
	return keys
}

// Response is an RTSP response ready to be serialised.
type Response struct {
	Status  int
	Reason  string
	Headers []Header
	Body    []byte
}

// NewResponse builds a response with the given status, reason phrase and CSeq.
// An empty reason is replaced by the canonical phrase for status. An empty
// cseq omits the CSeq header.
func NewResponse(cseq string, status int, reason string) *Response {
	if reason == "" {
		reason = ReasonText(status)
	}
	resp := &Response{Status: status, Reason: reason}
	if cseq != "" {
		resp.Set("CSeq", cseq)
	}
	return resp
}

// Set replaces the first header with the given name (case-insensitively) or
// appends a new one when no such header exists.
func (w *Response) Set(name, value string) {
	for i := range w.Headers {
		if strings.EqualFold(w.Headers[i].Name, name) {
			w.Headers[i] = Header{Name: name, Value: value}
			return
		}
	}
	w.Headers = append(w.Headers, Header{Name: name, Value: value})
}

// Add appends a header without replacing existing headers of the same name.
func (w *Response) Add(name, value string) {
	w.Headers = append(w.Headers, Header{Name: name, Value: value})
}

// WriteTo serialises the response: status line, headers, empty line and body.
// When the body is non-empty a Content-Length header with the body length is
// emitted, replacing any previously set Content-Length. With an empty body an
// explicit Content-Length is preserved and none is added.
func (w *Response) Write(out io.Writer) error {
	reason := w.Reason
	if reason == "" {
		reason = ReasonText(w.Status)
	}
	if reason == "" {
		reason = "Unknown"
	}

	var head bytes.Buffer
	fmt.Fprintf(&head, "RTSP/1.0 %d %s\r\n", w.Status, reason)

	hasBody := len(w.Body) > 0
	wroteLength := false
	for _, h := range w.Headers {
		if strings.EqualFold(h.Name, "Content-Length") {
			if hasBody {
				if !wroteLength {
					fmt.Fprintf(&head, "Content-Length: %d\r\n", len(w.Body))
					wroteLength = true
				}
				continue
			}
		}
		fmt.Fprintf(&head, "%s: %s\r\n", h.Name, h.Value)
	}
	if hasBody && !wroteLength {
		fmt.Fprintf(&head, "Content-Length: %d\r\n", len(w.Body))
	}
	head.WriteString("\r\n")

	if _, err := out.Write(head.Bytes()); err != nil {
		return err
	}
	if len(w.Body) > 0 {
		if _, err := out.Write(w.Body); err != nil {
			return err
		}
	}
	return nil
}

// ReasonText returns the canonical RTSP reason phrase for a status code, or ""
// when the code is not one of the common receiver codes.
func ReasonText(status int) string {
	switch status {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 403:
		return "Forbidden"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 453:
		return "Not Enough Bandwidth"
	case 454:
		return "Session Not Found"
	case 461:
		return "Unsupported Transport"
	case 500:
		return "Internal Server Error"
	case 501:
		return "Not Implemented"
	case 503:
		return "Service Unavailable"
	}
	return ""
}
