package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"capsulad/internal/ids"
	"capsulad/internal/manifest"
	"capsulad/internal/registry"
	"capsulad/internal/supervisor"
)

// SystemApp is an application compiled into capsulad. It has a normal app
// identity, manifest, app settings namespace, routed app-bus endpoint and
// routing ownership; only its packaging is special. It cannot be installed,
// replaced or uninstalled as a package.
type SystemApp struct {
	// Manifest declares identity, provides hints, settings and actions exactly
	// like an installed manifest. Entrypoint.Runtime must be RuntimeBuiltin.
	Manifest *manifest.Manifest
	// FrameworkOwned marks a built-in runtime the framework needs independently
	// of provider selection (for example, the clock's timer and alarm service).
	// It must be set explicitly; Provides does not imply framework ownership.
	FrameworkOwned bool
	// Interfaces is the discovery metadata (descriptor protoset, docs and
	// method list) published through AppInterfaceService. Manifest.Interfaces
	// must declare the same service names.
	Interfaces []registry.Interface
	// Start initializes the in-process runtime and opens its baseline
	// presentation through the normal presentations manager.
	Start func()
	// Stop closes the app's presentations and releases its captures.
	Stop func()
	// HandleCall answers AppBusService.Call routed through this app's ID.
	// Request and response frames are protobuf JSON, identical to supervised
	// apps. A unary method emits one frame; a server-streaming watch may emit
	// many until the context is cancelled. Returning a gRPC status preserves
	// its code and message.
	HandleCall func(ctx context.Context, service, rpc string, request []byte, emit func([]byte) error) error
	// Action runs a manifest-declared action button.
	Action func(ctx context.Context, actionID string) error
}

// systemAppRunPrefix keeps built-in run IDs stable across stop/start so bus
// callers and routing ownership survive a UI stop followed by Launch.
const systemAppRunPrefix = "builtin:"

// RegisterSystemApp publishes a compiled-in app in the registry. Call it after
// the registry exists and before Run; the runtime is started by Run.
func (d *Daemon) RegisterSystemApp(app SystemApp) error {
	if app.Manifest == nil {
		return fmt.Errorf("system app manifest is required")
	}
	m := app.Manifest
	if m.Entrypoint.Runtime != manifest.RuntimeBuiltin {
		return fmt.Errorf("system app %s must use runtime %q", m.ID, manifest.RuntimeBuiltin)
	}
	if err := m.Validate(); err != nil {
		return err
	}
	if app.Start == nil {
		return fmt.Errorf("system app %s has no start hook", m.ID)
	}
	d.systemMu.Lock()
	defer d.systemMu.Unlock()
	if d.systemApps == nil {
		d.systemApps = map[string]*SystemApp{}
	}
	if _, ok := d.systemApps[m.ID]; ok {
		return fmt.Errorf("system app %s is already registered", m.ID)
	}
	if err := d.reg.RegisterBuiltin(&registry.App{Manifest: m, Interfaces: app.Interfaces}); err != nil {
		return err
	}
	spec := app // copy so later caller mutation cannot change the published app
	d.systemApps[m.ID] = &spec
	return nil
}

// SystemAppRunID returns the stable run ID for a registered system app.
func (d *Daemon) SystemAppRunID(appID string) string {
	return systemAppRunPrefix + appID
}

// SystemAppInstalled reports whether appID is a compiled-in application.
func (d *Daemon) SystemAppInstalled(appID string) bool {
	d.systemMu.Lock()
	defer d.systemMu.Unlock()
	_, ok := d.systemApps[appID]
	return ok
}

func (d *Daemon) systemApp(appID string) (*SystemApp, bool) {
	d.systemMu.Lock()
	defer d.systemMu.Unlock()
	app, ok := d.systemApps[appID]
	return app, ok
}

// startSystemApps launches registered built-ins needed independently of
// provider selection, plus built-in providers selected in persisted settings.
// It runs once in Run before external boot apps.
func (d *Daemon) startSystemApps() {
	d.systemMu.Lock()
	list := make([]string, 0, len(d.systemApps))
	for id := range d.systemApps {
		list = append(list, id)
	}
	d.systemMu.Unlock()
	sort.Strings(list)
	for _, id := range list {
		if !d.builtinNeededAtBoot(id) {
			d.logs.Infof("daemon", id, "not selected and not framework-owned; leaving it stopped")
			continue
		}
		if _, err := d.startSystemApp(id); err != nil {
			d.logs.Warnf("daemon", id, "could not start system app: %v", err)
		}
	}
}

// builtinNeededAtBoot reports whether a built-in is required for the booted
// settings: framework-owned helpers always run, while provider built-ins run
// only when selected. Manifest boot/background flags do not override this.
func (d *Daemon) builtinNeededAtBoot(appID string) bool {
	app, ok := d.reg.Get(appID)
	if !ok || app.Manifest.Entrypoint.Runtime != manifest.RuntimeBuiltin {
		return false
	}
	systemApp, ok := d.systemApp(appID)
	if !ok {
		return false
	}
	return systemApp.FrameworkOwned || d.isSelectedDefault(appID)
}

// startSystemApp starts a system app if it is not already running and returns
// its stable run ID. It is used at boot and by ApplicationService.Launch after
// an explicit Stop.
func (d *Daemon) startSystemApp(appID string) (string, error) {
	d.systemRunMu.Lock()
	defer d.systemRunMu.Unlock()
	app, ok := d.systemApp(appID)
	if !ok {
		return "", fmt.Errorf("application %s is not installed", appID)
	}
	runID := d.SystemAppRunID(appID)
	if run, running := d.sup.Get(runID); running {
		if run.State == supervisor.Stopping {
			return "", status.Errorf(codes.FailedPrecondition, "application %s is stopping", appID)
		}
		return runID, nil
	}
	// The run and bus endpoint must exist before Start: a system app now opens
	// presentations, captures and playback through the same public service
	// handlers as external apps, and those handlers validate the caller run.
	if err := d.bus.RegisterInternal(appID, runID, app.HandleCall); err != nil {
		return "", err
	}
	if _, err := d.sup.RegisterBuiltin(app.Manifest, runID); err != nil {
		d.bus.UnregisterInternal(appID)
		return "", err
	}
	d.sup.BindBuiltinStop(runID, func() { d.stopSystemApp(appID, app) })
	app.Start()
	return runID, nil
}

// stopSystemApp is the supervisor's teardown hook for a built-in run. The
// supervisor owns state transitions; this only tears down the runtime and its
// bus endpoint.
func (d *Daemon) stopSystemApp(appID string, app *SystemApp) {
	d.bus.UnregisterInternal(appID)
	if app.Stop != nil {
		app.Stop()
	}
}

// recoverSystemApp fails a built-in run whose internal output stream died and
// schedules a bounded restart. The next start re-registers the bus endpoint,
// reopens presentations/captures and publishes a fresh READY run.
func (d *Daemon) recoverSystemApp(appID string) {
	if !d.sup.FailBuiltin(d.SystemAppRunID(appID), "internal output stream failed") {
		return
	}
	d.scheduleSystemRestart(appID)
}

func (d *Daemon) scheduleSystemRestart(appID string) {
	d.systemFailMu.Lock()
	if d.systemRestarting == nil {
		d.systemRestarting = map[string]bool{}
	}
	if d.systemFailures == nil {
		d.systemFailures = map[string]int{}
	}
	if d.systemRestarting[appID] {
		d.systemFailMu.Unlock()
		return
	}
	d.systemRestarting[appID] = true
	failures := d.systemFailures[appID]
	d.systemFailures[appID] = failures + 1
	delay := time.Duration(0)
	if failures > 0 {
		exp := failures
		if exp > 5 {
			exp = 5
		}
		delay = time.Duration(1<<uint(exp-1)) * time.Second
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
	}
	d.systemFailMu.Unlock()

	time.AfterFunc(delay, func() {
		d.systemFailMu.Lock()
		delete(d.systemRestarting, appID)
		d.systemFailMu.Unlock()
		if d.ctx.Err() != nil {
			return
		}
		if _, err := d.startSystemApp(appID); err != nil {
			d.logs.Errorf(appID, "", "system app restart failed: %v", err)
			d.scheduleSystemRestart(appID)
		}
	})
}

// markSystemHealthy clears the restart backoff after a successful attach.
func (d *Daemon) markSystemHealthy(appID string) {
	d.systemFailMu.Lock()
	delete(d.systemFailures, appID)
	d.systemFailMu.Unlock()
}

// TriggerSystemAction dispatches a manifest action to a compiled-in app.
func (d *Daemon) TriggerSystemAction(appID, actionID string) (string, error) {
	app, ok := d.systemApp(appID)
	if !ok {
		return "", fmt.Errorf("application %s is not a system app", appID)
	}
	declared := false
	for _, a := range app.Manifest.Actions {
		if a.ID == actionID {
			declared = true
			break
		}
	}
	if !declared {
		return "", fmt.Errorf("application %s does not declare action %s", appID, actionID)
	}
	if app.Action == nil {
		return "", fmt.Errorf("application %s does not implement action %s", appID, actionID)
	}
	if err := app.Action(d.ctx, actionID); err != nil {
		return "", err
	}
	return ids.New("act"), nil
}

// --- synchronous app settings validation ----------------------------------

// RegisterAppSettingsValidator installs a semantic validator for one app's
// typed settings. It runs synchronously inside ApplicationConfigurationService.
// Update after the manifest/type checks and before any write. An error is
// surfaced as InvalidArgument with its message and leaves the prior revision
// active. The callback must not call the settings store again.
func (d *Daemon) RegisterAppSettingsValidator(appID string, fn func(values map[string]json.RawMessage) error) {
	d.appValidatorMu.Lock()
	defer d.appValidatorMu.Unlock()
	if d.appValidators == nil {
		d.appValidators = map[string]func(values map[string]json.RawMessage) error{}
	}
	d.appValidators[appID] = fn
}

func (d *Daemon) validateAppSettings(appID string, values map[string]json.RawMessage) error {
	d.appValidatorMu.RLock()
	fn := d.appValidators[appID]
	d.appValidatorMu.RUnlock()
	if fn == nil {
		return nil
	}
	if err := fn(values); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return nil
}
