package linuxdev

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// partialWriter accepts a few bytes per call, so the sink must loop instead of
// dropping the tail of a chunk.
type partialWriter struct {
	got  int
	max  int
	fail error
}

func (w *partialWriter) Write(p []byte) (int, error) {
	if w.fail != nil {
		return 0, w.fail
	}
	n := w.max
	if n > len(p) {
		n = len(p)
	}
	w.got += n
	return n, nil
}

func (w *partialWriter) Close() error { return nil }

func TestPipeSinkCompletesShortWrites(t *testing.T) {
	sink := &PipeSink{w: &partialWriter{max: 3}}
	payload := []byte("0123456789")
	if err := sink.WritePCM(payload); err != nil {
		t.Fatal(err)
	}
	if got := sink.w.(*partialWriter).got; got != len(payload) {
		t.Fatalf("wrote %d of %d bytes", got, len(payload))
	}
}

func TestPipeSinkReportsWriteError(t *testing.T) {
	boom := errors.New("broken pipe")
	sink := &PipeSink{w: &partialWriter{max: 1, fail: boom}}
	if err := sink.WritePCM([]byte("abc")); !errors.Is(err, boom) {
		t.Fatalf("WritePCM = %v, want the writer error", err)
	}
}

// A reader blocked on a child's stdout must be released by Close: the child
// never writes, and Close must not wait behind the read lock.
func TestPipeSourceCloseUnblocksReader(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no sleep helper")
	}
	cmd := exec.Command("sleep", "30")
	src, err := NewPipeSource(cmd)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 64)
		_, _ = src.ReadPCM(buf)
	}()
	time.Sleep(50 * time.Millisecond) // let the reader block in ReadPCM

	closed := make(chan error, 1)
	go func() { closed <- src.Close() }()
	select {
	case err := <-closed:
		// Killing the child makes Wait return a signal error; that is expected.
		if err != nil && !strings.Contains(err.Error(), "signal") && !strings.Contains(err.Error(), "killed") {
			t.Fatalf("Close = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close blocked behind a blocked ReadPCM")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("reader did not unblock after Close")
	}
	if _, err := src.ReadPCM(make([]byte, 8)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("ReadPCM after Close = %v, want ErrClosed", err)
	}
}

// A child that never reads fills the pipe; a large write must block, and Close
// must still return promptly and release the writer.
func TestPipeSinkCloseUnblocksWedgedWriter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no sleep helper")
	}
	sink, err := NewPipeSink(exec.Command("sleep", "30"))
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 256<<10)
	writeDone := make(chan error, 1)
	go func() { writeDone <- sink.WritePCM(payload) }()
	time.Sleep(100 * time.Millisecond) // let the pipe fill and the write block

	closed := make(chan error, 1)
	go func() { closed <- sink.Close() }()
	select {
	case err := <-closed:
		if err != nil && !strings.Contains(err.Error(), "signal") && !strings.Contains(err.Error(), "killed") {
			t.Fatalf("Close = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close blocked behind a wedged WritePCM")
	}
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("wedged write reported success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("writer did not unblock after Close")
	}
}

func TestPipeSinkCloseIsIdempotentAndRejectsWrites(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	sink, err := NewPipeSink(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil && !strings.Contains(err.Error(), "signal") && !strings.Contains(err.Error(), "killed") {
		t.Fatalf("Close = %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if err := sink.WritePCM([]byte("x")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("WritePCM after Close = %v, want ErrClosed", err)
	}
}

var _ io.WriteCloser = (*partialWriter)(nil)
