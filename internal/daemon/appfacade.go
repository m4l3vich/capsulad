package daemon

import (
	"context"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"capsulad/internal/audio"
	"capsulad/internal/backend"
	"capsulad/internal/grpcpeer"
	"capsulad/internal/presentations"
	"capsulad/internal/routing"
)

// App-facing output facade.
//
// PresentationService, RoutingService and AudioService validate caller
// ownership and resource support before touching the presentation manager, the
// router or the audio manager. Compiled-in system apps and framework cues use
// these same methods, so they cannot acquire, present or play through a
// privileged shortcut that bypasses those checks. This is deliberately not a
// wrapper around private output writes: the single physical writer remains
// outputLoop.

// applicationCaller reports whether the caller is an application, even when its
// run record has already disappeared (a crashed or disconnected process that
// keeps the socket open). A live run supplies its app ID; otherwise the
// sandbox UID or supervised PID still identifies the connection as an app.
func (d *Daemon) applicationCaller(ctx context.Context) (appID string, isApp bool) {
	if appID, _, ok := d.callerIdentity(ctx); ok {
		return appID, true
	}
	if creds := grpcpeer.FromContext(ctx); creds.OK {
		if creds.UID >= d.cfg.SandboxUIDBase || d.isSupervisedPID(creds.PID) {
			return "", true
		}
	}
	return "", false
}

// requireAppScope scopes application-owned state: a local administrator may
// address any installed app, while a supervised app may address only itself.
// A connection that is recognizably an app but whose run is gone cannot fall
// back to administrator access.
func (d *Daemon) requireAppScope(ctx context.Context, targetAppID string) error {
	callerApp, isApp := d.applicationCaller(ctx)
	if !isApp {
		return d.requireAdministrativeCaller(ctx, "applications may only access their own state")
	}
	if callerApp == "" {
		d.logs.Warnf("daemon", "", "denied foreign app state for a connection whose run is not live")
		return status.Error(codes.PermissionDenied, "application run is not live")
	}
	if callerApp != targetAppID {
		d.logs.Warnf("daemon", callerApp, "denied cross-application access to %s", targetAppID)
		return status.Error(codes.PermissionDenied, "applications may only access their own state")
	}
	return nil
}

// appRunOwns reports whether appID/runID identify a live supervised or
// compiled-in run.
func (d *Daemon) appRunOwns(appID, runID string) bool {
	if appID == "" || runID == "" {
		return false
	}
	run, ok := d.sup.Get(runID)
	return ok && run.AppID == appID
}

// openAppPresentation is the single presentation-open path. System-app runs
// must exist and belong to the app; framework cues use the reserved
// SystemAppID without a run.
func (d *Daemon) openAppPresentation(appID, runID, resourceID string) (*presentations.Presentation, error) {
	kind, ok := d.presentationKind(resourceID)
	if !ok {
		return nil, fmt.Errorf("resource %s does not accept presentations", resourceID)
	}
	if appID == routing.SystemAppID {
		// Framework-owned presentations carry no run. A supervised package
		// that squats the reserved app ID always has a run ID and must not
		// inherit the framework's bypass.
		if runID != "" {
			return nil, fmt.Errorf("run %s cannot use the framework identity", runID)
		}
		return d.pres.Open(resourceID, appID, "system", kind), nil
	}
	if !d.appRunOwns(appID, runID) {
		return nil, fmt.Errorf("run %s does not belong to application %s", runID, appID)
	}
	return d.pres.Open(resourceID, appID, runID, kind), nil
}

// acquireAppCapture is the single capture path. A named presentation must
// belong to the calling app/run on the same resource; the router still owns
// priority and release semantics.
func (d *Daemon) acquireAppCapture(appID, runID, resourceID, presentationID string, mode routing.TouchMode) (*routing.Capture, error) {
	if appID != routing.SystemAppID && !d.appRunOwns(appID, runID) {
		return nil, fmt.Errorf("run %s does not belong to application %s", runID, appID)
	}
	if appID == routing.SystemAppID && runID != "" {
		return nil, fmt.Errorf("run %s cannot use the framework identity", runID)
	}
	if _, ok := d.resources[resourceID]; !ok {
		return nil, fmt.Errorf("unknown resource %s", resourceID)
	}
	if presentationID != "" {
		p, ok := d.pres.Get(presentationID)
		if !ok || p.AppID != appID || p.ResourceID != resourceID {
			return nil, fmt.Errorf("presentation %s does not belong to %s on %s", presentationID, appID, resourceID)
		}
		if appID != routing.SystemAppID && p.RunID != runID {
			return nil, fmt.Errorf("presentation %s does not belong to run %s", presentationID, runID)
		}
	}
	return d.router.Acquire(resourceID, appID, runID, presentationID, mode)
}

// openAppPlayback is the single playback-open path. Framework cues use the
// reserved SystemAppID; application streams must come from a live run.
func (d *Daemon) openAppPlayback(appID, runID, endpointID string, format backend.AudioFormat, conversationID string) (*audio.Playback, error) {
	if appID != routing.SystemAppID && !d.appRunOwns(appID, runID) {
		return nil, fmt.Errorf("run %s does not belong to application %s", runID, appID)
	}
	if appID == routing.SystemAppID && runID != "" {
		return nil, fmt.Errorf("run %s cannot use the framework identity", runID)
	}
	if endpointID == "" {
		return nil, fmt.Errorf("audio endpoint is required")
	}
	return d.audio.OpenPlayback(appID, runID, endpointID, format, conversationID)
}

// updateAppPresentation is the single in-place content path. The manager still
// owns content-type/kind validation; this adds the ownership check that the
// public stream applies implicitly through its own presentation pointer.
func (d *Daemon) updateAppPresentation(appID, runID string, p *presentations.Presentation, content presentations.Content) error {
	if p == nil {
		return fmt.Errorf("presentation is required")
	}
	if p.AppID != appID {
		return fmt.Errorf("presentation %s belongs to %s, not %s", p.ID, p.AppID, appID)
	}
	if appID != routing.SystemAppID && p.RunID != runID {
		return fmt.Errorf("presentation %s belongs to run %s, not %s", p.ID, p.RunID, runID)
	}
	return d.pres.Update(p, content)
}

// closeAppPresentation tears down one presentation through the same ownership
// check as the presentation stream's deferred cleanup.
func (d *Daemon) closeAppPresentation(appID, runID string, p *presentations.Presentation) {
	if p == nil || p.AppID != appID {
		return
	}
	if appID != routing.SystemAppID && p.RunID != runID {
		return
	}
	d.pres.Close(p)
}

// releaseAppCapture is the owner-scoped capture release used by RoutingService
// and system cues. ForceRelease remains only for explicit administrative or
// framework-timeout paths.
func (d *Daemon) releaseAppCapture(appID, runID, captureID string) bool {
	if appID == "" || captureID == "" {
		return false
	}
	return d.router.Release(captureID, appID, runID)
}
