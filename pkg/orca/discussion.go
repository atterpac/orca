package orca

import "time"

// DiscussionStatus tracks the lifecycle of a conversation.
type DiscussionStatus string

const (
	DiscOpen    DiscussionStatus = "open"
	DiscClosed  DiscussionStatus = "closed"
	DiscExpired DiscussionStatus = "expired"
)

// Discussion represents an open-ended conversation initiated by a human
// through a bridge agent (slack, discord, etc.). It's a lightweight,
// daemon-side counterpart to the per-task artifact lifecycle — useful
// for observability, bounded memory, and auto-closing stale threads.
//
// Discussions are keyed by their correlation_id — the same id that
// flows through the message bus for all posts in the conversation.
// The registry creates one on first inbound from a bridge, touches it
// on every subsequent correlated message, and sweeps stale ones after
// an inactivity timeout.
type Discussion struct {
	ID            string           `json:"id"`             // = correlation_id
	BridgeAgentID string           `json:"bridge_agent_id"` // e.g. "slack"
	Channel       string           `json:"channel,omitempty"`
	ThreadTS      string           `json:"thread_ts,omitempty"`
	ResponderID   string           `json:"responder_id,omitempty"`
	ResponderName string           `json:"responder_name,omitempty"`
	Participants  []string         `json:"participants,omitempty"` // orca agent ids involved
	OpenedAt      time.Time        `json:"opened_at"`
	LastActiveAt  time.Time        `json:"last_active_at"`
	MessageCount  int              `json:"message_count"`
	Status        DiscussionStatus `json:"status"`
}
