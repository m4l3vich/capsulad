// Command marusia-probe is the opt-in live probe for the Marusia voice path. It
// is never built or run by `go test ./...`; it exists so an operator can prove
// the recovered protocol against the real service with real credentials.
//
// It reads everything from the environment and never writes a credential
// anywhere:
//
//	MARUSIA_PROBE_ENDPOINT  base URL, default https://vc.go.mail.ru
//	MARUSIA_PROBE_SESSION   session id from the vendor session
//	MARUSIA_PROBE_SECRET    session secret (bearer); required
//	MARUSIA_PROBE_DEVICE    device id, e.g. :c:d:capsula_mini:<hex>
//	MARUSIA_PROBE_SERIAL    SoC serial, sent as x-device-sn
//	MARUSIA_PROBE_WAV       path to a 16 kHz mono 16-bit PCM WAV to send
//	MARUSIA_PROBE_PACE      optional per-20 ms-chunk delay, e.g. 20ms
//	MARUSIA_PROBE_CAPABILITIES_JSON  init capabilities payload; defaults to the
//	                        recovered Capsula Mini token
//	MARUSIA_PROBE_CLIENT_DATA_JSON   init client_data payload; empty by default
//	MARUSIA_PROBE_KEYWORD_MODE       no_kw (default) or kw_params for a whole
//	                        utterance that starts with the wake word
//	MARUSIA_PROBE_ACTIVATION_TYPE    init client_options.activation_type; empty
//	                        by default, otherwise one of the vendor's own wire
//	                        values: listen_command, keyword, button, flowmode
//	MARUSIA_PROBE_SDK_RESULT when "1", call the production Speech.Result after
//	                        the stream ends and print the recovered events
//	MARUSIA_PROBE_RESULT     when "1", also GET /phrase/result and print its
//	                        status and byte count (the body is not logged)
//	MARUSIA_PROBE_RESULT_OUT optional new 0600 file for the sensitive raw
//	                        result body; refuses to overwrite an existing file
//	MARUSIA_PROBE_STREAM     when "1", authenticate and GET the first tts
//	                        stream_id, then print its Content-Type and decoded
//	                        duration through the production media path
//
// What it reports: the init phrase id and QoS, the chunk accounting, every
// text_so_far, every command, the play_url, and then the HTTP status,
// Content-Type, size and first bytes of that URL — the TTS media type.
package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"strings"
	"time"

	"capsulad/apps/marusia/internal/media"
	"capsulad/apps/marusia/internal/redact"
	"capsulad/apps/marusia/internal/vkclient"
	"capsulad/apps/marusia/internal/vkproto/clientpb"
	"capsulad/sdk/capsulad"
)

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// speechOptions is the init the probe sends, assembled from the environment:
// the app's own defaults (wave framing, Europe/Moscow, the recovered Capsula
// Mini capability token) plus the operator knobs. It is separate from main so a
// test can pin the wire values without a credential or a live service.
func speechOptions(capabilities, clientData []byte) vkclient.SpeechOptions {
	return vkclient.SpeechOptions{
		Format:           vkclient.FormatWave,
		Timezone:         "Europe/Moscow",
		ActivationType:   env("MARUSIA_PROBE_ACTIVATION_TYPE", ""),
		HWID:             env("MARUSIA_PROBE_HWID", "9F25725B"),
		MediaState:       clientpb.MediaState_MEDIA_STATE_STOP,
		StatusIntervalMS: 1000,
		CapabilitiesJSON: capabilities,
		ClientDataJSON:   clientData,
	}
}

func main() {
	base := env("MARUSIA_PROBE_ENDPOINT", "https://vc.go.mail.ru")
	sessionID := env("MARUSIA_PROBE_SESSION", "")
	secret := env("MARUSIA_PROBE_SECRET", "")
	deviceID := env("MARUSIA_PROBE_DEVICE", "")
	serial := env("MARUSIA_PROBE_SERIAL", "")
	wavPath := env("MARUSIA_PROBE_WAV", "")
	if secret == "" || sessionID == "" || wavPath == "" {
		log.Fatalf("MARUSIA_PROBE_SESSION, MARUSIA_PROBE_SECRET and MARUSIA_PROBE_WAV are required")
	}
	pace, err := time.ParseDuration(env("MARUSIA_PROBE_PACE", "0s"))
	if err != nil {
		log.Fatalf("MARUSIA_PROBE_PACE: %v", err)
	}
	pcm, err := readWAV(wavPath)
	if err != nil {
		log.Fatalf("wav: %v", err)
	}

	client, err := vkclient.New(vkclient.Config{
		BaseURL:  base,
		Serial:   serial,
		DeviceID: deviceID,
		Session:  vkclient.Session{SessionID: sessionID, SessionSecret: secret},
	})
	if err != nil {
		log.Fatalf("client: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	log.Printf("probe: endpoint=%s session=%s pcm=%d bytes", base, redact.SessionID(sessionID), len(pcm))
	if err := client.Authorize(ctx); err != nil {
		log.Fatalf("probe: authorize: %v", err)
	}
	log.Printf("probe: AuthReady accepted the session")

	capabilities := []byte(env("MARUSIA_PROBE_CAPABILITIES_JSON", string(vkclient.DefaultCapabilitiesJSON)))
	if err := vkclient.ValidateCapabilitiesJSON(capabilities); err != nil {
		log.Fatalf("probe: capabilities: %v", err)
	}
	clientData := []byte(env("MARUSIA_PROBE_CLIENT_DATA_JSON", ""))
	if err := vkclient.ValidateClientDataJSON(clientData); err != nil {
		log.Fatalf("probe: client_data: %v", err)
	}
	keywordMode := env("MARUSIA_PROBE_KEYWORD_MODE", "no_kw")
	speech := speechOptions(capabilities, clientData)
	switch keywordMode {
	case "kw_params":
		// Opt-in: the probe sends a whole utterance, so a prompt that starts
		// with the wake word can be confirmed. The default stays no_kw, which
		// is what the app's touch and API turns always send, and what its
		// wake-word turns send unless keyword_mode=kw_params.
		speech.Keyword = env("MARUSIA_PROBE_KEYWORD", "marusya")
		speech.KeywordInAudio = true
	case "no_kw":
	default:
		log.Fatalf("MARUSIA_PROBE_KEYWORD_MODE: want kw_params or no_kw, got %q", keywordMode)
	}
	log.Printf("probe: init keyword_mode=%s activation_type=%q capabilities=%dB client_data=%dB",
		speech.KeywordMode(), speech.ActivationType, len(capabilities), len(clientData))

	stream, err := client.OpenSpeech(ctx, speech)
	if err != nil {
		log.Fatalf("probe: open speech: %v", err)
	}
	defer stream.Close()

	type frame struct {
		phraseID string
		text     string
		playURL  string
		commands []string
		finish   string
	}
	result := &frame{}
	consume := func(event *vkclient.Event) {
		switch event.Kind {
		case vkclient.EventInit:
			result.phraseID = event.PhraseID
			log.Printf("probe: init phrase=%s chunk_hint=%dms status_interval=%dms",
				event.PhraseID, event.ChunkLengthHintMS, event.StatusIntervalMS)
		case vkclient.EventStatus:
			log.Printf("probe: status chunks=%d audio=%dms status_num=%d", event.Status.ChunksReceived,
				event.Status.AudioReceivedMS, event.Status.StatusNum)
		case vkclient.EventTextSoFar:
			result.text = event.Text
			log.Printf("probe: text_so_far %q", event.Text)
		case vkclient.EventCommands:
			for _, command := range event.Commands {
				result.commands = append(result.commands, command.Kind.String())
				if command.URL != "" {
					result.playURL = command.URL
					log.Printf("probe: command %s url=%s", command.Kind, redact.URL(command.URL))
					continue
				}
				log.Printf("probe: command %s text=%q event=%q", command.Kind, command.Text, command.Event)
			}
		case vkclient.EventFinish:
			result.finish = event.Finish.String()
			log.Printf("probe: finish %s abort=%q", event.Finish, event.AbortReason)
		case vkclient.EventEOS:
			log.Printf("probe: eos")
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			event, err := stream.Recv()
			if err == io.EOF {
				log.Printf("probe: stream ended cleanly")
				return
			}
			if err != nil {
				log.Printf("probe: stream error: %v", err)
				return
			}
			consume(event)
			if event.Kind == vkclient.EventFinish {
				return
			}
		}
	}()

	for offset := 0; offset < len(pcm); offset += vkclient.FrameBytes {
		end := offset + vkclient.FrameBytes
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := stream.SendAudio(pcm[offset:end]); err != nil {
			log.Printf("probe: send: %v", err)
			break
		}
		if pace > 0 {
			time.Sleep(pace)
		}
	}
	if err := stream.EOS(); err != nil {
		log.Printf("probe: eos: %v", err)
	}
	timedOut := false
	select {
	case <-done:
	case <-time.After(45 * time.Second):
		timedOut = true
		log.Printf("probe: timed out waiting for the terminal frame")
		// Cancel the stream and join the receiver before any result work so
		// result.phraseID cannot be read while the receiver is still writing.
		stream.Close()
		<-done
	}
	if timedOut {
		log.Printf("probe: giving up after the terminal-frame timeout")
		return
	}

	if env("MARUSIA_PROBE_SDK_RESULT", "") == "1" && result.finish == vkclient.FinishResult.String() {
		// Match the turn's order: release the gRPC stream after the terminal
		// finish, then fetch the stored result over HTTP.
		stream.Close()
		events, err := stream.Result(ctx)
		if err != nil {
			log.Printf("probe: sdk result: %v", err)
		}
		for _, event := range events {
			consume(event)
		}
	}
	if result.phraseID != "" && env("MARUSIA_PROBE_RESULT", "") == "1" {
		fetchPhraseResult(base, sessionID, secret, deviceID, result.phraseID)
	}

	log.Printf("probe: RESULT text=%q finish=%q commands=%v play_url=%q",
		result.text, result.finish, result.commands, redact.URL(result.playURL))
	if result.playURL == "" {
		log.Printf("probe: no play_url; the TTS media type could not be observed")
		return
	}
	fetch(result.playURL)
}

// writeCaptured creates a new 0600 file for an explicitly requested sensitive
// raw payload. It refuses to overwrite an existing path, so a file created with
// broader permissions cannot be retained by accident.
func writeCaptured(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(0600); err != nil {
		return err
	}
	_, err = file.Write(data)
	return err
}

// fetchPhraseResult asks the vendor's HTTP result endpoint for the commands of
// a completed phrase. The stock agent's phrase_result_getter uses the same
// endpoint; this probe call exists to test whether the gRPC stream's terminal
// finish is the whole reply or only a signal to fetch the result separately.
func fetchPhraseResult(base, sessionID, secret, deviceID, phraseID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := capsulad.OpenHTTP(capsulad.FromEnv(), capsulad.HTTPOptions{
		Roots:           "system",
		UserAgent:       "voice-agent/1",
		MaxBody:         1 << 20,
		FollowRedirects: 0,
		ConnectTimeout:  10 * time.Second,
		ReadTimeout:     20 * time.Second,
	})
	if err != nil {
		log.Printf("probe: result http: %v", err)
		return
	}
	defer client.Close()
	query := url.Values{}
	query.Set("phrase_id", phraseID)
	query.Set("capabilities_hex", vkclient.DefaultCapabilitiesHex)
	query.Set("device_id", deviceID)
	query.Set("session_id", sessionID)
	resultURL := strings.TrimRight(base, "/") + "/phrase/result?" + query.Encode()
	response, err := client.Do(ctx, capsulad.HTTPRequest{
		Method: "GET",
		URL:    resultURL,
		Header: map[string]string{
			"Accept":        "application/json",
			"Authorization": "Bearer " + secret,
		},
		Timeout: 20 * time.Second,
	})
	if err != nil {
		log.Printf("probe: result: request failed")
		return
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		log.Printf("probe: result status=%d", response.StatusCode)
		return
	}
	if out := env("MARUSIA_PROBE_RESULT_OUT", ""); out != "" {
		if err := writeCaptured(out, response.Body); err != nil {
			log.Printf("probe: result capture: %v", err)
		}
	}
	log.Printf("probe: result status=%d bytes=%d", response.StatusCode, len(response.Body))
	if env("MARUSIA_PROBE_STREAM", "") != "1" {
		return
	}
	var parsed struct {
		Result struct {
			Commands []struct {
				Type     string `json:"type"`
				StreamID string `json:"stream_id"`
			} `json:"commands"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body, &parsed); err != nil {
		log.Printf("probe: result parse: %v", err)
		return
	}
	for _, command := range parsed.Result.Commands {
		if command.Type != "tts" || command.StreamID == "" {
			continue
		}
		query := url.Values{}
		query.Set("stream_id", command.StreamID)
		streamURL := strings.TrimRight(base, "/") + "/stream?" + query.Encode()
		fetcher := &media.Fetch{MaxBytes: media.MaxBody}
		body, contentType, err := fetcher.FetchMedia(ctx, streamURL, "Bearer "+secret)
		if err != nil {
			log.Printf("probe: stream %s: %s", command.StreamID, redact.ErrorText(err, streamURL))
			continue
		}
		pcm, err := media.DecodeMedia(body, contentType)
		if err != nil {
			log.Printf("probe: stream %s decode: %v", command.StreamID, err)
			continue
		}
		log.Printf("probe: stream content-type=%q bytes=%d decoded_bytes=%d ms=%d",
			contentType, len(body), len(pcm), len(pcm)/2*1000/media.PlaybackRate)
	}
}

// fetch reports the TTS media header through the same native HTTP client the
// app uses, so the probe exercises production egress rather than a second
// stack.
func fetch(rawURL string) {
	client, err := capsulad.OpenHTTP(capsulad.FromEnv(), capsulad.HTTPOptions{
		Roots:           "system",
		UserAgent:       "voice-agent/1",
		MaxBody:         1 << 20,
		FollowRedirects: 4,
		ConnectTimeout:  20 * time.Second,
		ReadTimeout:     30 * time.Second,
	})
	if err != nil {
		log.Printf("probe: media client: %v", err)
		return
	}
	defer client.Close()
	resp, err := client.Do(context.Background(), capsulad.HTTPRequest{
		Method:  "GET",
		URL:     rawURL,
		Timeout: 30 * time.Second,
	})
	if err != nil {
		// The native error may quote the signed request URL.
		log.Printf("probe: media fetch %s: %s", redact.URL(rawURL), redact.ErrorText(err, rawURL))
		return
	}
	head := resp.Body
	if len(head) > 32 {
		head = head[:32]
	}
	log.Printf("probe: MEDIA status=%d content_type=%q bytes=%d first=%s",
		resp.StatusCode, resp.Header["content-type"], len(resp.Body), hex.EncodeToString(head))
	pcm, err := media.DecodeMedia(resp.Body, resp.Header["content-type"])
	if err != nil {
		log.Printf("probe: MEDIA decode: %v", err)
		return
	}
	log.Printf("probe: MEDIA decoded bytes=%d ms=%d", len(pcm), len(pcm)/2*1000/media.PlaybackRate)
}

// readWAV returns the data chunk of a 16 kHz mono 16-bit PCM WAV.
func readWAV(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < 44 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		return nil, fmt.Errorf("not a RIFF/WAVE file")
	}
	rate := int(binary.LittleEndian.Uint32(raw[24:28]))
	channels := int(binary.LittleEndian.Uint16(raw[22:24]))
	bits := int(binary.LittleEndian.Uint16(raw[34:36]))
	if channels != 1 || bits != 16 || rate != vkclient.SampleRate {
		return nil, fmt.Errorf("want %d Hz mono 16-bit, got %d Hz %d ch %d bit",
			vkclient.SampleRate, rate, channels, bits)
	}
	for offset := 12; offset+8 <= len(raw); {
		id := string(raw[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(raw[offset+4 : offset+8]))
		if id == "data" {
			end := offset + 8 + size
			if end > len(raw) {
				end = len(raw)
			}
			return raw[offset+8 : end], nil
		}
		offset += 8 + size + size%2
	}
	return nil, fmt.Errorf("no data chunk")
}
