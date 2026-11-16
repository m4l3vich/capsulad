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
	"net/http"
	"os"
	"strings"
	"time"

	"capsula/apps/marusia/internal/vkclient"
	"capsula/apps/marusia/internal/vkproto/clientpb"
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

	log.Printf("probe: endpoint=%s session=%s… pcm=%d bytes", base, short(sessionID), len(pcm))
	if err := client.Authorize(ctx); err != nil {
		log.Fatalf("probe: authorize: %v", err)
	}
	log.Printf("probe: AuthReady accepted the session")

	stream, err := client.OpenSpeech(ctx, vkclient.SpeechOptions{
		Format:           vkclient.FormatWave,
		Timezone:         "Europe/Moscow",
		Keyword:          env("MARUSIA_PROBE_KEYWORD", "marusya"),
		HWID:             env("MARUSIA_PROBE_HWID", "9F25725B"),
		MediaState:       clientpb.MediaState_MEDIA_STATE_STOP,
		StatusIntervalMS: 1000,
	})
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
						log.Printf("probe: command %s url=%s", command.Kind, command.URL)
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
		result.text, result.finish, result.commands, result.playURL)
	if result.playURL == "" {
		log.Printf("probe: no play_url; the TTS media type could not be observed")
		return
	}
	fetch(result.playURL)
}

func fetch(rawURL string) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		log.Printf("probe: media request: %v", err)
		return
	}
	req.Header.Set("User-Agent", "voice-agent/1")
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("probe: media fetch: %v", err)
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		log.Printf("probe: media read: %v", err)
		return
	}
	head := body
	if len(head) > 32 {
		head = head[:32]
	}
	log.Printf("probe: MEDIA status=%d content_type=%q bytes=%d first=%s",
		resp.StatusCode, resp.Header.Get("Content-Type"), len(body), hex.EncodeToString(head))
}

func short(value string) string {
	if len(value) <= 8 {
		return "…"
	}
	return value[:8]
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
