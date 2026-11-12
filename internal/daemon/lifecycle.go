package daemon

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/backend"
	"capsulad/internal/ids"
	"capsulad/internal/manifest"
	"capsulad/internal/presentations"
	"capsulad/internal/supervisor"
)

const (
	invocationTTL        = 30 * time.Second
	maxPendingPerRun     = 8
	maxInvocationRecords = 512
)

// Launch implements the single-instance invocation contract: a ready
// singleton receives AppLaunchInvocation in place; a starting singleton holds
// a bounded, expiring invocation; ordinary apps spawn a new supervised run.
func (d *Daemon) Launch(appID string, args map[string]string) (runID, invocationID string, spawned bool, err error) {
	if isRetiredApp(appID) {
		return "", "", false, fmt.Errorf("application %s is retired and ignored; remove its package directory to reclaim space", appID)
	}
	app, ok := d.reg.Get(appID)
	if !ok {
		return "", "", false, fmt.Errorf("application %s is not installed", appID)
	}
	if err := app.Manifest.ValidateLaunchArguments(args); err != nil {
		return "", "", false, err
	}
	if app.Manifest.Entrypoint.Runtime == manifest.RuntimeBuiltin {
		runID, err := d.startSystemApp(appID)
		if err != nil {
			return "", "", false, err
		}
		return runID, "", false, nil
	}
	if err := d.compatible(app.Manifest); err != nil {
		return "", "", false, err
	}
	if app.Manifest.SingleInstance {
		if run, ok := d.sup.ReadyRun(appID); ok {
			rec := d.newInvocation(appID, run.RunID, args)
			if err := d.deliverOrQueue(rec); err != nil {
				return "", "", false, err
			}
			return run.RunID, rec.ID, false, nil
		}
		if runs := d.sup.List(appID); len(runs) > 0 {
			rec := d.newInvocation(appID, runs[0].RunID, args)
			if err := d.deliverOrQueue(rec); err != nil {
				return "", "", false, err
			}
			return runs[0].RunID, rec.ID, false, nil
		}
	}
	rec := &invocationRecord{
		ID: ids.New("inv"), AppID: appID, Arguments: cloneArgs(args),
		QueuedAtNS: d.clock.NowNS(),
	}
	run, err := d.sup.Start(app.Manifest, rec.ID)
	if err != nil {
		return "", "", false, err
	}
	rec.RunID = run.RunID
	d.invMu.Lock()
	d.invocations[rec.ID] = rec
	if len(d.invocations) > maxInvocationRecords {
		for id := range d.invocations {
			if id != rec.ID {
				delete(d.invocations, id)
				break
			}
		}
	}
	d.pending[run.RunID] = append(d.pending[run.RunID], rec)
	d.invMu.Unlock()
	d.scheduleExpiry(rec)
	return run.RunID, rec.ID, true, nil
}

// compatible rejects binaries that the target CPU/ABI cannot execute before
// spawning anything. The simulator accepts host builds for development.
func (d *Daemon) compatible(m *manifest.Manifest) error {
	if d.dev.Platform() == backend.PlatformSimulator {
		return nil
	}
	if m.Entrypoint.Runtime != manifest.RuntimeNative {
		return fmt.Errorf("runtime %s has no runner for this target", m.Entrypoint.Runtime)
	}
	arch := m.Entrypoint.Architecture
	if arch != "aarch64" && arch != "arm64" {
		return fmt.Errorf("package architecture %q is not executable on this aarch64 target", arch)
	}
	if !strings.Contains(m.Entrypoint.ABI, "linux") {
		return fmt.Errorf("package ABI %q is not a Linux ABI", m.Entrypoint.ABI)
	}
	return nil
}

func cloneArgs(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (d *Daemon) newInvocation(appID, runID string, args map[string]string) *invocationRecord {
	rec := &invocationRecord{
		ID: ids.New("inv"), AppID: appID, RunID: runID,
		Arguments: cloneArgs(args), QueuedAtNS: d.clock.NowNS(),
	}
	d.invMu.Lock()
	d.invocations[rec.ID] = rec
	d.invMu.Unlock()
	return rec
}

func (d *Daemon) scheduleExpiry(rec *invocationRecord) {
	time.AfterFunc(invocationTTL, func() {
		d.invMu.Lock()
		delivered := rec.Delivered
		if !delivered {
			pending := d.pending[rec.RunID]
			kept := pending[:0]
			for _, item := range pending {
				if item.ID != rec.ID {
					kept = append(kept, item)
				}
			}
			d.pending[rec.RunID] = kept
		}
		d.invMu.Unlock()
		if !delivered {
			d.logs.Warnf("daemon", rec.AppID, "invocation=%s expired before delivery (run=%s)", rec.ID, rec.RunID)
		}
	})
}

func (d *Daemon) deliverOrQueue(rec *invocationRecord) error {
	if _, ok := d.bus.ReadyApp(rec.AppID); ok && rec.RunID != "" {
		if d.bus.ReadyRun(rec.RunID) {
			if err := d.sendInvocation(rec.RunID, rec); err == nil {
				return nil
			}
		}
	}
	d.invMu.Lock()
	if len(d.pending[rec.RunID]) >= maxPendingPerRun {
		d.invMu.Unlock()
		return fmt.Errorf("invocation queue for %s is full", rec.AppID)
	}
	d.pending[rec.RunID] = append(d.pending[rec.RunID], rec)
	d.invMu.Unlock()
	d.scheduleExpiry(rec)
	return nil
}

func (d *Daemon) sendInvocation(runID string, rec *invocationRecord) error {
	msg := &frameworkv1.AppLaunchInvocation{InvocationId: rec.ID}
	for name, value := range rec.Arguments {
		msg.Arguments = append(msg.Arguments, &frameworkv1.LaunchArgument{Name: name, Value: value})
	}
	if err := d.bus.Send(runID, &frameworkv1.AppInbound{Message: &frameworkv1.AppInbound_Invocation{Invocation: msg}}); err != nil {
		return err
	}
	d.invMu.Lock()
	rec.Delivered = true
	d.invMu.Unlock()
	d.logs.Infof("daemon", rec.AppID, "invocation=%s delivered to run=%s", rec.ID, runID)
	return nil
}

func (d *Daemon) flushPending(runID string) {
	d.invMu.Lock()
	pending := d.pending[runID]
	delete(d.pending, runID)
	d.invMu.Unlock()
	for _, rec := range pending {
		if err := d.sendInvocation(runID, rec); err != nil {
			d.logs.Warnf("daemon", rec.AppID, "invocation=%s could not be delivered: %v", rec.ID, err)
		}
	}
}

func (d *Daemon) dropPending(runID string) {
	d.invMu.Lock()
	pending := d.pending[runID]
	delete(d.pending, runID)
	d.invMu.Unlock()
	for _, rec := range pending {
		d.logs.Warnf("daemon", rec.AppID, "invocation=%s dropped: run %s ended before delivery", rec.ID, runID)
	}
}

// onAppReady runs after AppReady: mark the run ready, send private settings,
// flush queued invocations.
func (d *Daemon) onAppReady(runID, appID string) {
	d.sup.MarkReady(runID)
	d.sendAppSettings(appID)
	d.flushPending(runID)
	d.broadcastRun(runID)
}

func (d *Daemon) sendAppSettings(appID string) {
	rev, values, err := d.appCfg.Private(appID)
	if err != nil {
		return
	}
	msg := &frameworkv1.AppSettingsChanged{Revision: rev}
	for key, raw := range values {
		msg.Values = append(msg.Values, &frameworkv1.AppPrivateSetting{Key: key, ValueJson: string(raw)})
	}
	if err := d.bus.SendApp(appID, &frameworkv1.AppInbound{Message: &frameworkv1.AppInbound_SettingsChanged{SettingsChanged: msg}}); err != nil {
		d.logs.Warnf("daemon", appID, "could not deliver settings: %v", err)
		return
	}
	d.logs.Debugf("daemon", appID, "settings revision=%d enqueued", rev)
}

func (d *Daemon) onAppDisconnect(runID, appID string) {
	d.logs.Warnf("daemon", appID, "bus disconnected for run=%s", runID)
	d.cleanupRun(runID)
	if run, ok := d.sup.Get(runID); ok && run.State != supervisor.Stopping {
		d.logs.Warnf("daemon", appID, "stopping run=%s because its framework connection was lost", runID)
		d.sup.Stop(runID)
	}
}

func (d *Daemon) onInvocationResult(runID string, res *frameworkv1.AppInvocationResult) {
	d.invMu.Lock()
	rec := d.invocations[res.InvocationId]
	if rec != nil {
		accepted := res.Accepted
		rec.Accepted = &accepted
		rec.Detail = res.Detail
	}
	d.invMu.Unlock()
	if rec == nil {
		return
	}
	d.logs.Infof("daemon", rec.AppID, "invocation=%s accepted=%v %s", res.InvocationId, res.Accepted, res.Detail)
}

func (d *Daemon) onActionResult(runID string, res *frameworkv1.AppActionResult) {
	d.logs.Infof("daemon", "", "action invocation=%s succeeded=%v %s", res.ActionInvocationId, res.Succeeded, res.Detail)
}

func (d *Daemon) onRunState(run supervisor.Run) {
	d.broadcastRun(run.RunID)
}

func (d *Daemon) onRunExit(run supervisor.Run) {
	d.logs.Infof("daemon", run.AppID, "run=%s exited state=%s code=%d", run.RunID, run.State, run.ExitCode)
	d.cleanupRun(run.RunID)
	d.broadcastRun(run.RunID)
}

// cleanupRun releases everything owned by a terminated process.
func (d *Daemon) cleanupRun(runID string) {
	d.pres.RemoveRun(runID)
	d.router.RemoveRun(runID)
	d.audio.RemoveRun(runID)
	d.dropPending(runID)
	d.input.OnRoutingChanged()
	d.updateSystemRing()
	d.launcher.CleanupRun(runID)
}

func (d *Daemon) broadcastRun(runID string) {
	var snapshot supervisor.Run
	if run, ok := d.sup.Get(runID); ok {
		snapshot = run
	} else {
		return
	}
	d.runMu.Lock()
	defer d.runMu.Unlock()
	for _, ch := range d.runSubs {
		select {
		case ch <- snapshot:
		default:
		}
	}
}

func (d *Daemon) subscribeRuns() (<-chan supervisor.Run, func()) {
	ch := make(chan supervisor.Run, 32)
	d.runMu.Lock()
	d.nextSub++
	id := d.nextSub
	d.runSubs[id] = ch
	d.runMu.Unlock()
	return ch, func() {
		d.runMu.Lock()
		if cur, ok := d.runSubs[id]; ok {
			close(cur)
			delete(d.runSubs, id)
		}
		d.runMu.Unlock()
	}
}

func (d *Daemon) Stop(runID string) bool {
	return d.sup.Stop(runID)
}

func (d *Daemon) TriggerAction(appID, actionID string) (string, error) {
	if d.SystemAppInstalled(appID) {
		return d.TriggerSystemAction(appID, actionID)
	}
	app, ok := d.reg.Get(appID)
	if !ok {
		return "", fmt.Errorf("application %s is not installed", appID)
	}
	found := false
	for _, a := range app.Manifest.Actions {
		if a.ID == actionID {
			found = true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("application %s declares no action %q", appID, actionID)
	}
	runID, ok := d.bus.ReadyApp(appID)
	if !ok {
		return "", fmt.Errorf("application %s is not ready", appID)
	}
	id := ids.New("actinv")
	if err := d.bus.Send(runID, &frameworkv1.AppInbound{Message: &frameworkv1.AppInbound_Action{
		Action: &frameworkv1.AppActionInvoked{ActionInvocationId: id, ActionId: actionID},
	}}); err != nil {
		return "", err
	}
	d.logs.Infof("daemon", appID, "action=%s invocation=%s sent to run=%s", actionID, id, runID)
	return id, nil
}

// AppSettings push after a configuration change.
func (d *Daemon) pushAppSettings(appID string, rev uint64, values map[string]json.RawMessage) {
	if d.SystemAppInstalled(appID) {
		// System apps subscribe to the settings store directly; they have no
		// bus connection to deliver a private settings event to.
		return
	}
	msg := &frameworkv1.AppSettingsChanged{Revision: rev}
	for key, raw := range values {
		msg.Values = append(msg.Values, &frameworkv1.AppPrivateSetting{Key: key, ValueJson: string(raw)})
	}
	if err := d.bus.SendApp(appID, &frameworkv1.AppInbound{Message: &frameworkv1.AppInbound_SettingsChanged{SettingsChanged: msg}}); err != nil {
		d.logs.Infof("daemon", appID, "settings stored; app not running (%v)", err)
		return
	}
	d.logs.Debugf("daemon", appID, "settings revision=%d enqueued", rev)
}

// ensureRunning starts a background/selected app when it is installed but not
// running. It never resurrects an app the user explicitly stopped mid-flight;
// selection changes and boot are the triggers.
func (d *Daemon) ensureRunning(appID string) {
	if appID == "" || isRetiredApp(appID) {
		return
	}
	if _, ok := d.reg.Get(appID); !ok {
		return
	}
	if runs := d.sup.List(appID); len(runs) > 0 {
		return
	}
	if _, _, _, err := d.Launch(appID, nil); err != nil {
		d.logs.Warnf("daemon", appID, "could not start selected application: %v", err)
	}
}

func (d *Daemon) startBootApps() {
	for _, app := range d.reg.List() {
		if isRetiredApp(app.Manifest.ID) {
			continue
		}
		if app.Manifest.Entrypoint.Runtime == manifest.RuntimeBuiltin {
			continue // started by startSystemApps before this pass
		}
		if app.Manifest.StartOnBoot || app.Manifest.Background {
			d.ensureRunning(app.Manifest.ID)
		}
	}
	snap := d.user.Get()
	for _, appID := range snap.Values.DefaultAppByResource {
		d.ensureRunning(appID)
	}
	d.ensureRunning(snap.Values.DefaultAssistantAppID)
}

func (d *Daemon) presentationKind(resourceID string) (presentations.Kind, bool) {
	info, ok := d.resources[resourceID]
	if !ok {
		return 0, false
	}
	switch info.Kind {
	case frameworkv1.ResourceKind_RESOURCE_SEGMENT_DISPLAY:
		return presentations.Segment, true
	case frameworkv1.ResourceKind_RESOURCE_LED_RING:
		return presentations.Ring, true
	default:
		return 0, false
	}
}
