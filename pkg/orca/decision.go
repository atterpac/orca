package orca

import "time"

// Severity categorizes how urgent a human decision is. Controls routing,
// rate-limit headroom, and delivery form (batched vs immediate).
type Severity string

const (
	SevLow      Severity = "low"
	SevMedium   Severity = "medium"
	SevHigh     Severity = "high"
	SevCritical Severity = "critical"
)

type DecisionStatus string

const (
	DecPending   DecisionStatus = "pending"
	DecAnswered  DecisionStatus = "answered"
	DecTimedOut  DecisionStatus = "timed_out"
	DecCancelled DecisionStatus = "cancelled"
	DecBlocked   DecisionStatus = "blocked"
)

// AnswerType captures how the human responded.
type AnswerType string

const (
	AnswerOption   AnswerType = "option"
	AnswerFreeform AnswerType = "freeform"
	AnswerCancel   AnswerType = "cancel"
	AnswerTimeout  AnswerType = "timeout"
)

// Decision is a pending or completed human-in-the-loop request. Stored in
// the decisions registry; serialized onto the bus as a KindDecision message
// body so bridges (slack, discord, …) can render it.
type Decision struct {
	ID             string   `json:"id"`
	AgentID        string   `json:"agent_id"`        // who asked
	TaskID         string   `json:"task_id,omitempty"`
	Question       string   `json:"question"`
	Options        []string `json:"options"`
	Context        []string `json:"context,omitempty"`
	Severity       Severity `json:"severity"`
	TimeoutSeconds int      `json:"timeout_seconds"`
	DefaultOption  int      `json:"default_option,omitempty"` // 1-indexed; 0 = none
	Channel        string   `json:"channel,omitempty"`        // bridge-specific hint
	BridgeAgentID  string   `json:"bridge_agent_id"`          // recipient bridge id

	Status     DecisionStatus  `json:"status"`
	CreatedAt  time.Time       `json:"created_at"`
	AnsweredAt *time.Time      `json:"answered_at,omitempty"`
	Answer     *DecisionAnswer `json:"answer,omitempty"`
}

// DecisionAnswer captures the human's response. `ResponderID` +
// `ResponderName` let the asking agent correlate and address the human by
// name on follow-ups.
type DecisionAnswer struct {
	Type          AnswerType `json:"type"`
	Option        int        `json:"option,omitempty"` // 1-indexed
	Text          string     `json:"text,omitempty"`
	Note          string     `json:"note,omitempty"`
	ResponderID   string     `json:"responder_id,omitempty"`
	ResponderName string     `json:"responder_name,omitempty"`
}

// AskHumanRequest is the body of POST /decisions and the structured args to
// the `ask_human` MCP tool.
type AskHumanRequest struct {
	AgentID        string   `json:"agent_id"`           // filled in by shim; ignored from client
	TaskID         string   `json:"task_id,omitempty"`
	Question       string   `json:"question"`
	Options        []string `json:"options"`
	Context        []string `json:"context,omitempty"`
	Severity       Severity `json:"severity,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	DefaultOption  int      `json:"default_option,omitempty"`
	Channel        string   `json:"channel,omitempty"`
	BridgeAgentID  string   `json:"bridge_agent_id,omitempty"` // defaults to "slack"
}

// KindDecision is an additional MessageKind used when orca routes a
// Decision to a bridge agent.
const KindDecision MessageKind = "decision"
