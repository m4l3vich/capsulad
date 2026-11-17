package session

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"capsulad/apps/airplay/internal/artwork"
	"capsulad/apps/airplay/internal/dmap"
	"capsulad/apps/airplay/internal/hapcrypt"
	"capsulad/apps/airplay/internal/media"
	"capsulad/apps/airplay/internal/pair"
	"capsulad/apps/airplay/internal/raop"
	"capsulad/apps/airplay/internal/rtsp"
)

// Audio payload types used by AirPlay.
const (
	streamTypeRealtime  = 96
	streamTypeRedundant = 97
	streamTypeBuffered  = 103
	defaultIdleTimeout  = 45 * time.Second
	maxRequestBody      = 8 << 20
)

// errSessionEnded reports that the sender ended the session on purpose.
var errSessionEnded = errors.New("session ended by the sender")

// channelKeys is one direction-keyed pair for the encrypted channels.
type channelKeys struct {
	write []byte
	read  []byte
}

// progress is the sender's start/current/end playback position report.
type progress struct {
	start, current, end uint32
	has                 bool
}

// Session is one sender's RTSP session: the control connection, the pairing
// state, the audio streams, the event channel and the ring presentation.
type Session struct {
	m    *Manager
	conn Stream
	br   *bufio.Reader
	out  io.Writer
	peer Peer

	writeMu sync.Mutex

	mu                 sync.Mutex
	setup              *pair.SetupServer
	verify             *pair.VerifyServer
	shared             []byte
	ctrlKeys           *channelKeys
	evKeys             *channelKeys
	encrypted          bool
	announce           *raop.Stream
	announceKey        []byte
	controllerID       string
	closeAfterResponse bool

	userAgent  string
	deviceName string

	streams []*audioStream

	ap2Control    *controlState
	eventListener Listener
	eventConn     Stream
	eventOut      io.Writer
	eventIn       *bufio.Reader
	eventMu       sync.Mutex

	volumeDB float64
	rate     float64

	metadata      dmap.Metadata
	palette       []artwork.RGB
	envelopeState *artwork.Envelope
	envelopeLevel float64
	ring          Ring
	ringDone      chan struct{}
	progress      progress

	boardGen   uint64
	done       chan struct{}
	closeOnce  sync.Once
	lastActive int64
	audioSeen  atomic.Bool
}

func newSession(m *Manager, conn Stream) *Session {
	s := &Session{
		m:    m,
		conn: conn,
		br:   bufio.NewReaderSize(conn, 16*1024),
		out:  conn,
		peer: conn.Peer(),
		rate: 1,
		done: make(chan struct{}),
	}
	s.touch()
	return s
}

func (s *Session) touch() { atomic.StoreInt64(&s.lastActive, time.Now().UnixNano()) }

func (s *Session) idleFor() time.Duration {
	return time.Since(time.Unix(0, atomic.LoadInt64(&s.lastActive)))
}

// run serves the session until the sender leaves, the connection breaks, the
// receiver shuts down or the idle timeout expires.
func (s *Session) run() {
	defer func() {
		if r := recover(); r != nil {
			s.m.logf("session: recovered from %v", r)
			s.cleanup(fmt.Sprintf("internal error: %v", r))
			return
		}
	}()
	if s.m.board != nil {
		s.boardGen = s.m.board.BeginSession(media.NamesFromSource(s.userAgent, "", s.peer.Address))
	}
	go s.idleWatchdog()
	go s.volumeWatch()
	err := s.serveRTSP()
	reason := "the sender closed the connection"
	switch {
	case err == nil:
	case errors.Is(err, errSessionEnded):
		reason = "teardown"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		reason = "the control connection ended"
	default:
		reason = err.Error()
	}
	s.cleanup(reason)
}

func (s *Session) idleWatchdog() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			limit := defaultIdleTimeout
			if s.audioSeen.Load() {
				// A sender that is streaming keeps the session alive; a source
				// that goes quiet without a teardown is still bounded.
				limit = 4 * defaultIdleTimeout
			}
			if s.idleFor() > limit {
				s.m.logf("session: no activity for %s; ending it", s.idleFor())
				s.close("idle timeout")
				return
			}
		}
	}
}

// close ends the session from another goroutine (takeover, shutdown, timeout).
func (s *Session) close(reason string) {
	s.closeOnce.Do(func() {
		close(s.done)
		if s.conn != nil {
			_ = s.conn.Close()
		}
		s.m.logf("closing session from %s: %s", s.peer.Address, reason)
	})
}

// cleanup releases every resource the session holds. It runs once, on the
// session goroutine, after the control connection ended.
func (s *Session) cleanup(reason string) {
	s.closeOnce.Do(func() { close(s.done) })
	s.stopRing()
	for _, stream := range s.activeStreams() {
		stream.stop()
	}
	s.eventMu.Lock()
	if s.eventConn != nil {
		_ = s.eventConn.Close()
		s.eventConn = nil
	}
	if s.eventListener != nil {
		_ = s.eventListener.Close()
		s.eventListener = nil
	}
	s.eventMu.Unlock()
	s.closeAP2Control()
	s.m.SessionEnded(s, reason)
}

func (s *Session) activeStreams() []*audioStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*audioStream(nil), s.streams...)
}

// --- RTSP loop ------------------------------------------------------------

func (s *Session) serveRTSP() error {
	for {
		select {
		case <-s.done:
			return nil
		default:
		}
		interleaved, err := s.readInterleaved()
		if err != nil {
			return err
		}
		if interleaved {
			continue
		}
		request, err := rtsp.ReadRequest(s.br)
		if err != nil {
			return err
		}
		s.touch()
		if s.userAgent == "" {
			s.userAgent = request.Get("User-Agent")
			if s.m.board != nil {
				s.m.board.SetSourceName(s.boardGen, media.NamesFromSource(s.userAgent, s.deviceName, s.peer.Address))
			}
		}
		response, upgrade, err := s.dispatch(request)
		if err != nil {
			return err
		}
		if response == nil {
			response = rtsp.NewResponse(request.CSeq(), 200, "")
		}
		if err := s.writeResponse(request, response); err != nil {
			return err
		}
		if upgrade != nil {
			if err := s.upgradeChannel(upgrade); err != nil {
				return err
			}
		}
		if request.Method == "TEARDOWN" || s.closeAfterResponse {
			return errSessionEnded
		}
	}
}

// readInterleaved consumes one interleaved RTP/RTCP frame when the next byte is
// the RTSP interleaved marker ($). Legacy senders that ask for
// `Transport: RTP/AVP/TCP;interleaved=0-1` carry their audio this way.
func (s *Session) readInterleaved() (bool, error) {
	prefix, err := s.br.Peek(1)
	if err != nil {
		return false, err
	}
	if prefix[0] != '$' {
		return false, nil
	}
	var header [4]byte
	if _, err := io.ReadFull(s.br, header[:]); err != nil {
		return true, err
	}
	channel := int(header[1])
	length := int(binary.BigEndian.Uint16(header[2:4]))
	if length <= 0 {
		return true, fmt.Errorf("interleaved frame with length %d", length)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(s.br, payload); err != nil {
		return true, err
	}
	s.touch()
	for _, stream := range s.activeStreams() {
		if stream.spec.interleavedData == channel {
			stream.deliver(payload)
			return true, nil
		}
		if stream.spec.interleavedControl == channel {
			stream.deliverControl(payload)
			return true, nil
		}
	}
	return true, nil
}

func (s *Session) writeResponse(request *rtsp.Request, response *rtsp.Response) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	response.Set("Server", "AirTunes/"+s.m.cfg.SourceVersion)
	if request != nil {
		response.Set("CSeq", request.CSeq())
	}
	return response.Write(s.out)
}

// channelUpgrade derives the control and event keys from a shared secret: the
// SRP session key for a transient pair-setup, or the X25519 secret after
// pair-verify. Keys are stored from the accessory's perspective (read opens
// inbound blocks, write seals outbound ones).
func (s *Session) channelUpgrade(shared []byte) (*channelKeys, error) {
	if len(shared) == 0 {
		return nil, errors.New("no shared secret for the encrypted channels")
	}
	control := hapcrypt.ControlChannelKeys(shared)
	events := hapcrypt.EventChannelKeys(shared)
	s.ctrlKeys = &channelKeys{write: control.Outbound, read: control.Inbound}
	s.evKeys = &channelKeys{write: events.Outbound, read: events.Inbound}
	return s.ctrlKeys, nil
}

// upgradeChannel switches the control connection to the encrypted HAP framing.
// The sender must not pipeline anything past the pair-verify response; buffered
// plaintext would otherwise be misread as ciphertext, so it is an error.
func (s *Session) upgradeChannel(keys *channelKeys) error {
	if keys == nil {
		return nil
	}
	if s.br.Buffered() > 0 {
		return errors.New("client pipelined plaintext past the pairing handshake")
	}
	session, err := hapcrypt.NewSession(s.conn, keys.write, keys.read)
	if err != nil {
		return err
	}
	s.br = bufio.NewReaderSize(session, 16*1024)
	s.out = session
	s.encrypted = true
	s.m.logf("session from %s: control channel encrypted", s.peer.Address)
	return nil
}
