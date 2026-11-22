package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
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
	// The install can outlast the server's write timeout, so give this response
	// the same budget as reading the archive.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(packageUploadReadBudget))
	if r.ContentLength > artifact.MaxArchiveBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, packageTooLargeMessage())
		return
	}
	// A slow browser is not a broken one: extend this request's read deadline
	// before reading the archive. A controller error means the transport
	// cannot extend it, so the server's own timeout still applies.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(packageUploadReadBudget))
	body := http.MaxBytesReader(w, r.Body, artifact.MaxArchiveBytes)

	res, err := s.installArchive(r.Context(), body, "")
	if err != nil {
		s.writePackageError(w, err)
		return
	}
	writePackageResult(w, res)
}

// writePackageResult is the install response both the raw upload and the URL
// download return, so the browser parses one shape.
func writePackageResult(w http.ResponseWriter, res *frameworkv1.InstallApplicationResult) {
	app := res.GetApplication()
	writeJSONValue(w, http.StatusOK, map[string]any{
		"replaced":         res.GetReplaced(),
		"previous_version": res.GetPreviousVersion(),
		"app_id":           app.GetAppId(),
		"display_name":     app.GetDisplayName(),
		"version":          app.GetVersion(),
	})
}

// packageURLRedirectLimit bounds how many redirects the daemon follows for a
// package URL. A redirect chain is a download detail, not a package feature;
// stopping early keeps a misconfigured URL from becoming an open relay walk.
const packageURLRedirectLimit = 5

var (
	errPackageDownloadTooLarge = errors.New("package archive exceeds the download limit")
	errPackageRedirectLimit    = errors.New("package URL redirected too many times")
)

// installAppFromURL downloads one package archive and streams it into the same
// installArchive path as a browser upload. The daemon still validates and
// installs the archive; this route only fetches bytes from an http(s) origin
// the administrator named. It deliberately accepts no other scheme: a file or
// data URL would let the browser reach the daemon's own filesystem.
func (s *Server) installAppFromURL(w http.ResponseWriter, r *http.Request) {
	// The download and install can outlast the server's write timeout, so give
	// this response the same budget as reading the request.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(packageUploadReadBudget))
	var body struct {
		URL string `json:"url"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "send a JSON object with a package URL")
		return
	}
	target, err := parsePackageURL(body.URL)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), packageUploadReadBudget)
	defer cancel()
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= packageURLRedirectLimit {
				return errPackageRedirectLimit
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("redirect to an unsupported scheme")
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "the package URL is not usable")
		return
	}
	req.Header.Set("Accept", "application/gzip, application/octet-stream")
	resp, err := client.Do(req)
	if err != nil {
		s.writePackageDownloadError(w, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		writeJSONError(w, http.StatusBadGateway, fmt.Sprintf("the package URL answered HTTP %d", resp.StatusCode))
		return
	}
	if resp.ContentLength > artifact.MaxArchiveBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, packageTooLargeMessage())
		return
	}

	// A declared length can lie and a chunked response has none, so the body is
	// capped independently. io.LimitReader carries the byte budget and the
	// extra byte distinguishes "exactly at the limit" from "the origin has
	// more to send"; boundedPackageBody turns reaching it into a refusal
	// instead of a silent truncation.
	limit := artifact.MaxArchiveBytes + 1
	bodyReader := &boundedPackageBody{r: io.LimitReader(resp.Body, limit), limit: limit}
	res, err := s.installArchive(ctx, bodyReader, packageDownloadName(resp.Request.URL))
	if err != nil {
		switch {
		case errors.Is(err, errPackageDownloadTooLarge):
			writeJSONError(w, http.StatusRequestEntityTooLarge, packageTooLargeMessage())
		case errors.Is(err, io.ErrUnexpectedEOF):
			writeJSONError(w, http.StatusBadGateway, "the download ended before the archive was complete")
		default:
			s.writePackageError(w, err)
		}
		return
	}
	writePackageResult(w, res)
}

// parsePackageURL accepts only an absolute http(s) URL with a host and no
// credentials or fragment. The message is browser-facing: it never echoes the
// offending value.
func parsePackageURL(raw string) (*url.URL, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, errors.New("enter an http or https URL for the package")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return nil, errors.New("the package URL is not valid")
	}
	if !parsed.IsAbs() || parsed.Host == "" {
		return nil, errors.New("the package URL must be an absolute http or https URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("the package URL must use http or https")
	}
	if parsed.User != nil {
		return nil, errors.New("the package URL must not contain credentials")
	}
	if parsed.Fragment != "" {
		return nil, errors.New("the package URL must not contain a fragment")
	}
	return parsed, nil
}

// boundedPackageBody caps a download at limit bytes. Reaching the cap returns
// errPackageDownloadTooLarge, which installArchive turns into an abandoned
// stream so a truncated archive is never installed.
type boundedPackageBody struct {
	r     io.Reader
	limit int64
	read  int64
}

func (b *boundedPackageBody) Read(p []byte) (int, error) {
	if b.read >= b.limit {
		return 0, errPackageDownloadTooLarge
	}
	if remaining := b.limit - b.read; int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := b.r.Read(p)
	b.read += int64(n)
	if err == io.EOF && b.read >= b.limit {
		// The origin ended exactly at the extra byte: the archive is over the
		// accepted size even though the reader saw a clean EOF.
		return n, errPackageDownloadTooLarge
	}
	return n, err
}

// packageDownloadName labels the archive for diagnostics only. The daemon
// never installs from a client-named location.
func packageDownloadName(target *url.URL) string {
	if target == nil {
		return "package.tar.gz"
	}
	name := path.Base(target.Path)
	if name == "." || name == "/" || name == "" {
		return "package.tar.gz"
	}
	return name
}

// writePackageDownloadError maps a failed fetch onto the same refusal
// vocabulary the install routes use. The URL, host and network detail stay in
// the daemon log; the browser gets an actionable sentence.
func (s *Server) writePackageDownloadError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		writeJSONError(w, http.StatusGatewayTimeout, "the download did not finish within 10 minutes")
	case errors.Is(err, errPackageRedirectLimit):
		writeJSONError(w, http.StatusBadGateway, fmt.Sprintf("the package URL redirected more than %d times", packageURLRedirectLimit))
	case errors.Is(err, context.Canceled):
		writeJSONError(w, http.StatusServiceUnavailable, "the download was cancelled")
	default:
		if s.Logs != nil {
			s.Logs.Warnf("webui", "", "package download failed: %v", err)
		}
		writeJSONError(w, http.StatusBadGateway, "the speaker could not download the package; check the URL and try again")
	}
}

// installArchive streams one package archive to the daemon's
// InstallApplication RPC. The browser upload route and the MCP install tool
// share it, so the open frame, chunking and truncation handling live once: a
// short read abandons the stream instead of letting the daemon treat a
// truncated archive as a package.
func (s *Server) installArchive(ctx context.Context, body io.Reader, filename string) (*frameworkv1.InstallApplicationResult, error) {
	stream, err := frameworkv1.NewApplicationServiceClient(s.Conn).InstallApplication(ctx)
	if err != nil {
		return nil, err
	}
	// The stream's first message is the open frame; it names the archive for
	// diagnostics only. The daemon never installs from a client-named location.
	if err := stream.Send(&frameworkv1.InstallApplicationRequest{
		Chunk: &frameworkv1.InstallApplicationRequest_Open{Open: &frameworkv1.InstallApplicationOpen{FileName: filename}},
	}); err != nil {
		return nil, err
	}
	buf := make([]byte, packageUploadChunk)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if err := stream.Send(&frameworkv1.InstallApplicationRequest{
				Chunk: &frameworkv1.InstallApplicationRequest_Content{Content: buf[:n]},
			}); err != nil {
				return nil, err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			// The upload stopped early: abandon the stream so the daemon does
			// not treat a truncated archive as a package.
			_ = stream.CloseSend()
			return nil, readErr
		}
	}
	return stream.CloseAndRecv()
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
// packageFailureClass is the shared classification of a package refusal. The
// browser route and the MCP install tool both consume it, so their status and
// error-code taxonomies cannot drift apart.
type packageFailureClass uint8

const (
	packageFailureInternal packageFailureClass = iota
	packageFailureInvalid
	packageFailureTooLarge
	packageFailureNotFound
	packageFailureConflict
	packageFailureForbidden
	packageFailureBusy
	packageFailureUnavailable
)

func classifyPackageFailure(err error) (packageFailureClass, string) {
	var tooLarge *http.MaxBytesError
	switch {
	case errors.Is(err, errPackageDownloadTooLarge):
		return packageFailureTooLarge, packageTooLargeMessage()
	case errors.As(err, &tooLarge):
		return packageFailureTooLarge, packageTooLargeMessage()
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return packageFailureInvalid, "the upload ended before the archive was complete; try again"
	}
	st, _ := status.FromError(err)
	message := st.Message()
	if message == "" {
		message = err.Error()
	}
	switch st.Code() {
	case codes.InvalidArgument, codes.OutOfRange:
		return packageFailureInvalid, message
	case codes.NotFound:
		return packageFailureNotFound, message
	case codes.FailedPrecondition, codes.AlreadyExists, codes.Aborted:
		return packageFailureConflict, message
	case codes.PermissionDenied, codes.Unauthenticated:
		return packageFailureForbidden, "the device refused this operation"
	case codes.ResourceExhausted:
		// Another package holds the single staging slot, or the device is out
		// of room for it. Both are retryable once the current upload ends.
		return packageFailureBusy, message
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return packageFailureUnavailable, "the device is not answering; try again"
	default:
		return packageFailureInternal, "the package operation failed"
	}
}

// writePackageError maps a package refusal onto the browser's status
// vocabulary. A daemon message for a client-visible refusal is shown verbatim
// (the running-app explanation is the actionable part); an unexpected failure
// is logged and reported generically, so no host path or package layout ever
// reaches the browser.
func (s *Server) writePackageError(w http.ResponseWriter, err error) {
	class, message := classifyPackageFailure(err)
	switch class {
	case packageFailureInvalid:
		writeJSONError(w, http.StatusBadRequest, message)
	case packageFailureTooLarge:
		writeJSONError(w, http.StatusRequestEntityTooLarge, message)
	case packageFailureNotFound:
		writeJSONError(w, http.StatusNotFound, message)
	case packageFailureConflict:
		writeJSONError(w, http.StatusConflict, message)
	case packageFailureForbidden:
		writeJSONError(w, http.StatusForbidden, message)
	case packageFailureBusy:
		writeJSONError(w, http.StatusTooManyRequests, message)
	case packageFailureUnavailable:
		writeJSONError(w, http.StatusServiceUnavailable, message)
	default:
		if s.Logs != nil {
			s.Logs.Warnf("webui", "", "package operation failed: %v", err)
		}
		writeJSONError(w, http.StatusInternalServerError, message)
	}
}
