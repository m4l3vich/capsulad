package daemon

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/discovery"
)

// discoveryService publishes and browses local mDNS services through the
// daemon's responder. Publications are scoped to the caller's supervised run
// and are removed on unpublish, run exit and daemon shutdown.
type discoveryService struct {
	frameworkv1.UnimplementedDiscoveryServiceServer
	d *Daemon
}

type mdnsPublication struct {
	appID string
	runID string
	pub   discovery.Publication
}

func (s *discoveryService) Publish(ctx context.Context, req *frameworkv1.PublishServiceRequest) (*frameworkv1.PublishServiceResponse, error) {
	appID, runID, err := s.d.requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	svc := discovery.Service{
		Type: req.GetServiceType(), Instance: req.GetInstanceName(),
		Domain: req.GetDomain(), Port: req.GetPort(), TXT: req.GetTxt(),
	}
	pub, err := s.d.mdns.Publish(ctx, svc)
	if err != nil {
		return nil, mdnsRPCError("publish mDNS service", err)
	}
	s.d.mdnsMu.Lock()
	if s.d.mdnsPubs == nil {
		s.d.mdnsPubs = map[string]mdnsPublication{}
	}
	s.d.mdnsPubs[pub.ID] = mdnsPublication{appID: appID, runID: runID, pub: pub}
	s.d.mdnsMu.Unlock()
	if s.d.sup != nil {
		if _, live := s.d.sup.Get(runID); !live {
			// The run ended while the responder call was in flight; its exit
			// cleanup may have run before the publication was recorded.
			s.d.removeRunPublications(runID)
			return nil, status.Error(codes.Aborted, "application run ended during publication")
		}
	}
	s.d.logs.Infof("mdns", appID, "published run=%s id=%s type=%s instance=%q port=%d",
		runID, pub.ID, pub.Service.Type, pub.Service.Instance, pub.Service.Port)
	return &frameworkv1.PublishServiceResponse{
		PublicationId: pub.ID,
		ServiceType:   pub.Service.Type,
		InstanceName:  pub.Service.Instance,
		Domain:        pub.Service.Domain,
		Host:          pub.Host,
		Port:          pub.Service.Port,
	}, nil
}

func (s *discoveryService) Unpublish(ctx context.Context, req *frameworkv1.UnpublishServiceRequest) (*frameworkv1.UnpublishServiceResponse, error) {
	appID, runID, err := s.d.requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	id := req.GetPublicationId()
	s.d.mdnsMu.Lock()
	rec, ok := s.d.mdnsPubs[id]
	s.d.mdnsMu.Unlock()
	if !ok {
		return nil, status.Error(codes.NotFound, "publication not found")
	}
	if rec.appID != appID || rec.runID != runID {
		// Never reveal or remove another app's registration.
		s.d.logs.Warnf("mdns", appID, "denied unpublish of %s owned by run=%s", id, rec.runID)
		return nil, status.Error(codes.NotFound, "publication not found")
	}
	removed, err := s.d.mdns.Unpublish(ctx, id)
	if err != nil {
		return nil, mdnsRPCError("unpublish mDNS service", err)
	}
	// The caller asked for removal; a successful responder call that reports
	// false (unknown ID after a responder restart) is still no live
	// registration to track.
	s.d.mdnsMu.Lock()
	delete(s.d.mdnsPubs, id)
	s.d.mdnsMu.Unlock()
	s.d.logs.Infof("mdns", appID, "unpublished run=%s id=%s removed=%v", runID, id, removed)
	return &frameworkv1.UnpublishServiceResponse{Removed: removed}, nil
}

func (s *discoveryService) Browse(req *frameworkv1.BrowseServicesRequest, stream frameworkv1.DiscoveryService_BrowseServer) error {
	if _, _, err := s.d.requireCaller(stream.Context()); err != nil {
		return err
	}
	browser, err := s.d.mdns.Browse(stream.Context(), discovery.Query{
		Type: req.GetServiceType(), Domain: req.GetDomain(),
	})
	if err != nil {
		return mdnsRPCError("browse mDNS services", err)
	}
	defer browser.Close()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case event, ok := <-browser.Events():
			if !ok {
				if err := browser.Err(); err != nil {
					return mdnsRPCError("browse mDNS services", err)
				}
				return nil
			}
			if err := stream.Send(discoveredServiceProto(event)); err != nil {
				return err
			}
		}
	}
}

func discoveredServiceProto(event discovery.Event) *frameworkv1.DiscoveredService {
	kind := frameworkv1.DiscoveredService_EVENT_KIND_UNSPECIFIED
	switch event.Kind {
	case discovery.EventAdded:
		kind = frameworkv1.DiscoveredService_EVENT_KIND_ADDED
	case discovery.EventRemoved:
		kind = frameworkv1.DiscoveredService_EVENT_KIND_REMOVED
	}
	return &frameworkv1.DiscoveredService{
		Kind: kind, Name: event.Name, ServiceType: event.Type, Domain: event.Domain,
		Host: event.Host, Addresses: event.Addresses, Port: event.Port, Txt: event.TXT,
	}
}

// mdnsRPCError maps responder failures to explicit gRPC codes. A missing
// responder is never reported as a successful publication.
func mdnsRPCError(op string, err error) error {
	switch {
	case errors.Is(err, discovery.ErrInvalidRequest):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, discovery.ErrUnsupported):
		return status.Errorf(codes.FailedPrecondition, "%s: %v", op, err)
	case errors.Is(err, discovery.ErrResponderNotReady):
		return status.Errorf(codes.FailedPrecondition, "%s: %v", op, err)
	case errors.Is(err, discovery.ErrResponderLost):
		return status.Errorf(codes.Unavailable, "%s: %v", op, err)
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, op+" cancelled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, op+" timed out")
	default:
		return status.Errorf(codes.Internal, "%s: %v", op, err)
	}
}

// removeRunPublications releases every registration owned by a terminated run.
// It is called from cleanupRun, so an app crash cannot leave a public
// advertisement behind.
func (d *Daemon) removeRunPublications(runID string) {
	if d.mdns == nil {
		return
	}
	d.mdnsMu.Lock()
	ids := make([]string, 0, len(d.mdnsPubs))
	for id, rec := range d.mdnsPubs {
		if rec.runID == runID {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		delete(d.mdnsPubs, id)
	}
	d.mdnsMu.Unlock()
	if len(ids) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, id := range ids {
		if _, err := d.mdns.Unpublish(ctx, id); err != nil {
			d.logs.Warnf("mdns", "", "release publication %s for run=%s: %v", id, runID, err)
		}
	}
}
