package assistant

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"capsulad/apps/opencapsula/internal/commands"
	"capsulad/apps/opencapsula/internal/protocol"
	"capsulad/apps/opencapsula/internal/remote"
	"capsulad/apps/opencapsula/internal/ring"
	"capsulad/sdk/capsulad"
	"capsulad/sdk/websocket"
)

const (
	pingInterval       = 20 * time.Second
	pongTimeout        = 10 * time.Second
	commandResultWrite = 5 * time.Second
	commandBudget      = 10 * time.Second
	inboundQueue       = 64
)

// session is one established device link and the turn runner that serializes
// conversations on it.
type session struct {
	a    *Assistant
	link *remote.Link
	log  *log.Logger

	ctx    context.Context
	cancel context.CancelFunc

	turns chan activationItem

	mu      sync.Mutex
	active  *turn
	pending *activationItem
}

type inboundFrame struct {
	op      websocket.Opcode
	payload []byte
}

func newSession(a *Assistant, link *remote.Link) *session {
	return &session{
		a:     a,
		link:  link,
		log:   a.log,
		turns: make(chan activationItem, activationQueue),
	}
}

// run serves the link until it drops or the app exits. It returns only after
// the turn runner has released the microphone, playback and ring.
func (s *session) run() {
	s.ctx, s.cancel = context.WithCancel(s.a.ctx)
	defer s.cancel()

	inbound := make(chan inboundFrame, inboundQueue)
	readErr := make(chan error, 1)
	go s.readLoop(inbound, readErr)
	go s.keepAlive()

	runnerDone := make(chan struct{})
	go s.turnRunner(runnerDone)

	for {
		select {
		case <-s.ctx.Done():
			s.shutdown(runnerDone)
			return
		case err := <-readErr:
			if s.ctx.Err() == nil {
				s.log.Printf("opencapsula: link read: %v", err)
			}
			s.shutdown(runnerDone)
			return
		case item := <-s.a.activations:
			s.onActivation(item)
		case action := <-s.a.actions:
			s.onDismiss(action)
		case frame := <-inbound:
			s.dispatch(frame)
		}
	}
}

func (s *session) shutdown(runnerDone <-chan struct{}) {
	s.cancel()
	s.mu.Lock()
	active := s.active
	s.pending = nil
	s.mu.Unlock()
	if active != nil {
		active.Cancel()
	}
	s.link.CloseNow()
	select {
	case <-runnerDone:
	case <-time.After(5 * time.Second):
		s.log.Printf("opencapsula: turn runner did not stop promptly")
	}
	s.a.ring.Release()
}

func (s *session) readLoop(inbound chan<- inboundFrame, readErr chan<- error) {
	for {
		op, payload, err := s.link.Read(s.ctx)
		if err != nil {
			select {
			case readErr <- err:
			case <-s.ctx.Done():
			}
			return
		}
		select {
		case inbound <- inboundFrame{op: op, payload: payload}:
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *session) keepAlive() {
	pong := make(chan struct{}, 1)
	s.link.SetPongHandler(func([]byte) {
		select {
		case pong <- struct{}{}:
		default:
		}
	})
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			select {
			case <-pong:
			default:
			}
			if err := s.link.Ping(s.ctx); err != nil {
				if s.ctx.Err() == nil {
					s.log.Printf("opencapsula: ping: %v", err)
				}
				s.link.CloseNow()
				return
			}
			timer := time.NewTimer(pongTimeout)
			select {
			case <-pong:
				timer.Stop()
			case <-timer.C:
				select {
				case <-pong:
					continue
				default:
				}
				s.log.Printf("opencapsula: no pong within %s; dropping the link", pongTimeout)
				s.link.CloseNow()
				return
			case <-s.ctx.Done():
				timer.Stop()
				return
			}
		}
	}
}

func (s *session) dispatch(frame inboundFrame) {
	if frame.op == websocket.OpBinary {
		s.mu.Lock()
		active := s.active
		s.mu.Unlock()
		if active != nil {
			active.PushAudio(frame.payload)
		}
		return
	}
	if frame.op != websocket.OpText {
		return
	}
	message, err := protocol.Decode(frame.payload)
	if err != nil {
		s.log.Printf("opencapsula: inbound message: %v", err)
		return
	}
	switch message.Type {
	case protocol.TypeTranscript:
		if message.Transcript != nil && message.Transcript.Final {
			s.log.Printf("opencapsula: transcript role=%s text=%q",
				message.Transcript.Role, message.Transcript.Text)
		}
	case protocol.TypeCommand:
		if message.Command != nil {
			go s.handleCommand(message.TurnID, message.Command)
		}
	case protocol.TypeListenEnd:
		s.pushControl(control{kind: controlListenEnd, turnID: message.TurnID})
	case protocol.TypeInterrupted:
		s.pushControl(control{kind: controlInterrupted, turnID: message.TurnID})
	case protocol.TypeTurnComplete:
		s.pushControl(control{kind: controlComplete, turnID: message.TurnID})
	case protocol.TypeError:
		if message.Error != nil {
			s.log.Printf("opencapsula: server error turn=%s code=%s message=%s",
				message.TurnID, message.Error.Code, message.Error.Message)
		}
		s.pushControl(control{kind: controlError, turnID: message.TurnID, serverErr: message.Error})
	case protocol.TypeReady:
		s.log.Printf("opencapsula: duplicate ready ignored")
	default:
		s.log.Printf("opencapsula: ignoring inbound %s", message.Type)
	}
}

func (s *session) pushControl(control control) {
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	if active == nil {
		return
	}
	active.PushControl(control)
}

func (s *session) onActivation(item activationItem) {
	if item.expired() {
		s.log.Printf("opencapsula: activation %s expired before its turn", item.activation.ID)
		return
	}
	s.mu.Lock()
	active := s.active
	if active == nil {
		s.mu.Unlock()
		select {
		case s.turns <- item:
		default:
			s.log.Printf("opencapsula: activation %s dropped; turn queue is full", item.activation.ID)
		}
		return
	}
	state := active.State()
	if state == turnStateListening {
		s.mu.Unlock()
		s.log.Printf("opencapsula: activation %s ignored while listening", item.activation.ID)
		return
	}
	s.pending = &item
	s.mu.Unlock()
	s.log.Printf("opencapsula: activation %s barges in on turn %s (%s)",
		item.activation.ID, active.ID(), state)
	active.Cancel()
}

func (s *session) onDismiss(action actionItem) {
	s.mu.Lock()
	active := s.active
	s.pending = nil
	s.mu.Unlock()
	if active != nil {
		s.log.Printf("opencapsula: dismiss cancels turn %s", active.ID())
		active.Cancel()
	}
	s.drainTurns()
	s.drainActivations()
	s.a.ring.Release()
	if action.invocation != nil {
		go s.a.notifyAction(action.invocation, true, "dismissed")
	}
}

func (s *session) drainTurns() {
	for {
		select {
		case <-s.turns:
		default:
			return
		}
	}
}

func (s *session) drainActivations() {
	for {
		select {
		case <-s.a.activations:
		default:
			return
		}
	}
}

func (s *session) handleCommand(turnID string, command *protocol.Command) {
	ctx, cancel := context.WithTimeout(s.ctx, commandBudget)
	defer cancel()
	data, err := s.executeCommand(ctx, command)
	result := &protocol.CommandResult{ID: command.ID, OK: err == nil, Data: data}
	if err != nil {
		result.Error = err.Error()
		s.log.Printf("opencapsula: command %s failed: %v", command.Action, err)
	} else {
		s.log.Printf("opencapsula: command %s ok", command.Action)
	}
	writeCtx, writeCancel := context.WithTimeout(s.ctx, commandResultWrite)
	defer writeCancel()
	err = s.link.SendMessage(writeCtx, &protocol.Message{
		Type:          protocol.TypeCommandResult,
		TurnID:        turnID,
		CommandResult: result,
	})
	if err != nil && s.ctx.Err() == nil {
		s.log.Printf("opencapsula: command result for %s: %v", command.Action, err)
	}
}

func (s *session) executeCommand(ctx context.Context, command *protocol.Command) (map[string]any, error) {
	switch command.Action {
	case commands.ActionVolumeGet:
		state, err := s.a.client.AudioState(ctx)
		if err != nil {
			return nil, err
		}
		return commands.VolumeData(state), nil

	case commands.ActionMediaStatus:
		state, err := s.a.client.BluetoothMediaState(ctx)
		if err != nil {
			return nil, err
		}
		return commands.MediaData(state), nil

	case commands.ActionMediaControl:
		operation, ok := command.String("operation")
		if !ok {
			return nil, fmt.Errorf("media.control requires an operation")
		}
		bluetoothCommand, err := commands.BluetoothCommand(operation)
		if err != nil {
			return nil, err
		}
		state, err := s.a.client.BluetoothMediaState(ctx)
		if err != nil {
			return nil, err
		}
		if !state.Present {
			return nil, fmt.Errorf("no Bluetooth audio source is connected")
		}
		state, err = s.a.client.BluetoothMediaCommand(ctx, bluetoothCommand)
		if err != nil {
			return nil, err
		}
		return commands.MediaData(state), nil

	case commands.ActionConversationEnd:
		s.mu.Lock()
		active := s.active
		ending := active != nil
		if active != nil {
			active.SetEnding()
		}
		s.mu.Unlock()
		return map[string]any{"ending": ending}, nil

	default:
		return nil, fmt.Errorf("unsupported action %q", command.Action)
	}
}

func (a *Assistant) notifyAction(invocation *capsulad.ActionInvoked, succeeded bool, detail string) {
	if invocation == nil || invocation.ID == "" {
		return
	}
	if err := a.client.NotifyActionResult(invocation.ID, succeeded, detail); err != nil {
		a.log.Printf("opencapsula: action result: %v", err)
	}
}

// turnRunner serializes conversations. A barge-in stores exactly one pending
// activation and cancels the active turn; the runner starts that activation
// after the old microphone, playback and conversation are released.
func (s *session) turnRunner(done chan<- struct{}) {
	defer close(done)
	for {
		item, ok := s.nextTurn()
		if !ok {
			return
		}
		if item.expired() {
			s.log.Printf("opencapsula: activation %s expired before its turn", item.activation.ID)
			continue
		}
		t := newTurn(s, &item)
		s.mu.Lock()
		s.active = t
		s.mu.Unlock()

		t.run()
		failed := t.Failed()

		s.mu.Lock()
		if s.active == t {
			s.active = nil
		}
		next := s.pending
		s.pending = nil
		s.mu.Unlock()

		if next != nil {
			s.mu.Lock()
			s.pending = next
			s.mu.Unlock()
			continue
		}
		if s.ctx.Err() != nil {
			return
		}
		if failed {
			s.showError()
		} else {
			s.a.ring.Release()
		}
	}
}

func (s *session) nextTurn() (activationItem, bool) {
	s.mu.Lock()
	if s.pending != nil {
		item := *s.pending
		s.pending = nil
		s.mu.Unlock()
		return item, true
	}
	s.mu.Unlock()
	select {
	case item := <-s.turns:
		return item, true
	case <-s.ctx.Done():
		return activationItem{}, false
	}
}

// showError leaves the red indication visible briefly, unless a newer turn
// already took the ring.
func (s *session) showError() {
	s.a.ring.Set(ring.StateError)
	generation := s.a.ring.Generation()
	go func() {
		timer := time.NewTimer(1200 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			if s.a.ring.Generation() == generation {
				s.a.ring.Release()
			}
		case <-s.ctx.Done():
			return
		}
	}()
}
