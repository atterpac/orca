// Package decisions implements the human-in-the-loop decision registry.
// Agents call AskHuman via the ask_human MCP tool; the registry enforces
// rate limits, publishes the decision as a KindDecision message to the
// configured bridge agent, starts a timeout timer, and routes the eventual
// answer back to the asking agent as a correlated response.
package decisions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/atterpac/orca/pkg/orca"
)

// Publisher is the minimal publish surface we need. Supervisor's bus
// satisfies this.
type Publisher interface {
	Publish(ctx context.Context, m orca.Message) error
}

// EventSink surfaces lifecycle events to observers (TUIs, metrics, logs).
type EventSink interface {
	Emit(e orca.Event)
}

// AgentLookup returns whether an agent id exists (used to validate the
// bridge target at decision creation time).
type AgentLookup func(id string) bool

// Limits caps decision flow. Zero disables the corresponding check.
type Limits struct {
	PerAgentPerHour int
	PerTaskPerHour  int
	DefaultTimeout  time.Duration
	// MinOptions / MaxOptions bound the options slice length. 0 disables.
	MinOptions int
	MaxOptions int
}

// Registry manages decision lifecycle.
type Registry struct {
	mu        sync.RWMutex
	decisions map[string]*orca.Decision
	timers    map[string]*time.Timer

	Limits       Limits
	publisher    Publisher
	events       EventSink
	agentExists  AgentLookup
	defaultBridge string
}

func New(pub Publisher, sink EventSink, lookup AgentLookup) *Registry {
	return &Registry{
		decisions:     map[string]*orca.Decision{},
		timers:        map[string]*time.Timer{},
		publisher:     pub,
		events:        sink,
		agentExists:   lookup,
		defaultBridge: "slack",
		Limits: Limits{
			DefaultTimeout: 30 * time.Minute,
			MaxOptions:     5,
		},
	}
}

// SetDefaultBridge configures the default bridge_agent_id used when a request
// doesn't specify one. Defaults to "slack".
func (r *Registry) SetDefaultBridge(id string) {
	r.mu.Lock()
	r.defaultBridge = id
	r.mu.Unlock()
}

var (
	ErrQuestionEmpty     = errors.New("question required")
	ErrAgentMissing      = errors.New("agent_id required")
	ErrBridgeMissing     = errors.New("bridge agent not found")
	ErrRateLimitedAgent  = errors.New("rate_limited: per_agent_per_hour exceeded")
	ErrRateLimitedTask   = errors.New("rate_limited: per_task_per_hour exceeded")
	ErrTooManyOptions    = errors.New("too many options")
	ErrDefaultOutOfRange = errors.New("default_option out of range")
	ErrDecisionNotFound  = errors.New("decision not found")
	ErrAlreadyAnswered   = errors.New("decision already answered")
	ErrDecisionNotOpen   = errors.New("decision is not open for clarification")
)

// Ask creates a new decision and publishes it to the bridge agent.
func (r *Registry) Ask(ctx context.Context, req orca.AskHumanRequest) (*orca.Decision, error) {
	if req.Question == "" {
		return nil, ErrQuestionEmpty
	}
	if req.AgentID == "" {
		return nil, ErrAgentMissing
	}
	if r.Limits.MaxOptions > 0 && len(req.Options) > r.Limits.MaxOptions {
		return nil, fmt.Errorf("%w (max %d)", ErrTooManyOptions, r.Limits.MaxOptions)
	}
	if req.DefaultOption != 0 && (req.DefaultOption < 1 || req.DefaultOption > len(req.Options)) {
		return nil, ErrDefaultOutOfRange
	}
	bridge := req.BridgeAgentID
	if bridge == "" {
		r.mu.RLock()
		bridge = r.defaultBridge
		r.mu.RUnlock()
	}
	if r.agentExists != nil && !r.agentExists(bridge) {
		return nil, fmt.Errorf("%w: %s", ErrBridgeMissing, bridge)
	}
	if err := r.rateCheck(req.AgentID, req.TaskID); err != nil {
		r.events.Emit(orca.Event{Kind: orca.EvtMessageDropped, AgentID: req.AgentID, Payload: map[string]any{
			"scope":  "decision",
			"reason": err.Error(),
		}})
		return nil, err
	}

	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = r.Limits.DefaultTimeout
	}
	if req.Severity == "" {
		req.Severity = orca.SevMedium
	}

	d := &orca.Decision{
		ID:             newID(),
		AgentID:        req.AgentID,
		TaskID:         req.TaskID,
		Question:       req.Question,
		Options:        slices.Clone(req.Options),
		Context:        slices.Clone(req.Context),
		Severity:       req.Severity,
		TimeoutSeconds: int(timeout.Seconds()),
		DefaultOption:  req.DefaultOption,
		Channel:        req.Channel,
		BridgeAgentID:  bridge,
		Status:         orca.DecPending,
		CreatedAt:      time.Now(),
	}

	r.mu.Lock()
	r.decisions[d.ID] = d
	r.timers[d.ID] = time.AfterFunc(timeout, func() { r.onTimeout(d.ID) })
	r.mu.Unlock()

	// Publish to bridge agent as KindDecision — body is the full Decision JSON.
	body, _ := json.Marshal(d)
	msg := orca.Message{
		From:          req.AgentID,
		To:            bridge,
		Kind:          orca.KindDecision,
		Body:          body,
		CorrelationID: d.ID,
		Timestamp:     time.Now(),
	}
	if err := r.publisher.Publish(ctx, msg); err != nil {
		// Roll back: remove the registered decision on publish failure.
		r.mu.Lock()
		delete(r.decisions, d.ID)
		if t, ok := r.timers[d.ID]; ok {
			t.Stop()
			delete(r.timers, d.ID)
		}
		r.mu.Unlock()
		return nil, fmt.Errorf("publish to bridge: %w", err)
	}

	r.events.Emit(orca.Event{Kind: "DecisionRequested", AgentID: req.AgentID, Payload: map[string]any{
		"decision_id":    d.ID,
		"severity":       d.Severity,
		"bridge_agent_id": bridge,
		"task_id":        d.TaskID,
		"question":       d.Question,
	}})
	return d, nil
}

// Answer records a human response and routes it back to the asking agent.
func (r *Registry) Answer(ctx context.Context, id string, ans orca.DecisionAnswer) error {
	r.mu.Lock()
	d, ok := r.decisions[id]
	if !ok {
		r.mu.Unlock()
		return ErrDecisionNotFound
	}
	if d.Status != orca.DecPending {
		r.mu.Unlock()
		return ErrAlreadyAnswered
	}
	d.Status = orca.DecAnswered
	if ans.Type == orca.AnswerCancel {
		d.Status = orca.DecCancelled
	}
	if ans.Type == orca.AnswerTimeout {
		d.Status = orca.DecTimedOut
	}
	now := time.Now()
	d.AnsweredAt = &now
	d.Answer = &ans
	if t, ok := r.timers[id]; ok {
		t.Stop()
		delete(r.timers, id)
	}
	agentID := d.AgentID
	bridge := d.BridgeAgentID
	r.mu.Unlock()

	body, _ := json.Marshal(map[string]any{
		"decision_id":     id,
		"answer":          ans,
	})
	reply := orca.Message{
		From:          bridge,
		To:            agentID,
		Kind:          orca.KindResponse,
		Body:          body,
		CorrelationID: id,
		Timestamp:     time.Now(),
	}
	if err := r.publisher.Publish(ctx, reply); err != nil {
		return fmt.Errorf("publish answer to agent: %w", err)
	}

	r.events.Emit(orca.Event{Kind: "DecisionAnswered", AgentID: agentID, Payload: map[string]any{
		"decision_id":   id,
		"answer_type":   ans.Type,
		"option":        ans.Option,
		"responder_id":  ans.ResponderID,
		"responder_name": ans.ResponderName,
	}})
	return nil
}

// Cancel marks a decision cancelled (e.g. the agent died before a reply).
func (r *Registry) Cancel(id string) error {
	return r.Answer(context.Background(), id, orca.DecisionAnswer{Type: orca.AnswerCancel})
}

// Clarify forwards a human's free-form reply to the asking agent without
// finalizing the decision. Used for multi-turn back-and-forth — the human
// can ask questions, the agent replies via send_message(to=bridge,
// correlation_id=<decision_id>), and the loop continues until the human
// finalizes via an option click, typed number, typed CANCEL, or timeout.
func (r *Registry) Clarify(ctx context.Context, id string, text, responderID, responderName string) error {
	r.mu.RLock()
	d, ok := r.decisions[id]
	if !ok {
		r.mu.RUnlock()
		return ErrDecisionNotFound
	}
	if d.Status != orca.DecPending {
		r.mu.RUnlock()
		return ErrDecisionNotOpen
	}
	agentID := d.AgentID
	bridge := d.BridgeAgentID
	r.mu.RUnlock()

	// Deliver as KindClarification so the agent can distinguish it from a
	// finalizing response. Body carries both the text and responder info.
	body, _ := json.Marshal(map[string]any{
		"decision_id":    id,
		"text":           text,
		"responder_id":   responderID,
		"responder_name": responderName,
	})
	msg := orca.Message{
		From:          bridge,
		To:            agentID,
		Kind:          orca.KindClarification,
		Body:          body,
		CorrelationID: id,
		Timestamp:     time.Now(),
	}
	if err := r.publisher.Publish(ctx, msg); err != nil {
		return fmt.Errorf("publish clarification: %w", err)
	}

	r.events.Emit(orca.Event{Kind: "DecisionClarification", AgentID: agentID, Payload: map[string]any{
		"decision_id":    id,
		"responder_id":   responderID,
		"responder_name": responderName,
		"text_preview":   previewN(text, 120),
	}})
	return nil
}

func previewN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Get returns the decision with the given id.
func (r *Registry) Get(id string) (*orca.Decision, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.decisions[id]
	if !ok {
		return nil, false
	}
	cp := *d
	return &cp, true
}

// Pending returns a snapshot of currently-pending decisions.
func (r *Registry) Pending() []orca.Decision {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]orca.Decision, 0)
	for _, d := range r.decisions {
		if d.Status == orca.DecPending {
			out = append(out, *d)
		}
	}
	return out
}

func (r *Registry) onTimeout(id string) {
	r.mu.Lock()
	d, ok := r.decisions[id]
	if !ok || d.Status != orca.DecPending {
		r.mu.Unlock()
		return
	}
	defaultOpt := d.DefaultOption
	bridge := d.BridgeAgentID
	r.mu.Unlock()

	ans := orca.DecisionAnswer{Type: orca.AnswerTimeout}
	if defaultOpt > 0 {
		ans.Type = orca.AnswerOption
		ans.Option = defaultOpt
		ans.Note = "timeout; applied default_option"
	}
	_ = r.Answer(context.Background(), id, ans)

	// Also notify the bridge in-thread so the slack channel sees the
	// decision was auto-closed. Bridge's plain-message handler looks at
	// correlation_id and posts into the matching thread.
	notice := "⏰ decision `" + id + "` timed out."
	if defaultOpt > 0 {
		notice = "⏰ decision `" + id + "` timed out — applied default option " + itoa(defaultOpt) + "."
	}
	body, _ := json.Marshal(notice)
	if err := r.publisher.Publish(context.Background(), orca.Message{
		From:          "orca",
		To:            bridge,
		Kind:          orca.KindEvent,
		Body:          body,
		CorrelationID: id,
		Timestamp:     time.Now(),
	}); err != nil {
		r.events.Emit(orca.Event{Kind: orca.EvtError, Payload: map[string]any{
			"scope":       "decisions",
			"msg":         "failed to publish timeout notice to bridge",
			"decision_id": id,
			"bridge":      bridge,
			"err":         err.Error(),
		}})
	}

	r.events.Emit(orca.Event{Kind: "DecisionTimedOut", Payload: map[string]any{
		"decision_id":     id,
		"applied_default": defaultOpt,
	}})
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	sign := ""
	if i < 0 {
		sign = "-"
		i = -i
	}
	var buf [20]byte
	p := len(buf)
	for i > 0 {
		p--
		buf[p] = byte('0' + i%10)
		i /= 10
	}
	return sign + string(buf[p:])
}

func (r *Registry) rateCheck(agentID, taskID string) error {
	if r.Limits.PerAgentPerHour == 0 && r.Limits.PerTaskPerHour == 0 {
		return nil
	}
	cutoff := time.Now().Add(-time.Hour)
	agentCount := 0
	taskCount := 0
	r.mu.RLock()
	for _, d := range r.decisions {
		if d.CreatedAt.Before(cutoff) {
			continue
		}
		if d.AgentID == agentID {
			agentCount++
		}
		if taskID != "" && d.TaskID == taskID {
			taskCount++
		}
	}
	r.mu.RUnlock()

	if r.Limits.PerAgentPerHour > 0 && agentCount >= r.Limits.PerAgentPerHour {
		return ErrRateLimitedAgent
	}
	if r.Limits.PerTaskPerHour > 0 && taskCount >= r.Limits.PerTaskPerHour {
		return ErrRateLimitedTask
	}
	return nil
}

func newID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
