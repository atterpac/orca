package orca

import (
	"encoding/json"
	"time"
)

type MessageKind string

const (
	KindRequest       MessageKind = "request"
	KindResponse      MessageKind = "response"
	KindBroadcast     MessageKind = "broadcast"
	KindEvent         MessageKind = "event"
	KindHandoff       MessageKind = "handoff"
	KindClarification MessageKind = "clarification"
	// KindDiscussion: open-ended conversation with a human. No formal
	// decision, no options, no timeout. The human may be gathering info
	// before committing to a task, venting, asking a status question, or
	// just chatting. Agents should answer concisely and be ready for the
	// conversation to become a Request/Handoff later.
	KindDiscussion MessageKind = "discussion"
	// KindUpdate: a structured progress post (phase, status, title,
	// details, metrics). Bridges render these as consistent, styled
	// messages. The body is a JSON-encoded Update.
	KindUpdate MessageKind = "update"
)

// DispatchMode controls tag-routed delivery.
//   - ModeAny: deliver to ONE agent whose tags ⊇ message tags (supervisor picks).
//   - ModeAll: deliver to EVERY matching agent (broadcast).
type DispatchMode string

const (
	ModeAny DispatchMode = "any"
	ModeAll DispatchMode = "all"
)

type Message struct {
	ID            string          `json:"id"`
	From          string          `json:"from"`
	To            string          `json:"to,omitempty"`
	Tags          []string        `json:"tags,omitempty"`
	Mode          DispatchMode    `json:"mode,omitempty"`
	Topic         string          `json:"topic,omitempty"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	Kind          MessageKind     `json:"kind"`
	Body          json.RawMessage `json:"body"`
	TTL           int             `json:"ttl,omitempty"`
	Timestamp     time.Time       `json:"ts"`
}

type EventKind string

const (
	EvtAgentSpawned     EventKind = "AgentSpawned"
	EvtAgentReady       EventKind = "AgentReady"
	EvtAgentExited      EventKind = "AgentExited"
	EvtMessageSent      EventKind = "MessageSent"
	EvtMessageDelivered EventKind = "MessageDelivered"
	EvtToolCallStart    EventKind = "ToolCallStart"
	EvtToolCallEnd      EventKind = "ToolCallEnd"
	EvtTokenChunk       EventKind = "TokenChunk"
	EvtTurnCompleted    EventKind = "TurnCompleted"
	EvtUsageSnapshot    EventKind = "UsageSnapshot"
	EvtPromptSubmitted  EventKind = "PromptSubmitted"
	EvtError            EventKind = "Error"
	EvtChannelPublished EventKind = "ChannelPublished"
	EvtBudgetWarn       EventKind = "BudgetWarn"
	EvtBudgetExceeded   EventKind = "BudgetExceeded"
	EvtTaskOpened       EventKind = "TaskOpened"
	EvtTaskClosed       EventKind = "TaskClosed"
	EvtMessageDropped   EventKind = "MessageDropped"
)

type Event struct {
	V         int       `json:"v"`
	Kind      EventKind `json:"kind"`
	AgentID   string    `json:"agent_id,omitempty"`
	Timestamp time.Time `json:"ts"`
	Payload   any       `json:"payload,omitempty"`
}

type TokenUsage struct {
	InputTokens         uint64    `json:"input_tokens"`
	OutputTokens        uint64    `json:"output_tokens"`
	CacheCreationTokens uint64    `json:"cache_creation_tokens"`
	CacheReadTokens     uint64    `json:"cache_read_tokens"`
	ReasoningTokens     uint64    `json:"reasoning_tokens"`
	Turns               uint64    `json:"turns"`
	CostUSD             float64   `json:"cost_usd"`
	LastUpdated         time.Time `json:"last_updated"`
}

func (u TokenUsage) Add(o TokenUsage) TokenUsage {
	return TokenUsage{
		InputTokens:         u.InputTokens + o.InputTokens,
		OutputTokens:        u.OutputTokens + o.OutputTokens,
		CacheCreationTokens: u.CacheCreationTokens + o.CacheCreationTokens,
		CacheReadTokens:     u.CacheReadTokens + o.CacheReadTokens,
		ReasoningTokens:     u.ReasoningTokens + o.ReasoningTokens,
		Turns:               u.Turns + o.Turns,
		CostUSD:             u.CostUSD + o.CostUSD,
		LastUpdated:         time.Now(),
	}
}

type Budget struct {
	MaxInputTokens  uint64  `yaml:"max_input_tokens" json:"max_input_tokens"`
	MaxOutputTokens uint64  `yaml:"max_output_tokens" json:"max_output_tokens"`
	MaxCostUSD      float64 `yaml:"max_cost_usd" json:"max_cost_usd"`
	OnBreach        string  `yaml:"on_breach" json:"on_breach"`
}

type AgentSpec struct {
	ID               string         `yaml:"id" json:"id"`
	Role             string         `yaml:"role" json:"role"`
	Runtime          string         `yaml:"runtime" json:"runtime"`
	// RoleTemplate names a framework-provided prompt block that's composed
	// into the system prompt alongside the user's persona prompt. Required.
	// Valid values are enumerated in pkg/orca/roletemplates.Known.
	RoleTemplate     string         `yaml:"role_template" json:"role_template"`
	RuntimeOpts      map[string]any `yaml:"runtime_opts,omitempty" json:"runtime_opts,omitempty"`
	SystemPromptFile string         `yaml:"system_prompt_file,omitempty" json:"system_prompt_file,omitempty"`
	SystemPrompt     string         `yaml:"system_prompt,omitempty" json:"system_prompt,omitempty"`
	ContextFiles     []string       `yaml:"context_files,omitempty" json:"context_files,omitempty"`
	Skills           []string       `yaml:"skills,omitempty" json:"skills,omitempty"`
	Tools            []ToolDef      `yaml:"tools,omitempty" json:"tools,omitempty"`
	Tags             []string       `yaml:"tags,omitempty" json:"tags,omitempty"`
	Workdir          string         `yaml:"workdir,omitempty" json:"workdir,omitempty"`
	Restart          string         `yaml:"restart,omitempty" json:"restart,omitempty"`
	Budget           *Budget        `yaml:"budget,omitempty" json:"budget,omitempty"`
	InitialPrompt    string         `yaml:"initial_prompt,omitempty" json:"initial_prompt,omitempty"`
	ParentID         string         `yaml:"parent_id,omitempty" json:"parent_id,omitempty"`
	TaskID           string         `yaml:"task_id,omitempty" json:"task_id,omitempty"`
	Isolation        string         `yaml:"isolation,omitempty" json:"isolation,omitempty"` // "" | "worktree"
	CanSpawn         bool           `yaml:"can_spawn,omitempty" json:"can_spawn,omitempty"`
	// OnParentExit decides child lifecycle when its parent agent is killed or
	// exits. "orphan" (default) keeps the child alive; "kill" cascades the
	// parent's termination to the child.
	OnParentExit string `yaml:"on_parent_exit,omitempty" json:"on_parent_exit,omitempty"`
	ACL          *ACL   `yaml:"acl,omitempty" json:"acl,omitempty"`
}

// ACL controls inter-agent message routing at the supervisor level. Selectors
// can be exact ids ("id:foo"), tag sets ("tag:code" or "tag:code,bug" requiring
// ALL listed tags on the target), or the wildcard "*". An omitted or nil
// field is permissive — any sender/recipient allowed. A non-nil empty list is
// treated the same (permissive) to avoid accidental lockout via malformed yaml.
type ACL struct {
	// SendsTo: who this agent is allowed to address. Checked when this agent
	// initiates a send_message.
	SendsTo []string `yaml:"sends_to,omitempty" json:"sends_to,omitempty"`
	// AcceptsFrom: who this agent is willing to receive messages from.
	// Checked at delivery time for inbound messages.
	AcceptsFrom []string `yaml:"accepts_from,omitempty" json:"accepts_from,omitempty"`
}

type ToolDef struct {
	Name        string         `yaml:"name" json:"name"`
	Description string         `yaml:"description,omitempty" json:"description,omitempty"`
	Schema      map[string]any `yaml:"schema,omitempty" json:"schema,omitempty"`
}

type AgentStatus string

const (
	StatusPending AgentStatus = "pending"
	StatusReady   AgentStatus = "ready"
	StatusBusy    AgentStatus = "busy"
	StatusExited  AgentStatus = "exited"
	StatusFailed  AgentStatus = "failed"
)

type AgentInfo struct {
	Spec         AgentSpec   `json:"spec"`
	Status       AgentStatus `json:"status"`
	Usage        TokenUsage  `json:"usage"`
	StartedAt    time.Time   `json:"started_at"`
	ExitedAt     *time.Time  `json:"exited_at,omitempty"`
	ExitError    string      `json:"exit_error,omitempty"`
	BudgetPaused bool        `json:"budget_paused,omitempty"`
	// SessionID is the runtime's own session identifier (Claude Code's
	// session_id, for example). Surfaced via AgentReady events, persisted so
	// the agent can be resumed later with --resume.
	SessionID string `json:"session_id,omitempty"`
}
