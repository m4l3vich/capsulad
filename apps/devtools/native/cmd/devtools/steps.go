package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"capsulad/sdk/capsulad"
)

type subsystemBackend struct {
	Subsystem string `json:"subsystem"`
	Status    string `json:"status"`
	Detail    string `json:"detail"`
}
type deviceInfo struct {
	DeviceID          string             `json:"deviceId"`
	Model             string             `json:"model"`
	DaemonVersion     string             `json:"daemonVersion"`
	TargetPlatform    string             `json:"targetPlatform"`
	SubsystemBackends []subsystemBackend `json:"subsystemBackends"`
}

// stepInfo reads device identity, resources and audio state through framework
// services: the cheapest proof that the app bus and unary calls are alive.
func (a *devApp) stepInfo(ctx context.Context, gen uint64) stepResult {
	var info deviceInfo
	if err := a.client.CallJSON(ctx, "", deviceService, "GetInfo", struct{}{}, &info, callTimeout); err != nil {
		return classify("device info", err)
	}
	resources, err := a.resources(ctx)
	if err != nil {
		return classify("list resources", err)
	}
	state, err := a.client.AudioState(ctx)
	if err != nil {
		return classify("audio state", err)
	}
	platform := info.TargetPlatform
	if platform == "" {
		platform = "unspecified"
	}
	log.Printf("devtools: info gen=%d device=%q model=%q platform=%s daemon=%s resources=%d backends=%d",
		gen, info.DeviceID, info.Model, platform, info.DaemonVersion,
		len(resources), len(info.SubsystemBackends))
	for _, resource := range resources {
		log.Printf("devtools: info gen=%d resource=%s kind=%s available=%v",
			gen, resource.ResourceID, resource.Kind, resource.Available)
	}
	log.Printf("devtools: info gen=%d audio volume=%.2f muted=%v hardware_mute=%v",
		gen, state.MasterVolume, state.MicrophoneMuted, state.MicrophoneMuteIsHardware)
	return okResult("model=%s daemon=%s platform=%s resources=%d volume=%.2f muted=%v",
		info.Model, info.DaemonVersion, platform, len(resources),
		state.MasterVolume, state.MicrophoneMuted)
}

// stepDisplay presents 1234, then explicitly blanks and releases. The capture
// makes the presentation routable; a frame alone would not be selected.
func (a *devApp) stepDisplay(ctx context.Context, gen uint64) stepResult {
	resources, err := a.resources(ctx)
	if err != nil {
		return classify("list resources", err)
	}
	segment := findResource(resources, segmentResource, "RESOURCE_SEGMENT_DISPLAY")
	if segment == nil || !segment.Available || segment.Segment == nil || segment.Segment.DigitCount == 0 {
		return unsupported("resource %s has no usable segment digits", segmentResource)
	}
	pres, err := a.client.OpenPresentation(ctx, segment.ResourceID)
	if err != nil {
		return classify("open segment presentation", err)
	}
	defer pres.Close()

	cells := segmentMasks("1234", int(segment.Segment.DigitCount))
	colon := segment.Segment.SupportsColon
	if err := pres.Segment(cells, colon); err != nil {
		return classify("write segment frame", err)
	}
	log.Printf("devtools: display gen=%d presentation=%s digits=1234 masks=%s colon=%v",
		gen, pres.ID(), maskHex(cells), colon)
	capture, err := a.client.OpenCapture(ctx, segment.ResourceID, pres.ID(), capsulad.CaptureModeUnspecified)
	if err != nil {
		return classify("open segment capture", err)
	}
	defer capture.Close()
	log.Printf("devtools: display gen=%d capture=%s selected=%v ready=%v",
		gen, capture.ID(), capture.Info().Selected, capture.Info().Ready)
	if err := wait(ctx, 1500*time.Millisecond); err != nil {
		return classify("hold segment frame", err)
	}
	if err := pres.Blank(); err != nil {
		return classify("blank segment presentation", err)
	}
	if err := pres.Close(); err != nil {
		return classify("close segment presentation", err)
	}
	return okResult("resource=%s digits=1234 cells=%d colon=%v presentation=%s capture=%s",
		segment.ResourceID, len(cells), colon, pres.ID(), capture.ID())
}

// stepRing presents a deterministic gradient, then explicitly blanks and
// releases it.
func (a *devApp) stepRing(ctx context.Context, gen uint64) stepResult {
	resources, err := a.resources(ctx)
	if err != nil {
		return classify("list resources", err)
	}
	ring := findResource(resources, ringResource, "RESOURCE_LED_RING")
	if ring == nil || !ring.Available || ring.Ring == nil || ring.Ring.PixelCount == 0 {
		return unsupported("resource %s has no usable pixels", ringResource)
	}
	pixels := int(ring.Ring.PixelCount)
	pres, err := a.client.OpenPresentation(ctx, ring.ResourceID)
	if err != nil {
		return classify("open ring presentation", err)
	}
	defer pres.Close()
	frame := ringPattern(pixels)
	if err := pres.Ring(frame); err != nil {
		return classify("write ring frame", err)
	}
	log.Printf("devtools: ring gen=%d presentation=%s pixels=%d frame=%s",
		gen, pres.ID(), pixels, rgbSummary(frame))
	capture, err := a.client.OpenCapture(ctx, ring.ResourceID, pres.ID(), capsulad.CaptureModeUnspecified)
	if err != nil {
		return classify("open ring capture", err)
	}
	defer capture.Close()
	log.Printf("devtools: ring gen=%d capture=%s selected=%v ready=%v",
		gen, capture.ID(), capture.Info().Selected, capture.Info().Ready)
	if err := wait(ctx, 1200*time.Millisecond); err != nil {
		return classify("hold ring frame", err)
	}
	if err := pres.Blank(); err != nil {
		return classify("blank ring presentation", err)
	}
	if err := pres.Close(); err != nil {
		return classify("close ring presentation", err)
	}
	return okResult("resource=%s pixels=%d presentation=%s capture=%s",
		ring.ResourceID, pixels, pres.ID(), capture.ID())
}

// stepSettings reads the app's own configuration, CAS-writes one declared
// setting and verifies the stored JSON value.
func (a *devApp) stepSettings(ctx context.Context, gen uint64) stepResult {
	before, err := a.configGet(ctx)
	if err != nil {
		return classify("settings get", err)
	}
	oldValue, hadValue := settingValue(before, "note")
	a.mu.Lock()
	a.settingsWrites++
	write := a.settingsWrites
	a.mu.Unlock()
	newValue := fmt.Sprintf("devtools-%d", write)
	encoded, err := json.Marshal(newValue)
	if err != nil {
		return errorResult("encode settings value: %v", err)
	}
	after, err := a.configUpdateNote(ctx, before.Revision, newValue)
	if err != nil {
		return classify("settings update", err)
	}
	stored, hasStored := settingValue(after, "note")
	if !hasStored || stored != string(encoded) {
		return errorResult("update accepted but note=%q has_value=%v, want %q", stored, hasStored, string(encoded))
	}
	log.Printf("devtools: settings gen=%d key=note before=%q has_value=%v after=%q revision=%d->%d",
		gen, oldValue, hadValue, stored, before.Revision, after.Revision)
	return okResult("key=note before=%q has_value=%v after=%q revision=%d->%d",
		oldValue, hadValue, stored, before.Revision, after.Revision)
}

// stepEcho proves the reverse-RPC path: the self-call goes through the
// daemon's AppBusService.Call relay exactly like a call from another app. Peer
// calls exercise the script hosts' raw-protobuf handlers when they are running.
func (a *devApp) stepEcho(ctx context.Context, gen uint64) stepResult {
	reply, mode, err := a.selfEcho(ctx)
	if err != nil {
		return classify("self echo", err)
	}
	log.Printf("devtools: echo gen=%d target=self mode=%s reply=%q", gen, mode, reply)
	peers, unavailable := 0, 0
	for _, peer := range []string{"dev.quickjs", "dev.micropython"} {
		peerReply, err := a.peerEcho(ctx, peer, "ping")
		if err != nil {
			if peerUnavailable(err) {
				unavailable++
				log.Printf("devtools: echo gen=%d target=%s status=unavailable detail=%q",
					gen, peer, statusDetail(err))
			} else {
				log.Printf("devtools: echo gen=%d target=%s status=error detail=%q",
					gen, peer, statusDetail(err))
			}
			continue
		}
		peers++
		log.Printf("devtools: echo gen=%d target=%s status=ok reply=%q", gen, peer, peerReply)
	}
	return okResult("self=%q mode=%s service=%s peers=%d unavailable=%d",
		reply, mode, echoService, peers, unavailable)
}

func peerUnavailable(err error) bool {
	var status *capsulad.StatusError
	if !errors.As(err, &status) {
		return false
	}
	switch status.Code {
	case 3, 5, 12, 14: // InvalidArgument, NotFound, Unimplemented, Unavailable
		return true
	}
	return false
}

func statusDetail(err error) string {
	var status *capsulad.StatusError
	if errors.As(err, &status) {
		if status.Message != "" {
			return status.Message
		}
		return fmt.Sprintf("grpc status %d", status.Code)
	}
	return err.Error()
}
