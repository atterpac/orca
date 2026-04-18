package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/atterpac/orca/internal/bus"
	"github.com/atterpac/orca/internal/events"
	"github.com/atterpac/orca/internal/testutil"
	"github.com/atterpac/orca/pkg/orca"
)

func newHarness(t *testing.T) (*Supervisor, *testutil.FakeRuntime, *bus.InProc, *events.Bus, func()) {
	t.Helper()
	b := bus.NewInProc()
	ev := events.NewBus(64)
	rt := testutil.NewRuntime()
	sup := New(b, ev)
	sup.RegisterRuntime(rt)
	cleanup := func() { sup.Shutdown() }
	return sup, rt, b, ev, cleanup
}

func TestSpawnAndList(t *testing.T) {
	sup, _, _, _, done := newHarness(t)
	defer done()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	info, err := sup.Spawn(ctx, orca.AgentSpec{ID: "alice", Runtime: "fake"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if info.Spec.ID != "alice" {
		t.Fatalf("bad spec id: %s", info.Spec.ID)
	}
	list := sup.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(list))
	}
}

func TestDuplicateSpawnRejected(t *testing.T) {
	sup, _, _, _, done := newHarness(t)
	defer done()
	ctx := context.Background()
	if _, err := sup.Spawn(ctx, orca.AgentSpec{ID: "a", Runtime: "fake"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Spawn(ctx, orca.AgentSpec{ID: "a", Runtime: "fake"}); err == nil {
		t.Fatal("expected duplicate spawn to fail")
	}
}

func TestUnknownRuntimeRejected(t *testing.T) {
	sup, _, _, _, done := newHarness(t)
	defer done()
	if _, err := sup.Spawn(context.Background(), orca.AgentSpec{ID: "a", Runtime: "nope"}); err == nil {
		t.Fatal("expected unknown runtime to fail")
	}
}

func TestInboxDelivery(t *testing.T) {
	sup, rt, b, _, done := newHarness(t)
	defer done()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := sup.Spawn(ctx, orca.AgentSpec{ID: "bob", Runtime: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	// Allow supervisor to wire its Subscribe.
	time.Sleep(50 * time.Millisecond)

	body, _ := json.Marshal("ping")
	msg := orca.Message{From: "cli", To: "bob", Kind: orca.KindRequest, Body: body}
	if err := b.Publish(ctx, msg); err != nil {
		t.Fatalf("publish: %v", err)
	}

	deadline := time.After(500 * time.Millisecond)
	for {
		sess := rt.Session("bob")
		if sess != nil && len(sess.Sent()) > 0 {
			if sess.Sent()[0].From != "cli" {
				t.Fatalf("wrong from: %s", sess.Sent()[0].From)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("message never delivered to session")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestInboxDelivery_SendFailureEmitsDrop confirms a Send error on
// inbox delivery surfaces as an EvtMessageDropped event tagged
// reason=delivery_error, so operators can see messages that never
// reached the agent.
func TestInboxDelivery_SendFailureEmitsDrop(t *testing.T) {
	sup, rt, b, ev, done := newHarness(t)
	defer done()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := sup.Spawn(ctx, orca.AgentSpec{ID: "bob", Runtime: "fake"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	// Subscribe BEFORE triggering the drop so we can't miss the event.
	subCtx, cancelSub := context.WithCancel(context.Background())
	defer cancelSub()
	evCh, _ := ev.Subscribe(subCtx, events.Filter{Kinds: []orca.EventKind{orca.EvtMessageDropped}})

	rt.Session("bob").SetSendErr(errors.New("pipe closed"))

	body, _ := json.Marshal("ping")
	if err := b.Publish(ctx, orca.Message{From: "cli", To: "bob", Kind: orca.KindRequest, Body: body}); err != nil {
		t.Fatal(err)
	}

	select {
	case e := <-evCh:
		p, ok := e.Payload.(map[string]any)
		if !ok {
			t.Fatalf("payload shape unexpected: %T", e.Payload)
		}
		if p["reason"] != "delivery_error" {
			t.Fatalf("want reason=delivery_error, got %v", p["reason"])
		}
		if p["err"] != "pipe closed" {
			t.Fatalf("want err=pipe closed, got %v", p["err"])
		}
		if p["from"] != "cli" || p["to"] != "bob" {
			t.Fatalf("wrong from/to: %v / %v", p["from"], p["to"])
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no MessageDropped event emitted after Send failure")
	}
}

func TestUsageAggregation(t *testing.T) {
	sup, rt, _, _, done := newHarness(t)
	defer done()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for _, id := range []string{"a", "b"} {
		if _, err := sup.Spawn(ctx, orca.AgentSpec{ID: id, Runtime: "fake"}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(50 * time.Millisecond)

	rt.Session("a").AddUsage(orca.TokenUsage{InputTokens: 10, OutputTokens: 5, CostUSD: 0.01})
	rt.Session("b").AddUsage(orca.TokenUsage{InputTokens: 20, OutputTokens: 3, CostUSD: 0.02})

	// Give events a tick to propagate into supervisor bookkeeping.
	time.Sleep(50 * time.Millisecond)

	total := sup.AggregateUsage()
	if total.InputTokens != 30 {
		t.Fatalf("expected 30 input, got %d", total.InputTokens)
	}
	if total.OutputTokens != 8 {
		t.Fatalf("expected 8 output, got %d", total.OutputTokens)
	}
	if total.CostUSD < 0.029 || total.CostUSD > 0.031 {
		t.Fatalf("expected ~$0.03, got %f", total.CostUSD)
	}
}

func TestKillRemovesAgent(t *testing.T) {
	sup, _, _, _, done := newHarness(t)
	defer done()
	ctx := context.Background()
	if _, err := sup.Spawn(ctx, orca.AgentSpec{ID: "a", Runtime: "fake"}); err != nil {
		t.Fatal(err)
	}
	if err := sup.Kill("a"); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if len(sup.List()) != 0 {
		t.Fatal("agent still listed after kill")
	}
	if err := sup.Kill("a"); err == nil {
		t.Fatal("expected not-found on second kill")
	}
}

func TestEventsPumpedThroughBus(t *testing.T) {
	sup, rt, _, ev, done := newHarness(t)
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink, _ := ev.Subscribe(ctx, events.Filter{AgentID: "a"})
	if _, err := sup.Spawn(ctx, orca.AgentSpec{ID: "a", Runtime: "fake"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	rt.Session("a").Emit(orca.Event{Kind: orca.EvtTurnCompleted})

	deadline := time.After(300 * time.Millisecond)
	saw := map[orca.EventKind]bool{}
	for len(saw) < 2 {
		select {
		case e := <-sink:
			saw[e.Kind] = true
		case <-deadline:
			t.Fatalf("expected AgentReady + TurnCompleted, got %v", saw)
		}
	}
}
