package supervisor

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/atterpac/orca/internal/bus"
	"github.com/atterpac/orca/internal/events"
	"github.com/atterpac/orca/internal/testutil"
	"github.com/atterpac/orca/pkg/orca"
)

// collectEvents subscribes and returns a func that snapshots the events
// observed so far. Filters by kind so we ignore the usual lifecycle churn.
func collectEvents(t *testing.T, ev *events.Bus, kinds ...orca.EventKind) (func() []orca.Event, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := ev.Subscribe(ctx, events.Filter{Kinds: kinds})
	var mu sync.Mutex
	var out []orca.Event
	go func() {
		for e := range ch {
			mu.Lock()
			out = append(out, e)
			mu.Unlock()
		}
	}()
	snapshot := func() []orca.Event {
		mu.Lock()
		defer mu.Unlock()
		cp := make([]orca.Event, len(out))
		copy(cp, out)
		return cp
	}
	return snapshot, cancel
}

func budgetHarness(t *testing.T) (*Supervisor, *testutil.FakeRuntime, *bus.InProc, *events.Bus, func()) {
	t.Helper()
	b := bus.NewInProc()
	ev := events.NewBus(64)
	rt := testutil.NewRuntime()
	sup := New(b, ev)
	sup.RegisterRuntime(rt)
	return sup, rt, b, ev, func() { sup.Shutdown() }
}

func TestBudget_NoBudgetNoEvents(t *testing.T) {
	sup, rt, _, ev, done := budgetHarness(t)
	defer done()

	snap, stop := collectEvents(t, ev, orca.EvtBudgetWarn, orca.EvtBudgetExceeded)
	defer stop()

	_, err := sup.Spawn(context.Background(), orca.AgentSpec{ID: "a", Runtime: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	rt.Session("a").AddUsage(orca.TokenUsage{InputTokens: 999999, OutputTokens: 999999, CostUSD: 100})
	time.Sleep(50 * time.Millisecond)

	if got := snap(); len(got) != 0 {
		t.Fatalf("expected no budget events without Budget set, got %+v", got)
	}
}

func TestBudget_WarnAt80Percent(t *testing.T) {
	sup, rt, _, ev, done := budgetHarness(t)
	defer done()
	snap, stop := collectEvents(t, ev, orca.EvtBudgetWarn, orca.EvtBudgetExceeded)
	defer stop()

	_, err := sup.Spawn(context.Background(), orca.AgentSpec{
		ID: "a", Runtime: "fake",
		Budget: &orca.Budget{MaxInputTokens: 1000, OnBreach: "warn"},
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)

	// 50% → nothing
	rt.Session("a").AddUsage(orca.TokenUsage{InputTokens: 500})
	time.Sleep(40 * time.Millisecond)
	if got := snap(); len(got) != 0 {
		t.Fatalf("no events expected at 50%%, got %v", got)
	}
	// 85% → exactly one BudgetWarn
	rt.Session("a").AddUsage(orca.TokenUsage{InputTokens: 350})
	time.Sleep(40 * time.Millisecond)
	got := snap()
	if len(got) != 1 || got[0].Kind != orca.EvtBudgetWarn {
		t.Fatalf("want 1 BudgetWarn, got %v", got)
	}
	// further turns in warn zone → no duplicate warns
	rt.Session("a").AddUsage(orca.TokenUsage{InputTokens: 50})
	time.Sleep(40 * time.Millisecond)
	if got := snap(); len(got) != 1 {
		t.Fatalf("duplicate warn emitted: %v", got)
	}
}

func TestBudget_ExceededWarnPolicy(t *testing.T) {
	sup, rt, _, ev, done := budgetHarness(t)
	defer done()
	snap, stop := collectEvents(t, ev, orca.EvtBudgetWarn, orca.EvtBudgetExceeded)
	defer stop()

	info, _ := sup.Spawn(context.Background(), orca.AgentSpec{
		ID: "a", Runtime: "fake",
		Budget: &orca.Budget{MaxCostUSD: 1.0, OnBreach: "warn"},
	})
	time.Sleep(30 * time.Millisecond)
	rt.Session("a").AddUsage(orca.TokenUsage{CostUSD: 1.5})
	time.Sleep(50 * time.Millisecond)

	got := snap()
	kinds := map[orca.EventKind]int{}
	for _, e := range got {
		kinds[e.Kind]++
	}
	if kinds[orca.EvtBudgetExceeded] != 1 {
		t.Fatalf("want 1 BudgetExceeded, got %v", got)
	}
	// Warn-only policy must not pause the agent.
	cur, _ := sup.Get(info.Spec.ID)
	if cur.BudgetPaused {
		t.Fatal("warn policy should not pause agent")
	}
}

func TestBudget_SoftStopPausesDelivery(t *testing.T) {
	sup, rt, b, ev, done := budgetHarness(t)
	defer done()
	_, stop := collectEvents(t, ev, orca.EvtBudgetExceeded)
	defer stop()

	_, err := sup.Spawn(context.Background(), orca.AgentSpec{
		ID: "a", Runtime: "fake",
		Budget: &orca.Budget{MaxInputTokens: 100, OnBreach: "soft_stop"},
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)

	// Before breach, deliver one message — session should receive it.
	body, _ := json.Marshal("hello 1")
	_ = b.Publish(context.Background(), orca.Message{From: "cli", To: "a", Body: body, Kind: orca.KindRequest})
	time.Sleep(60 * time.Millisecond)
	sess := rt.Session("a")
	if len(sess.Sent()) != 1 {
		t.Fatalf("want 1 pre-breach delivery, got %d", len(sess.Sent()))
	}

	// Trip the budget.
	sess.AddUsage(orca.TokenUsage{InputTokens: 200})
	time.Sleep(60 * time.Millisecond)

	// Now deliveries should be dropped by supervisor.
	body2, _ := json.Marshal("hello 2")
	_ = b.Publish(context.Background(), orca.Message{From: "cli", To: "a", Body: body2, Kind: orca.KindRequest})
	time.Sleep(60 * time.Millisecond)
	if len(sess.Sent()) != 1 {
		t.Fatalf("soft_stop should have dropped second message; session received %d", len(sess.Sent()))
	}

	cur, _ := sup.Get("a")
	if !cur.BudgetPaused {
		t.Fatal("expected BudgetPaused=true under soft_stop breach")
	}
}

func TestBudget_HardInterruptKillsAgent(t *testing.T) {
	sup, rt, _, ev, done := budgetHarness(t)
	defer done()
	snap, stop := collectEvents(t, ev, orca.EvtBudgetExceeded)
	defer stop()

	_, err := sup.Spawn(context.Background(), orca.AgentSpec{
		ID: "a", Runtime: "fake",
		Budget: &orca.Budget{MaxInputTokens: 50, OnBreach: "hard_interrupt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)

	rt.Session("a").AddUsage(orca.TokenUsage{InputTokens: 100})
	// Give supervisor time to emit event + schedule kill + remove from registry.
	deadline := time.After(500 * time.Millisecond)
	for {
		if list := sup.List(); len(list) == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("agent not removed after hard_interrupt; list=%v", sup.List())
		case <-time.After(20 * time.Millisecond):
		}
	}

	if got := snap(); len(got) != 1 || got[0].Kind != orca.EvtBudgetExceeded {
		t.Fatalf("want one BudgetExceeded, got %v", got)
	}
}

func TestBudget_PctAcrossMultipleDimensions(t *testing.T) {
	// A single dimension tripping is enough; here cost_usd trips while token
	// dimensions are nowhere near.
	sup, rt, _, ev, done := budgetHarness(t)
	defer done()
	snap, stop := collectEvents(t, ev, orca.EvtBudgetWarn, orca.EvtBudgetExceeded)
	defer stop()

	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{
		ID: "a", Runtime: "fake",
		Budget: &orca.Budget{
			MaxInputTokens:  1_000_000,
			MaxOutputTokens: 1_000_000,
			MaxCostUSD:      0.10,
			OnBreach:        "warn",
		},
	})
	time.Sleep(30 * time.Millisecond)
	rt.Session("a").AddUsage(orca.TokenUsage{InputTokens: 100, OutputTokens: 50, CostUSD: 0.15})
	time.Sleep(50 * time.Millisecond)

	got := snap()
	seen := map[orca.EventKind]bool{}
	for _, e := range got {
		seen[e.Kind] = true
	}
	if !seen[orca.EvtBudgetExceeded] {
		t.Fatalf("expected BudgetExceeded triggered by cost dim, got %v", got)
	}
}
