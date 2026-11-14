package daemon

import (
	"context"
	"testing"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/sandbox"
)

// ListRuntimes must expose the launcher's real readiness, not a hardcoded
// script-runner denial. The repository build/lib normally contains both hosts;
// when it does not, the corresponding entry must be unavailable with a reason.
func TestListRuntimesUsesLauncherReadiness(t *testing.T) {
	launcher := sandbox.New(sandbox.Config{Mode: "none", LibraryDir: nativeLibDir(t)})
	svc := &applicationService{d: &Daemon{launcher: launcher}}

	res, err := svc.ListRuntimes(context.Background(), &frameworkv1.ListRuntimesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Runtimes) != 4 {
		t.Fatalf("runtimes = %d, want 4", len(res.Runtimes))
	}
	native := res.Runtimes[0]
	if native.Runtime != frameworkv1.ApplicationRuntime_RUNTIME_NATIVE || !native.Available {
		t.Fatalf("native = %+v", native)
	}
	for _, entry := range res.Runtimes {
		if entry.Detail == "" {
			t.Fatalf("runtime %s has no detail", entry.Runtime)
		}
		if entry.Available && entry.ImplementationVersion == "" &&
			entry.Runtime != frameworkv1.ApplicationRuntime_RUNTIME_NATIVE &&
			entry.Runtime != frameworkv1.ApplicationRuntime_RUNTIME_BUILTIN {
			t.Fatalf("available runtime %s has no implementation version", entry.Runtime)
		}
	}
}
