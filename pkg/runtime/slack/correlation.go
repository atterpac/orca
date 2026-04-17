package slack

import "sync"

// correlation maps decision_id (or slack-conversation id) ↔ slack
// thread_ts so thread replies resolve to the right open decision /
// conversation, and agent-initiated follow-ups post in the correct
// thread. Also remembers the option list per decision so thread
// replies can be parsed against a known option count.
type correlation struct {
	mu          sync.RWMutex
	decisionCh  map[string]channelThread // decision_id → {channel, thread_ts}
	threadDec   map[string]string        // "channel|thread_ts" → decision_id
	optionsByID map[string][]string      // decision_id → options list
}

type channelThread struct {
	Channel  string
	ThreadTS string
}

func newCorrelation() *correlation {
	return &correlation{
		decisionCh:  map[string]channelThread{},
		threadDec:   map[string]string{},
		optionsByID: map[string][]string{},
	}
}

func (c *correlation) set(id, channel, threadTS string, options []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.decisionCh[id] = channelThread{channel, threadTS}
	c.threadDec[channel+"|"+threadTS] = id
	c.optionsByID[id] = options
}

func (c *correlation) byDecision(id string) (channelThread, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ct, ok := c.decisionCh[id]
	return ct, ok
}

func (c *correlation) byThread(channel, threadTS string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	id, ok := c.threadDec[channel+"|"+threadTS]
	return id, ok
}

// conversationID generates a stable key for a slack location. For DMs
// (no thread_ts, channel type=im) we use the IM channel id so subsequent
// messages in that DM correlate. For channel messages we anchor on
// (channel, thread_or_message_ts) so agent replies thread under the
// originating message.
func conversationID(channel, threadTS, msgTS string) string {
	anchor := threadTS
	if anchor == "" {
		anchor = msgTS
	}
	return "slack:" + channel + ":" + anchor
}

// isDecisionID reports whether a correlation id refers to a Decision
// (registered via decisions.Ask — 8 hex chars) as opposed to a Slack
// conversation (prefixed with "slack:").
func isDecisionID(id string) bool {
	if id == "" {
		return false
	}
	if len(id) >= len("slack:") && id[:len("slack:")] == "slack:" {
		return false
	}
	return true
}
