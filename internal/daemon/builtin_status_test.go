package daemon

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"capsulad/internal/settings"
)

// Storage details and host paths never reach the app caller; other classified
// errors keep their message.
func TestAppSettingsStatusRedactsInternalErrors(t *testing.T) {
	for _, err := range []error{
		errors.New("open /var/lib/capsulad/app-settings/x: permission denied"),
		status.Error(codes.Internal, "open /var/lib/capsulad/app-settings/x: permission denied"),
	} {
		got := (&Daemon{}).appSettingsStatus("test.app", err)
		if status.Code(got) != codes.Internal {
			t.Fatalf("code = %v, want Internal", status.Code(got))
		}
		if msg := status.Convert(got).Message(); msg != internalSettingsMessage {
			t.Fatalf("message = %q, want %q", msg, internalSettingsMessage)
		}
		if strings.Contains(status.Convert(got).Message(), "/var/lib") {
			t.Fatal("message leaks a host path")
		}
	}

	stale := (&Daemon{}).appSettingsStatus("test.app", status.Error(codes.Aborted, "revision 3 is stale"))
	if status.Code(stale) != codes.Aborted || status.Convert(stale).Message() != "revision 3 is stale" {
		t.Fatalf("non-internal status = %v, want the original ABORTED error", stale)
	}

	missing := (&Daemon{}).appSettingsStatus("test.app", &settings.NotFoundError{AppID: "test.app"})
	if status.Code(missing) != codes.NotFound {
		t.Fatalf("unknown app code = %v, want NotFound", status.Code(missing))
	}
}
