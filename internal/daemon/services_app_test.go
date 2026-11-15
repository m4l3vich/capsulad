package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/settings"
)

// The generic application-configuration update must classify store errors the
// same way the built-in apps do: a stale revision is retryable ABORTED, a
// rejected value is INVALID_ARGUMENT, and a storage failure is INTERNAL.
func TestAppConfigUpdateClassifiesStoreErrors(t *testing.T) {
	logs, _ := testLogBus(t)
	defer logs.Close()
	lookup := func(appID string) (*settings.ManifestSchema, bool) {
		if appID != "test.app" {
			return nil, false
		}
		return &settings.ManifestSchema{AppID: appID, Settings: []settings.SchemaSetting{
			{Key: "value", Type: "text"},
			{Key: "count", Type: "integer"},
		}}, true
	}
	update := func(store *settings.AppStore, expected uint64, change *frameworkv1.AppSettingChange) error {
		svc := &appConfigService{d: &Daemon{appCfg: store, logs: logs}}
		_, err := svc.Update(context.Background(), &frameworkv1.UpdateAppConfigurationRequest{
			AppId: "test.app", ExpectedRevision: expected,
			Changes: []*frameworkv1.AppSettingChange{change},
		})
		return err
	}

	t.Run("storage failure", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "apps")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		// A regular file where the per-app directory belongs: persistence
		// cannot create it, which is an IO failure, not a bad value.
		if err := os.WriteFile(filepath.Join(root, "test.app"), []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		store, err := settings.OpenApps(root, logs, lookup)
		if err != nil {
			t.Fatal(err)
		}
		err = update(store, 0, &frameworkv1.AppSettingChange{Key: "value", ValueJson: `"ok"`})
		if status.Code(err) != codes.Internal {
			t.Fatalf("storage failure code = %v (%v), want Internal", status.Code(err), err)
		}
		// The storage error names a host path; the app caller sees only the
		// generic message.
		msg := status.Convert(err).Message()
		if msg != internalSettingsMessage {
			t.Fatalf("storage failure message = %q, want %q", msg, internalSettingsMessage)
		}
		if strings.Contains(msg, root) || strings.Contains(msg, "not a directory") {
			t.Fatalf("storage failure message leaks the data dir: %q", msg)
		}
	})

	t.Run("invalid value", func(t *testing.T) {
		store, err := settings.OpenApps(filepath.Join(t.TempDir(), "apps"), logs, lookup)
		if err != nil {
			t.Fatal(err)
		}
		err = update(store, 0, &frameworkv1.AppSettingChange{Key: "count", ValueJson: `"two"`})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("invalid value code = %v (%v), want InvalidArgument", status.Code(err), err)
		}
	})

	t.Run("stale revision", func(t *testing.T) {
		store, err := settings.OpenApps(filepath.Join(t.TempDir(), "apps"), logs, lookup)
		if err != nil {
			t.Fatal(err)
		}
		// The fresh store is at revision 1; expecting 2 is a stale write.
		err = update(store, 2, &frameworkv1.AppSettingChange{Key: "value", ValueJson: `"ok"`})
		if status.Code(err) != codes.Aborted {
			t.Fatalf("stale revision code = %v (%v), want Aborted", status.Code(err), err)
		}
	})
}
