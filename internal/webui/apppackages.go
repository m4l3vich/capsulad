package webui

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/artifact"
)

// Application package administration for the administrator's browser.
//
// The daemon owns the package tree: these routes only carry the archive and
// the reply. Installing sends the raw body of the request (a .tar.gz) as the
// InstallApplication stream, so the browser uploads once and the daemon
// unpacks the archive straight into a staging tree on the applications
// filesystem. Removing addresses one installed app by ID.
//
// A package mutation is a local-administrator operation: sessionOnly requires
// the browser session, same-origin intent and the session CSRF token, and an
// API key is never accepted. The daemon applies the same rule again on its own
// side, refuses an app that is running and refuses to touch a compiled-in
// system application.

// packageUploadChunk is the size of one streamed slice. It is deliberately
// the same order as the daemon's staging buffer: the point of streaming is
// that neither side holds the whole archive.
const packageUploadChunk = 64 << 10

// packageUploadReadBudget is how long the administrator's browser may take to
// deliver one archive. A package is up to 64 MiB unpacked, which can exceed
// the server's default 30 s read timeout over Wi-Fi; the budget is raised per
// request through the response controller, so no other route is affected.
const packageUploadReadBudget = 10 * time.Minute

// installApp installs or atomically replaces one application package.
func (s *Server) installApp(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > artifact.MaxArchiveBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, packageTooLargeMessage())
		return
	}
	// A slow browser is not a broken one: extend this request's read deadline
	// before reading the archive. A controller error means the transport
	// cannot extend it, so the server's own timeout still applies.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(packageUploadReadBudget))
	body := http.MaxBytesReader(w, r.Body, artifact.MaxArchiveBytes)

	client := frameworkv1.NewApplicationServiceClient(s.Conn)
	stream, err := client.InstallApplication(r.Context())
	if err != nil {
		s.writePackageError(w, err)
		return
	}
	// The stream's first message is the open frame; it names the archive for
	// diagnostics only. The daemon never installs from a client-named location.
	if err := stream.Send(&frameworkv1.InstallApplicationRequest{
		Chunk: &frameworkv1.InstallApplicationRequest_Open{Open: &frameworkv1.InstallApplicationOpen{}},
	}); err != nil {
		s.writePackageError(w, err)
		return
	}
	buf := make([]byte, packageUploadChunk)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if err := stream.Send(&frameworkv1.InstallApplicationRequest{
				Chunk: &frameworkv1.InstallApplicationRequest_Content{Content: buf[:n]},
			}); err != nil {
				s.writePackageError(w, err)
				return
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			// The upload stopped early: abandon the stream so the daemon does
			// not treat a truncated archive as a package.
			_ = stream.CloseSend()
			s.writePackageError(w, readErr)
			return
		}
	}
	res, err := stream.CloseAndRecv()
	if err != nil {
		s.writePackageError(w, err)
		return
	}
	app := res.GetApplication()
	writeJSONValue(w, http.StatusOK, map[string]any{
		"replaced":         res.GetReplaced(),
		"previous_version": res.GetPreviousVersion(),
		"app_id":           app.GetAppId(),
		"display_name":     app.GetDisplayName(),
		"version":          app.GetVersion(),
	})
}

// removeApp removes one installed package. Settings are deliberately retained.
func (s *Server) removeApp(w http.ResponseWriter, r *http.Request) {
	appID := r.PathValue("appID")
	client := frameworkv1.NewApplicationServiceClient(s.Conn)
	if _, err := client.UninstallApplication(r.Context(), &frameworkv1.UninstallApplicationRequest{AppId: appID}); err != nil {
		s.writePackageError(w, err)
		return
	}
	writeJSONValue(w, http.StatusOK, map[string]any{"app_id": appID, "removed": true})
}

func packageTooLargeMessage() string {
	return fmt.Sprintf("the package archive exceeds the %d-byte upload limit", artifact.MaxArchiveBytes)
}

// writePackageError maps a package refusal onto the browser's status
// vocabulary. A daemon message for a client-visible refusal is shown verbatim
// (the running-app explanation is the actionable part); an unexpected failure
// is logged and reported generically, so no host path or package layout ever
// reaches the browser.
func (s *Server) writePackageError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeJSONError(w, http.StatusRequestEntityTooLarge, packageTooLargeMessage())
		return
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		writeJSONError(w, http.StatusBadRequest, "the upload ended before the archive was complete; try again")
		return
	}
	st, _ := status.FromError(err)
	message := st.Message()
	if message == "" {
		message = err.Error()
	}
	switch st.Code() {
	case codes.InvalidArgument, codes.OutOfRange:
		writeJSONError(w, http.StatusBadRequest, message)
	case codes.NotFound:
		writeJSONError(w, http.StatusNotFound, message)
	case codes.FailedPrecondition, codes.AlreadyExists, codes.Aborted:
		writeJSONError(w, http.StatusConflict, message)
	case codes.PermissionDenied, codes.Unauthenticated:
		writeJSONError(w, http.StatusForbidden, "the device refused this operation")
	case codes.ResourceExhausted:
		// Another package holds the single staging slot, or the device is out
		// of room for it. Both are retryable once the current upload ends.
		writeJSONError(w, http.StatusTooManyRequests, message)
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		writeJSONError(w, http.StatusServiceUnavailable, "the device is not answering; try again")
	default:
		if s.Logs != nil {
			s.Logs.Warnf("webui", "", "package operation failed: %v", err)
		}
		writeJSONError(w, http.StatusInternalServerError, "the package operation failed")
	}
}
