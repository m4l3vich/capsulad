package webui

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	webv1 "capsulad/gen/go/capsulad/web/v1"
	"capsulad/internal/uichanges"
)

// Subscriptions translate the daemon's streams into browser events. Each
// connection owns at most one watcher per topic/app pair; cancelling the
// subscription or closing the socket ends the upstream stream. Updates are
// coalesced so a burst of run transitions cannot flood a slow browser, and an
// overflow is reported as a resync requirement instead of silent loss.

const (
	runDebounce   = 150 * time.Millisecond
	logFlush      = 150 * time.Millisecond
	maxLogBatch   = 100
	maxLogPending = 400
)

// topicSignals subscribes to the in-process invalidation hub for topics that
// have no framework watch stream. The caller must install this before taking
// its initial snapshot so a racing mutation is not lost.
func (s *Server) topicSignals(topic webv1.Topic) (<-chan uichanges.Topic, func()) {
	if s.Changes == nil {
		return nil, nil
	}
	switch topic {
	case webv1.Topic_TOPIC_WAKEWORD, webv1.Topic_TOPIC_ACCESS, webv1.Topic_TOPIC_APPLICATION:
		// These topics subscribe to the shared invalidation hub; each watcher
		// filters the signals it understands. Subscribing before the initial
		// snapshot closes the mutation race for topics without a watch stream.
		return s.Changes.Subscribe()
	case webv1.Topic_TOPIC_WIFI, webv1.Topic_TOPIC_OVERVIEW, webv1.Topic_TOPIC_SETTINGS:
		return s.Changes.Subscribe()
	default:
		return nil, nil
	}
}

// closeForUpstreamLoss closes the socket when the daemon stream that feeds a
// subscription ends or fails. Leaving the subscription registered would keep
// the tab silently stale: the browser only re-sends subscriptions after a
// reconnect. This is deliberate teardown, not a retry loop.
func (c *wsConn) closeForUpstreamLoss() {
	c.closeWith(closeInternal, "live updates stopped")
}

// policyRefusal reports whether err is a per-read refusal — the caller may not
// see this data, or the target no longer exists — rather than a broken
// transport.
//
// A refusal must not tear down the browser socket. Closing it would turn one
// denied read into a "disconnected" banner and a reconnect loop for a device
// that is answering fine, and the reconnected socket would issue the same
// refused read again. The initial snapshot already delivered the error to the
// page, which shows it inline; the refused watcher simply ends because a
// denied read cannot become allowed inside this connection epoch.
func policyRefusal(err error) bool {
	switch status.Code(err) {
	case codes.PermissionDenied, codes.Unauthenticated, codes.NotFound,
		codes.InvalidArgument, codes.FailedPrecondition:
		return true
	}
	return false
}

// stopRefusedWatcher ends one watcher whose upstream read was refused, keeping
// the socket and every other subscription alive.
func (c *wsConn) stopRefusedWatcher(topic webv1.Topic, appID string, err error) {
	if c.srv.Logs != nil {
		c.srv.Logs.Warnf("webui", appID, "subscription %d stopped: %v", topic, err)
	}
}

// upstreamLost applies the teardown rule to a failed subscription read: a
// per-read refusal stops only that watcher, any transport failure closes the
// socket so the browser reconnects and re-subscribes.
func (c *wsConn) upstreamLost(topic webv1.Topic, appID string, err error) {
	if policyRefusal(err) {
		c.stopRefusedWatcher(topic, appID, err)
		return
	}
	c.closeForUpstreamLoss()
}

func (s *Server) watchTopic(ctx context.Context, c *wsConn, topic webv1.Topic, appID string) {
	signals, cancel := s.topicSignals(topic)
	if cancel != nil {
		defer cancel()
	}
	s.watchTopicWith(ctx, c, topic, appID, signals, logSnapshotBoundary{})
}

func (s *Server) watchTopicWith(ctx context.Context, c *wsConn, topic webv1.Topic, appID string, signals <-chan uichanges.Topic, logBoundary logSnapshotBoundary) {
	switch topic {
	case webv1.Topic_TOPIC_OVERVIEW:
		// The settings stream blocks for the subscription lifetime; run it
		// beside the run watcher instead of before it. Bluetooth and settings
		// invalidations rebuild observed/adapter and audio state.
		go s.watchOverviewSettings(ctx, c)
		go s.watchOverviewBluetooth(ctx, c, signals)
		s.watchRuns(ctx, c, "", webv1.Topic_TOPIC_OVERVIEW, func() {
			ctx2, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			view, err := s.overviewView(ctx2)
			if err != nil {
				c.sendResync(topic, appID)
				return
			}
			c.sendEvent(&webv1.Event{Topic: topic, Body: &webv1.Event_Overview{Overview: view}})
		})
	case webv1.Topic_TOPIC_SETTINGS:
		go s.watchSettingsSignals(ctx, c, signals)
		s.watchSettings(ctx, c)
	case webv1.Topic_TOPIC_APPLICATION:
		// watchRuns blocks for the lifetime of the subscription, so the
		// independent settings/log watchers must start first or they never run.
		go s.watchApplicationSignals(ctx, c, appID, signals)
		go s.watchLogs(ctx, c, appID, logBoundary)
		s.watchRuns(ctx, c, appID, webv1.Topic_TOPIC_APPLICATION, func() {
			ctx2, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			view, err := s.applicationView(ctx2, appID)
			if err != nil {
				c.sendResync(topic, appID)
				return
			}
			c.sendEvent(&webv1.Event{Topic: topic, AppId: appID, Body: &webv1.Event_Application{Application: view}})
		})
	case webv1.Topic_TOPIC_DIAGNOSTICS:
		// watchRuns blocks, so the log stream and the low-frequency host
		// metrics sampler must run beside it.
		go s.watchLogs(ctx, c, "", logBoundary)
		go s.watchDiagnosticsMetrics(ctx, c)
		s.watchRuns(ctx, c, "", webv1.Topic_TOPIC_DIAGNOSTICS, func() {
			ctx2, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			view, err := s.diagnosticsView(ctx2)
			if err != nil {
				c.sendResync(topic, appID)
				return
			}
			c.sendEvent(&webv1.Event{Topic: topic, Body: &webv1.Event_Diagnostics{Diagnostics: view}})
		})
	case webv1.Topic_TOPIC_WAKEWORD:
		s.watchWakewordSignals(ctx, c, signals)
	case webv1.Topic_TOPIC_ACCESS:
		s.watchAccessSignals(ctx, c, signals)
	case webv1.Topic_TOPIC_WIFI:
		s.watchWifiSignals(ctx, c, signals)
	}
}

// watchWakeword is the standalone entry point used by focused tests; the
// socket path passes an already-installed subscription to the *Signals form.
func (s *Server) watchWakeword(ctx context.Context, c *wsConn) {
	signals, cancel := s.topicSignals(webv1.Topic_TOPIC_WAKEWORD)
	if cancel == nil {
		return
	}
	defer cancel()
	s.watchWakewordSignals(ctx, c, signals)
}

// watchApplicationSignals republishes the application view when any app's
// settings change. The hub carries no payload, so the authoritative
// configuration snapshot is rebuilt here; a short coalescing window keeps a
// slider drag from flooding every open session with full views.
func (s *Server) watchApplicationSignals(ctx context.Context, c *wsConn, appID string, signals <-chan uichanges.Topic) {
	if signals == nil {
		return
	}
	var timer *time.Timer
	var timerC <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	flush := func() {
		viewCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		view, err := s.applicationView(viewCtx, appID)
		cancel()
		if err != nil {
			c.sendResync(webv1.Topic_TOPIC_APPLICATION, appID)
			return
		}
		c.sendEvent(&webv1.Event{Topic: webv1.Topic_TOPIC_APPLICATION, AppId: appID,
			Body: &webv1.Event_Application{Application: view}})
	}
	for {
		select {
		case <-ctx.Done():
			return
		case signal, ok := <-signals:
			if !ok {
				return
			}
			if signal != uichanges.AppSettings {
				continue
			}
			if timer == nil {
				timer = time.NewTimer(75 * time.Millisecond)
				timerC = timer.C
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(75 * time.Millisecond)
			}
		case <-timerC:
			timer = nil
			timerC = nil
			flush()
		}
	}
}

// watchWakewordSignals turns an in-process catalog invalidation into a full
// view. The signal carries no data, so a burst coalesces into authoritative
// reads rather than replaying stale samples or models.
func (s *Server) watchWakewordSignals(ctx context.Context, c *wsConn, signals <-chan uichanges.Topic) {
	if signals == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case signal, ok := <-signals:
			if !ok {
				return
			}
			if signal != uichanges.Wakeword {
				continue
			}
			viewCtx, viewCancel := context.WithTimeout(ctx, 8*time.Second)
			view, err := s.wakewordView(viewCtx)
			viewCancel()
			if err != nil {
				c.sendResync(webv1.Topic_TOPIC_WAKEWORD, "")
				continue
			}
			c.sendEvent(&webv1.Event{Topic: webv1.Topic_TOPIC_WAKEWORD,
				Body: &webv1.Event_Wakeword{Wakeword: view}})
		}
	}
}

func (s *Server) watchAccess(ctx context.Context, c *wsConn) {
	signals, cancel := s.topicSignals(webv1.Topic_TOPIC_ACCESS)
	if cancel == nil {
		return
	}
	defer cancel()
	s.watchAccessSignals(ctx, c, signals)
}

// watchAccessSignals publishes API-key changes to every browser session, not
// only the one that issued the command. Password changes close all sockets, so
// the next login takes a fresh snapshot.
func (s *Server) watchAccessSignals(ctx context.Context, c *wsConn, signals <-chan uichanges.Topic) {
	if signals == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case signal, ok := <-signals:
			if !ok {
				return
			}
			if signal != uichanges.Access {
				continue
			}
			c.sendEvent(&webv1.Event{Topic: webv1.Topic_TOPIC_ACCESS,
				Body: &webv1.Event_Access{Access: s.accessView()}})
		}
	}
}

// publish invalidates a topic for every live browser socket. A missing hub is
// a snapshot-only server (focused tests), not an error.
func (s *Server) publish(topic uichanges.Topic) {
	if s.Changes != nil {
		s.Changes.Publish(topic)
	}
}

// watchOverviewBluetooth republishes the overview when observed adapter state
// changes (a BlueZ-side change, not only a persisted setting).
func (s *Server) watchOverviewBluetooth(ctx context.Context, c *wsConn, signals <-chan uichanges.Topic) {
	if signals == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case signal, ok := <-signals:
			if !ok {
				return
			}
			if signal != uichanges.Bluetooth {
				continue
			}
			viewCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			view, err := s.overviewView(viewCtx)
			cancel()
			if err != nil {
				c.sendResync(webv1.Topic_TOPIC_OVERVIEW, "")
				continue
			}
			c.sendEvent(&webv1.Event{Topic: webv1.Topic_TOPIC_OVERVIEW, Body: &webv1.Event_Overview{Overview: view}})
		}
	}
}

// watchSettingsSignals republishes the settings view on Bluetooth status
// changes that are not persisted-setting updates.
func (s *Server) watchSettingsSignals(ctx context.Context, c *wsConn, signals <-chan uichanges.Topic) {
	if signals == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case signal, ok := <-signals:
			if !ok {
				return
			}
			if signal != uichanges.Bluetooth {
				continue
			}
			viewCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			view, err := s.settingsView(viewCtx)
			cancel()
			if err != nil {
				c.sendResync(webv1.Topic_TOPIC_SETTINGS, "")
				continue
			}
			c.sendEvent(&webv1.Event{Topic: webv1.Topic_TOPIC_SETTINGS, Body: &webv1.Event_Settings{Settings: view}})
		}
	}
}

// watchOverviewSettings republishes the overview when device settings change,
// so volume, mute and Bluetooth toggles changed by hardware or another session
// appear without a refresh. It is separate from TOPIC_SETTINGS because the
// overview also carries audio and Bluetooth state.
func (s *Server) watchOverviewSettings(ctx context.Context, c *wsConn) {
	stream, err := frameworkv1.NewSettingsServiceClient(s.Conn).WatchSettings(ctx, &frameworkv1.WatchSettingsRequest{})
	if err != nil {
		if ctx.Err() == nil {
			c.closeForUpstreamLoss()
		}
		return
	}
	updates := make(chan struct{}, 1)
	errs := make(chan error, 1)
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				errs <- err
				return
			}
			select {
			case updates <- struct{}{}:
			default:
			}
		}
	}()
	var timer *time.Timer
	var timerC <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-errs:
			if ctx.Err() == nil {
				c.closeForUpstreamLoss()
			}
			return
		case <-updates:
			if timer == nil {
				timer = time.NewTimer(150 * time.Millisecond)
				timerC = timer.C
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(150 * time.Millisecond)
			}
		case <-timerC:
			timer = nil
			timerC = nil
			viewCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			view, err := s.overviewView(viewCtx)
			cancel()
			if err != nil {
				c.sendResync(webv1.Topic_TOPIC_OVERVIEW, "")
				continue
			}
			c.sendEvent(&webv1.Event{Topic: webv1.Topic_TOPIC_OVERVIEW, Body: &webv1.Event_Overview{Overview: view}})
		}
	}
}

// watchRuns debounces run-transition streams into at most one refresh per
// debounce window while still delivering the trailing state.
func (s *Server) watchRuns(ctx context.Context, c *wsConn, appID string, topic webv1.Topic, refresh func()) {
	stream, err := frameworkv1.NewApplicationServiceClient(s.Conn).WatchRuns(ctx, &frameworkv1.WatchRunsRequest{AppId: appID})
	if err != nil {
		if ctx.Err() == nil {
			c.upstreamLost(topic, appID, err)
		}
		return
	}
	events := make(chan error, 1)
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				select {
				case events <- err:
				case <-ctx.Done():
				}
				return
			}
			select {
			case events <- nil:
			case <-ctx.Done():
				return
			}
		}
	}()
	var timer *time.Timer
	var timerC <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-events:
			if err != nil {
				if ctx.Err() == nil {
					c.upstreamLost(topic, appID, err)
				}
				return
			}
			if timer == nil {
				timer = time.NewTimer(runDebounce)
				timerC = timer.C
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(runDebounce)
			}
		case <-timerC:
			timer = nil
			timerC = nil
			refresh()
		}
	}
}

func (s *Server) watchSettings(ctx context.Context, c *wsConn) {
	stream, err := frameworkv1.NewSettingsServiceClient(s.Conn).WatchSettings(ctx, &frameworkv1.WatchSettingsRequest{})
	if err != nil {
		if ctx.Err() == nil {
			c.closeForUpstreamLoss()
		}
		return
	}
	updates := make(chan struct{}, 1)
	errs := make(chan error, 1)
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				errs <- err
				return
			}
			select {
			case updates <- struct{}{}:
			default:
			}
		}
	}()
	var timer *time.Timer
	var timerC <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	flush := func() {
		ctx2, cancel := context.WithTimeout(ctx, 8*time.Second)
		view, err := s.settingsView(ctx2)
		cancel()
		if err != nil {
			c.sendResync(webv1.Topic_TOPIC_SETTINGS, "")
			return
		}
		c.sendEvent(&webv1.Event{Topic: webv1.Topic_TOPIC_SETTINGS, Body: &webv1.Event_Settings{Settings: view}})
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-errs:
			if ctx.Err() == nil {
				c.closeForUpstreamLoss()
			}
			return
		case <-updates:
			// Debounce paired edits but always deliver the trailing update;
			// never drop a settings snapshot merely because another arrived
			// inside the floor window.
			if timer == nil {
				timer = time.NewTimer(100 * time.Millisecond)
				timerC = timer.C
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(100 * time.Millisecond)
			}
		case <-timerC:
			timer = nil
			timerC = nil
			flush()
		}
	}
}

// watchDiagnosticsMetrics pushes only the host metrics object at a low fixed
// rate while this diagnostics subscription lives. It never rebuilds logs or
// runs, and it does not run on a platform without real host telemetry.
func (s *Server) watchDiagnosticsMetrics(ctx context.Context, c *wsConn) {
	if !platformMetricsSupported() {
		return
	}
	tick := time.NewTicker(metricsSamplePeriod)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			c.sendEvent(&webv1.Event{Topic: webv1.Topic_TOPIC_DIAGNOSTICS,
				Body: &webv1.Event_DiagnosticsMetrics{DiagnosticsMetrics: s.metricsView()}})
		}
	}
}

func (s *Server) watchLogs(ctx context.Context, c *wsConn, appID string, logBoundary logSnapshotBoundary) {
	topic := webv1.Topic_TOPIC_DIAGNOSTICS
	if appID != "" {
		topic = webv1.Topic_TOPIC_APPLICATION
	}
	stream, err := frameworkv1.NewDiagnosticsServiceClient(s.Conn).WatchLogs(ctx, &frameworkv1.WatchLogsRequest{AppId: appID})
	if err != nil {
		if ctx.Err() == nil {
			c.upstreamLost(topic, appID, err)
		}
		return
	}
	type recv struct {
		entry *webv1.LogEntry
		err   error
	}
	ch := make(chan recv, 64)
	go func() {
		for {
			record, err := stream.Recv()
			if err != nil {
				select {
				case ch <- recv{err: err}:
				case <-ctx.Done():
				}
				return
			}
			entry := &webv1.LogEntry{
				MonotonicTimeNs: record.GetMonotonicTimeNs(), Component: record.GetComponent(),
				AppId: record.GetAppId(), Severity: record.GetSeverity(), Message: record.GetMessage(),
			}
			select {
			case ch <- recv{entry: entry}:
			case <-ctx.Done():
				return
			}
		}
	}()
	tick := time.NewTicker(logFlush)
	defer tick.Stop()
	pending := make([]*webv1.LogEntry, 0, maxLogBatch)
	flush := func() {
		if len(pending) == 0 {
			return
		}
		c.sendEvent(&webv1.Event{Topic: topic, AppId: appID,
			Body: &webv1.Event_Logs{Logs: &webv1.LogEntries{Entries: pending}}})
		pending = make([]*webv1.LogEntry, 0, maxLogBatch)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			flush()
		case r := <-ch:
			if r.err != nil {
				flush()
				if ctx.Err() == nil {
					c.upstreamLost(topic, appID, r.err)
				}
				return
			}
			if logBoundary.includes(r.entry) {
				// Already present in the initial snapshot: the upstream stream
				// replays its ring from the oldest retained record.
				continue
			}
			pending = append(pending, r.entry)
			if len(pending) >= maxLogPending {
				// The browser is not draining a high-rate log stream fast
				// enough. Drop the batch and force a snapshot rather than
				// growing memory or pretending the gap did not happen.
				pending = pending[:0]
				c.sendResync(topic, appID)
			}
			if len(pending) >= maxLogBatch {
				flush()
			}
		}
	}
}

func (c *wsConn) sendResync(topic webv1.Topic, appID string) {
	c.sendEvent(&webv1.Event{Topic: topic, AppId: appID, ResyncRequired: true})
}
