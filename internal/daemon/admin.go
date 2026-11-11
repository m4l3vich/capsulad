package daemon

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"capsulad/internal/backend"
)

type httpAdmin struct {
	srv  *http.Server
	path string
}

// startAdmin exposes local development injection hooks on a 0600 Unix socket.
// It is not part of the framework API and is disabled on production targets.
func (d *Daemon) startAdmin(path string) *httpAdmin {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		d.logs.Warnf("admin", "", "dev admin socket: %v", err)
		return nil
	}
	_ = os.Remove(path)
	lis, err := net.Listen("unix", path)
	if err != nil {
		d.logs.Warnf("admin", "", "dev admin listen: %v", err)
		return nil
	}
	_ = os.Chmod(path, 0o600)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, r *http.Request) {
		out := map[string]any{
			"platform": d.dev.Platform(),
			"settings": d.user.Get().Values,
			"routing":  d.router.AllSelections(),
		}
		if s, ok := d.dev.(*backend.Simulator); ok {
			out["segment"] = s.SegmentSnapshot()
			out["ring"] = s.RingSnapshot()
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("POST /wakeword", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		act, ok := d.wake.Trigger("api", req.Model)
		writeJSON(w, map[string]any{"triggered": ok, "activation": act})
	})
	mux.HandleFunc("POST /input/gesture", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID    string `json:"id"`
			Phase string `json:"phase"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		d.simTouchSignal(backend.InputEvent{Gesture: &backend.GestureEvent{ID: req.ID, Phase: req.Phase}})
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /input/key", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Key     string `json:"key"`
			Pressed bool   `json:"pressed"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		d.simTouchSignal(backend.InputEvent{Key: &backend.KeyEvent{Key: req.Key, Pressed: req.Pressed}})
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /input/raw", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			X        float32 `json:"x"`
			Y        float32 `json:"y"`
			Touching bool    `json:"touching"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		d.simTouchSignal(backend.InputEvent{Report: &backend.RawReport{
			Contacts:         []backend.Contact{{ID: 0, X: req.X, Y: req.Y, Touching: req.Touching}},
			CoordinatesValid: true, SampleFormat: "simulator.contact.v1",
		}})
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /audio/pcm", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if s, ok := d.dev.(*backend.Simulator); ok {
			s.InjectPCM(body)
		}
		writeJSON(w, map[string]any{"bytes": len(body)})
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(lis) }()
	d.logs.Infof("admin", "", "development admin socket %s", path)
	return &httpAdmin{srv: srv, path: path}
}

func (a *httpAdmin) Close() {
	if a == nil {
		return
	}
	_ = a.srv.Close()
	_ = os.Remove(a.path)
}

func (d *Daemon) simTouchSignal(e backend.InputEvent) {
	e.MonotonicNS = d.clock.NowNS()
	d.input.Handle(e)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.MarshalIndent(v, "", "  ")
	_, _ = w.Write(append(b, '\n'))
}
