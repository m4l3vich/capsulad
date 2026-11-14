package daemon

import (
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"capsulad/internal/settings"
)

// appSettingsStatus classifies a store error without guessing from arbitrary
// messages: CAS conflicts are ABORTED, semantic validation is INVALID_ARGUMENT,
// an already-typed gRPC status is preserved, and IO/storage failures are
// INTERNAL so clients do not retry a disk error as a bad value or stale write.
func appSettingsStatus(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
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
	return status.Error(codes.Internal, err.Error())
}
