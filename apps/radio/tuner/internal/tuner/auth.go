package tuner

import (
	"context"
	"errors"
	"fmt"
	"log"

	"capsulad/sdk/capsulad"
)

// appInterfaceService is the daemon service that reports, per installed app,
// the run that is currently ready and the interfaces it publishes.
const appInterfaceService = "capsulad.framework.v1.AppInterfaceService"

// appInterface is one AppInterfaceService.ListInterfaces row. The run id comes
// from the daemon's supervisor state, not from the caller.
type appInterface struct {
	AppID     string `json:"appId"`
	RunID     string `json:"runId"`
	Service   string `json:"grpcService"`
	Available bool   `json:"available"`
}

// listInterfaces queries the daemon for every ready app's published
// interfaces. It is a field on Manager so tests can substitute it.
func (m *Manager) listInterfaces(ctx context.Context) ([]appInterface, error) {
	var response struct {
		Interfaces []appInterface `json:"interfaces"`
	}
	if err := m.client.CallJSON(ctx, "", appInterfaceService, "ListInterfaces",
		map[string]any{"runningOnly": true}, &response, m.authTimeout); err != nil {
		return nil, err
	}
	return response.Interfaces, nil
}

// authorizeCaller accepts only the configured player app's currently ready
// run. callerRunID is daemon-issued: the relay derives it from the connection's
// peer credentials and verifies a supervised run id against the peer process
// before relaying, so a caller cannot claim someone else's run. The run-to-app
// mapping is then read from the daemon's own interface table, so it cannot be
// asserted by the caller either.
//
// A caller with no supervised identity (empty, or the "pid:<pid>" fallback for
// the CLI and Web UI) is refused: a privileged local interface must not accept
// a request whose origin the framework cannot name.
func (m *Manager) authorizeCaller(ctx context.Context, callerRunID string) error {
	if callerRunID == "" {
		return errors.New("caller run identity is unavailable")
	}
	allowed := m.PlayerApp()
	list := m.listAppInterfaces
	if list == nil {
		list = m.listInterfaces
	}
	interfaces, err := list(ctx)
	if err != nil {
		return fmt.Errorf("resolve caller run %s: %w", callerRunID, err)
	}
	for _, iface := range interfaces {
		if iface.RunID == callerRunID && iface.AppID == allowed && iface.Available {
			return nil
		}
	}
	return fmt.Errorf("caller run %s is not the configured player %s", callerRunID, allowed)
}

// authorizeCall drops a routed call whose caller is not the configured radio
// app. It ends the call with PERMISSION_DENIED so the caller sees a refusal
// rather than a result it could act on.
func (m *Manager) authorizeCall(call *capsulad.ServerCall) bool {
	timeout := m.authTimeout
	if timeout <= 0 {
		timeout = authTimeout
	}
	ctx, cancel := context.WithTimeout(m.ctx, timeout)
	defer cancel()
	caller := call.CallerRunID()
	if err := m.authorizeCaller(ctx, caller); err != nil {
		logCallerRefusal(call, caller, err)
		_ = call.End(7, "permission denied: "+err.Error())
		return false
	}
	return true
}

func logCallerRefusal(call *capsulad.ServerCall, caller string, err error) {
	log.Printf("radio-tuner: refusing %s/%s from run %q: %v", call.Service(), call.Method(), caller, err)
}
