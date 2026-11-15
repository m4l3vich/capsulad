package daemon

import (
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"capsulad/internal/settings"
)

// internalSettingsMessage is the only text an app caller sees for a storage
// failure. The underlying error may name host paths in the data dir, which are
// not part of the app contract; the detail goes to the daemon log instead.
const internalSettingsMessage = "app settings storage is unavailable"

// appSettingsStatus classifies a store error without guessing from arbitrary
// messages: CAS conflicts are ABORTED, semantic validation is INVALID_ARGUMENT,
// an already-typed gRPC status is preserved, and IO/storage failures are
// INTERNAL with a generic message so clients do not retry a disk error as a
// bad value or stale write and do not learn the daemon's data-dir layout.
func (d *Daemon) appSettingsStatus(appID string, err error) error {
	if err == nil {
		return nil
	}
	if st, ok := status.FromError(err); ok {
		if st.Code() == codes.Internal {
			if d != nil && d.logs != nil {
				d.logs.Errorf(appID, "", "app settings write failed: %v", err)
			}
			return status.Error(codes.Internal, internalSettingsMessage)
		}
		return err
	}
	var stale *settings.StaleRevisionError
	if errors.As(err, &stale) {
		return status.Error(codes.Aborted, err.Error())
	}
	var invalid *settings.InvalidValueError
	if errors.As(err, &invalid) {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	var missing *settings.NotFoundError
	if errors.As(err, &missing) {
		return status.Error(codes.NotFound, err.Error())
	}
	if d != nil && d.logs != nil {
		d.logs.Errorf(appID, "", "app settings write failed: %v", err)
	}
	return status.Error(codes.Internal, internalSettingsMessage)
}
