package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
)

type wakewordPageData struct {
	Samples []*frameworkv1.WakewordSample
	Models  []*frameworkv1.WakewordModelInfo
}

func (s *Server) wakewordPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxTimeout(r)
	defer cancel()
	data := wakewordPageData{}
	if resp, err := frameworkv1.NewAssistantServiceClient(s.Conn).ListWakewordSamples(ctx, &frameworkv1.ListWakewordSamplesRequest{}); err == nil {
		data.Samples = resp.Samples
	}
	if resp, err := frameworkv1.NewAssistantServiceClient(s.Conn).ListWakewordModels(ctx, &frameworkv1.ListWakewordModelsRequest{}); err == nil {
		data.Models = resp.Models
	}
	p := s.base(w, r, "Wakeword", "wakeword")
	p.Data = data
	s.render(w, "wakeword", p)
}

func (s *Server) wakewordRecord(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20e9)
	defer cancel()
	label := r.PostFormValue("label")
	ms, _ := strconv.ParseUint(r.PostFormValue("duration_ms"), 10, 32)
	resp, err := frameworkv1.NewAssistantServiceClient(s.Conn).RecordWakewordSample(ctx,
		&frameworkv1.RecordWakewordSampleRequest{Label: label, DurationMs: uint32(ms)})
	if err != nil {
		if strings.Contains(r.Header.Get("Accept"), "application/json") {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		s.redirect(w, r, "/wakeword", "", err.Error())
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		writeJSON(w, resp)
		return
	}
	s.redirect(w, r, "/wakeword", fmt.Sprintf("recorded %s sample", label), "")
}

func (s *Server) wakewordDelete(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxTimeout(r)
	defer cancel()
	_, err := frameworkv1.NewAssistantServiceClient(s.Conn).DeleteWakewordSample(ctx,
		&frameworkv1.DeleteWakewordSampleRequest{SampleId: r.PostFormValue("sample_id")})
	if err != nil {
		if strings.Contains(r.Header.Get("Accept"), "application/json") {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		s.redirect(w, r, "/wakeword", "", err.Error())
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		writeJSON(w, struct{}{})
		return
	}
	s.redirect(w, r, "/wakeword", "sample deleted", "")
}

// wakewordTrain proxies the device's server-streaming training call to the
// browser as NDJSON. The browser only sends the explicit sample selection; the
// takes, embeddings, fitting and quality gates all stay on the device.
func (s *Server) wakewordTrain(w http.ResponseWriter, r *http.Request) {
	word := strings.TrimSpace(r.PostFormValue("word"))
	ids := func(name string) []string {
		var out []string
		for _, id := range strings.Split(r.PostFormValue(name), ",") {
			if id = strings.TrimSpace(id); id != "" {
				out = append(out, id)
			}
		}
		return out
	}
	seed, _ := strconv.ParseUint(r.PostFormValue("seed"), 10, 64)
	req := &frameworkv1.TrainWakewordClassifierRequest{
		DisplayName:     word,
		Language:        "ru",
		Seed:            seed,
		WordSampleIds:   ids("word_ids"),
		RoomSampleIds:   ids("room_ids"),
		SpeechSampleIds: ids("speech_ids"),
		FalseSampleIds:  ids("false_ids"),
	}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	// Training legitimately runs for minutes on the Mini, longer than the
	// server's normal response deadline.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	enc := json.NewEncoder(w)
	flusher, _ := w.(http.Flusher)
	emit := func(ev any) bool {
		if err := enc.Encode(ev); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	if word == "" {
		w.WriteHeader(http.StatusBadRequest)
		emit(trainEvent{Stage: "error", Log: "name the wake word before training"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	stream, err := frameworkv1.NewAssistantServiceClient(s.Conn).TrainWakewordClassifier(ctx, req)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		emit(trainEvent{Stage: "error", Log: trainErrorMessage(err)})
		return
	}
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			emit(trainEvent{Stage: "error", Log: trainErrorMessage(err)})
			return
		}
		out := trainEvent{Stage: ev.Stage, Fraction: ev.Fraction, Log: ev.Log}
		if ev.Classifier != nil {
			out.Classifier = &trainClassifier{
				ModelID:         ev.Classifier.ModelId,
				DisplayName:     ev.Classifier.DisplayName,
				Language:        ev.Classifier.Language,
				Threshold:       ev.Classifier.Threshold,
				ContextFrames:   ev.Classifier.ContextFrames,
				EmbeddingDim:    ev.Classifier.EmbeddingDim,
				ProjectionWidth: ev.Classifier.ProjectionWidth,
				HiddenWidth:     ev.Classifier.HiddenWidth,
			}
		}
		if !emit(out) {
			// Browser closed the tab; cancel the RPC it can no longer receive.
			cancel()
			return
		}
	}
}

// trainEvent is the NDJSON line the browser reads. Stage "error" is terminal.
type trainEvent struct {
	Stage      string           `json:"stage"`
	Fraction   float32          `json:"fraction"`
	Log        string           `json:"log,omitempty"`
	Classifier *trainClassifier `json:"classifier,omitempty"`
}

type trainClassifier struct {
	ModelID         string  `json:"model_id"`
	DisplayName     string  `json:"display_name"`
	Language        string  `json:"language"`
	Threshold       float32 `json:"threshold"`
	ContextFrames   uint32  `json:"context_frames"`
	EmbeddingDim    uint32  `json:"embedding_dim"`
	ProjectionWidth uint32  `json:"projection_width"`
	HiddenWidth     uint32  `json:"hidden_width"`
}

// trainErrorMessage removes gRPC transport noise so the page can show the
// device's own actionable explanation.
func trainErrorMessage(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "training timed out after five minutes; the installed model is unchanged"
	}
	if errors.Is(err, context.Canceled) {
		return "training cancelled; the installed model is unchanged"
	}
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.DeadlineExceeded:
			return "training timed out after five minutes; the installed model is unchanged"
		case codes.Canceled:
			return "training cancelled; the installed model is unchanged"
		}
		if st.Message() != "" {
			return st.Message()
		}
	}
	return err.Error()
}

func (s *Server) wakewordTrainerJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	_, _ = w.Write([]byte(trainerJS))
}

func (s *Server) wakewordSamplesJSON(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxTimeout(r)
	defer cancel()
	resp, err := frameworkv1.NewAssistantServiceClient(s.Conn).ListWakewordSamples(ctx, &frameworkv1.ListWakewordSamplesRequest{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, resp)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}
