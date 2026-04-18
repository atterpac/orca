package supervisor

import (
	"context"
	"testing"
	"time"

	"github.com/atterpac/orca/internal/bus"
	"github.com/atterpac/orca/internal/discussions"
	"github.com/atterpac/orca/internal/events"
	"github.com/atterpac/orca/internal/testutil"
	"github.com/atterpac/orca/pkg/orca"
	"github.com/atterpac/orca/pkg/runtime/bridge"
)

// TestIntegration_MultiSessionBridgeDiscussion exercises the full
// coordination boundary that Phase 1.1 and Phase 2 fixes touched but
// never proved end-to-end: a bridge-originated correlated message
// arrives at a multi-session agent, supervisor spawns a per-corr
// sub-session, the discussion registry records the touch, and a
// second message on the same correlation reuses the sub (no second
// Start call).
//
// Failure of any piece manifests as one of: wrong sub-session count,
// missing discussion record, or mismatched participant list.
func TestIntegration_MultiSessionBridgeDiscussion(t *testing.T) {
	b := bus.NewInProc()
	ev := events.NewBus(64)
	sup := New(b, ev)
	defer sup.Shutdown()

	// Multi-session-capable fake for the agent; bridge runtime for the
	// bridge side. Discussion registry hooked in via OnDiscussionTouch
	// exactly as cmd/orca/daemon.go does it in production.
	fake := testutil.NewRuntime()
	msRT := &multiRuntime{inner: fake}
	sup.RegisterRuntime(msRT)
	bridgeRT := bridge.New(b)
	sup.RegisterRuntime(bridgeRT)

	disc := discussions.New(ev)
	defer disc.Stop()
	sup.OnDiscussionTouch = func(bridgeID, agentID, corrID string) {
		disc.Touch(discussions.TouchInfo{
			ID:            corrID,
			BridgeAgentID: bridgeID,
			Participant:   agentID,
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Spawn the bridge and the multi-session agent.
	if _, err := sup.Spawn(ctx, orca.AgentSpec{ID: "slack", Runtime: "bridge"}); err != nil {
		t.Fatalf("spawn bridge: %v", err)
	}
	if _, err := sup.Spawn(ctx, orca.AgentSpec{ID: "arch", Runtime: "fake-multi"}); err != nil {
		t.Fatalf("spawn arch: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // let deliverInbox subscribe

	// Simulate an inbound bridge message: slack user writes into a
	// thread, bridge assigns a correlation, delivery routes to arch.
	corr := "slack:C123:T1"
	if err := b.Publish(ctx, orca.Message{
		From: "slack", To: "arch", Kind: orca.KindRequest, CorrelationID: corr,
	}); err != nil {
		t.Fatal(err)
	}

	// Wait for the multi-session machinery to materialize a sub.
	sup.mu.RLock()
	rec := sup.agents["arch"]
	sup.mu.RUnlock()
	ms := rec.session.(*multiSession)

	deadline := time.After(time.Second)
	for {
		ms.mu.Lock()
		_, ok := ms.subs[corr]
		n := len(ms.subs)
		ms.mu.Unlock()
		if ok {
			if n != 1 {
				t.Fatalf("expected 1 sub after first message, got %d", n)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("sub-session for correlation never materialized")
		case <-time.After(20 * time.Millisecond):
		}
	}

	// Discussion registry should have recorded the touch with slack as
	// the bridge and arch as a participant.
	var d *orca.Discussion
	discDeadline := time.After(time.Second)
	for {
		if cur, ok := disc.Get(corr); ok {
			d = cur
			break
		}
		select {
		case <-discDeadline:
			t.Fatal("discussion never opened for correlation")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if d.BridgeAgentID != "slack" {
		t.Fatalf("bridge_agent_id=%s, want slack", d.BridgeAgentID)
	}
	foundArch := false
	for _, p := range d.Participants {
		if p == "arch" {
			foundArch = true
		}
	}
	if !foundArch {
		t.Fatalf("arch not in participants: %v", d.Participants)
	}

	// Second message on same correlation must reuse the sub — no new
	// session spawned, just re-activated.
	if err := b.Publish(ctx, orca.Message{
		From: "slack", To: "arch", Kind: orca.KindRequest, CorrelationID: corr,
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	ms.mu.Lock()
	n := len(ms.subs)
	ms.mu.Unlock()
	if n != 1 {
		t.Fatalf("second message should reuse sub; want 1 sub, got %d", n)
	}

	// Discussion message count should reflect both touches.
	after, _ := disc.Get(corr)
	if after.MessageCount < 2 {
		t.Fatalf("want ≥2 messages recorded, got %d", after.MessageCount)
	}
}
