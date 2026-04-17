// Package testutil provides a scripted FakeRuntime / FakeSession pair so the
// supervisor, bus, and event plumbing can be exercised without spawning real
// Claude subprocesses. Tests import this package; production code never does.
package testutil

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/atterpac/orca/pkg/orca"
)

// Script controls how a FakeSession behaves after Start. Each Step is fired in
// order, with Delay preceding the Step's Event emission.
//
// A Step emits either an Event (push into session.Events) or a Reply (a
// Message the session forwards to the supervisor's inbox as if another agent
// sent it). Steps with Usage update internal token counters.
type Script struct {
	Steps []Step
}

type Step struct {
	Delay time.Duration
	Event *orca.Event
	Usage *orca.TokenUsage
	OnSend func(m orca.Message, s *FakeSession)
}

type FakeRuntime struct {
	mu       sync.Mutex
	Name_    string
	Caps     orca.RuntimeCaps
	Scripts  map[string]*Script // keyed by agent id
	sessions map[string]*FakeSession
}

func NewRuntime() *FakeRuntime {
	return &FakeRuntime{
		Name_: "fake",
		Caps: orca.RuntimeCaps{
			Streaming:   true,
			NativeTools: false,
			SkillFormat: "none",
		},
		Scripts:  map[string]*Script{},
		sessions: map[string]*FakeSession{},
	}
}

func (r *FakeRuntime) Name() string                 { return r.Name_ }
func (r *FakeRuntime) Capabilities() orca.RuntimeCaps { return r.Caps }

func (r *FakeRuntime) SetScript(agentID string, s *Script) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Scripts[agentID] = s
}

func (r *FakeRuntime) Session(agentID string) *FakeSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[agentID]
}

func (r *FakeRuntime) Start(ctx context.Context, spec orca.AgentSpec) (orca.Session, error) {
	r.mu.Lock()
	script := r.Scripts[spec.ID]
	if script == nil {
		script = &Script{}
	}
	s := &FakeSession{
		id:     spec.ID,
		spec:   spec,
		events: make(chan orca.Event, 64),
		inbox:  make(chan orca.Message, 64),
		done:   make(chan struct{}),
		script: script,
	}
	r.sessions[spec.ID] = s
	r.mu.Unlock()

	go s.run(ctx)
	return s, nil
}

type FakeSession struct {
	id     string
	spec   orca.AgentSpec
	events chan orca.Event
	inbox  chan orca.Message
	done   chan struct{}
	script *Script

	mu      sync.RWMutex
	usage   orca.TokenUsage
	closed  atomic.Bool
	waitErr error

	sendMu sync.Mutex
	sent   []orca.Message // captures every Send for assertions; read via Sent()
}

// Sent returns a snapshot of all messages received via Send. Safe to call
// from test goroutines concurrently with Send.
func (s *FakeSession) Sent() []orca.Message {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	out := make([]orca.Message, len(s.sent))
	copy(out, s.sent)
	return out
}

func (s *FakeSession) ID() string { return s.id }

func (s *FakeSession) Usage() orca.TokenUsage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.usage
}

func (s *FakeSession) Send(ctx context.Context, m orca.Message) error {
	s.sendMu.Lock()
	s.sent = append(s.sent, m)
	s.sendMu.Unlock()
	for _, step := range s.script.Steps {
		if step.OnSend != nil {
			step.OnSend(m, s)
		}
	}
	select {
	case s.inbox <- m:
	default:
	}
	return nil
}

func (s *FakeSession) Events(ctx context.Context) (<-chan orca.Event, error) {
	return s.events, nil
}

func (s *FakeSession) Interrupt(ctx context.Context) error {
	_ = s.Close()
	return nil
}

func (s *FakeSession) Wait() error {
	<-s.done
	return s.waitErr
}

func (s *FakeSession) Close() error {
	// Signal run() to exit; it will invoke finish() which handles the close
	// sequence atomically (guarded by s.closed).
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	return nil
}

// Emit lets tests push synthetic events through the session mid-run.
func (s *FakeSession) Emit(e orca.Event) {
	if s.closed.Load() {
		return
	}
	if e.AgentID == "" {
		e.AgentID = s.id
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	select {
	case s.events <- e:
	default:
	}
}

// AddUsage bumps the session's token accumulator and emits a TurnCompleted
// event carrying the delta + cumulative.
func (s *FakeSession) AddUsage(delta orca.TokenUsage) {
	s.mu.Lock()
	s.usage = s.usage.Add(delta)
	total := s.usage
	s.mu.Unlock()
	s.Emit(orca.Event{
		Kind: orca.EvtTurnCompleted,
		Payload: map[string]any{
			"delta":      delta,
			"cumulative": total,
		},
	})
}

func (s *FakeSession) run(ctx context.Context) {
	// Emit AgentReady synchronously on start.
	s.Emit(orca.Event{Kind: orca.EvtAgentReady})

	for _, step := range s.script.Steps {
		if step.Delay > 0 {
			select {
			case <-time.After(step.Delay):
			case <-ctx.Done():
				s.finish()
				return
			case <-s.done:
				return
			}
		}
		if step.Usage != nil {
			s.AddUsage(*step.Usage)
		}
		if step.Event != nil {
			s.Emit(*step.Event)
		}
	}

	// After the scripted timeline completes, stay alive until ctx is cancelled
	// or Close() is called. This mirrors real sessions which outlive their
	// last event and only exit on explicit termination.
	select {
	case <-ctx.Done():
	case <-s.done:
	}
	s.finish()
}

func (s *FakeSession) finish() {
	if s.closed.Swap(true) {
		return
	}
	// Emit the exit event BEFORE closing events chan.
	e := orca.Event{
		Kind:      orca.EvtAgentExited,
		AgentID:   s.id,
		Timestamp: time.Now(),
		Payload:   map[string]any{"final_usage": s.Usage()},
	}
	select {
	case s.events <- e:
	default:
	}
	close(s.events)
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}
