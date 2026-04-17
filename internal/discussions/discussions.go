// Package discussions implements the conversation registry. Discussions
// are first-class, observable, and auto-closed on inactivity — unlike
// the sidecar's implicit correlation table, which they replace at the
// daemon level for bridge-originated traffic.
//
// Lifecycle:
//   - Touch(id, ...) opens a discussion on first call and updates
//     LastActiveAt + MessageCount on subsequent calls.
//   - A background sweeper moves discussions whose LastActiveAt is
//     older than Limits.InactivityTimeout to status=expired.
//   - Close(id) is the explicit close path.
//
// The registry emits DiscussionOpened / DiscussionMessage /
// DiscussionClosed events for every transition.
package discussions

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/atterpac/orca/pkg/orca"
)

// EventSink is the minimal surface for emitting events.
type EventSink interface {
	Emit(e orca.Event)
}

// Limits bounds the registry. Zero values are ignored.
type Limits struct {
	// InactivityTimeout is how long a discussion may remain open without
	// any message before it's auto-closed with status=expired. Default
	// 30 minutes. Set <=0 to disable the sweeper.
	InactivityTimeout time.Duration
	// SweepInterval is how often the background sweeper runs.
	// Default 1 minute.
	SweepInterval time.Duration
}

type Registry struct {
	mu    sync.RWMutex
	byID  map[string]*orca.Discussion
	events EventSink

	Limits Limits

	sweepCh chan struct{}
	wg      sync.WaitGroup
}

// New constructs a Registry and starts the background sweeper.
func New(events EventSink) *Registry {
	r := &Registry{
		byID:   map[string]*orca.Discussion{},
		events: events,
		Limits: Limits{
			InactivityTimeout: 30 * time.Minute,
			SweepInterval:     1 * time.Minute,
		},
		sweepCh: make(chan struct{}),
	}
	r.wg.Add(1)
	go r.sweepLoop()
	return r
}

// TouchInfo is the subset of message context the registry needs when
// recording activity. Fields beyond ID are used only when opening a
// fresh discussion.
type TouchInfo struct {
	ID            string
	BridgeAgentID string
	Channel       string
	ThreadTS      string
	ResponderID   string
	ResponderName string
	Participant   string // orca agent id that was party to this message
}

// Touch opens or updates a discussion. The first call with a given
// TouchInfo.ID opens it; subsequent calls update LastActiveAt and
// MessageCount and add Participant (if new) to the participant list.
func (r *Registry) Touch(ti TouchInfo) *orca.Discussion {
	if ti.ID == "" {
		return nil
	}
	now := time.Now()

	r.mu.Lock()
	d, existed := r.byID[ti.ID]
	if !existed {
		d = &orca.Discussion{
			ID:            ti.ID,
			BridgeAgentID: ti.BridgeAgentID,
			Channel:       ti.Channel,
			ThreadTS:      ti.ThreadTS,
			ResponderID:   ti.ResponderID,
			ResponderName: ti.ResponderName,
			OpenedAt:      now,
			Status:        orca.DiscOpen,
		}
		r.byID[ti.ID] = d
	}
	d.LastActiveAt = now
	d.MessageCount++
	if ti.Participant != "" && !slices.Contains(d.Participants, ti.Participant) {
		d.Participants = append(d.Participants, ti.Participant)
	}
	snapshot := *d
	r.mu.Unlock()

	if !existed {
		r.events.Emit(orca.Event{Kind: "DiscussionOpened", Payload: map[string]any{
			"id":             snapshot.ID,
			"bridge_agent_id": snapshot.BridgeAgentID,
			"responder_name": snapshot.ResponderName,
		}})
	}
	r.events.Emit(orca.Event{Kind: "DiscussionMessage", Payload: map[string]any{
		"id":    snapshot.ID,
		"count": snapshot.MessageCount,
	}})
	return &snapshot
}

// Close explicitly closes a discussion. Noop if already closed/expired.
func (r *Registry) Close(id string) error {
	r.mu.Lock()
	d, ok := r.byID[id]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("discussion %s not found", id)
	}
	if d.Status != orca.DiscOpen {
		r.mu.Unlock()
		return nil
	}
	d.Status = orca.DiscClosed
	snapshot := *d
	r.mu.Unlock()

	r.events.Emit(orca.Event{Kind: "DiscussionClosed", Payload: map[string]any{
		"id":     snapshot.ID,
		"reason": "explicit",
	}})
	return nil
}

// Get returns a snapshot of the discussion, or (nil, false) if absent.
func (r *Registry) Get(id string) (*orca.Discussion, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.byID[id]
	if !ok {
		return nil, false
	}
	cp := *d
	cp.Participants = slices.Clone(d.Participants)
	return &cp, true
}

// List returns all discussions, newest first.
func (r *Registry) List() []orca.Discussion {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]orca.Discussion, 0, len(r.byID))
	for _, d := range r.byID {
		cp := *d
		cp.Participants = slices.Clone(d.Participants)
		out = append(out, cp)
	}
	slices.SortFunc(out, func(a, b orca.Discussion) int {
		switch {
		case a.LastActiveAt.After(b.LastActiveAt):
			return -1
		case a.LastActiveAt.Before(b.LastActiveAt):
			return 1
		default:
			return 0
		}
	})
	return out
}

// Stop halts the background sweeper. After Stop the registry is still
// readable but Touch/Close may race with shutdown; call from main exit.
func (r *Registry) Stop() {
	select {
	case <-r.sweepCh:
		// already stopped
	default:
		close(r.sweepCh)
	}
	r.wg.Wait()
}

func (r *Registry) sweepLoop() {
	defer r.wg.Done()
	interval := r.Limits.SweepInterval
	if interval <= 0 {
		interval = 1 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.sweepCh:
			return
		case <-ticker.C:
			r.sweepOnce()
		}
	}
}

func (r *Registry) sweepOnce() {
	timeout := r.Limits.InactivityTimeout
	if timeout <= 0 {
		return
	}
	cutoff := time.Now().Add(-timeout)

	r.mu.Lock()
	expired := []string{}
	for id, d := range r.byID {
		if d.Status == orca.DiscOpen && d.LastActiveAt.Before(cutoff) {
			d.Status = orca.DiscExpired
			expired = append(expired, id)
		}
	}
	r.mu.Unlock()

	for _, id := range expired {
		r.events.Emit(orca.Event{Kind: "DiscussionClosed", Payload: map[string]any{
			"id":     id,
			"reason": "inactivity",
		}})
	}
}

// NewForTest creates a registry with no background sweeper — unit tests
// invoke sweepOnce() manually to exercise expiry.
func NewForTest(events EventSink) *Registry {
	r := &Registry{
		byID:    map[string]*orca.Discussion{},
		events:  events,
		Limits:  Limits{InactivityTimeout: 30 * time.Minute},
		sweepCh: make(chan struct{}),
	}
	// No goroutine. Tests call sweepOnce directly.
	return r
}

// SweepForTest is the test hook for triggering expiry deterministically.
func (r *Registry) SweepForTest(ctx context.Context) {
	r.sweepOnce()
}
