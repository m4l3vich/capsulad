package webui

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
)

// The PA Speaker is the only application with an HTTP route of its own. It is
// not a generic app-bus, audio or asset proxy: the route names one app and one
// operation, and the daemon is a relay, not a second owner of the speaker.
//
// Playback ownership stays with org.capsulad.pa: the route opens a routed
// AppBusService.Call, streams the request body as PlayRequest frames and relays
// the app's PlayResult. The daemon never opens AudioService.Playback itself,
// never changes master volume and never buffers the clip. Cancelling the HTTP
// request cancels the routed call, which is the PA app's documented stop path;
// the app discards queued audio and releases its playback stream.
//
// Authentication follows the two existing frontends. An operator-scoped API
// key is the automation credential (the same scope the MCP administration
// tools require), and a browser administrator session is accepted with the
// usual same-origin and CSRF proof. A status key cannot reach the route.
const (
	paAppID          = "org.capsulad.pa"
	paSpeakerService = "org.capsulad.pa.v1.SpeakerService"
	paPlayRPC        = "Play"

	// paPlayDeadline is the routed-call deadline. It is also the daemon app
	// bus maximum, so a client may upload a clip and let it drain without a
	// second, shorter timeout cutting it off. The PA app independently bounds
	// one clip to max_seconds and stops accepting request frames after 15 s.
	paPlayDeadline = 5 * time.Minute
	// paHTTPBudget leaves room for the response write after the routed call
	// has ended. The listener defaults to 30 s read / 60 s write timeouts,
	// which are too short for a clip plus its drain.
	paHTTPBudget = paPlayDeadline + 30*time.Second

	// paRequestChunk is the largest PlayRequest.data slice this route sends.
	// The app limits the whole protobuf message to 128 KiB, so leave room for
	// the field tag and varint. Full frames keep the number of messages low:
	// the script host's inbound request queue is only 32 messages.
	paRequestChunk = 127 << 10

	// paMaxWAVBytes is a transport cap, not the clip bound: the PA app's
	// max_seconds setting is authoritative. A 60 s, 48 kHz, stereo, 32-bit WAV
	// is about 23 MiB, so 32 MiB admits every clip the app can accept while
	// bounding a client that streams forever.
	paMaxWAVBytes = 32 << 20

	// paEndGrace lets a rejection that the PA app has already sent win over a
	// concurrent upload error. Without it, a body read failure racing the app's
	// INVALID_ARGUMENT could be reported as a transport failure.
	paEndGrace = 500 * time.Millisecond

	paTimeoutMessage   = "the PA speaker did not finish within the 5 minute playback window"
	paBodyLimitMessage = "the WAV body exceeds the 32 MiB HTTP limit"
)

// paPlayResult is the JSON projection of org.capsulad.pa.v1.PlayResult. The
// field names are the canonical protobuf JSON names the packaged descriptor
// publishes, so a raw-HTTP client and an app-bus caller see one contract.
type paPlayResult struct {
	Played       bool   `json:"played"`
	Frames       uint32 `json:"frames"`
	SampleRateHz uint32 `json:"sampleRateHz"`
	Channels     uint32 `json:"channels"`
	Detail       string `json:"detail"`
}

// paRecv is one frame from the routed response stream. A response message and
// the terminal End arrive as separate frames; the PA app sends at most one
// result message.
type paRecv struct {
	data []byte
	end  *frameworkv1.AppCallEnd
	err  error
}

// paMethodNotAllowed answers every method except the registered POST. The
// methodless pattern exists so an API probe does not fall through to the SPA
// fallback and receive index.html with a 200.
func (s *Server) paMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "POST")
	writeJSONError(w, http.StatusMethodNotAllowed, "only POST is supported")
}

// paAdmissionGate returns the process-wide PA admission semaphore. A buffered
// channel of capacity one is both the lock and the FIFO-ish waiter queue: a
// sender blocks until the active playback route releases it.
func (s *Server) paAdmissionGate() chan struct{} {
	s.paGateOnce.Do(func() { s.paGate = make(chan struct{}, 1) })
	return s.paGate
}

// acquirePAGate waits for the single PA relay slot or the request deadline.
// It reports false without taking the slot when ctx ends first.
func (s *Server) acquirePAGate(ctx context.Context) bool {
	select {
	case s.paAdmissionGate() <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// releasePAGate frees the slot taken by a successful acquirePAGate.
func (s *Server) releasePAGate() { <-s.paAdmissionGate() }

// playPASpeaker streams one raw RIFF/WAVE request body to the PA app and relays
// its result. See apps/pa/docs/speaker.md for the request/response contract.
func (s *Server) playPASpeaker(w http.ResponseWriter, r *http.Request) {
	// This route can return a rejection while the upload is still arriving.
	// Full duplex lets the sender goroutine keep a body Read in flight while
	// the handler writes the app's response; Connection: close tells net/http
	// not to drain that unread upload before flushing the response. Without
	// both, an HTTP/1.1 client that pauses after the bytes that provoked the
	// rejection can wait until the read deadline. Do this before any early
	// return so even a refused request does not drain a stalled body.
	w.Header().Set("Connection", "close")
	rc := http.NewResponseController(w)
	_ = rc.EnableFullDuplex()
	if s.Conn == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "the application bus is unavailable")
		return
	}
	// A clip may outlive the listener defaults. Extend this request only.
	_ = rc.SetReadDeadline(time.Now().Add(paHTTPBudget))
	_ = rc.SetWriteDeadline(time.Now().Add(paHTTPBudget))

	if r.ContentLength > paMaxWAVBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, paBodyLimitMessage)
		return
	}
	body := http.MaxBytesReader(w, r.Body, paMaxWAVBytes)
	// Do not Close the body here. Close waits for a pending Read to finish, and
	// the server closes the body itself after flushing this response (aborting
	// a blocked read first). Closing in the handler would reintroduce the stall
	// this route exists to avoid.

	ctx, cancel := context.WithTimeout(r.Context(), paPlayDeadline)

	// The PA app serves one Play call at a time and its script host has a
	// small inbound frame budget. Admit one HTTP upload at a time and do not
	// forward a second clip's body while the first is playing; the second
	// request waits here, bounded by its route deadline, instead of filling the
	// app's queue and being rejected as RESOURCE_EXHAUSTED.
	if !s.acquirePAGate(ctx) {
		cancel()
		if r.Context().Err() != nil {
			return
		}
		writeJSONError(w, http.StatusGatewayTimeout, paTimeoutMessage)
		return
	}
	credit := newPACredit()
	var senderFinished chan struct{}
	// Cancel the routed call, stop and join the body reader, then release the
	// admission slot, in that order. Releasing before the reader is gone would
	// let the next clip start while this request still owns an HTTP body read.
	defer func() {
		cancel()
		stopPAUpload(w, body, senderFinished, credit)
		s.releasePAGate()
	}()

	stream, err := frameworkv1.NewAppBusServiceClient(s.Conn).Call(ctx)
	if err != nil {
		s.writePATransportError(w, r, err)
		return
	}
	if err := stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_Start{
		Start: &frameworkv1.RoutedCallStart{
			TargetAppId: paAppID,
			GrpcService: paSpeakerService,
			RpcName:     paPlayRPC,
			TimeoutMs:   uint32(paPlayDeadline / time.Millisecond),
		},
	}}); err != nil {
		s.writePATransportError(w, r, err)
		return
	}

	senderResult := make(chan error, 1)
	senderFinished = make(chan struct{})
	go func() {
		defer close(senderFinished)
		senderResult <- s.sendPAWAV(ctx, body, stream, credit)
	}()

	recvCh := make(chan paRecv, 4)
	go recvPATransport(stream, recvCh, credit)

	var result []byte
	for {
		select {
		case <-ctx.Done():
			if r.Context().Err() != nil {
				// The HTTP client went away; the deferred cancel is the stop
				// signal the PA app already handles.
				return
			}
			writeJSONError(w, http.StatusGatewayTimeout, paTimeoutMessage)
			return

		case err := <-senderResult:
			senderResult = nil
			if err == nil {
				// The request stream is half-closed; wait for the app's result.
				continue
			}
			if ctx.Err() != nil {
				if r.Context().Err() != nil {
					return
				}
				writeJSONError(w, http.StatusGatewayTimeout, paTimeoutMessage)
				return
			}
			// The app may have ended the call first: a malformed clip is
			// rejected while the upload is still running. Give that End a
			// short chance to arrive before reporting the upload failure.
			if s.drainPARejection(w, r, recvCh) {
				return
			}
			if r.Context().Err() != nil {
				return
			}
			s.writePAUploadError(w, err)
			return

		case rr := <-recvCh:
			if rr.err != nil {
				if r.Context().Err() != nil {
					return
				}
				s.writePATransportError(w, r, rr.err)
				return
			}
			if rr.data != nil {
				result = rr.data
				continue
			}
			if rr.end == nil {
				s.writePATransportError(w, r, errors.New("the app bus returned an empty frame"))
				return
			}
			credit.close()
			s.writePAEnd(w, r, rr.end, result)
			return
		}
	}
}

// stopPAUpload expires the HTTP read deadline, joins the sender goroutine and
// only then closes the body. Close waits for a pending Read, and net/http's
// own post-handler close can block on an unfinished upload; doing this before
// the handler returns prevents a stuck connection after an early rejection.
func stopPAUpload(w http.ResponseWriter, body io.Closer, senderFinished <-chan struct{}, credit *paCredit) {
	if credit != nil {
		credit.close()
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(-time.Second))
	if senderFinished == nil {
		_ = body.Close()
		return
	}
	select {
	case <-senderFinished:
		_ = body.Close()
	case <-time.After(2 * time.Second):
		// The expired read deadline is the stop signal; leave the final close
		// to net/http rather than risk blocking the handler on a body read.
	}
}

// drainPARejection waits briefly for the PA app's terminal End after an upload
// failure. It writes the End response itself and reports whether the route is
// finished. A result message followed by an End is handled in the same wait;
// if only the result arrives, the loop keeps waiting for the End.
func (s *Server) drainPARejection(w http.ResponseWriter, r *http.Request, recvCh <-chan paRecv) bool {
	timer := time.NewTimer(paEndGrace)
	defer timer.Stop()
	var result []byte
	for {
		select {
		case rr := <-recvCh:
			if rr.err != nil {
				if r.Context().Err() != nil {
					return true
				}
				s.writePATransportError(w, r, rr.err)
				return true
			}
			if rr.data != nil {
				result = rr.data
				continue
			}
			if rr.end == nil {
				s.writePATransportError(w, r, errors.New("the app bus returned an empty frame"))
				return true
			}
			s.writePAEnd(w, r, rr.end, result)
			return true
		case <-timer.C:
			return false
		case <-r.Context().Done():
			return true
		}
	}
}

// paCredit is the route's view of the PA consumer's frame credits. The app
// starts a call with a bounded initial grant and adds one after it has fully
// consumed each request frame. The sender waits here instead of reading ahead,
// so a slow app or another call already playing cannot overflow the native
// SDK's 32-frame inbound queue.
type paCredit struct {
	mu        sync.Mutex
	available int
	closed    bool
	wake      chan struct{}
}

func newPACredit() *paCredit { return &paCredit{wake: make(chan struct{}, 1)} }

func (c *paCredit) add(n uint32) {
	if n == 0 {
		return
	}
	c.mu.Lock()
	if !c.closed {
		c.available += int(n)
	}
	c.mu.Unlock()
	c.signal()
}

func (c *paCredit) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// take waits for one frame credit. It returns false when the call has ended or
// the request deadline expires.
func (c *paCredit) take(ctx context.Context) bool {
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return false
		}
		if c.available > 0 {
			c.available--
			c.mu.Unlock()
			return true
		}
		c.mu.Unlock()
		select {
		case <-c.wake:
		case <-ctx.Done():
			return false
		}
	}
}

func (c *paCredit) close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.signal()
}

var errPACreditClosed = errors.New("PA request credit stream closed")

// sendPAWAV reads the HTTP body and sends it as PlayRequest frames until the
// body ends. It forwards whatever a single Read returns (including a partial
// WAV header) and waits for a consumer credit before each frame; it never
// waits for a full 127 KiB buffer, so a steady slow producer is not mistaken
// for a stalled one.
func (s *Server) sendPAWAV(ctx context.Context, body io.Reader, stream frameworkv1.AppBusService_CallClient, credit *paCredit) error {
	buf := make([]byte, paRequestChunk)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if !credit.take(ctx) {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return errPACreditClosed
			}
			frame := paPlayRequestFrame(buf[:n])
			if err := stream.Send(&frameworkv1.RoutedCallRequest{
				Message: &frameworkv1.RoutedCallRequest_RequestMessage{RequestMessage: frame},
			}); err != nil {
				return err
			}
		}
		switch {
		case readErr == nil:
			continue
		case readErr == io.EOF:
			if err := stream.Send(&frameworkv1.RoutedCallRequest{
				Message: &frameworkv1.RoutedCallRequest_HalfClose{HalfClose: &frameworkv1.CallHalfClose{}},
			}); err != nil {
				return err
			}
			return stream.CloseSend()
		default:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return readErr
		}
	}
}

// recvPATransport decodes the routed PlayEvent stream. Credits are applied to
// the sender's window; result and terminal End frames reach the handler.
func recvPATransport(stream frameworkv1.AppBusService_CallClient, out chan<- paRecv, credit *paCredit) {
	for {
		resp, err := stream.Recv()
		if err != nil {
			out <- paRecv{err: err}
			return
		}
		switch m := resp.GetMessage().(type) {
		case *frameworkv1.RoutedCallResponse_ResponseMessage:
			frames, result, err := decodePAPlayEvent(m.ResponseMessage)
			if err != nil {
				out <- paRecv{err: err}
				return
			}
			if frames > 0 {
				credit.add(frames)
			}
			if result != nil {
				out <- paRecv{data: result}
			}
		case *frameworkv1.RoutedCallResponse_End:
			out <- paRecv{end: m.End}
			return
		}
	}
}

// decodePAPlayEvent splits org.capsulad.pa.v1.PlayEvent into its credit and
// result payloads. Unknown fields are skipped.
func decodePAPlayEvent(data []byte) (uint32, []byte, error) {
	var frames uint32
	var result []byte
	for len(data) > 0 {
		number, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return 0, nil, protowire.ParseError(n)
		}
		data = data[n:]
		switch number {
		case 1:
			if typ != protowire.BytesType {
				return 0, nil, errors.New("the PA event has a malformed credit")
			}
			payload, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return 0, nil, protowire.ParseError(n)
			}
			count, err := decodePACredit(payload)
			if err != nil {
				return 0, nil, err
			}
			frames += count
			data = data[n:]
		case 2:
			if typ != protowire.BytesType {
				return 0, nil, errors.New("the PA event has a malformed result")
			}
			payload, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return 0, nil, protowire.ParseError(n)
			}
			result = append([]byte(nil), payload...)
			data = data[n:]
		default:
			n := protowire.ConsumeFieldValue(number, typ, data)
			if n < 0 {
				return 0, nil, protowire.ParseError(n)
			}
			data = data[n:]
		}
	}
	return frames, result, nil
}

func decodePACredit(data []byte) (uint32, error) {
	var frames uint32
	for len(data) > 0 {
		number, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return 0, protowire.ParseError(n)
		}
		data = data[n:]
		if number == 1 && typ == protowire.VarintType {
			value, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return 0, protowire.ParseError(n)
			}
			if value > 1<<32-1 {
				return 0, errors.New("PA credit frames above uint32")
			}
			frames = uint32(value)
			data = data[n:]
			continue
		}
		n = protowire.ConsumeFieldValue(number, typ, data)
		if n < 0 {
			return 0, protowire.ParseError(n)
		}
		data = data[n:]
	}
	return frames, nil
}

// writePAEnd maps the PA app's terminal status onto the HTTP contract. The app
// only sends a result on success; an End with a non-zero code is a rejection
// whose detail is shown verbatim.
func (s *Server) writePAEnd(w http.ResponseWriter, r *http.Request, end *frameworkv1.AppCallEnd, result []byte) {
	if end == nil {
		s.writePATransportError(w, r, errors.New("the app bus ended without a status"))
		return
	}
	if code := codes.Code(end.GetGrpcStatusCode()); code != codes.OK {
		s.writePAStatus(w, code, end.GetDetail())
		return
	}
	if len(result) == 0 {
		s.writePATransportError(w, r, errors.New("the PA speaker returned no result"))
		return
	}
	played, err := decodePAPlayResult(result)
	if err != nil {
		s.writePATransportError(w, r, err)
		return
	}
	if !played.Played {
		s.writePATransportError(w, r, errors.New("the PA speaker reported that the clip was not played"))
		return
	}
	if s.Logs != nil {
		s.Logs.Infof("webui", "", "PA speaker played %d frames at %d Hz/%d ch for %s",
			played.Frames, played.SampleRateHz, played.Channels, remoteHost(r))
	}
	writeJSONValue(w, http.StatusOK, played)
}

func (s *Server) writePAStatus(w http.ResponseWriter, code codes.Code, detail string) {
	if detail == "" {
		detail = "the PA speaker rejected the clip"
	}
	switch code {
	case codes.InvalidArgument, codes.OutOfRange:
		writeJSONError(w, http.StatusBadRequest, detail)
	case codes.PermissionDenied, codes.Unauthenticated:
		writeJSONError(w, http.StatusForbidden, detail)
	case codes.NotFound:
		writeJSONError(w, http.StatusNotFound, detail)
	case codes.ResourceExhausted:
		writeJSONError(w, http.StatusRequestEntityTooLarge, detail)
	case codes.FailedPrecondition, codes.Unavailable:
		writeJSONError(w, http.StatusServiceUnavailable, detail)
	case codes.DeadlineExceeded:
		writeJSONError(w, http.StatusGatewayTimeout, detail)
	case codes.Canceled:
		// If the caller is still on the connection, its clip was discarded, so
		// this is not a successful playback.
		writeJSONError(w, http.StatusConflict, detail)
	case codes.Unimplemented:
		writeJSONError(w, http.StatusNotImplemented, detail)
	case codes.Internal, codes.Unknown:
		writeJSONError(w, http.StatusInternalServerError, detail)
	default:
		writeJSONError(w, http.StatusBadGateway, detail)
	}
}

// paDeadline reports every deadline shape the PA relay can observe: a local
// context deadline, and a gRPC DeadlineExceeded status returned by start, send
// or recv. Both mean the same thing to an HTTP caller.
func paDeadline(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return status.Code(err) == codes.DeadlineExceeded
}

// writePAUploadError distinguishes the HTTP body cap, a routed deadline and a
// truncated or unreadable request body. Start/send/recv gRPC deadline statuses
// are 504 here too; a client that disconnected owes no reply.
func (s *Server) writePAUploadError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeJSONError(w, http.StatusRequestEntityTooLarge, paBodyLimitMessage)
		return
	}
	if paDeadline(err) {
		writeJSONError(w, http.StatusGatewayTimeout, paTimeoutMessage)
		return
	}
	writeJSONError(w, http.StatusBadRequest, "the request body ended before the WAV was complete")
}

// writePATransportError reports a daemon/app-bus failure without exposing
// transport internals. A client that has already disconnected owes no reply.
func (s *Server) writePATransportError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	if paDeadline(err) {
		writeJSONError(w, http.StatusGatewayTimeout, paTimeoutMessage)
		return
	}
	if s.Logs != nil {
		s.Logs.Warnf("webui", "", "PA speaker request failed: %v", err)
	}
	writeJSONError(w, http.StatusBadGateway, "the PA speaker is not answering")
}

// paPlayRequestFrame encodes org.capsulad.pa.v1.PlayRequest{bytes data = 1}
// without a generated binding: the PA descriptor is published as a descriptor
// set, not compiled into the daemon.
func paPlayRequestFrame(data []byte) []byte {
	frame := make([]byte, 0, len(data)+protowire.SizeTag(1)+protowire.SizeVarint(uint64(len(data))))
	frame = protowire.AppendTag(frame, 1, protowire.BytesType)
	frame = protowire.AppendBytes(frame, data)
	return frame
}

// decodePAPlayResult decodes the five PlayResult fields. Unknown fields are
// skipped, so an app that adds a field remains readable.
func decodePAPlayResult(data []byte) (paPlayResult, error) {
	var out paPlayResult
	for len(data) > 0 {
		number, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return out, protowire.ParseError(n)
		}
		data = data[n:]
		switch number {
		case 1:
			if typ != protowire.VarintType {
				return out, errors.New("the PA result has a malformed played flag")
			}
			value, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return out, protowire.ParseError(n)
			}
			out.Played = value != 0
			data = data[n:]
		case 2, 3, 4:
			if typ != protowire.VarintType {
				return out, errors.New("the PA result has a malformed numeric field")
			}
			value, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return out, protowire.ParseError(n)
			}
			if value > 1<<32-1 {
				return out, errors.New("the PA result has a numeric field above uint32")
			}
			switch number {
			case 2:
				out.Frames = uint32(value)
			case 3:
				out.SampleRateHz = uint32(value)
			case 4:
				out.Channels = uint32(value)
			}
			data = data[n:]
		case 5:
			if typ != protowire.BytesType {
				return out, errors.New("the PA result has a malformed detail field")
			}
			value, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return out, protowire.ParseError(n)
			}
			out.Detail = string(value)
			data = data[n:]
		default:
			n := protowire.ConsumeFieldValue(number, typ, data)
			if n < 0 {
				return out, protowire.ParseError(n)
			}
			data = data[n:]
		}
	}
	return out, nil
}
