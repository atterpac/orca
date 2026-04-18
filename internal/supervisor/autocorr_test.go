package supervisor

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/atterpac/orca/pkg/orca"
)

// autocorrHarness wires supervisor + fake runtime with two agents.
// Returns the supervisor and a function that publishes a message to an
// agent's inbox so we can seed a "last inbound correlation_id".
func autocorrHarness(t *testing.T) (*Supervisor, func(ctx context.Context, m orca.Message), func()) {
	t.Helper()
	sup, _, b, _, done := budgetHarness(t)
	return sup, func(ctx context.Context, m orca.Message) { _ = b.Publish(ctx, m) }, done
}

func TestAutoCorr_EmptyOutboundFilledFromLastInbound(t *testing.T) {
	sup, publish, done := autocorrHarness(t)
	defer done()

	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{ID: "a", Runtime: "fake"})
	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{ID: "b", Runtime: "fake"})
	time.Sleep(30 * time.Millisecond)

	// Seed: someone sends a message to a with correlation_id=ABC.
	publish(context.Background(), orca.Message{
		From: "ext", To: "a", CorrelationID: "CONV-ABC", Kind: orca.KindRequest,
	})
	time.Sleep(40 * time.Millisecond)

	// Confirm supervisor recorded the inbound correlation.
	if got := sup.AutoCorrelationFor("a"); got != "CONV-ABC" {
		t.Fatalf("expected last-inbound=CONV-ABC, got %q", got)
	}

	// Agent a sends to b WITHOUT correlation_id. Supervisor should fill.
	body, _ := json.Marshal("pass it along")
	msg := orca.Message{From: "a", To: "b", Body: body, Kind: orca.KindRequest}
	if err := sup.DispatchDirect(context.Background(), msg); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	time.Sleep(40 * time.Millisecond)

	// Supervisor should also have recorded b's inbound = CONV-ABC.
	if got := sup.AutoCorrelationFor("b"); got != "CONV-ABC" {
		t.Fatalf("expected b's last-inbound=CONV-ABC (propagated), got %q", got)
	}
}

func TestAutoCorr_ExplicitOverridesAuto(t *testing.T) {
	sup, publish, done := autocorrHarness(t)
	defer done()

	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{ID: "a", Runtime: "fake"})
	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{ID: "b", Runtime: "fake"})
	time.Sleep(30 * time.Millisecond)

	publish(context.Background(), orca.Message{
		From: "ext", To: "a", CorrelationID: "CONV-ABC", Kind: orca.KindRequest,
	})
	time.Sleep(40 * time.Millisecond)

	// Agent a explicitly sets a different correlation. Must not be overwritten.
	body, _ := json.Marshal("unrelated topic")
	msg := orca.Message{
		From: "a", To: "b", Body: body, Kind: orca.KindRequest,
		CorrelationID: "EXPLICIT-OTHER",
	}
	if err := sup.DispatchDirect(context.Background(), msg); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	time.Sleep(40 * time.Millisecond)

	if got := sup.AutoCorrelationFor("b"); got != "EXPLICIT-OTHER" {
		t.Fatalf("explicit correlation should win; b's last-inbound=%q", got)
	}
}

func TestAutoCorr_NoSeedLeavesEmpty(t *testing.T) {
	sup, _, done := autocorrHarness(t)
	defer done()

	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{ID: "a", Runtime: "fake"})
	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{ID: "b", Runtime: "fake"})
	time.Sleep(30 * time.Millisecond)

	// No inbound to a yet. Outbound with empty correlation must stay empty.
	body, _ := json.Marshal("first")
	msg := orca.Message{From: "a", To: "b", Body: body, Kind: orca.KindRequest}
	if err := sup.DispatchDirect(context.Background(), msg); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	time.Sleep(40 * time.Millisecond)

	if got := sup.AutoCorrelationFor("b"); got != "" {
		t.Fatalf("b should have no last-inbound, got %q", got)
	}
}

// TestAutoCorr_KillClearsEntry ensures Kill removes the agent's
// lastInboundCorr record so a long-running daemon with churning agents
// doesn't accumulate stale entries.
func TestAutoCorr_KillClearsEntry(t *testing.T) {
	sup, publish, done := autocorrHarness(t)
	defer done()

	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{ID: "a", Runtime: "fake"})
	time.Sleep(30 * time.Millisecond)

	publish(context.Background(), orca.Message{
		From: "ext", To: "a", CorrelationID: "CONV-DEAD", Kind: orca.KindRequest,
	})
	time.Sleep(40 * time.Millisecond)

	if got := sup.AutoCorrelationFor("a"); got != "CONV-DEAD" {
		t.Fatalf("seed did not stick; got %q", got)
	}

	if err := sup.Kill("a"); err != nil {
		t.Fatalf("kill: %v", err)
	}

	if got := sup.AutoCorrelationFor("a"); got != "" {
		t.Fatalf("lastInboundCorr entry should be cleared after Kill; got %q", got)
	}
}

func TestAutoCorr_TaggedDispatchAlsoFills(t *testing.T) {
	sup, publish, done := autocorrHarness(t)
	defer done()

	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{ID: "a", Runtime: "fake"})
	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{ID: "b", Runtime: "fake", Tags: []string{"review"}})
	time.Sleep(30 * time.Millisecond)

	publish(context.Background(), orca.Message{
		From: "ext", To: "a", CorrelationID: "CONV-XYZ", Kind: orca.KindRequest,
	})
	time.Sleep(40 * time.Millisecond)

	// Tag-routed dispatch without correlation_id should also auto-fill.
	targets, err := sup.DispatchTagged(context.Background(), orca.Message{
		From: "a", Tags: []string{"review"}, Mode: orca.ModeAny, Kind: orca.KindRequest,
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(targets) != 1 || targets[0] != "b" {
		t.Fatalf("want [b], got %v", targets)
	}
	time.Sleep(40 * time.Millisecond)

	if got := sup.AutoCorrelationFor("b"); got != "CONV-XYZ" {
		t.Fatalf("tagged dispatch should carry correlation; b's last-inbound=%q", got)
	}
}
