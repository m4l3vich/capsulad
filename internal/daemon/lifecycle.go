package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/backend"
	"capsulad/internal/ids"
	"capsulad/internal/manifest"
	"capsulad/internal/presentations"
	"capsulad/internal/settings"
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
//
// It holds the package mutex so a process is never started from a package tree
// that InstallPackageStream or UninstallPackage is replacing or removing: the
// launch either completes first (and the mutation then refuses because the app
// is running) or starts from the already-swapped tree.
func (d *Daemon) Launch(appID string, args map[string]string) (runID, invocationID string, spawned bool, err error) {
	d.pkgMu.Lock()
	defer d.pkgMu.Unlock()
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
	if m.Entrypoint.Runtime == manifest.RuntimeQuickJS || m.Entrypoint.Runtime == manifest.RuntimeMicroPython {
		status := d.launcher.RuntimeStatus(m.Entrypoint.Runtime)
		if !status.Available {
			return fmt.Errorf("runtime %s is unavailable: %s", m.Entrypoint.Runtime, status.Detail)
		}
		return nil
	}
	if m.Entrypoint.Runtime != manifest.RuntimeNative {
		return fmt.Errorf("runtime %s has no runner for this target", m.Entrypoint.Runtime)
	}
	arch := m.Entrypoint.Architecture
	archOK := false
	for _, accepted := range d.profile().Architecture.Accepted {
		if arch == accepted {
			archOK = true
			break
		}
	}
	if !archOK {
		return fmt.Errorf("package architecture %q is not executable on this %s target", arch, d.profile().Architecture.Target)
	}
	if !strings.Contains(m.Entrypoint.ABI, d.profile().Architecture.ABIContains) {
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
	d.cleanupRun(runID, appID)
	if run, ok := d.sup.Get(runID); ok && run.State != supervisor.Stopping {
		// A lost framework connection is a failure, not a user stop: stop the
		// orphaned process but keep restart_on_failure semantics. A provider
		// killed mid-operation is otherwise left unavailable for the rest of
		// the boot with no way to activate it.
		d.logs.Warnf("daemon", appID, "stopping run=%s because its framework connection was lost; it will restart if the app allows it", runID)
		d.sup.StopCrashed(runID)
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
	d.broadcastRun(run)
}

func (d *Daemon) onRunExit(run supervisor.Run) {
	d.logs.Infof("daemon", run.AppID, "run=%s exited state=%s code=%d", run.RunID, run.State, run.ExitCode)
	d.cleanupRun(run.RunID, run.AppID)
}

// cleanupRun releases everything owned by a terminated process. The app
// identity is passed in because the supervisor has already removed the run
// record by the time the exit callback runs.
func (d *Daemon) cleanupRun(runID, appID string) {
	d.pres.RemoveRun(runID)
	d.router.RemoveRun(runID)
	d.removeRunPublications(runID)
	if d.appControl != nil {
		d.appControl.CloseRun(runID)
	}
	d.audio.RemoveRun(runID)
	d.dropPending(runID)
	// A held talk-key activation belongs to the app that received the press.
	// Once that run is gone, its release must not be delivered to a restarted
	// run, which never saw the activation. If a replacement run is already
	// ready, leave its freshly delivered activation alone: this cleanup may
	// arrive after the bus disconnect callback of the previous run.
	otherReady := d.hasOtherReadyRun(appID, runID)
	d.touchActMu.Lock()
	if d.touchAssistant == appID && !otherReady {
		d.touchActivation, d.touchAssistant = "", ""
	}
	d.touchActMu.Unlock()
	d.input.OnRoutingChanged()
	d.updateSystemRing()
	d.launcher.CleanupRun(runID)
}

// hasOtherReadyRun reports whether appID still has a ready run other than
// runID, which means a restarted provider may already own a new activation.
func (d *Daemon) hasOtherReadyRun(appID, runID string) bool {
	for _, run := range d.sup.List(appID) {
		if run.RunID != runID && run.State == supervisor.Ready {
			return true
		}
	}
	return false
}

func (d *Daemon) broadcastRun(snapshot supervisor.Run) {
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

// Stop requests termination of one run.
//
// A run that is the operator-selected default provider (a display, ring or
// assistant selection, or a boot application the operator chose) is recovered:
// the selected function of the device must stay available, so killing it is a
// recovery event, not an off switch. The supported ways to keep it stopped are
// to deselect another provider, shut the daemon down, or uninstall it; those
// paths never reach here for a run that is still selected, and a run of a
// deselected app stops for good. Compiled-in providers are re-registered in
// place because they have no child process to relaunch.
func (d *Daemon) Stop(runID string) bool {
	run, ok := d.sup.Get(runID)
	if !ok {
		return false
	}
	if d.shuttingDown() || !d.isSelectedDefault(run.AppID) {
		return d.sup.Stop(runID)
	}
	if run.Virtual {
		if !d.sup.Stop(runID) {
			return false
		}
		// Re-check the selection now that the teardown has completed: a
		// "choose none" update can land while the built-in stops, and it must
		// not be overridden by the recovery relaunch. The teardown is
		// synchronous, so this check happens immediately before the new run.
		if !d.shuttingDown() && d.isSelectedDefault(run.AppID) {
			d.ensureRunning(run.AppID)
		}
		return true
	}
	return d.sup.StopForRestart(runID)
}

// isSelectedDefault reports whether appID is still an installed provider that
// the persisted settings choose for a resource or for the assistant role.
func (d *Daemon) isSelectedDefault(appID string) bool {
	if appID == "" {
		return false
	}
	app, ok := d.reg.Get(appID)
	if !ok {
		return false
	}
	// An uninstalled package has no directory left even if this process still
	// has its registry entry cached from boot; it is no longer a provider the
	// framework can recover.
	if app.Dir != "" {
		if _, err := os.Stat(app.Dir); err != nil {
			return false
		}
	}
	values := d.user.Get().Values
	if values.DefaultAssistantAppID == appID {
		return true
	}
	for _, selected := range values.DefaultAppByResource {
		if selected == appID {
			return true
		}
	}
	// A bound wake word is an operator selection too: its app must be recovered
	// after a crash so saying the word still reaches it.
	for _, entry := range values.Wakewords {
		if entry.AppID == appID {
			return true
		}
	}
	return false
}

// shuttingDown reports whether the framework has begun tearing itself down. A
// stop that races shutdown must not start anything new.
func (d *Daemon) shuttingDown() bool {
	select {
	case <-d.closing:
		return true
	default:
		return false
	}
}

// reconcileProviderRuns stops providers the operator just deselected. Boot
// and background supervised apps retain their own lifecycle contract; a
// framework-owned built-in retains its runtime state, while a non-framework-
// owned built-in exists only to serve a current selection. A deselected
// assistant is also stopped so it cannot keep a turn or reply alive.
func (d *Daemon) reconcileProviderRuns(prev *settings.UserSettings, next settings.UserSettings) {
	if prev == nil {
		return
	}
	now := make(map[string]bool, len(next.DefaultAppByResource)+1+len(next.Wakewords))
	for _, appID := range next.DefaultAppByResource {
		now[appID] = true
	}
	if next.DefaultAssistantAppID != "" {
		now[next.DefaultAssistantAppID] = true
	}
	for _, entry := range next.Wakewords {
		if entry.AppID != "" {
			now[entry.AppID] = true
		}
	}
	before := make(map[string]bool, len(prev.DefaultAppByResource)+1+len(prev.Wakewords))
	for _, appID := range prev.DefaultAppByResource {
		before[appID] = true
	}
	if prev.DefaultAssistantAppID != "" {
		before[prev.DefaultAssistantAppID] = true
	}
	for _, entry := range prev.Wakewords {
		if entry.AppID != "" {
			before[entry.AppID] = true
		}
	}
	for appID := range before {
		if appID == "" || now[appID] {
			continue
		}
		app, ok := d.reg.Get(appID)
		if !ok {
			continue
		}
		if app.Manifest.Entrypoint.Runtime == manifest.RuntimeBuiltin {
			systemApp, registered := d.systemApp(appID)
			if !registered || systemApp.FrameworkOwned {
				continue
			}
		} else if app.Manifest.StartOnBoot || app.Manifest.Background {
			continue
		}
		d.sup.StopApp(appID)
	}
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
	if appID == "" {
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
		if app.Manifest.Entrypoint.Runtime == manifest.RuntimeBuiltin {
			continue // built-in start policy is handled by startSystemApps
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
