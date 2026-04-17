// Package bridge implements a Runtime that does not host an LLM. Its
// Sessions are pure channel pairs: messages the supervisor delivers to the
// session are forwarded to an external consumer (a "sidecar" — slack,
// discord, webhook, email, ...), and messages the sidecar pushes in become
// orca bus messages.
//
// The consumer attaches to a Session via (*Runtime).Attach. The registry
// exposes that as a WebSocket endpoint so out-of-process sidecars can plug
// in without speaking orca's internals.
//
// A Bridge agent:
//   - is listed, tag-routed, and ACL-checked like any agent
//   - always reports zero TokenUsage (no LLM)
//   - stays alive until Close() is called (by Kill from supervisor or a
//     sidecar disconnect policy)
//
// The supervisor treats it identically to a Claude Code agent; no core
// changes required.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/atterpac/orca/pkg/orca"
)

type Runtime struct {
	mu        sync.Mutex
	sessions  map[string]*Session
	publisher Publisher
}

// New constructs a Bridge runtime. The publisher is wired onto every Session
// created by Start so Deliver() can route into orca without per-session setup.
func New(publisher Publisher) *Runtime {
	return &Runtime{sessions: map[string]*Session{}, publisher: publisher}
}

func (r *Runtime) Name() string { return "bridge" }

func (r *Runtime) Capabilities() orca.RuntimeCaps {
	return orca.RuntimeCaps{
		Streaming:   false,
		NativeTools: false,
		FileAccess:  false,
		MCP:         false,
		Resume:      false,
		SkillFormat: "none",
	}
}

func (r *Runtime) Start(ctx context.Context, spec orca.AgentSpec) (orca.Session, error) {
	s := &Session{
		id:        spec.ID,
		outbox:    make(chan orca.Message, 128),
		events:    make(chan orca.Event, 32),
		done:      make(chan struct{}),
		createdAt: time.Now(),
	}
	if r.publisher != nil {
		s.SetPublisher(r.publisher)
	}
	r.mu.Lock()
	if _, exists := r.sessions[spec.ID]; exists {
		r.mu.Unlock()
		return nil, fmt.Errorf("bridge: session %s already exists", spec.ID)
	}
	r.sessions[spec.ID] = s
	r.mu.Unlock()

	s.Emit(orca.Event{Kind: orca.EvtAgentReady, AgentID: spec.ID, Payload: map[string]any{
		"runtime": "bridge",
	}})
	return s, nil
}

// Attach returns a read-only handle on the session identified by agentID so
// an external sidecar (registered via the WS endpoint) can drive it.
// Returns an error if no session exists. The sidecar is expected to:
//
//   - Read from Outbox (messages delivered by the supervisor to this agent)
//   - Call Deliver(msg) to publish a message on behalf of this agent
//   - Call EmitEvent() to surface observability events (e.g. a slack
//     connection status change) through the regular event bus
//   - Call Close() when the sidecar disconnects; the session will be kept
//     alive unless the caller opts into auto-termination.
func (r *Runtime) Attach(agentID string) (*Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[agentID]
	if !ok {
		return nil, fmt.Errorf("bridge: no session for %s", agentID)
	}
	return s, nil
}

// Session implements orca.Session. It exposes extra methods for the sidecar
// that go beyond the standard Runtime contract.
type Session struct {
	id     string
	outbox chan orca.Message // supervisor → bridge
	events chan orca.Event   // bridge → event bus (via supervisor pump)
	done   chan struct{}

	// sidecarIn is populated once a sidecar attaches. Messages it pushes get
	// published by a caller-supplied publisher (set via SetPublisher).
	publisher atomic.Pointer[Publisher]

	closed    atomic.Bool
	createdAt time.Time
}

// Publisher is the minimal dependency the bridge needs to route messages
// pushed by a sidecar onto the orca bus. Supervisor wires this on startup.
type Publisher interface {
	Publish(ctx context.Context, m orca.Message) error
}

// SetPublisher sets the bus-side publisher used by Deliver. Called once at
// wiring time (registry/daemon setup).
func (s *Session) SetPublisher(p Publisher) {
	if p == nil {
		return
	}
	s.publisher.Store(&p)
}

func (s *Session) ID() string             { return s.id }
func (s *Session) Usage() orca.TokenUsage { return orca.TokenUsage{} }

// Send is called by the supervisor when a message is routed to this agent.
// The sidecar consumes Outbox() to receive it.
func (s *Session) Send(ctx context.Context, m orca.Message) error {
	if s.closed.Load() {
		return errors.New("bridge session closed")
	}
	select {
	case s.outbox <- m:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		// Drop rather than block the supervisor's delivery goroutine; sidecar
		// is expected to drain Outbox promptly.
		return errors.New("bridge outbox full — sidecar falling behind")
	}
}

// Events is consumed by the supervisor pump (same contract as all sessions).
func (s *Session) Events(ctx context.Context) (<-chan orca.Event, error) {
	return s.events, nil
}

func (s *Session) Interrupt(ctx context.Context) error { return s.Close() }
func (s *Session) Wait() error                         { <-s.done; return nil }

func (s *Session) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	// Emit exit event directly rather than through Emit(), which short-
	// circuits once closed=true. We hold the atomic-critical section here
	// and the events channel is not yet closed.
	exit := orca.Event{
		Kind:      orca.EvtAgentExited,
		AgentID:   s.id,
		Timestamp: time.Now(),
		Payload:   map[string]any{"reason": "bridge closed"},
	}
	select {
	case s.events <- exit:
	default:
	}
	close(s.outbox)
	close(s.events)
	close(s.done)
	return nil
}

// Outbox returns the channel of messages flowing from the orca supervisor to
// this bridge agent. The sidecar consumes this.
func (s *Session) Outbox() <-chan orca.Message { return s.outbox }

// Deliver publishes a message on the orca bus as if it originated from this
// bridge agent. The sidecar calls this to forward inbound external messages
// (e.g. a slack user saying something) into orca.
func (s *Session) Deliver(ctx context.Context, m orca.Message) error {
	if s.closed.Load() {
		return errors.New("bridge session closed")
	}
	p := s.publisher.Load()
	if p == nil || *p == nil {
		return errors.New("bridge: no publisher configured")
	}
	if m.From == "" {
		m.From = s.id
	}
	if m.Timestamp.IsZero() {
		m.Timestamp = time.Now()
	}
	return (*p).Publish(ctx, m)
}

// Emit pushes an event onto the bridge's event stream. Guarded against
// races with Close().
func (s *Session) Emit(e orca.Event) {
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
