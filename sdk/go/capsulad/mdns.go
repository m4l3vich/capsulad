package capsulad

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

// MDNSEventKind is the canonical protobuf enum name of a Browse event.
type MDNSEventKind string

const (
	MDNSEventKindUnspecified MDNSEventKind = "EVENT_KIND_UNSPECIFIED"
	MDNSEventKindAdded       MDNSEventKind = "EVENT_KIND_ADDED"
	MDNSEventKindRemoved     MDNSEventKind = "EVENT_KIND_REMOVED"
)

// MDNSService is a PublishServiceRequest: one service instance to advertise
// through the daemon's Avahi responder. Field names match canonical protobuf
// JSON (lowerCamelCase).
type MDNSService struct {
	// ServiceType is "_snapcast._tcp", "_googlecast._tcp", "_airplay._tcp", ...
	ServiceType string `json:"serviceType"`
	// InstanceName is the displayed instance name, at most 63 UTF-8 bytes.
	InstanceName string `json:"instanceName"`
	// Port is the advertised port, 1..65535.
	Port uint32 `json:"port"`
	// TXT holds at most 16 records totalling at most 1024 bytes.
	TXT map[string]string `json:"txt,omitempty"`
	// Domain is "" for "local".
	Domain string `json:"domain,omitempty"`
}

// MDNSPublication is a PublishServiceResponse.
type MDNSPublication struct {
	PublicationID string `json:"publicationId"`
	ServiceType   string `json:"serviceType"`
	InstanceName  string `json:"instanceName"`
	Domain        string `json:"domain"`
	Host          string `json:"host"`
	Port          uint32 `json:"port"`
}

// MDNSEvent is one Browse DiscoveredService event. Kind is
// EVENT_KIND_ADDED for each resolved address (the daemon emits one event per
// address) and EVENT_KIND_REMOVED when the service disappears.
type MDNSEvent struct {
	Kind        MDNSEventKind     `json:"kind"`
	Name        string            `json:"name"`
	ServiceType string            `json:"serviceType"`
	Domain      string            `json:"domain"`
	Host        string            `json:"host"`
	Addresses   []string          `json:"addresses"`
	Port        uint32            `json:"port"`
	TXT         map[string]string `json:"txt"`
}

type mdnsBrowseRequest struct {
	ServiceType string `json:"serviceType"`
	Domain      string `json:"domain,omitempty"`
}

// PublishMDNS advertises one service instance through the daemon. The client
// must be a connected supervised app client; remote gRPC channels and
// unsupported runtimes fail with a visible native status (UNSUPPORTED when no
// responder exists).
func (c *Client) PublishMDNS(ctx context.Context, service MDNSService,
	timeout time.Duration) (*MDNSPublication, error) {
	ctx = orBackground(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	client, err := c.clientHandle()
	if err != nil {
		return nil, err
	}
	request, err := json.Marshal(service)
	if err != nil {
		return nil, fmt.Errorf("capsulad: encode mdns publish request: %w", err)
	}
	payload, status, detail := mdnsPublishNative(client, request, timeoutMillis(ctx, timeout))
	if err := nativeStatusError("publish mdns service", status, detail); err != nil {
		return nil, err
	}
	var publication MDNSPublication
	if err := unmarshalProtoJSON(payload, &publication); err != nil {
		return nil, fmt.Errorf("capsulad: decode mdns publication: %w", err)
	}
	return &publication, nil
}

// UnpublishMDNS withdraws a publication by its id. It is safe to call after
// the run has already removed the publication.
func (c *Client) UnpublishMDNS(ctx context.Context, publicationID string,
	timeout time.Duration) error {
	ctx = orBackground(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if publicationID == "" {
		return errors.New("capsulad: publication id is required")
	}
	client, err := c.clientHandle()
	if err != nil {
		return err
	}
	status, detail := mdnsUnpublishNative(client, publicationID, timeoutMillis(ctx, timeout))
	return nativeStatusError("unpublish mdns service", status, detail)
}

// BrowseMDNS starts the server-streaming Browse call and returns an iterator.
// Cancelling ctx cancels the native call; call Close when finished to release
// it deterministically.
func (c *Client) BrowseMDNS(ctx context.Context, serviceType, domain string) (*MDNSBrowse, error) {
	ctx = orBackground(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	client, err := c.clientHandle()
	if err != nil {
		return nil, err
	}
	request, err := json.Marshal(mdnsBrowseRequest{ServiceType: serviceType, Domain: domain})
	if err != nil {
		return nil, fmt.Errorf("capsulad: encode mdns browse request: %w", err)
	}
	handle, err := mdnsBrowseOpenNative(client, request, timeoutMillis(ctx, 0))
	if err != nil {
		return nil, err
	}
	return &MDNSBrowse{call: newCallHandle(handle, ctx, "DiscoveryService/Browse")}, nil
}

// MDNSBrowse is one server-streaming Browse call. Next returns io.EOF at a
// clean end of stream.
type MDNSBrowse struct{ call *callHandle }

// Next waits for one event. Cancelling ctx cancels the browse call (the stream
// is not reusable afterwards); the returned error is ctx.Err(). A clean end of
// stream returns io.EOF.
func (b *MDNSBrowse) Next(ctx context.Context) (*MDNSEvent, error) {
	if b == nil || b.call == nil {
		return nil, ErrClosed
	}
	ctx = orBackground(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	payload, err := b.recv(ctx)
	if err != nil {
		if err == io.EOF {
			return nil, io.EOF
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	var event MDNSEvent
	if err := unmarshalProtoJSON(payload, &event); err != nil {
		return nil, fmt.Errorf("capsulad: decode mdns event: %w", err)
	}
	return &event, nil
}

func (b *MDNSBrowse) recv(ctx context.Context) ([]byte, error) {
	handle := b.call.value()
	if handle == 0 {
		return nil, ErrClosed
	}
	timeoutMS := ioTimeoutMS(ctx)
	if ctx.Done() == nil {
		return b.call.recvJSONTimeout(timeoutMS)
	}
	var cancelled atomic.Bool
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			// A deadline is enforced by the native receive timeout, which
			// leaves the browse usable. Only an explicit cancellation aborts
			// the call.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return
			}
			cancelled.Store(true)
			_ = callCancelNative(handle)
		case <-stop:
		}
	}()
	payload, err := b.call.recvJSONTimeout(timeoutMS)
	close(stop)
	<-done
	if err != nil && cancelled.Load() {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
	}
	return payload, err
}

// Close cancels the call and releases its native handle. It is idempotent.
func (b *MDNSBrowse) Close() error {
	if b == nil || b.call == nil {
		return nil
	}
	return b.call.release()
}
