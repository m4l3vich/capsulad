package linuxdev

import (
	"io"
	"os"
	"os/exec"
	"sync"

	"capsulad/internal/backend"
)

// PipeSink is a PCM sink backed by a child process's stdin (pacat, aplay).
//
// WritePCM and Close never block each other: WritePCM takes the lock only to
// check the closed flag and snapshot the pipe, then loops on the pipe without
// it. Close marks the handle closed and closes/kills the child outside the lock,
// so a wedged client (a full pipe, a stopped reader) cannot deadlock mute,
// stream end or shutdown; the blocked write returns the pipe error. Writes loop
// until the whole buffer is accepted, so a short write is completed instead of
// silently dropping samples.
type PipeSink struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	w      io.WriteCloser
	closed bool
}

// NewPipeSink starts cmd with a stdin pipe and returns the sink.
func NewPipeSink(cmd *exec.Cmd) (*PipeSink, error) {
	w, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &PipeSink{cmd: cmd, w: w}, nil
}

// WritePCM writes all of p.
func (s *PipeSink) WritePCM(p []byte) error {
	s.mu.Lock()
	w := s.w
	closed := s.closed
	s.mu.Unlock()
	if closed || w == nil {
		return os.ErrClosed
	}
	for len(p) > 0 {
		n, err := w.Write(p)
		if n > 0 {
			p = p[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// Close closes the pipe and reaps the child without holding the lock.
func (s *PipeSink) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	w, cmd := s.w, s.cmd
	s.w, s.cmd = nil, nil
	s.mu.Unlock()

	_ = w.Close()
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	return cmd.Wait()
}

// PipeSource is a PCM source backed by a child process's stdout (parec,
// arecord). Close is not serialized behind a blocked read.
type PipeSource struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	r      io.ReadCloser
	closed bool
}

// NewPipeSource starts cmd with a stdout pipe and returns the source.
func NewPipeSource(cmd *exec.Cmd) (*PipeSource, error) {
	r, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &PipeSource{cmd: cmd, r: r}, nil
}

// ReadPCM fills p; a closed child is a normal end of stream.
func (s *PipeSource) ReadPCM(p []byte) (int, error) {
	s.mu.Lock()
	r := s.r
	s.mu.Unlock()
	if r == nil {
		return 0, os.ErrClosed
	}
	n, err := io.ReadFull(r, p)
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		return n, nil
	}
	return n, err
}

// Close closes the pipe and kills the child without holding the lock, so it can
// unblock a reader that is waiting inside ReadPCM.
func (s *PipeSource) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	r, cmd := s.r, s.cmd
	s.r, s.cmd = nil, nil
	s.mu.Unlock()

	_ = r.Close()
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	return cmd.Wait()
}

var (
	_ backend.Sink   = (*PipeSink)(nil)
	_ backend.Source = (*PipeSource)(nil)
)
