package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/backend"
	"capsulad/internal/websecurity"
)

// This is the production-path PA relay check: the real HTTPS Web UI listener,
// the real AppBusService.Call route, and the packaged MicroPython app running
// on the simulator backend. The simulator's LastOutput is digital-output
// evidence (mixed PCM reached the virtual sink), not acoustic output. The test
// is isolated: its data dir, apps, security store, web port and host runtime
// are all temporary, and it never touches a device.
//
// Run it with a host runtime that contains capsulad-micropython and
// capsulad-native.json built for this host:
//
//	CAPSULAD_NATIVE_LIB_DIR=/tmp/pa-http-host/lib \
//	  go test -ldflags=-linkmode=external -run TestPAHTTPProductionIntegration \
//	  ./internal/daemon -count=1 -v
const paE2EPassword = "pa-http-e2e-pass"

type paE2E struct {
	d         *Daemon
	sim       *backend.Simulator
	addr      string
	opKey     string
	statusKey string
	client    *http.Client
}

func newPAE2E(t *testing.T) *paE2E {
	t.Helper()
	libDir := os.Getenv("CAPSULAD_NATIVE_LIB_DIR")
	if libDir == "" {
		t.Skip("set CAPSULAD_NATIVE_LIB_DIR to a host runtime containing capsulad-micropython")
	}
	if _, err := os.Stat(filepath.Join(libDir, "capsulad-micropython")); err != nil {
		t.Skipf("no host capsulad-micropython in %s", libDir)
	}
	if _, err := os.Stat(filepath.Join(libDir, "capsulad-native.json")); err != nil {
		t.Skipf("no capsulad-native.json in %s", libDir)
	}

	base, err := os.MkdirTemp("", "cph")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	apps := filepath.Join(base, "apps")
	root := moduleRoot(t)
	stage := exec.Command("sh", filepath.Join(root, "apps", "pa", "build.sh"))
	stage.Dir = root
	stage.Env = append(os.Environ(), "OUT="+apps, "PACKAGE=0")
	if out, err := stage.CombinedOutput(); err != nil {
		t.Fatalf("stage packaged PA app: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(apps, "org.capsulad.pa", "main.py")); err != nil {
		t.Fatalf("staged PA package is incomplete: %v", err)
	}
	// This is not the installation boot: keep the framework startup cue out of
	// the simulator sink so LastOutput is the PA clip, not the boot jingle.
	if err := os.WriteFile(filepath.Join(base, "startup-sound.played"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := websecurity.Open(filepath.Join(base, "security"), websecurity.DefaultOptions())
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InitServerCert(websecurity.CertOptions{Hostname: "localhost", IPs: []string{"127.0.0.1"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPassword(paE2EPassword); err != nil {
		t.Fatal(err)
	}
	opKey, _, err := store.CreateAPIKeyWithScopes("pa-http-e2e", []string{websecurity.ScopeOperator})
	if err != nil {
		t.Fatal(err)
	}
	statusKey, _, err := store.CreateAPIKey("pa-http-status")
	if err != nil {
		t.Fatal(err)
	}

	addr := freeTCPAddr(t)
	cfg := DefaultConfig()
	cfg.DataDir = base
	cfg.RunDir = filepath.Join(base, "run")
	cfg.SocketPath = filepath.Join(cfg.RunDir, "hal.sock")
	cfg.AppsDir = apps
	cfg.AssetsDir = filepath.Join(base, "assets")
	cfg.WebUIAddr = addr
	cfg.SandboxMode = "none"
	cfg.NativeLibDir = libDir
	ctx, cancel := context.WithCancel(context.Background())
	d, err := New(ctx, cfg)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Run() }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Log("daemon shutdown timed out")
		}
	})
	waitFor(t, 5*time.Second, "daemon socket", func() bool {
		_, err := os.Stat(cfg.SocketPath)
		return err == nil
	})
	waitReady(t, d, "org.capsulad.pa", 30*time.Second)

	sim, ok := d.dev.(*backend.Simulator)
	if !ok {
		t.Fatalf("test backend = %T, want the simulator", d.dev)
	}
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		// The long-clip probe is a 30 s WAV; the route itself allows 5 min.
		Timeout: 90 * time.Second,
	}
	waitFor(t, 10*time.Second, "HTTPS Web UI", func() bool {
		resp, err := client.Get("https://" + addr + "/healthz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	return &paE2E{d: d, sim: sim, addr: addr, opKey: opKey, statusKey: statusKey, client: client}
}

func freeTCPAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	return addr
}

func (e *paE2E) target() string {
	return "https://" + e.addr + "/api/apps/org.capsulad.pa/play"
}

func (e *paE2E) postWith(t *testing.T, c *http.Client, headers map[string]string, body []byte, chunked bool) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.target(), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "audio/wav")
	if chunked {
		req.ContentLength = -1
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func (e *paE2E) operatorHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + e.opKey}
}

func (e *paE2E) asynchronously(c *http.Client, headers map[string]string, body []byte) <-chan paE2EResult {
	out := make(chan paE2EResult, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, e.target(), bytes.NewReader(body))
		if err != nil {
			out <- paE2EResult{err: err}
			return
		}
		req.Header.Set("Content-Type", "audio/wav")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := c.Do(req)
		if err != nil {
			out <- paE2EResult{err: err}
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		body := map[string]any{}
		_ = json.Unmarshal(raw, &body)
		out <- paE2EResult{code: resp.StatusCode, body: body}
	}()
	return out
}

type paE2EResult struct {
	code int
	body map[string]any
	err  error
}

func (e *paE2E) activePAStreams() int {
	n := 0
	for _, stream := range e.d.audio.ActiveStreams() {
		if stream.AppID == "org.capsulad.pa" && stream.EndpointID == "audio.output.speaker" {
			n++
		}
	}
	return n
}

func (e *paE2E) waitNoPAStreams(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if e.activePAStreams() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("PA held %d playback stream(s) after %s", e.activePAStreams(), timeout)
}

func (e *paE2E) waitPAStream(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if e.activePAStreams() > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("PA never opened a playback stream")
}

// paWAV builds one RIFF/WAVE file the PA contract accepts. F32 selects the
// 32-bit IEEE-float encoding (2.3 MiB for a 6 s 48 kHz stereo clip, large
// enough to overflow the script host's inbound frame budget when two uploads
// are forwarded at once).
func paWAV(rate, channels, bits int, seconds float64, f32 bool) []byte {
	frames := int(float64(rate) * seconds)
	sampleBytes := bits / 8
	dataSize := frames * channels * sampleBytes
	out := make([]byte, 44+dataSize)
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(36+dataSize))
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16)
	format := uint16(1)
	if f32 {
		format = 3
	}
	binary.LittleEndian.PutUint16(out[20:22], format)
	binary.LittleEndian.PutUint16(out[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(out[24:28], uint32(rate))
	binary.LittleEndian.PutUint32(out[28:32], uint32(rate*channels*sampleBytes))
	binary.LittleEndian.PutUint16(out[32:34], uint16(channels*sampleBytes))
	binary.LittleEndian.PutUint16(out[34:36], uint16(bits))
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(dataSize))
	pos := 44
	for i := range frames {
		sign := int16(1)
		if (i/40)%2 == 1 {
			sign = -1
		}
		for range channels {
			if f32 {
				value := float32(sign) * 0.25
				binary.LittleEndian.PutUint32(out[pos:], math.Float32bits(value))
				pos += 4
			} else {
				binary.LittleEndian.PutUint16(out[pos:], uint16(sign*0x2000))
				pos += 2
			}
		}
	}
	return out
}

func hasNonZero(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return true
		}
	}
	return false
}

func assertPAOK(t *testing.T, code int, body map[string]any) {
	t.Helper()
	if code != http.StatusOK {
		t.Fatalf("play = %d %v, want 200", code, body)
	}
	if body["played"] != true {
		t.Fatalf("play result = %v, want played true", body)
	}
}

func (e *paE2E) assertPlayed(t *testing.T, before []byte, code int, body map[string]any) {
	t.Helper()
	assertPAOK(t, code, body)
	e.waitNoPAStreams(t, 3*time.Second)
	after := e.sim.LastOutput()
	if !hasNonZero(after) {
		t.Fatal("the simulator sink received no non-zero PCM")
	}
	if bytes.Equal(before, after) {
		t.Fatal("the simulator sink did not change after playback")
	}
}

func paDirectFrame(data []byte) []byte {
	out := protowire.AppendTag(nil, 1, protowire.BytesType)
	return protowire.AppendBytes(out, data)
}

// paDirectEvent decodes PlayEvent into its credit count and result payload.
func paDirectEvent(data []byte) (uint32, []byte, error) {
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
			payload, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return 0, nil, protowire.ParseError(n)
			}
			for len(payload) > 0 {
				num, wire, n := protowire.ConsumeTag(payload)
				if n < 0 {
					return 0, nil, protowire.ParseError(n)
				}
				payload = payload[n:]
				if num == 1 && wire == protowire.VarintType {
					value, n := protowire.ConsumeVarint(payload)
					if n < 0 {
						return 0, nil, protowire.ParseError(n)
					}
					frames += uint32(value)
					payload = payload[n:]
					continue
				}
				n = protowire.ConsumeFieldValue(num, wire, payload)
				if n < 0 {
					return 0, nil, protowire.ParseError(n)
				}
				payload = payload[n:]
			}
			data = data[n:]
		case 2:
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

func paDirectPlayed(result []byte) bool {
	for len(result) > 0 {
		number, typ, n := protowire.ConsumeTag(result)
		if n < 0 {
			return false
		}
		result = result[n:]
		if number == 1 && typ == protowire.VarintType {
			value, n := protowire.ConsumeVarint(result)
			return n >= 0 && value != 0
		}
		n = protowire.ConsumeFieldValue(number, typ, result)
		if n < 0 {
			return false
		}
		result = result[n:]
	}
	return false
}

func (e *paE2E) postSlow(t *testing.T, body []byte, chunk int, delay time.Duration) (int, map[string]any) {
	t.Helper()
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		for off := 0; off < len(body); off += chunk {
			end := off + chunk
			if end > len(body) {
				end = len(body)
			}
			if _, err := pw.Write(body[off:end]); err != nil {
				return
			}
			if end < len(body) {
				time.Sleep(delay)
			}
		}
	}()
	req, err := http.NewRequest(http.MethodPost, e.target(), pr)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1
	req.Header.Set("Content-Type", "audio/wav")
	for k, v := range e.operatorHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// TestPAHTTPProductionIntegration is the real HTTPS + AppBus + packaged
// MicroPython proof. It covers authorization, fixed-length and chunked
// uploads, a stalled HTTP/1.1 upload, a second clip waiting for the first, and
// cancellation during the drain, with simulator PCM and release evidence.
func TestPAHTTPProductionIntegration(t *testing.T) {
	e := newPAE2E(t)
	small := paWAV(16000, 1, 16, 0.4, false)

	t.Run("operator fixed-length", func(t *testing.T) {
		before := e.sim.LastOutput()
		code, body := e.postWith(t, e.client, e.operatorHeaders(), small, false)
		e.assertPlayed(t, before, code, body)
	})

	t.Run("operator chunked", func(t *testing.T) {
		before := e.sim.LastOutput()
		code, body := e.postWith(t, e.client, e.operatorHeaders(), small, true)
		e.assertPlayed(t, before, code, body)
	})

	t.Run("session with CSRF", func(t *testing.T) {
		jar, _ := cookiejar.New(nil)
		session := &http.Client{
			Jar:       jar,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
			Timeout:   30 * time.Second,
		}
		token := e.csrf(t, session)
		loginReq, _ := http.NewRequest(http.MethodPost, "https://"+e.addr+"/api/login",
			strings.NewReader(`{"password":"`+paE2EPassword+`"}`))
		loginReq.Header.Set("Content-Type", "application/json")
		loginReq.Header.Set("X-Capsulad-CSRF", token)
		loginResp, err := session.Do(loginReq)
		if err != nil {
			t.Fatal(err)
		}
		_ = loginResp.Body.Close()
		if loginResp.StatusCode != http.StatusOK {
			t.Fatalf("login = %d", loginResp.StatusCode)
		}
		token = e.csrf(t, session)
		before := e.sim.LastOutput()
		code, body := e.postWith(t, session, map[string]string{"X-Capsulad-CSRF": token}, small, false)
		e.assertPlayed(t, before, code, body)
	})

	t.Run("status key denied", func(t *testing.T) {
		code, body := e.postWith(t, e.client, map[string]string{"X-API-Key": e.statusKey}, small, false)
		if code != http.StatusForbidden {
			t.Fatalf("status-key play = %d %v, want 403", code, body)
		}
		if e.activePAStreams() != 0 {
			t.Fatal("status-key request opened a PA playback stream")
		}
	})

	t.Run("stalled HTTP/1.1 upload", func(t *testing.T) {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", e.addr,
			&tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		// At least twelve bytes make the PA parser reject the header while the
		// HTTP client is still nominally uploading ten more megabytes.
		prefix := []byte("not-a-riff-wav-file-and-then-the-client-pauses")
		header := fmt.Sprintf("POST /api/apps/org.capsulad.pa/play HTTP/1.1\r\nHost: %s\r\n"+
			"Authorization: Bearer %s\r\nContent-Type: audio/wav\r\nContent-Length: %d\r\n\r\n",
			e.addr, e.opKey, 10<<20)
		if _, err := io.WriteString(conn, header); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write(prefix); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
		if err != nil {
			t.Fatalf("stalled upload did not receive an early response: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("stalled upload = %d, want 400", resp.StatusCode)
		}
		if !resp.Close {
			t.Fatal("stalled upload rejection did not close the connection")
		}
		e.waitNoPAStreams(t, 3*time.Second)
	})

	t.Run("second clip waits and plays", func(t *testing.T) {
		firstBody := paWAV(16000, 1, 16, 2, false)
		secondBody := paWAV(48000, 2, 32, 6, true) // 2.3 MiB, over the host's inbound frame budget
		before := e.sim.LastOutput()
		first := e.asynchronously(e.client, e.operatorHeaders(), firstBody)
		e.waitPAStream(t, 5*time.Second)
		second := e.asynchronously(e.client, e.operatorHeaders(), secondBody)

		select {
		case got := <-first:
			if got.err != nil {
				t.Fatalf("first clip: %v", got.err)
			}
			assertPAOK(t, got.code, got.body)
		case <-time.After(15 * time.Second):
			t.Fatal("first clip did not finish")
		}
		select {
		case got := <-second:
			if got.err != nil {
				t.Fatalf("second clip: %v", got.err)
			}
			if got.code == http.StatusRequestEntityTooLarge {
				t.Fatalf("second clip was rejected as RESOURCE_EXHAUSTED: %v", got.body)
			}
			assertPAOK(t, got.code, got.body)
		case <-time.After(20 * time.Second):
			t.Fatal("second clip did not finish")
		}
		e.waitNoPAStreams(t, 3*time.Second)
		after := e.sim.LastOutput()
		if !hasNonZero(after) || bytes.Equal(before, after) {
			t.Fatal("the simulator sink did not receive the two clips")
		}
	})

	t.Run("cancel during drain releases playback", func(t *testing.T) {
		body := paWAV(48000, 2, 16, 4, false)
		ctx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.target(), bytes.NewReader(body))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "audio/wav")
		req.Header.Set("Authorization", "Bearer "+e.opKey)
		done := make(chan error, 1)
		go func() {
			resp, err := e.client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
			done <- err
		}()
		e.waitPAStream(t, 5*time.Second)
		// The 4 s clip is fully queued and the app is playing/draining by now;
		// the cancel must cut it off rather than wait for it to play out.
		time.Sleep(2500 * time.Millisecond)
		started := time.Now()
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("cancelled request did not return")
		}
		e.waitNoPAStreams(t, 3*time.Second)
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("cancel released playback after %s; it did not interrupt the drain", elapsed)
		}
	})

	t.Run("slow producer partial frames", func(t *testing.T) {
		body := paWAV(16000, 1, 16, 4.2, false) // >127 KiB, 8 KiB/s takes ~16 s
		before := e.sim.LastOutput()
		code, reply := e.postSlow(t, body, 8<<10, time.Second)
		e.assertPlayed(t, before, code, reply)
	})

	t.Run("direct call and HTTP share consumer credits", func(t *testing.T) {
		directBody := paWAV(16000, 1, 16, 2, false)
		conn := dialTest(t, e.d.cfg.SocketPath)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		stream, err := frameworkv1.NewAppBusServiceClient(conn).Call(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_Start{
			Start: &frameworkv1.RoutedCallStart{
				TargetAppId: "org.capsulad.pa",
				GrpcService: "org.capsulad.pa.v1.SpeakerService",
				RpcName:     "Play",
				TimeoutMs:   30000,
			},
		}}); err != nil {
			t.Fatal(err)
		}
		// Wait for the app's initial credit, then send one complete clip.
		gotCredit := false
		for !gotCredit {
			resp, err := stream.Recv()
			if err != nil {
				t.Fatalf("direct initial credit: %v", err)
			}
			if end := resp.GetEnd(); end != nil {
				t.Fatalf("direct call ended before credit: %d %s", end.GetGrpcStatusCode(), end.GetDetail())
			}
			frames, _, err := paDirectEvent(resp.GetResponseMessage())
			if err != nil {
				t.Fatal(err)
			}
			gotCredit = frames > 0
		}
		if err := stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_RequestMessage{
			RequestMessage: paDirectFrame(directBody),
		}}); err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_HalfClose{
			HalfClose: &frameworkv1.CallHalfClose{},
		}}); err != nil {
			t.Fatal(err)
		}
		e.waitPAStream(t, 5*time.Second)

		// The app is busy with the direct clip. The HTTP relay must wait for
		// consumer credit instead of forwarding its body into a full queue.
		code, reply := e.postWith(t, e.client, e.operatorHeaders(), paWAV(16000, 1, 16, 0.4, false), false)
		assertPAOK(t, code, reply)

		var directResult []byte
		for {
			resp, err := stream.Recv()
			if err != nil {
				t.Fatalf("direct result: %v", err)
			}
			if end := resp.GetEnd(); end != nil {
				if end.GetGrpcStatusCode() != 0 {
					t.Fatalf("direct call ended %d %s", end.GetGrpcStatusCode(), end.GetDetail())
				}
				break
			}
			_, result, err := paDirectEvent(resp.GetResponseMessage())
			if err != nil {
				t.Fatal(err)
			}
			if result != nil {
				directResult = result
			}
		}
		if !paDirectPlayed(directResult) {
			t.Fatal("direct clip did not report played=true")
		}
		e.waitNoPAStreams(t, 3*time.Second)
	})
}

func TestPAHTTPProductionIntegrationLongClip(t *testing.T) {
	if os.Getenv("PA_HTTP_E2E_LONG") == "" {
		t.Skip("set PA_HTTP_E2E_LONG=1 to run the 30 s / 11.5 MiB pacing probe")
	}
	e := newPAE2E(t)
	body := paWAV(48000, 2, 32, 30, true)
	before := e.sim.LastOutput()
	code, reply := e.postWith(t, e.client, e.operatorHeaders(), body, false)
	e.assertPlayed(t, before, code, reply)
}

func (e *paE2E) csrf(t *testing.T, c *http.Client) string {
	t.Helper()
	resp, err := c.Get("https://" + e.addr + "/api/session")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		CSRF string `json:"csrf"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.CSRF == "" {
		t.Fatal("session endpoint returned no CSRF token")
	}
	return out.CSRF
}
