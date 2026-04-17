package supervisor

import (
	"context"
	"testing"
	"time"

	"github.com/atterpac/orca/internal/events"
	"github.com/atterpac/orca/pkg/orca"
)

// collectDropped subscribes and returns a snapshot fn for MessageDropped events.
func collectDropped(t *testing.T, ev *events.Bus) (func() []orca.Event, func()) {
	t.Helper()
	return collectEvents(t, ev, orca.EvtMessageDropped)
}

func TestACL_Permissive_WhenOmitted(t *testing.T) {
	sup, rt, _, _, done := budgetHarness(t)
	defer done()

	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{ID: "a", Runtime: "fake", Tags: []string{"code"}})
	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{ID: "b", Runtime: "fake"})
	time.Sleep(30 * time.Millisecond)

	targets, err := sup.DispatchTagged(context.Background(), orca.Message{
		From: "b", Tags: []string{"code"}, Mode: orca.ModeAny,
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(targets) != 1 || targets[0] != "a" {
		t.Fatalf("want [a], got %v", targets)
	}
	time.Sleep(40 * time.Millisecond)
	if len(rt.Session("a").Sent()) != 1 {
		t.Fatalf("a should have received: got %d", len(rt.Session("a").Sent()))
	}
}

func TestACL_SenderSendsToBlocks(t *testing.T) {
	sup, rt, _, ev, done := budgetHarness(t)
	defer done()
	snap, stop := collectDropped(t, ev)
	defer stop()

	// sender allows only id:qa. Target a is id:impl, so blocked.
	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{ID: "impl", Runtime: "fake", Tags: []string{"code"}})
	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{ID: "qa", Runtime: "fake", Tags: []string{"code"}})
	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{
		ID:      "arch",
		Runtime: "fake",
		Tags:    []string{"orchestrator"},
		ACL:     &orca.ACL{SendsTo: []string{"id:qa"}},
	})
	time.Sleep(30 * time.Millisecond)

	// Direct send to impl must be blocked.
	err := sup.DispatchDirect(context.Background(), orca.Message{
		From: "arch", To: "impl", Kind: orca.KindRequest,
	})
	if err == nil {
		t.Fatal("expected direct send to be blocked")
	}
	// qa send should succeed.
	if err := sup.DispatchDirect(context.Background(), orca.Message{
		From: "arch", To: "qa", Kind: orca.KindRequest,
	}); err != nil {
		t.Fatalf("direct to qa should succeed: %v", err)
	}
	time.Sleep(40 * time.Millisecond)
	if len(rt.Session("impl").Sent()) != 0 {
		t.Fatalf("impl must not have received")
	}
	if len(rt.Session("qa").Sent()) != 1 {
		t.Fatalf("qa should have 1: got %d", len(rt.Session("qa").Sent()))
	}

	dropped := snap()
	foundImpl := false
	for _, e := range dropped {
		p, _ := e.Payload.(map[string]any)
		if p["to"] == "impl" && p["reason"] == "acl:sends_to" {
			foundImpl = true
		}
	}
	if !foundImpl {
		t.Fatalf("expected MessageDropped reason=acl:sends_to for impl, got %v", dropped)
	}
}

func TestACL_RecipientAcceptsFromBlocks(t *testing.T) {
	sup, rt, b, ev, done := budgetHarness(t)
	defer done()
	snap, stop := collectDropped(t, ev)
	defer stop()

	// Recipient accepts only from tag:orchestrator.
	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{
		ID:      "impl",
		Runtime: "fake",
		Tags:    []string{"code"},
		ACL:     &orca.ACL{AcceptsFrom: []string{"tag:orchestrator"}},
	})
	// Non-orchestrator sender.
	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{ID: "peer", Runtime: "fake", Tags: []string{"code"}})
	time.Sleep(30 * time.Millisecond)

	// Publish directly onto the bus bypassing DispatchDirect — simulates an
	// internal path that skipped the sender check. The inbox goroutine
	// should still drop it.
	err := b.Publish(context.Background(), orca.Message{
		From: "peer", To: "impl", Kind: orca.KindRequest,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	if len(rt.Session("impl").Sent()) != 0 {
		t.Fatal("impl should have rejected message on delivery")
	}

	dropped := snap()
	found := false
	for _, e := range dropped {
		p, _ := e.Payload.(map[string]any)
		if p["reason"] == "acl:accepts_from" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected acl:accepts_from drop, got %v", dropped)
	}
}

func TestACL_TagSelector(t *testing.T) {
	sup, rt, _, _, done := budgetHarness(t)
	defer done()

	// sender may only reach tag:review
	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{ID: "reviewer", Runtime: "fake", Tags: []string{"review", "security"}})
	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{ID: "impl", Runtime: "fake", Tags: []string{"code"}})
	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{
		ID:      "arch",
		Runtime: "fake",
		ACL:     &orca.ACL{SendsTo: []string{"tag:review"}},
	})
	time.Sleep(30 * time.Millisecond)

	if err := sup.DispatchDirect(context.Background(), orca.Message{
		From: "arch", To: "reviewer", Kind: orca.KindRequest,
	}); err != nil {
		t.Fatalf("reviewer should be reachable: %v", err)
	}
	if err := sup.DispatchDirect(context.Background(), orca.Message{
		From: "arch", To: "impl", Kind: orca.KindRequest,
	}); err == nil {
		t.Fatal("impl should be blocked (lacks tag:review)")
	}
	time.Sleep(30 * time.Millisecond)
	if len(rt.Session("reviewer").Sent()) != 1 || len(rt.Session("impl").Sent()) != 0 {
		t.Fatalf("reviewer=%d impl=%d (want 1, 0)",
			len(rt.Session("reviewer").Sent()), len(rt.Session("impl").Sent()))
	}
}

func TestACL_TagRoutedDispatchFiltersCandidates(t *testing.T) {
	sup, rt, _, ev, done := budgetHarness(t)
	defer done()
	snap, stop := collectDropped(t, ev)
	defer stop()

	// Three agents share tag "review"; arch may only reach reviewer-senior.
	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{ID: "reviewer-junior", Runtime: "fake", Tags: []string{"review"}})
	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{ID: "reviewer-senior", Runtime: "fake", Tags: []string{"review"}})
	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{ID: "reviewer-extern", Runtime: "fake", Tags: []string{"review"}})
	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{
		ID:      "arch",
		Runtime: "fake",
		ACL:     &orca.ACL{SendsTo: []string{"id:reviewer-senior"}},
	})
	time.Sleep(30 * time.Millisecond)

	// Dispatch to tag:review should be forced onto reviewer-senior only.
	for range 3 {
		targets, err := sup.DispatchTagged(context.Background(), orca.Message{
			From: "arch", Tags: []string{"review"}, Mode: orca.ModeAny,
		})
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if len(targets) != 1 || targets[0] != "reviewer-senior" {
			t.Fatalf("want [reviewer-senior], got %v", targets)
		}
	}
	time.Sleep(40 * time.Millisecond)

	if len(rt.Session("reviewer-senior").Sent()) != 3 {
		t.Fatalf("senior should receive all 3: got %d", len(rt.Session("reviewer-senior").Sent()))
	}
	if len(rt.Session("reviewer-junior").Sent()) != 0 || len(rt.Session("reviewer-extern").Sent()) != 0 {
		t.Fatal("junior/extern must receive nothing")
	}

	dropped := snap()
	if len(dropped) < 2 {
		t.Fatalf("expected ≥2 drops for junior/extern, got %d: %v", len(dropped), dropped)
	}
}

func TestACL_Wildcard(t *testing.T) {
	sup, _, _, _, done := budgetHarness(t)
	defer done()

	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{ID: "a", Runtime: "fake"})
	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{
		ID:      "b",
		Runtime: "fake",
		ACL:     &orca.ACL{SendsTo: []string{"*"}},
	})
	time.Sleep(30 * time.Millisecond)

	if err := sup.DispatchDirect(context.Background(), orca.Message{
		From: "b", To: "a", Kind: orca.KindRequest,
	}); err != nil {
		t.Fatalf("wildcard should permit: %v", err)
	}
}

func TestACL_ReachableFrom(t *testing.T) {
	sup, _, _, _, done := budgetHarness(t)
	defer done()

	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{ID: "alpha", Runtime: "fake", Tags: []string{"code"}})
	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{ID: "bravo", Runtime: "fake", Tags: []string{"review"}})
	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{
		ID:      "ctrl",
		Runtime: "fake",
		ACL:     &orca.ACL{SendsTo: []string{"tag:review"}},
	})
	time.Sleep(20 * time.Millisecond)

	got := sup.ReachableFrom("ctrl")
	if _, ok := got["bravo"]; !ok {
		t.Fatalf("bravo should be reachable, got %v", got)
	}
	if _, ok := got["alpha"]; ok {
		t.Fatalf("alpha should NOT be reachable, got %v", got)
	}
}

func TestACL_SelectorParser(t *testing.T) {
	cases := []struct {
		raw    string
		id     string
		tags   []string
		any    bool
	}{
		{"*", "", nil, true},
		{"id:foo", "foo", nil, false},
		{"tag:code", "", []string{"code"}, false},
		{"tag:code,bug", "", []string{"code", "bug"}, false},
		{" tag:code , bug ", "", []string{"code", "bug"}, false},
		{"implementer", "implementer", nil, false}, // unqualified → id
	}
	for _, c := range cases {
		parsed := parseSelectors([]string{c.raw})
		if len(parsed) != 1 {
			t.Fatalf("%q: want 1 selector, got %d", c.raw, len(parsed))
		}
		s := parsed[0]
		if s.Any != c.any {
			t.Errorf("%q: any=%v want %v", c.raw, s.Any, c.any)
		}
		if s.ID != c.id {
			t.Errorf("%q: id=%q want %q", c.raw, s.ID, c.id)
		}
		if len(s.Tags) != len(c.tags) {
			t.Errorf("%q: tags=%v want %v", c.raw, s.Tags, c.tags)
			continue
		}
		for i := range c.tags {
			if s.Tags[i] != c.tags[i] {
				t.Errorf("%q: tags=%v want %v", c.raw, s.Tags, c.tags)
			}
		}
	}
}
