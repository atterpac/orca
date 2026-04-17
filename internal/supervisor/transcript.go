package supervisor

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/atterpac/orca/internal/events"
	"github.com/atterpac/orca/pkg/orca"
)

// defaultTranscriptCap bounds per-agent event retention. 500 covers most
// debugging needs without unbounded growth in long-lived sessions.
const defaultTranscriptCap = 500

// transcript is a per-agent ring buffer of recent Events. Older events
// are evicted when the buffer is full. TokenChunk events are skipped at
// ingest because they're high-volume and rarely useful for debugging.
type transcript struct {
	mu    sync.Mutex
	items []orca.Event
	cap   int
}

func newTranscript(cap int) *transcript {
	return &transcript{cap: cap}
}

func (t *transcript) add(e orca.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.items) >= t.cap {
		// Evict oldest. Compact via copy to keep underlying capacity sane.
		copy(t.items, t.items[1:])
		t.items = t.items[:len(t.items)-1]
	}
	t.items = append(t.items, e)
}

func (t *transcript) snapshot(limit int, kinds []orca.EventKind) []orca.Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]orca.Event, 0, len(t.items))
	for _, e := range t.items {
		if len(kinds) > 0 && !slices.Contains(kinds, e.Kind) {
			continue
		}
		out = append(out, e)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// startTranscriptCollector subscribes to the event bus and streams events
// into per-agent ring buffers. Runs for the lifetime of the supervisor.
// TokenChunk events are dropped (too noisy); everything else with a
// non-empty AgentID is recorded.
func (s *Supervisor) startTranscriptCollector(ev *events.Bus) {
	ctx := context.Background()
	ch, _ := ev.Subscribe(ctx, events.Filter{})
	go func() {
		for e := range ch {
			if e.Kind == orca.EvtTokenChunk {
				continue
			}
			id := e.AgentID
			if id == "" {
				continue
			}
			t := s.getOrCreateTranscript(id)
			t.add(e)
		}
	}()
}

func (s *Supervisor) getOrCreateTranscript(agentID string) *transcript {
	if v, ok := s.transcripts.Load(agentID); ok {
		return v.(*transcript)
	}
	t := newTranscript(defaultTranscriptCap)
	actual, _ := s.transcripts.LoadOrStore(agentID, t)
	return actual.(*transcript)
}

// Transcript returns the most recent events recorded for agentID.
// limit=0 returns everything; kinds=nil returns all kinds. Events are
// ordered oldest → newest. Returns an empty slice when the agent has
// produced no recorded events.
func (s *Supervisor) Transcript(agentID string, limit int, kinds []orca.EventKind) []orca.Event {
	if v, ok := s.transcripts.Load(agentID); ok {
		return v.(*transcript).snapshot(limit, kinds)
	}
	return nil
}

// TaskTimeline reconstructs the chronology of events touching a task.
// Scans every agent's transcript (transcripts are bounded so this is
// cheap) and includes events that:
//   - reference the task_id via correlation_id or task_id payload field
//   - are a lifecycle/tool event from a declared task participant
//     during the task's open window
//
// We deliberately cast a wide net since the goal is debugging
// visibility, not strict filtering. limit=0 returns everything;
// otherwise returns the most recent N.
func (s *Supervisor) TaskTimeline(taskID string, limit int) []orca.Event {
	s.mu.RLock()
	t, ok := s.tasks[taskID]
	if !ok {
		s.mu.RUnlock()
		return nil
	}
	participantSet := map[string]struct{}{}
	for _, p := range t.Agents {
		participantSet[p] = struct{}{}
	}
	openedAt := t.OpenedAt
	closedAt := t.ClosedAt
	s.mu.RUnlock()

	var all []orca.Event
	// Iterate ALL agents' transcripts — pre-spawned fleet agents that
	// joined a task via correlation_id (rather than spec.TaskID) won't
	// appear in t.Agents but their events still touch the task.
	s.transcripts.Range(func(key, value any) bool {
		agentID, _ := key.(string)
		t, _ := value.(*transcript)
		if t == nil {
			return true
		}
		_, isParticipant := participantSet[agentID]
		evs := t.snapshot(0, nil)
		for _, e := range evs {
			if e.Timestamp.Before(openedAt) {
				continue
			}
			if closedAt != nil && e.Timestamp.After(closedAt.Add(5*time.Second)) {
				// Allow a small tail-window after close for any straggler
				// events that arrive just after CloseTask returns.
				continue
			}
			if eventTouchesTask(e, taskID, openedAt) ||
				(isParticipant && participantLifecycleEvent(e)) {
				all = append(all, e)
			}
		}
		return true
	})

	slices.SortFunc(all, func(a, b orca.Event) int {
		return a.Timestamp.Compare(b.Timestamp)
	})
	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all
}

// participantLifecycleEvent reports whether an event is the kind of
// lifecycle / tool event we want to include for a known participant
// even when no correlation_id ties it to a specific task.
func participantLifecycleEvent(e orca.Event) bool {
	switch e.Kind {
	case orca.EvtAgentSpawned, orca.EvtAgentReady, orca.EvtAgentExited,
		orca.EvtToolCallStart, orca.EvtToolCallEnd:
		return true
	}
	return false
}

// eventTouchesTask returns true when an event explicitly references the
// task — task-lifecycle event for THIS task, or a payload that names
// the task via correlation_id or task_id.
//
// Participant lifecycle events (AgentReady, ToolCallStart, etc.) are
// handled separately in TaskTimeline via participantLifecycleEvent so
// we only include them for declared participants and not for every
// agent on the daemon.
func eventTouchesTask(e orca.Event, taskID string, _ time.Time) bool {
	switch e.Kind {
	case orca.EvtTaskOpened, orca.EvtTaskClosed:
		if p, ok := e.Payload.(map[string]any); ok {
			if id, _ := p["task_id"].(string); id == taskID {
				return true
			}
		}
		return false
	}
	if p, ok := e.Payload.(map[string]any); ok {
		if c, _ := p["correlation_id"].(string); c == taskID {
			return true
		}
		if t, _ := p["task_id"].(string); t == taskID {
			return true
		}
	}
	return false
}
