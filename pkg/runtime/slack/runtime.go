// Package slack implements an in-process orca Runtime that brings a
// Slack workspace into the bus as a virtual agent. It replaces the
// standalone orca-slack sidecar binary — same functionality, no IPC.
//
// The runtime owns one Session (the slack agent). The Session connects
// to Slack via Socket Mode (preferred) or HTTP Events API, renders
// outbound orca messages as Block Kit posts, and parses inbound thread
// replies / button clicks back into orca events.
//
// Construction takes a Deps struct — small interfaces that the daemon
// implements over its own bus + decision registry. This keeps the slack
// package decoupled from the supervisor implementation while letting it
// publish messages and finalize decisions directly (no HTTP boundary).
package slack

import (
	"context"
	"errors"
	"sync"

	"github.com/atterpac/orca/pkg/orca"
)

// Deps is the minimal surface the runtime requires from the daemon.
// Implemented by trivial adapters over bus.Bus + *decisions.Registry.
type Deps struct {
	Bus       Publisher
	Decisions DecisionsAPI
}

// Publisher publishes a message onto the orca bus.
type Publisher interface {
	Publish(ctx context.Context, m orca.Message) error
}

// DecisionsAPI lets the runtime finalize / clarify decisions without
// going through HTTP.
type DecisionsAPI interface {
	Answer(ctx context.Context, id string, ans orca.DecisionAnswer) error
	Clarify(ctx context.Context, id, text, responderID, responderName string) error
}

// Runtime is the orca.Runtime implementation. It serves a single
// Session — one slack workspace per runtime instance.
type Runtime struct {
	cfg     Config
	deps    Deps

	mu      sync.Mutex
	session *Session
}

func New(cfg Config, deps Deps) (*Runtime, error) {
	if cfg.BotToken == "" {
		return nil, errors.New("slack: bot token required")
	}
	if deps.Bus == nil {
		return nil, errors.New("slack: bus publisher required")
	}
	return &Runtime{cfg: cfg, deps: deps}, nil
}

func (r *Runtime) Name() string { return "slack" }

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
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.session != nil {
		return nil, errors.New("slack runtime already has a live session — only one slack agent is supported per runtime")
	}
	s, err := newSession(r.cfg, spec, r.deps)
	if err != nil {
		return nil, err
	}
	if err := s.start(ctx); err != nil {
		return nil, err
	}
	r.session = s
	return s, nil
}
