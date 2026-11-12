// Package routing implements temporary explicit captures and physical output
// selection.
//
// Selection for display/ring resources follows ARCHITECTURE.md section 4:
// latest acquired eligible live capture, else the configured default app's
// baseline presentation (including compiled-in system apps), else a framework
// fallback. There is no hidden provider tier: an app is selected only because
// it is the configured default, holds a capture, or is the framework fallback.
package routing

import (
	"fmt"
	"sort"
	"sync"

	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/presentations"
)

// SystemAppID owns framework fallback and overlay presentations. It is not an
// installed application and cannot be launched or configured.
const SystemAppID = "capsula.system"

type TouchMode int

const (
	TouchNone TouchMode = iota
	TouchPartial
	TouchFull
)

func (m TouchMode) String() string {
	switch m {
	case TouchPartial:
		return "partial"
	case TouchFull:
		return "full_raw"
	default:
		return "none"
	}
}

type Capture struct {
	ID             string
	ResourceID     string
	AppID          string
	RunID          string
	PresentationID string
	TouchMode      TouchMode
	AcquiredNS     uint64
	done           chan struct{}
}

// Done closes when ownership is released or its presentation/run disappears.
func (c *Capture) Done() <-chan struct{} { return c.done }

type CaptureView struct {
	Capture
	Ready    bool
	Selected bool
}

type Selection struct {
	ResourceID           string
	ConfiguredDefaultApp string
	SelectedPresentation string
	SelectedAppID        string
	SelectedCaptureID    string
	Reason               string
	TouchMode            TouchMode
	SystemTouchActive    bool
	Revision             uint64
}

type CaptureEvent struct {
	Granted *CaptureView
	Change  *SelectionChange
}

type SelectionChange struct {
	Selected bool
	Reason   string
}

type Router struct {
	clock *ids.Clock
	logs  *logbus.Bus

	mu        sync.Mutex
	pres      *presentations.Manager
	classes   map[string]string
	defaults  map[string]string
	captures  map[string][]*Capture
	selected  map[string]Selection
	revision  uint64
	watchers  map[string]map[int]chan CaptureEvent // capture ID -> watchers
	nextWatch int
	onChange  func(resourceID string)
}

func New(clock *ids.Clock, logs *logbus.Bus, pres *presentations.Manager) *Router {
	return &Router{
		clock: clock, logs: logs, pres: pres,
		defaults: map[string]string{}, classes: map[string]string{},
		captures: map[string][]*Capture{},
		selected: map[string]Selection{},
		watchers: map[string]map[int]chan CaptureEvent{},
	}
}

func (r *Router) SetOnChange(fn func(resourceID string)) { r.onChange = fn }

// SetResourceClass records the routing class: display, ring, touch,
// assistant, audio or sensor. Classes are declared by the daemon from the
// probed backend, never guessed inside capture validation.
func (r *Router) SetResourceClass(resourceID, class string) {
	r.mu.Lock()
	r.classes[resourceID] = class
	r.mu.Unlock()
}

func (r *Router) class(resourceID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.classes[resourceID]
}

// SetDefault sets the configured default application for a resource.
func (r *Router) SetDefault(resourceID, appID string) {
	r.mu.Lock()
	if appID == "" {
		delete(r.defaults, resourceID)
	} else {
		r.defaults[resourceID] = appID
	}
	r.mu.Unlock()
	r.Recompute(resourceID, "default changed")
}

func (r *Router) Default(resourceID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.defaults[resourceID]
}

// Acquire adds a capture. mode must be non-zero exactly for touch resources.
func (r *Router) Acquire(resourceID, appID, runID, presentationID string, mode TouchMode) (*Capture, error) {
	if resourceID == "" {
		return nil, fmt.Errorf("resource_id is required")
	}
	switch r.class(resourceID) {
	case "touch":
		if mode == TouchNone {
			return nil, fmt.Errorf("touch captures require a capture mode")
		}
		if presentationID != "" {
			return nil, fmt.Errorf("touch captures do not reference a presentation")
		}
	case "assistant":
		if mode != TouchNone || presentationID != "" {
			return nil, fmt.Errorf("assistant-role captures take no mode or presentation")
		}
	case "display", "ring":
		if mode != TouchNone {
			return nil, fmt.Errorf("touch_mode must be unset for %s captures", resourceID)
		}
		if presentationID == "" {
			return nil, fmt.Errorf("%s captures require a presentation_id", resourceID)
		}
		p, ok := r.pres.Get(presentationID)
		if !ok {
			return nil, fmt.Errorf("presentation %s not found", presentationID)
		}
		if p.AppID != appID {
			return nil, fmt.Errorf("presentation %s is not owned by %s", presentationID, appID)
		}
		want := presentations.Segment
		if r.class(resourceID) == "ring" {
			want = presentations.Ring
		}
		if p.Kind != want {
			return nil, fmt.Errorf("presentation %s is not valid for %s", presentationID, resourceID)
		}
	default:
		return nil, fmt.Errorf("resource %s does not support captures", resourceID)
	}
	c := &Capture{
		ID: ids.New("cap"), ResourceID: resourceID, AppID: appID, RunID: runID,
		PresentationID: presentationID, TouchMode: mode, AcquiredNS: r.clock.NowNS(),
		done: make(chan struct{}),
	}
	r.mu.Lock()
	r.captures[resourceID] = append(r.captures[resourceID], c)
	r.mu.Unlock()
	r.Recompute(resourceID, "capture acquired")
	return c, nil
}

// Release removes a capture owned by appID/runID. Out-of-order release is
// allowed; the previous capture may resume.
func (r *Router) Release(captureID, appID, runID string) bool {
	r.mu.Lock()
	for resourceID, list := range r.captures {
		for i, c := range list {
			if c.ID != captureID {
				continue
			}
			if appID != "" && c.AppID != appID {
				r.mu.Unlock()
				return false
			}
			if runID != "" && c.RunID != runID {
				r.mu.Unlock()
				return false
			}
			r.captures[resourceID] = append(list[:i], list[i+1:]...)
			close(c.done)
			r.mu.Unlock()
			r.Recompute(resourceID, "capture released")
			return true
		}
	}
	r.mu.Unlock()
	return false
}

// ForceRelease removes a capture regardless of owner (diagnostics/UI).
func (r *Router) ForceRelease(captureID string) bool {
	return r.Release(captureID, "", "")
}

// RemovePresentation drops captures that reference a destroyed presentation.
func (r *Router) RemovePresentation(presentationID string) {
	affected := map[string]bool{}
	r.mu.Lock()
	for resourceID, list := range r.captures {
		kept := list[:0]
		for _, c := range list {
			if c.PresentationID == presentationID {
				close(c.done)
				affected[resourceID] = true
				continue
			}
			kept = append(kept, c)
		}
		r.captures[resourceID] = kept
	}
	r.mu.Unlock()
	for resourceID := range affected {
		r.Recompute(resourceID, "presentation closed")
	}
}

// RemoveRun drops every capture owned by a terminated process.
func (r *Router) RemoveRun(runID string) {
	affected := map[string]bool{}
	r.mu.Lock()
	for resourceID, list := range r.captures {
		kept := list[:0]
		for _, c := range list {
			if c.RunID == runID {
				close(c.done)
				affected[resourceID] = true
				continue
			}
			kept = append(kept, c)
		}
		r.captures[resourceID] = kept
	}
	r.mu.Unlock()
	for resourceID := range affected {
		r.Recompute(resourceID, "app run ended")
	}
}

// recomputeSelection must be called with r.mu held.
func (r *Router) recomputeSelection(resourceID string) Selection {
	sel := Selection{ResourceID: resourceID, ConfiguredDefaultApp: r.defaults[resourceID]}
	list := r.captures[resourceID]
	class := r.classes[resourceID]

	if class == "touch" || class == "assistant" {
		if n := len(list); n > 0 {
			c := list[n-1]
			sel.SelectedAppID = c.AppID
			sel.SelectedCaptureID = c.ID
			sel.TouchMode = c.TouchMode
			sel.SystemTouchActive = class == "touch" && c.TouchMode != TouchFull
			sel.Reason = "capture"
			return sel
		}
		if class == "touch" {
			sel.SystemTouchActive = true
		}
		sel.Reason = "default"
		return sel
	}
	if class != "display" && class != "ring" {
		sel.Reason = "not_routable"
		return sel
	}

	// Display/ring: newest eligible capture wins. An unready newer capture does
	// not blank the older selected presentation.
	for i := len(list) - 1; i >= 0; i-- {
		c := list[i]
		if c.PresentationID == "" {
			continue
		}
		p, ok := r.pres.Get(c.PresentationID)
		if !ok || !p.Ready() {
			continue
		}
		sel.SelectedPresentation = p.ID
		sel.SelectedAppID = c.AppID
		sel.SelectedCaptureID = c.ID
		sel.Reason = "capture"
		return sel
	}

	// Configured default app's baseline presentation.
	if appID := r.defaults[resourceID]; appID != "" {
		for _, p := range r.pres.ListResource(resourceID) {
			if p.AppID == appID && p.Ready() {
				sel.SelectedPresentation = p.ID
				sel.SelectedAppID = p.AppID
				sel.Reason = "default_baseline"
				return sel
			}
		}
	}

	// Framework fallback presentation owned by SystemAppID.
	for _, p := range r.pres.ListResource(resourceID) {
		if p.AppID == SystemAppID && p.Ready() {
			sel.SelectedPresentation = p.ID
			sel.SelectedAppID = p.AppID
			sel.Reason = "system_fallback"
			return sel
		}
	}
	sel.Reason = "none"
	return sel
}

// Recompute recalculates one resource selection, updates visibility and emits
// capture selection changes.
func (r *Router) Recompute(resourceID, reason string) {
	r.mu.Lock()
	// A closed presentation is no longer an eligible output or a live
	// capture. Prune ownership, not merely selection, so clients and
	// diagnostics cannot retain a phantom capture indefinitely.
	list := r.captures[resourceID]
	kept := list[:0]
	for _, c := range list {
		if c.PresentationID != "" {
			p, exists := r.pres.Get(c.PresentationID)
			if !exists || p.Closed() {
				close(c.done)
				continue
			}
		}
		kept = append(kept, c)
	}
	r.captures[resourceID] = kept
	prev := r.selected[resourceID]
	next := r.recomputeSelection(resourceID)
	r.revision++
	next.Revision = r.revision
	r.selected[resourceID] = next
	prevWatchers := make([]chan CaptureEvent, 0)
	for _, ch := range r.watchers[prev.SelectedCaptureID] {
		prevWatchers = append(prevWatchers, ch)
	}
	nextWatchers := make([]chan CaptureEvent, 0)
	if next.SelectedCaptureID != prev.SelectedCaptureID {
		for _, ch := range r.watchers[next.SelectedCaptureID] {
			nextWatchers = append(nextWatchers, ch)
		}
	}
	r.mu.Unlock()

	// Presentation visibility is derived from selection.
	if prev.SelectedPresentation != next.SelectedPresentation {
		if prev.SelectedPresentation != "" {
			if p, ok := r.pres.Get(prev.SelectedPresentation); ok {
				r.pres.SetVisible(p, false, "routing: "+reason)
			}
		}
		if next.SelectedPresentation != "" {
			if p, ok := r.pres.Get(next.SelectedPresentation); ok {
				r.pres.SetVisible(p, true, "routing: "+reason)
			}
		}
	}

	if prev.SelectedCaptureID != "" && prev.SelectedCaptureID != next.SelectedCaptureID {
		for _, ch := range prevWatchers {
			select {
			case ch <- CaptureEvent{Change: &SelectionChange{Selected: false, Reason: reason}}:
			default:
			}
		}
	}
	for _, ch := range nextWatchers {
		select {
		case ch <- CaptureEvent{Change: &SelectionChange{Selected: true, Reason: reason}}:
		default:
		}
	}

	if r.onChange != nil {
		r.onChange(resourceID)
	}
}

// Current returns the last computed selection for a resource.
func (r *Router) Current(resourceID string) Selection {
	r.mu.Lock()
	defer r.mu.Unlock()
	sel, computed := r.selected[resourceID]
	if !computed {
		sel = r.recomputeSelection(resourceID)
	}
	return sel
}

// State returns a full routing snapshot including capture list and readiness.
func (r *Router) State(resourceID string) (Selection, []CaptureView) {
	r.mu.Lock()
	sel := r.selected[resourceID]
	if sel.ResourceID == "" {
		sel = r.recomputeSelection(resourceID)
	}
	list := append([]*Capture(nil), r.captures[resourceID]...)
	r.mu.Unlock()
	views := make([]CaptureView, 0, len(list))
	for _, c := range list {
		ready := true
		if c.PresentationID != "" {
			ready = false
			if p, ok := r.pres.Get(c.PresentationID); ok {
				ready = p.Ready()
			}
		}
		views = append(views, CaptureView{Capture: *c, Ready: ready, Selected: c.ID == sel.SelectedCaptureID})
	}
	sort.SliceStable(views, func(i, j int) bool { return views[i].AcquiredNS < views[j].AcquiredNS })
	return sel, views
}

// SubscribeCapture returns current state and a buffered event channel.
func (r *Router) SubscribeCapture(c *Capture) (*CaptureView, <-chan CaptureEvent, func()) {
	r.mu.Lock()
	if r.watchers[c.ID] == nil {
		r.watchers[c.ID] = map[int]chan CaptureEvent{}
	}
	r.nextWatch++
	id := r.nextWatch
	ch := make(chan CaptureEvent, 16)
	r.watchers[c.ID][id] = ch
	sel := r.selected[c.ResourceID]
	r.mu.Unlock()

	ready := true
	if c.PresentationID != "" {
		ready = false
		if p, ok := r.pres.Get(c.PresentationID); ok {
			ready = p.Ready()
		}
	}
	view := &CaptureView{Capture: *c, Ready: ready, Selected: c.ID == sel.SelectedCaptureID}
	ch <- CaptureEvent{Granted: view}
	if view.Selected {
		ch <- CaptureEvent{Change: &SelectionChange{Selected: true, Reason: "granted"}}
	}
	closeFn := func() {
		r.mu.Lock()
		// The channel is deliberately not closed: Recompute may already hold a
		// snapshot of watchers and send without the lock. Removal is enough for
		// garbage collection, and the gRPC handler exits on its own context.
		if m := r.watchers[c.ID]; m != nil {
			delete(m, id)
			if len(m) == 0 {
				delete(r.watchers, c.ID)
			}
		}
		r.mu.Unlock()
	}
	return view, ch, closeFn
}

// Revision returns the current routing revision.
func (r *Router) Revision() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.revision
}

// AllSelections returns every computed selection sorted by resource ID.
func (r *Router) AllSelections() []Selection {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Selection, 0, len(r.selected))
	for _, s := range r.selected {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ResourceID < out[j].ResourceID })
	return out
}

// ActiveCaptures returns the newest capture for a resource, or nil.
func (r *Router) ActiveCaptures(resourceID string) []*Capture {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*Capture(nil), r.captures[resourceID]...)
}

// EffectiveTouchMode returns the mode currently governing the panel.
func (r *Router) EffectiveTouchMode(resourceID string) TouchMode {
	r.mu.Lock()
	defer r.mu.Unlock()
	sel := r.selected[resourceID]
	return sel.TouchMode
}
