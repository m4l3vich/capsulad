package session

// Full independent pyatv flow over a real loopback listener: pyatv performs its
// own AirPlay/RAOP setup (transient HAP pairing, RTSP, RTP) and streams a WAV;
// this receiver must deliver PCM to the framework sink. The test is gated on
// PYATV_PYTHON and skips otherwise, so it never masquerades as a default suite.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"capsulad/apps/airplay/internal/media"
)

// realTransport binds real loopback sockets for the session's seams.
type realTransport struct {
	mu  sync.Mutex
	all []interface{ Close() error }
}

func (t *realTransport) track(c interface{ Close() error }) *realTransport {
	t.mu.Lock()
	t.all = append(t.all, c)
	t.mu.Unlock()
	return t
}

func (t *realTransport) OpenTCPListener(_ string, port int) (Listener, error) {
	listener, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, err
	}
	l := &realTCPListener{listener: listener}
	t.track(l)
	return l, nil
}

func (t *realTransport) OpenUDPListener(_ string, port int, group string) (UDPListener, error) {
	if group != "" {
		return nil, fmt.Errorf("multicast is not part of the loopback harness")
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		return nil, err
	}
	l := &realUDPListener{conn: conn}
	t.track(l)
	return l, nil
}

func (t *realTransport) Close() {
	t.mu.Lock()
	all := t.all
	t.all = nil
	t.mu.Unlock()
	for _, c := range all {
		_ = c.Close()
	}
}

type realTCPListener struct{ listener net.Listener }

func (l *realTCPListener) Accept() (Stream, error) {
	conn, err := l.listener.Accept()
	if err != nil {
		return nil, err
	}
	peer := Peer{}
	if addr, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		peer = Peer{Address: addr.IP.String(), Port: addr.Port}
	}
	return &realStream{conn: conn, peer: peer}, nil
}

func (l *realTCPListener) Addr() (string, int) {
	addr := l.listener.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

func (l *realTCPListener) Close() error { return l.listener.Close() }

type realStream struct {
	conn net.Conn
	peer Peer
	once sync.Once
}

func (s *realStream) Read(p []byte) (int, error)  { return s.conn.Read(p) }
func (s *realStream) Write(p []byte) (int, error) { return s.conn.Write(p) }
func (s *realStream) Peer() Peer                  { return s.peer }
func (s *realStream) Close() error {
	var err error
	s.once.Do(func() { err = s.conn.Close() })
	return err
}

type realUDPListener struct{ conn *net.UDPConn }

func (l *realUDPListener) Receive() ([]byte, UDPAddr, error) {
	buf := make([]byte, 65507)
	n, addr, err := l.conn.ReadFromUDP(buf)
	if err != nil {
		return nil, UDPAddr{}, err
	}
	return buf[:n], UDPAddr{Address: addr.IP.String(), Port: addr.Port}, nil
}

func (l *realUDPListener) Send(peer UDPAddr, data []byte) error {
	addr := &net.UDPAddr{IP: net.ParseIP(peer.Address), Port: peer.Port}
	_, err := l.conn.WriteToUDP(data, addr)
	return err
}

func (l *realUDPListener) Addr() (string, int) {
	addr := l.conn.LocalAddr().(*net.UDPAddr)
	return addr.IP.String(), addr.Port
}

func (l *realUDPListener) Group() string { return "" }
func (l *realUDPListener) Close() error  { return l.conn.Close() }

const pyatvFlow = `
import asyncio, sys, pyatv
from pyatv.conf import AppleTV, AirPlayService, RaopService

async def main(host, port, wav):
    conf = AppleTV(host, "Loopback AirPlay")
    conf.add_service(AirPlayService(host, port))
    conf.add_service(RaopService(host, port))
    atv = await pyatv.connect(conf, asyncio.get_event_loop())
    try:
        await atv.stream.stream_file(wav)
    finally:
        atv.close()

asyncio.get_event_loop().run_until_complete(main(sys.argv[1], int(sys.argv[2]), sys.argv[3]))
print("pyatv flow complete")
`

func writeWAV(t *testing.T) string {
	t.Helper()
	const rate, seconds = 44100, 2
	samples := rate * seconds
	data := make([]byte, samples*4)
	for i := 0; i < samples; i++ {
		value := int16(9000 * math.Sin(2*math.Pi*440*float64(i)/rate))
		binary.LittleEndian.PutUint16(data[i*4:], uint16(value))
		binary.LittleEndian.PutUint16(data[i*4+2:], uint16(value))
	}
	var body bytes.Buffer
	body.WriteString("RIFF")
	binary.Write(&body, binary.LittleEndian, uint32(36+len(data)))
	body.WriteString("WAVEfmt ")
	binary.Write(&body, binary.LittleEndian, uint32(16))
	binary.Write(&body, binary.LittleEndian, uint16(1))
	binary.Write(&body, binary.LittleEndian, uint16(2))
	binary.Write(&body, binary.LittleEndian, uint32(rate))
	binary.Write(&body, binary.LittleEndian, uint32(rate*4))
	binary.Write(&body, binary.LittleEndian, uint16(4))
	binary.Write(&body, binary.LittleEndian, uint16(16))
	body.WriteString("data")
	binary.Write(&body, binary.LittleEndian, uint32(len(data)))
	body.Write(data)
	path := t.TempDir() + "/tone.wav"
	if err := os.WriteFile(path, body.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPyatvFullFlowStreamsPCM(t *testing.T) {
	python := os.Getenv("PYATV_PYTHON")
	if python == "" {
		t.Skip("set PYATV_PYTHON to a python with pyatv installed to run the independent loopback flow")
	}
	transport := &realTransport{}
	t.Cleanup(transport.Close)
	sink := &fakeSink{}
	board := media.NewBoard()
	mgr, err := NewManager(Config{
		DeviceID: "aabbccddeeff", Name: "Loopback AirPlay", Port: 0,
		FrameTarget: 20 * time.Millisecond, RingEnabled: false, Logf: t.Logf,
	}, transport, &fakeAdvertiser{}, fakeRings{ring: &fakeRing{}}, fakeSinks{sink: sink},
		board, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()
	port := mgr.advertisedPort()
	if port == 0 {
		t.Fatal("no listener port")
	}

	script := t.TempDir() + "/flow.py"
	if err := os.WriteFile(script, []byte(pyatvFlow), 0o600); err != nil {
		t.Fatal(err)
	}
	wav := writeWAV(t)
	command := exec.Command(python, script, "127.0.0.1", fmt.Sprint(port), wav)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	command.Dir = t.TempDir()
	done := make(chan error, 1)
	go func() { done <- command.Run() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("pyatv flow failed: %v\n%s", err, output.String())
		}
	case <-time.After(60 * time.Second):
		_ = command.Process.Kill()
		t.Fatalf("pyatv flow timed out\n%s", output.String())
	}
	written, _ := sink.state()
	if written == 0 {
		t.Fatalf("pyatv completed but no PCM reached the framework sink\n%s", output.String())
	}
	t.Logf("pyatv streamed %d bytes of PCM to the framework sink", written)
}
