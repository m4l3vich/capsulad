package vkclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// The gRPC Phrase/Speech stream ends with SpeechResponseFinish.result and no
// commands; the vendor's asynchronously-fetched phrase result carries them.
// The shipped agent's phrase_result_getter.cpp follows the same path: a
// GET /phrase/result with phrase_id, device_id, session_id and
// capabilities_hex, then a GET /stream?stream_id=... for the TTS body.
const (
	phraseResultPath    = "/phrase/result"
	phraseStreamPath    = "/stream"
	phraseResultTimeout = 10 * time.Second
	phraseResultMaxBody = 1 << 20
)

// phraseResultEnvelope is the subset of GET /phrase/result this app acts on.
type phraseResultEnvelope struct {
	Result *struct {
		Commands []phraseResultCommand `json:"commands"`
	} `json:"result"`
}

// phraseResultCommand is one JSON command. The result endpoint's command
// vocabulary is the same family as the protobuf CommandAny payloads, but the
// TTS command identifies its body by stream_id rather than by a URL.
type phraseResultCommand struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	URL          string          `json:"url"`
	StreamID     string          `json:"stream_id"`
	Event        string          `json:"event"`
	CallbackData json.RawMessage `json:"callback_data"`
}

// Result retrieves the commands the service stored for the phrase the gRPC
// stream just finished. The turn calls it after receiving the terminal finish
// and gives it its own deadline, so a slow result fetch cannot be mistaken for
// a missing terminal frame. It is idempotent.
func (s *Speech) Result(ctx context.Context) ([]*Event, error) {
	if s.client == nil || s.phraseID == "" || s.resultFetched {
		return nil, nil
	}
	s.resultFetched = true
	events, detail := s.fetchPhraseResult(ctx)
	if detail != "" {
		return events, errors.New(detail)
	}
	return events, nil
}

// fetchPhraseResult performs the one HTTP request. Every error is reduced to
// a failure class and HTTP status: the transport may quote a redirect Location
// that preserves the result query, and the result query carries identifiers
// that must not reach the log.
func (s *Speech) fetchPhraseResult(ctx context.Context) ([]*Event, string) {
	if s.client == nil || s.phraseID == "" {
		return nil, ""
	}

	session := s.client.Session()
	endpoint := *s.client.base
	endpoint.Path = phraseResultPath
	query := url.Values{}
	query.Set("phrase_id", s.phraseID)
	query.Set("capabilities_hex", s.capabilitiesHex())
	query.Set("device_id", s.client.cfg.DeviceID)
	query.Set("session_id", session.SessionID)
	endpoint.RawQuery = query.Encode()

	response, err := s.client.transport.Do(ctx, HTTPRequest{
		Method: "GET",
		URL:    endpoint.String(),
		Header: map[string]string{
			"Accept":        "application/json",
			"Authorization": bearer(session.SessionSecret),
		},
		// The result query carries the phrase, device and session ids. A
		// redirect may expose them in a cross-origin Location even though the
		// SDK strips headers on that hop.
		NoRedirects: true,
	}, phraseResultTimeout)
	if err != nil {
		return nil, "the phrase result request failed"
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, fmt.Sprintf("the phrase result request returned HTTP %d", response.StatusCode)
	}
	if len(response.Body) > phraseResultMaxBody {
		return nil, fmt.Sprintf("the phrase result exceeds %d MiB", phraseResultMaxBody>>20)
	}
	var decoded phraseResultEnvelope
	if err := json.Unmarshal(response.Body, &decoded); err != nil {
		return nil, "the phrase result is not valid JSON"
	}
	return s.resultEvents(decoded), ""
}

// capabilitiesHex is the raw token the result endpoint expects. The init field
// is JSON; the result endpoint takes the same token under its own query key.
// An operator-provided payload without that key falls back to the model default
// rather than sending an empty capability string.
func (s *Speech) capabilitiesHex() string {
	var payload struct {
		CapabilitiesHex string `json:"capabilities_hex"`
	}
	if json.Unmarshal(s.opts.CapabilitiesJSON, &payload) == nil && payload.CapabilitiesHex != "" {
		return payload.CapabilitiesHex
	}
	return DefaultCapabilitiesHex
}

// streamURL turns a TTS command's stream_id into the body URL the media fetcher
// downloads. The vendor's media_fetcher identifies the body the same way.
func (s *Speech) streamURL(streamID string) string {
	endpoint := *s.client.base
	endpoint.Path = phraseStreamPath
	query := url.Values{}
	query.Set("stream_id", streamID)
	endpoint.RawQuery = query.Encode()
	return endpoint.String()
}

// resultEvents maps the result JSON commands onto the same Event values the
// gRPC decoder produces. The last text command also becomes text_so_far so the
// turn's completed line carries the spoken text when a TTS command follows it.
func (s *Speech) resultEvents(decoded phraseResultEnvelope) []*Event {
	if decoded.Result == nil {
		return nil
	}
	var events []*Event
	var commands []Command
	var text string
	for _, command := range decoded.Result.Commands {
		mapped := Command{TypeURL: command.Type, CallbackData: []byte(command.CallbackData)}
		switch strings.ToLower(command.Type) {
		case "text", "show_text":
			mapped.Kind = CommandShowText
			mapped.Text = command.Text
			text = command.Text
		case "tts":
			mapped.Kind = CommandPlayURL
			mapped.Text = command.Text
			if command.StreamID != "" {
				mapped.URL = s.streamURL(command.StreamID)
			} else {
				mapped.URL = command.URL
			}
		case "play_url":
			mapped.Kind = CommandPlayURL
			mapped.URL = command.URL
		case "play_urls_preload":
			mapped.Kind = CommandPlayURLsPreload
			mapped.URL = command.URL
		case "bg_music":
			mapped.Kind = CommandBgMusic
			mapped.URL = command.URL
		case "bg_music_preload":
			mapped.Kind = CommandBgMusicPreload
			mapped.URL = command.URL
		case "send_event":
			mapped.Kind = CommandSendEvent
			mapped.Event = command.Event
		case "listen":
			mapped.Kind = CommandListen
		default:
			mapped.Kind = CommandUnknown
		}
		commands = append(commands, mapped)
	}
	if text != "" {
		events = append(events, &Event{Kind: EventTextSoFar, Text: text})
	}
	if len(commands) > 0 {
		events = append(events, &Event{Kind: EventCommands, Commands: commands})
	}
	return events
}
