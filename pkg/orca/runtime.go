package orca

import "context"

type RuntimeCaps struct {
	Streaming   bool   `json:"streaming"`
	NativeTools bool   `json:"native_tools"`
	FileAccess  bool   `json:"file_access"`
	MCP         bool   `json:"mcp"`
	Resume      bool   `json:"resume"`
	SkillFormat string `json:"skill_format"`
	// MultiSession declares the runtime wants one live session per
	// correlation (task/thread) rather than a single long-running process
	// per agent. Supervisor wraps such runtimes so Send routes to the
	// right sub-session, spawning (or --resume'ing) on demand and idle-
	// reaping. Bridge runtimes (slack, bridge) keep MultiSession=false.
	MultiSession bool `json:"multi_session"`
}

type Runtime interface {
	Name() string
	Capabilities() RuntimeCaps
	Start(ctx context.Context, spec AgentSpec) (Session, error)
}

type Session interface {
	ID() string
	Send(ctx context.Context, m Message) error
	Events(ctx context.Context) (<-chan Event, error)
	Usage() TokenUsage
	Interrupt(ctx context.Context) error
	Wait() error
	Close() error
}
