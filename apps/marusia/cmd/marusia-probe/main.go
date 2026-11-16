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
//
// What it reports: the init phrase id and QoS, the chunk accounting, every
// text_so_far, every command, the play_url, and then the HTTP status,
// Content-Type, size and first bytes of that URL — the TTS media type.
package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

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
	speech := vkclient.SpeechOptions{
		Format:           vkclient.FormatWave,
		Timezone:         "Europe/Moscow",
		HWID:             env("MARUSIA_PROBE_HWID", "9F25725B"),
		MediaState:       clientpb.MediaState_MEDIA_STATE_STOP,
		StatusIntervalMS: 1000,
		CapabilitiesJSON: capabilities,
		ClientDataJSON:   clientData,
	}
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
	log.Printf("probe: init keyword_mode=%s capabilities=%dB client_data=%dB",
		speech.KeywordMode(), len(capabilities), len(clientData))

	stream, err := client.OpenSpeech(ctx, speech)
	if err != nil {
		log.Fatalf("probe: open speech: %v", err)
	}
	defer stream.Close()

	type frame struct {
		text     string
		playURL  string
		commands []string
		finish   string
	}
	result := &frame{}
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
			switch event.Kind {
			case vkclient.EventInit:
				log.Printf("probe: init phrase=%s chunk_hint=%dms status_interval=%dms",
					event.PhraseID, event.ChunkLengthHintMS, event.StatusIntervalMS)
			case vkclient.EventStatus:
				log.Printf("probe: status chunks=%d audio=%dms", event.Status.ChunksReceived,
					event.Status.AudioReceivedMS)
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
	select {
	case <-done:
	case <-time.After(45 * time.Second):
		log.Printf("probe: timed out waiting for the terminal frame")
	}

	log.Printf("probe: RESULT text=%q finish=%q commands=%v play_url=%q",
		result.text, result.finish, result.commands, redact.URL(result.playURL))
	if result.playURL == "" {
		log.Printf("probe: no play_url; the TTS media type could not be observed")
		return
	}
	fetch(result.playURL)
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
