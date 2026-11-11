// Package presentations owns dynamic runtime presentations.
//
// Apps create a presentation for a display or ring resource and stream content
// into it. A presentation only becomes eligible for physical selection after
// its first valid content. There are no declared output ports or stable output
// IDs, and closing the stream destroys the presentation and its captures.
package presentations

import (
	"fmt"
	"sync"

	"capsulad/internal/backend"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/presets"
)

type Kind int

const (
	Segment Kind = iota + 1
	Ring
)

func (k Kind) String() string {
	switch k {
	case Segment:
		return "segment"
	case Ring:
		return "ring"
	default:
		return "unknown"
	}
}

type ContentType int

const (
	ContentNone ContentType = iota
	ContentSegmentFrame
	ContentRingFrame
	ContentRingEffect
	ContentSegmentFace
	ContentBlank
)

// Content is one in-place presentation value. Replacing an effect with a raw
// frame does not change ownership.
type Content struct {
	Type     ContentType
	Cells    []uint16
	Colon    bool
	Pixels   []backend.RGB
	PresetID string
	Params   presets.Params
}

func SegmentFrame(cells []uint16, colon bool) Content {
	return Content{Type: ContentSegmentFrame, Cells: append([]uint16(nil), cells...), Colon: colon}
}

func RingFrame(pixels []backend.RGB) Content {
	return Content{Type: ContentRingFrame, Pixels: append([]backend.RGB(nil), pixels...)}
}

func RingEffect(id string, params presets.Params) Content {
	return Content{Type: ContentRingEffect, PresetID: id, Params: params}
}

func SegmentFace(id string, params presets.Params) Content {
	return Content{Type: ContentSegmentFace, PresetID: id, Params: params}
}

func Blank() Content { return Content{Type: ContentBlank} }

type EventType int

const (
	EventReady EventType = iota + 1
	EventVisibility
)

type Event struct {
	Type       EventType
	Visible    bool
	Reason     string
	HasContent bool
}

type Presentation struct {
	ID         string
	ResourceID string
	AppID      string
	RunID      string
	Kind       Kind
	CreatedNS  uint64

	mu         sync.Mutex
	content    Content
	hasContent bool
	lastUpdate uint64
	visible    bool
	visReason  string
	closed     bool
	subs       map[int]chan Event
	nextSub    int
	clock      *ids.Clock
}

func (p *Presentation) Info() (id, resourceID, appID, runID string, kind Kind) {
	return p.ID, p.ResourceID, p.AppID, p.RunID, p.Kind
}

// Ready reports whether the presentation has received valid content.
func (p *Presentation) Ready() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hasContent && !p.closed
}

// Closed reports whether the presentation stream has ended.
func (p *Presentation) Closed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// LastUpdateNS returns the monotonic time of the last content update.
func (p *Presentation) LastUpdateNS() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastUpdate
}

// Visible reports whether this presentation is currently selected.
func (p *Presentation) Visible() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.visible && !p.closed
}

// Subscribe returns buffered presentation events.
func (p *Presentation) Subscribe(buffer int) *Subscription {
	if buffer <= 0 {
		buffer = 8
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextSub++
	ch := make(chan Event, buffer)
	p.subs[p.nextSub] = ch
	return &Subscription{p: p, id: p.nextSub, C: ch}
}

type Subscription struct {
	p    *Presentation
	id   int
	C    <-chan Event
	once sync.Once
}

func (s *Subscription) Close() {
	s.once.Do(func() {
		s.p.mu.Lock()
		if ch, ok := s.p.subs[s.id]; ok {
			close(ch)
			delete(s.p.subs, s.id)
		}
		s.p.mu.Unlock()
	})
}

func (p *Presentation) emitLocked(e Event) {
	for _, ch := range p.subs {
		select {
		case ch <- e:
		default:
			// A slow presentation consumer must not stall routing.
		}
	}
}

// RenderSegment resolves segment content for the given geometry. Effects are
// not valid on segment presentations; only faces, frames and blanks are.
func (p *Presentation) RenderSegment(caps backend.SegmentCaps) ([]uint16, bool, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.hasContent || p.closed {
		return nil, false, false
	}
	switch p.content.Type {
	case ContentSegmentFrame:
		return append([]uint16(nil), p.content.Cells...), p.content.Colon, true
	case ContentSegmentFace:
		cells, colon, err := presets.SegmentMask(p.content.PresetID, caps)
		if err != nil {
			return nil, false, true
		}
		return cells, colon, true
	case ContentBlank:
		return make([]uint16, caps.DigitCount), false, true
	default:
		return nil, false, false
	}
}

// RenderRing resolves ring content at nowNS. Built-in effects are rendered by
// the framework so apps do not stream every animation frame.
func (p *Presentation) RenderRing(caps backend.RingCaps, nowNS uint64) ([]backend.RGB, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.hasContent || p.closed {
		return nil, false
	}
	switch p.content.Type {
	case ContentRingFrame:
		return fitPixels(p.content.Pixels, caps.PixelCount), true
	case ContentRingEffect:
		t := float64(nowNS-p.lastUpdate) / 1e9
		px, err := presets.RingFrame(p.content.PresetID, p.content.Params, caps.PixelCount, t)
		if err != nil {
			return make([]backend.RGB, caps.PixelCount), true
		}
		return px, true
	case ContentBlank:
		return make([]backend.RGB, caps.PixelCount), true
	default:
		return nil, false
	}
}

// ContentKind describes the current content for diagnostics.
func (p *Presentation) ContentKind() ContentType {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.content.Type
}

func fitPixels(in []backend.RGB, n int) []backend.RGB {
	out := make([]backend.RGB, n)
	copy(out, in)
	return out
}

// Manager owns all live presentations and notifies the router when eligibility
// or existence changes.
type Manager struct {
	clock    *ids.Clock
	logs     *logbus.Bus
	mu       sync.Mutex
	byID     map[string]*Presentation
	byRes    map[string][]*Presentation
	onChange func(resourceID string)
}

func NewManager(clock *ids.Clock, logs *logbus.Bus, onChange func(resourceID string)) *Manager {
	return &Manager{
		clock: clock, logs: logs,
		byID: map[string]*Presentation{}, byRes: map[string][]*Presentation{},
		onChange: onChange,
	}
}

// OnChange replaces the eligibility/removal callback. Used during wiring so the
// router and manager can reference each other safely.
func (m *Manager) OnChange(fn func(resourceID string)) { m.onChange = fn }

func (m *Manager) Open(resourceID, appID, runID string, kind Kind) *Presentation {
	p := &Presentation{
		ID: ids.New("prs"), ResourceID: resourceID, AppID: appID, RunID: runID,
		Kind: kind, CreatedNS: m.clock.NowNS(), subs: map[int]chan Event{},
	}
	m.mu.Lock()
	m.byID[p.ID] = p
	m.byRes[resourceID] = append(m.byRes[resourceID], p)
	m.mu.Unlock()
	return p
}

func (m *Manager) Get(id string) (*Presentation, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.byID[id]
	return p, ok
}

func (m *Manager) ListResource(resourceID string) []*Presentation {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]*Presentation(nil), m.byRes[resourceID]...)
	return out
}

// Update replaces presentation content in place. A first valid frame makes
// the presentation eligible and notifies the router.
func (m *Manager) Update(p *Presentation, c Content) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return fmt.Errorf("presentation %s is closed", p.ID)
	}
	if !validFor(p.Kind, c) {
		p.mu.Unlock()
		return fmt.Errorf("content type %d is not valid for a %s presentation", c.Type, p.Kind)
	}
	first := !p.hasContent
	p.content = c
	p.hasContent = true
	p.lastUpdate = m.clock.NowNS()
	p.mu.Unlock()

	if first {
		if m.onChange != nil {
			m.onChange(p.ResourceID)
		}
		p.mu.Lock()
		p.emitLocked(Event{Type: EventReady, HasContent: true})
		p.mu.Unlock()
	}
	m.checkBaselineContract(p)
	return nil
}

func validFor(kind Kind, c Content) bool {
	switch kind {
	case Segment:
		return c.Type == ContentSegmentFrame || c.Type == ContentSegmentFace || c.Type == ContentBlank
	case Ring:
		return c.Type == ContentRingFrame || c.Type == ContentRingEffect || c.Type == ContentBlank
	default:
		return false
	}
}

// checkBaselineContract logs (does not arbitrate) when a provider offers more
// than one eligible baseline candidate for the same resource.
func (m *Manager) checkBaselineContract(p *Presentation) {
	if p.AppID == "" {
		return
	}
	m.mu.Lock()
	var others int
	for _, other := range m.byRes[p.ResourceID] {
		if other == p || other.AppID != p.AppID {
			continue
		}
		if other.Ready() {
			others++
		}
	}
	m.mu.Unlock()
	if others > 0 && m.logs != nil {
		m.logs.Warnf("presentations", p.AppID,
			"contract error: app holds %d eligible presentations on %s; only the earliest is the baseline",
			others+1, p.ResourceID)
	}
}

// Close destroys a presentation. Captures referring to it are cleaned up by
// the router through the change callback.
func (m *Manager) Close(p *Presentation) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.visible = false
	p.mu.Unlock()

	m.mu.Lock()
	delete(m.byID, p.ID)
	list := m.byRes[p.ResourceID]
	for i, cand := range list {
		if cand == p {
			m.byRes[p.ResourceID] = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(m.byRes[p.ResourceID]) == 0 {
		delete(m.byRes, p.ResourceID)
	}
	m.mu.Unlock()

	p.mu.Lock()
	p.emitLocked(Event{Type: EventVisibility, Visible: false, Reason: "closed"})
	for id, ch := range p.subs {
		close(ch)
		delete(p.subs, id)
	}
	p.mu.Unlock()

	if m.onChange != nil {
		m.onChange(p.ResourceID)
	}
}

// RemoveRun closes every presentation owned by a process run.
func (m *Manager) RemoveRun(runID string) {
	m.mu.Lock()
	var doomed []*Presentation
	for _, p := range m.byID {
		if p.RunID == runID {
			doomed = append(doomed, p)
		}
	}
	m.mu.Unlock()
	for _, p := range doomed {
		m.Close(p)
	}
}

// SetVisible updates selection state and notifies the owning app.
func (m *Manager) SetVisible(p *Presentation, visible bool, reason string) {
	p.mu.Lock()
	if p.closed || p.visible == visible {
		p.mu.Unlock()
		return
	}
	p.visible = visible
	p.visReason = reason
	p.mu.Unlock()
	p.mu.Lock()
	p.emitLocked(Event{Type: EventVisibility, Visible: visible, Reason: reason})
	p.mu.Unlock()
}

// VisibleReason returns the last visibility reason (diagnostics).
func (p *Presentation) VisibleReason() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.visReason
}

// CloseAll closes every presentation (daemon shutdown).
func (m *Manager) CloseAll() {
	m.mu.Lock()
	all := make([]*Presentation, 0, len(m.byID))
	for _, p := range m.byID {
		all = append(all, p)
	}
	m.mu.Unlock()
	for _, p := range all {
		m.Close(p)
	}
}
