package supervisor

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/atterpac/orca/pkg/orca"
)

// defaultIdleTTL is how long a per-correlation subsession may sit idle
// before we close its underlying process. The sessionID is preserved so a
// later message on the same correlation resumes the conversation via
// `--resume`.
const defaultIdleTTL = 10 * time.Minute

// multiSession implements orca.Session by fanning out to one sub-session
// per correlation_id. Lets a single "agent" run many concurrent claude
// conversations (one per task / slack-thread) without polluting context
// across them. Bridge runtimes (slack, bridge) opt out via
// RuntimeCaps.MultiSession=false and keep the classic single-session path.
type multiSession struct {
	agentID string
	runtime orca.Runtime
	spec    orca.AgentSpec
	idleTTL time.Duration

	mu     sync.Mutex
	subs   map[string]*subSession // key: corrKey(corr)
	closed bool
	events chan orca.Event
	done   chan struct{}

	// usageSumFixed accumulates usage harvested from sub-sessions that have
	// already exited, so Usage() stays monotonic across sub lifecycles.
	usageSumFixed orca.TokenUsage
}

type subSession struct {
	corr       string
	live       orca.Session // nil while dormant / after exit
	sessionID  string       // captured from claude init; used for --resume
	lastActive time.Time
	cancel     context.CancelFunc
}

func newMultiSession(rt orca.Runtime, spec orca.AgentSpec) *multiSession {
	ms := &multiSession{
		agentID: spec.ID,
		runtime: rt,
		spec:    spec,
		idleTTL: defaultIdleTTL,
		subs:    map[string]*subSession{},
		events:  make(chan orca.Event, 256),
		done:    make(chan struct{}),
	}
	go ms.idleSweeper()
	// Announce the agent itself; individual sub sessions suppress their
	// AgentSpawned/Exited since those are per-conversation lifecycle.
	ms.forward(orca.Event{
		Kind:      orca.EvtAgentSpawned,
		AgentID:   spec.ID,
		Timestamp: time.Now(),
		Payload: map[string]any{
			"runtime":       rt.Name(),
			"role":          spec.Role,
			"multi_session": true,
		},
	})
	return ms
}

func corrKey(corr string) string {
	if corr == "" {
		return "_default"
	}
	return corr
}

// orca.Session

func (m *multiSession) ID() string { return m.agentID }

func (m *multiSession) Send(ctx context.Context, msg orca.Message) error {
	live, err := m.ensure(msg.CorrelationID)
	if err != nil {
		return err
	}
	return live.Send(ctx, msg)
}

func (m *multiSession) Events(ctx context.Context) (<-chan orca.Event, error) {
	return m.events, nil
}

func (m *multiSession) Usage() orca.TokenUsage {
	m.mu.Lock()
	defer m.mu.Unlock()
	total := m.usageSumFixed
	for _, sub := range m.subs {
		if sub.live != nil {
			total = total.Add(sub.live.Usage())
		}
	}
	return total
}

func (m *multiSession) Interrupt(ctx context.Context) error {
	m.mu.Lock()
	lives := make([]orca.Session, 0, len(m.subs))
	for _, sub := range m.subs {
		if sub.live != nil {
			lives = append(lives, sub.live)
		}
	}
	m.mu.Unlock()
	var firstErr error
	for _, l := range lives {
		if err := l.Interrupt(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *multiSession) Wait() error {
	<-m.done
	return nil
}

func (m *multiSession) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	lives := make([]orca.Session, 0, len(m.subs))
	for _, sub := range m.subs {
		if sub.live != nil {
			lives = append(lives, sub.live)
			sub.live = nil
		}
	}
	// Close the aggregated events channel while still holding the lock so
	// any concurrent forward() sees m.closed and returns without sending.
	close(m.events)
	m.mu.Unlock()
	for _, l := range lives {
		_ = l.Close()
	}
	close(m.done)
	return nil
}

// internals

// ensure returns a live session for the given correlation, spawning one
// if needed. The returned Session reference is captured under m.mu so
// callers never read sub.live without holding the lock. A concurrent
// sweeper may Close the underlying process while a caller still holds
// the reference; that surfaces as an error from live.Send, not a
// nil-pointer deref.
func (m *multiSession) ensure(corr string) (orca.Session, error) {
	key := corrKey(corr)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("multi-session closed")
	}
	sub, ok := m.subs[key]
	if ok && sub.live != nil {
		sub.lastActive = time.Now()
		live := sub.live
		m.mu.Unlock()
		return live, nil
	}
	if !ok {
		sub = &subSession{corr: corr}
		m.subs[key] = sub
	}
	resume := sub.sessionID
	m.mu.Unlock()

	// Build a per-sub spec copy so we don't mutate the caller's spec when
	// setting resume_session_id / mcp_config.
	spec := m.spec
	if spec.RuntimeOpts != nil {
		cp := make(map[string]any, len(spec.RuntimeOpts))
		for k, v := range spec.RuntimeOpts {
			cp[k] = v
		}
		spec.RuntimeOpts = cp
	} else {
		spec.RuntimeOpts = map[string]any{}
	}
	if resume != "" {
		spec.RuntimeOpts["resume_session_id"] = resume
	}

	ctx, cancel := context.WithCancel(context.Background())
	live, err := m.runtime.Start(ctx, spec)
	if err != nil {
		cancel()
		// Leave the entry in place with no live — another ensure() can retry.
		return nil, err
	}

	m.mu.Lock()
	if m.closed {
		// Close() ran while we were spawning. Drop the just-started session
		// rather than leaking it into m.subs.
		m.mu.Unlock()
		cancel()
		_ = live.Close()
		return nil, errors.New("multi-session closed")
	}
	if sub.live != nil {
		// Another ensure() raced us and installed a live session first.
		// Drop ours, use theirs.
		existing := sub.live
		sub.lastActive = time.Now()
		m.mu.Unlock()
		cancel()
		_ = live.Close()
		return existing, nil
	}
	sub.live = live
	sub.cancel = cancel
	sub.lastActive = time.Now()
	m.mu.Unlock()

	go m.pumpSub(sub, live)
	return live, nil
}

// pumpSub forwards a sub-session's events to the aggregated channel,
// filtering out per-sub lifecycle events so the supervisor sees one
// logical agent. On sub exit, usage is harvested and live is cleared so
// the next ensure() spawns a fresh process (resuming via sessionID when
// present). live is passed by value so this goroutine never races the
// sweeper on sub.live.
func (m *multiSession) pumpSub(sub *subSession, live orca.Session) {
	ch, err := live.Events(context.Background())
	if err != nil {
		return
	}
	for e := range ch {
		switch e.Kind {
		case orca.EvtAgentReady:
			if p, ok := e.Payload.(map[string]any); ok {
				if sid, _ := p["session_id"].(string); sid != "" {
					m.mu.Lock()
					sub.sessionID = sid
					m.mu.Unlock()
				}
			}
			// Forward so supervisor tracks Ready status + captures the
			// most recently activated session_id.
			m.forward(e)
		case orca.EvtAgentSpawned, orca.EvtAgentExited:
			// Suppress — these describe sub-session lifecycle, not the
			// logical agent's.
		default:
			m.forward(e)
		}
	}
	m.mu.Lock()
	// Only harvest + clear if our live is still the one installed — the
	// sweeper or Close may have already cleared it and harvested usage.
	if sub.live == live {
		m.usageSumFixed = m.usageSumFixed.Add(live.Usage())
		sub.live = nil
		if sub.cancel != nil {
			sub.cancel()
			sub.cancel = nil
		}
	}
	m.mu.Unlock()
}

func (m *multiSession) forward(e orca.Event) {
	// Hold the lock across the send so Close() can atomically set closed
	// and close(m.events) without racing a concurrent forward().
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	select {
	case m.events <- e:
	default:
	}
}

func (m *multiSession) idleSweeper() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-t.C:
			m.reapIdle()
		}
	}
}

func (m *multiSession) reapIdle() {
	now := time.Now()
	var toClose []orca.Session
	m.mu.Lock()
	for _, sub := range m.subs {
		if sub.live != nil && now.Sub(sub.lastActive) > m.idleTTL {
			toClose = append(toClose, sub.live)
			m.usageSumFixed = m.usageSumFixed.Add(sub.live.Usage())
			sub.live = nil
			if sub.cancel != nil {
				sub.cancel()
				sub.cancel = nil
			}
		}
	}
	m.mu.Unlock()
	for _, l := range toClose {
		_ = l.Close()
	}
}

// closeSub tears down the live process for a correlation, preserving
// sessionID so a follow-up message can --resume.
func (m *multiSession) closeSub(corr string) {
	key := corrKey(corr)
	m.mu.Lock()
	sub, ok := m.subs[key]
	if !ok {
		m.mu.Unlock()
		return
	}
	live := sub.live
	if live != nil {
		m.usageSumFixed = m.usageSumFixed.Add(live.Usage())
		sub.live = nil
		if sub.cancel != nil {
			sub.cancel()
			sub.cancel = nil
		}
	}
	m.mu.Unlock()
	if live != nil {
		_ = live.Close()
	}
}

// dropSub forgets the sub-session entirely, including sessionID. Used
// when a correlation is known to be done (task closed) so future messages
// on that corr spawn fresh.
func (m *multiSession) dropSub(corr string) {
	m.closeSub(corr)
	key := corrKey(corr)
	m.mu.Lock()
	delete(m.subs, key)
	m.mu.Unlock()
}
