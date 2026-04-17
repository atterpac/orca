package decisions

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/atterpac/orca/pkg/orca"
)

type capturePub struct {
	mu sync.Mutex
	ms []orca.Message
}

func (p *capturePub) Publish(ctx context.Context, m orca.Message) error {
	p.mu.Lock()
	p.ms = append(p.ms, m)
	p.mu.Unlock()
	return nil
}

func (p *capturePub) snapshot() []orca.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make([]orca.Message, len(p.ms))
	copy(cp, p.ms)
	return cp
}

type captureEvents struct {
	mu sync.Mutex
	es []orca.Event
}

func (e *captureEvents) Emit(ev orca.Event) {
	e.mu.Lock()
	e.es = append(e.es, ev)
	e.mu.Unlock()
}

func newRegistry(t *testing.T) (*Registry, *capturePub, *captureEvents) {
	t.Helper()
	pub := &capturePub{}
	ev := &captureEvents{}
	r := New(pub, ev, func(id string) bool { return id == "slack" })
	return r, pub, ev
}

func TestAsk_PublishesToBridge(t *testing.T) {
	r, pub, _ := newRegistry(t)
	d, err := r.Ask(context.Background(), orca.AskHumanRequest{
		AgentID:  "architect",
		Question: "ship it?",
		Options:  []string{"yes", "no"},
		Severity: orca.SevHigh,
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != orca.DecPending {
		t.Fatalf("status=%s", d.Status)
	}
	msgs := pub.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("want 1 publish, got %d", len(msgs))
	}
	if msgs[0].To != "slack" || msgs[0].Kind != orca.KindDecision {
		t.Fatalf("wrong routing: %+v", msgs[0])
	}
	if msgs[0].CorrelationID != d.ID {
		t.Fatalf("correlation id mismatch: %s vs %s", msgs[0].CorrelationID, d.ID)
	}
}

func TestAsk_RejectsUnknownBridge(t *testing.T) {
	r, _, _ := newRegistry(t)
	_, err := r.Ask(context.Background(), orca.AskHumanRequest{
		AgentID:       "arch",
		Question:      "q",
		BridgeAgentID: "discord", // not registered
	})
	if err == nil {
		t.Fatal("expected bridge-not-found error")
	}
}

func TestAsk_RejectsEmptyQuestion(t *testing.T) {
	r, _, _ := newRegistry(t)
	_, err := r.Ask(context.Background(), orca.AskHumanRequest{
		AgentID: "arch",
	})
	if err != ErrQuestionEmpty {
		t.Fatalf("want ErrQuestionEmpty, got %v", err)
	}
}

func TestAsk_RejectsTooManyOptions(t *testing.T) {
	r, _, _ := newRegistry(t)
	r.Limits.MaxOptions = 3
	_, err := r.Ask(context.Background(), orca.AskHumanRequest{
		AgentID:  "arch",
		Question: "q",
		Options:  []string{"a", "b", "c", "d"},
	})
	if err == nil {
		t.Fatal("expected too-many-options error")
	}
}

func TestAsk_RejectsBadDefaultOption(t *testing.T) {
	r, _, _ := newRegistry(t)
	_, err := r.Ask(context.Background(), orca.AskHumanRequest{
		AgentID:       "arch",
		Question:      "q",
		Options:       []string{"a", "b"},
		DefaultOption: 5,
	})
	if err != ErrDefaultOutOfRange {
		t.Fatalf("want ErrDefaultOutOfRange, got %v", err)
	}
}

func TestAsk_RateLimitPerAgent(t *testing.T) {
	r, _, _ := newRegistry(t)
	r.Limits.PerAgentPerHour = 2
	r.Limits.PerTaskPerHour = 100

	for range 2 {
		if _, err := r.Ask(context.Background(), orca.AskHumanRequest{
			AgentID: "arch", Question: "q",
		}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := r.Ask(context.Background(), orca.AskHumanRequest{AgentID: "arch", Question: "q"})
	if err != ErrRateLimitedAgent {
		t.Fatalf("want ErrRateLimitedAgent, got %v", err)
	}
	// Different agent still allowed.
	if _, err := r.Ask(context.Background(), orca.AskHumanRequest{AgentID: "qa", Question: "q"}); err != nil {
		t.Fatalf("different agent should be allowed: %v", err)
	}
}

func TestAnswer_RoutesBackToAgent(t *testing.T) {
	r, pub, _ := newRegistry(t)
	d, _ := r.Ask(context.Background(), orca.AskHumanRequest{
		AgentID: "arch", Question: "q", Options: []string{"a", "b"},
	})
	// Clear the first publish (decision→bridge).
	pub.mu.Lock()
	pub.ms = pub.ms[:0]
	pub.mu.Unlock()

	err := r.Answer(context.Background(), d.ID, orca.DecisionAnswer{
		Type:          orca.AnswerOption,
		Option:        1,
		ResponderID:   "U04XYZ",
		ResponderName: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	msgs := pub.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("want 1 reply, got %d", len(msgs))
	}
	if msgs[0].From != "slack" || msgs[0].To != "arch" || msgs[0].Kind != orca.KindResponse {
		t.Fatalf("reply routing: %+v", msgs[0])
	}
	if msgs[0].CorrelationID != d.ID {
		t.Fatalf("correlation mismatch")
	}
	// Body contains responder info.
	var body map[string]any
	_ = json.Unmarshal(msgs[0].Body, &body)
	ans := body["answer"].(map[string]any)
	if ans["responder_id"] != "U04XYZ" || ans["responder_name"] != "alice" {
		t.Fatalf("responder not surfaced: %v", ans)
	}

	// Re-answering should fail.
	if err := r.Answer(context.Background(), d.ID, orca.DecisionAnswer{Type: orca.AnswerFreeform, Text: "x"}); err != ErrAlreadyAnswered {
		t.Fatalf("want ErrAlreadyAnswered, got %v", err)
	}
}

func TestTimeout_AppliesDefaultOption(t *testing.T) {
	r, pub, _ := newRegistry(t)
	d, err := r.Ask(context.Background(), orca.AskHumanRequest{
		AgentID:        "arch",
		Question:       "q",
		Options:        []string{"a", "b"},
		TimeoutSeconds: 0, // force default
		DefaultOption:  2,
	})
	// Override timeout to something immediate for test sanity.
	r.mu.Lock()
	if t0, ok := r.timers[d.ID]; ok {
		t0.Stop()
	}
	r.timers[d.ID] = time.AfterFunc(10*time.Millisecond, func() { r.onTimeout(d.ID) })
	r.mu.Unlock()

	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)

	got, _ := r.Get(d.ID)
	if got.Status != orca.DecAnswered {
		t.Fatalf("status=%s (expected answered via default)", got.Status)
	}
	if got.Answer == nil || got.Answer.Option != 2 {
		t.Fatalf("answer=%+v", got.Answer)
	}
	// Second publish should have routed the default answer back.
	msgs := pub.snapshot()
	if len(msgs) < 2 {
		t.Fatalf("expected decision + answer publish, got %d", len(msgs))
	}
}

func TestTimeout_NoDefault_MarksTimedOut(t *testing.T) {
	r, _, _ := newRegistry(t)
	d, _ := r.Ask(context.Background(), orca.AskHumanRequest{
		AgentID: "arch", Question: "q", Options: []string{"a"},
	})
	r.mu.Lock()
	if t0, ok := r.timers[d.ID]; ok {
		t0.Stop()
	}
	r.timers[d.ID] = time.AfterFunc(10*time.Millisecond, func() { r.onTimeout(d.ID) })
	r.mu.Unlock()

	time.Sleep(60 * time.Millisecond)
	got, _ := r.Get(d.ID)
	if got.Status != orca.DecTimedOut {
		t.Fatalf("status=%s (expected timed_out)", got.Status)
	}
}
