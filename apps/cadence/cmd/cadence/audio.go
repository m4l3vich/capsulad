package main

import (
	"context"

	"capsulad/sdk/capsulad"

	"capsulad/apps/cadence/internal/opus"
	"capsulad/apps/cadence/internal/turn"
)

// frameworkAudio adapts the SDK's audio helpers to the turn engine's seams.
// Everything goes through the normal presentation/capture/audio services; the
// app never touches a device node.
type frameworkAudio struct{ client *capsulad.Client }

func (a frameworkAudio) OpenMic(ctx context.Context, activationID, conversationID string) (turn.MicStream, error) {
	return a.client.OpenMic(ctx, activationID, conversationID)
}

func (a frameworkAudio) OpenPlayback(ctx context.Context, endpoint string, format capsulad.AudioFormat,
	conversationID string) (turn.Playback, error) {
	return a.client.OpenPlayback(ctx, endpoint, format, conversationID)
}

func (a frameworkAudio) BeginConversation(ctx context.Context, activationID string) (turn.Conversation, error) {
	return a.client.BeginConversation(ctx, activationID)
}

// opusCodecs is the real codec: 16 kHz mono 20 ms uplink packets and a fresh
// 48 kHz decoder per reply stream.
type opusCodecs struct{}

func (opusCodecs) NewEncoder() (turn.Encoder, error) { return opus.NewEncoder() }

func (opusCodecs) NewDecoder(channels int) (turn.Decoder, error) { return opus.NewDecoder(channels) }
