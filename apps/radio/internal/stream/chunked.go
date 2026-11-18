package stream

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// maxChunkSize bounds one transfer-encoding chunk. Radio servers use small
// chunks; a larger value is a malformed or hostile response.
const maxChunkSize = 4 << 20

// readLine reads one CRLF-terminated line. The bufio buffer size is the line
// cap, so an oversized header cannot grow memory without bound.
func readLine(br *bufio.Reader, limit int) (string, int, error) {
	if limit <= 0 {
		return "", 0, errors.New("header block too large")
	}
	raw, err := br.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return "", 0, errors.New("header line too long")
		}
		if errors.Is(err, io.EOF) && len(raw) == 0 {
			return "", 0, io.EOF
		}
		return "", 0, err
	}
	if len(raw) > limit {
		return "", 0, errors.New("header block too large")
	}
	line := strings.TrimRight(string(raw), "\r\n")
	return line, len(raw), nil
}

// chunkedReader decodes Transfer-Encoding: chunked. It is deliberately small:
// one chunk at a time, extensions ignored, trailers drained and bounded.
type chunkedReader struct {
	br        *bufio.Reader
	remaining int64
	done      bool
}

func newChunkedReader(br *bufio.Reader) *chunkedReader {
	return &chunkedReader{br: br}
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.done {
		return 0, io.EOF
	}
	for c.remaining == 0 {
		if err := c.nextChunk(); err != nil {
			return 0, err
		}
		if c.done {
			return 0, io.EOF
		}
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.br.Read(p)
	c.remaining -= int64(n)
	if err == nil && c.remaining == 0 {
		if crlfErr := readChunkCRLF(c.br); crlfErr != nil {
			return n, crlfErr
		}
	}
	return n, err
}

func (c *chunkedReader) nextChunk() error {
	line, _, err := readLine(c.br, 8<<10)
	if err != nil {
		return err
	}
	if semi := strings.IndexByte(line, ';'); semi >= 0 {
		line = line[:semi]
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return errors.New("empty chunk size")
	}
	size, err := strconv.ParseInt(line, 16, 64)
	if err != nil || size < 0 || size > maxChunkSize {
		return fmt.Errorf("invalid chunk size %q", line)
	}
	if size == 0 {
		if err := drainTrailers(c.br); err != nil {
			return err
		}
		c.done = true
		return nil
	}
	c.remaining = size
	return nil
}

func readChunkCRLF(br *bufio.Reader) error {
	first, err := br.ReadByte()
	if err != nil {
		return err
	}
	if first == '\n' {
		return nil
	}
	if first != '\r' {
		return errors.New("chunk data is not followed by CRLF")
	}
	second, err := br.ReadByte()
	if err != nil {
		return err
	}
	if second != '\n' {
		return errors.New("chunk data is not followed by CRLF")
	}
	return nil
}

func drainTrailers(br *bufio.Reader) error {
	used := 0
	for {
		line, n, err := readLine(br, 32<<10-used)
		if err != nil {
			return err
		}
		used += n
		if line == "" {
			return nil
		}
	}
}
