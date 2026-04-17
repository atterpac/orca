package supervisor

import (
	"context"
	"testing"
	"time"

	"github.com/atterpac/orca/internal/bus"
	"github.com/atterpac/orca/internal/events"
	"github.com/atterpac/orca/internal/testutil"
	"github.com/atterpac/orca/pkg/orca"
)

func newTagHarness(t *testing.T) (*Supervisor, *testutil.FakeRuntime, *bus.InProc, func()) {
	t.Helper()
	b := bus.NewInProc()
	ev := events.NewBus(64)
	rt := testutil.NewRuntime()
	sup := New(b, ev)
	sup.RegisterRuntime(rt)
	return sup, rt, b, func() { sup.Shutdown() }
}

func spawn(t *testing.T, sup *Supervisor, id string, tags ...string) {
	t.Helper()
	if _, err := sup.Spawn(context.Background(), orca.AgentSpec{ID: id, Runtime: "fake", Tags: tags}); err != nil {
		t.Fatalf("spawn %s: %v", id, err)
	}
}

func TestFindByTagsAndMatch(t *testing.T) {
	sup, _, _, done := newTagHarness(t)
	defer done()

	spawn(t, sup, "a", "code", "bug")
	spawn(t, sup, "b", "code", "feature")
	spawn(t, sup, "c", "gtm")
	time.Sleep(20 * time.Millisecond)

	got := sup.FindByTags([]string{"code"}, false)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("code match: %v", got)
	}
	got = sup.FindByTags([]string{"code", "bug"}, false)
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("code+bug match: %v", got)
	}
	got = sup.FindByTags([]string{"gtm"}, false)
	if len(got) != 1 || got[0] != "c" {
		t.Fatalf("gtm match: %v", got)
	}
	got = sup.FindByTags([]string{"ops"}, false)
	if len(got) != 0 {
		t.Fatalf("no match expected, got %v", got)
	}
}

func TestDispatchModeAny_RoundRobin(t *testing.T) {
	sup, rt, _, done := newTagHarness(t)
	defer done()

	spawn(t, sup, "a", "code")
	spawn(t, sup, "b", "code")
	spawn(t, sup, "c", "gtm")
	time.Sleep(30 * time.Millisecond)

	// Four consecutive dispatches to tags=[code] with mode=any must
	// alternate between a and b (sorted order) — round-robin on sorted-tag key.
	var chosen []string
	for i := 0; i < 4; i++ {
		targets, err := sup.DispatchTagged(context.Background(), orca.Message{
			From: "cli", Tags: []string{"code"}, Mode: orca.ModeAny, Kind: orca.KindRequest,
		})
		if err != nil {
			t.Fatalf("dispatch %d: %v", i, err)
		}
		if len(targets) != 1 {
			t.Fatalf("want 1 target, got %v", targets)
		}
		chosen = append(chosen, targets[0])
	}
	// Expect pattern a,b,a,b (candidates sorted: [a,b], rr starts at 0).
	want := []string{"a", "b", "a", "b"}
	for i := range want {
		if chosen[i] != want[i] {
			t.Fatalf("round-robin wanted %v got %v", want, chosen)
		}
	}

	// Each dispatch should have delivered exactly once total to the picked
	// session (we can check the captured sends on sessions a and b).
	time.Sleep(40 * time.Millisecond)
	aSent := len(rt.Session("a").Sent())
	bSent := len(rt.Session("b").Sent())
	if aSent != 2 || bSent != 2 {
		t.Fatalf("expected 2 each; a=%d b=%d", aSent, bSent)
	}
	// c should have received nothing.
	if c := rt.Session("c"); c != nil && len(c.Sent()) > 0 {
		t.Fatalf("c got %d unwanted sends", len(c.Sent()))
	}
}

func TestDispatchModeAll_Broadcast(t *testing.T) {
	sup, rt, _, done := newTagHarness(t)
	defer done()

	spawn(t, sup, "a", "code", "bug")
	spawn(t, sup, "b", "code", "bug")
	spawn(t, sup, "c", "code") // does NOT have bug, should NOT receive
	time.Sleep(30 * time.Millisecond)

	targets, err := sup.DispatchTagged(context.Background(), orca.Message{
		From: "cli", Tags: []string{"code", "bug"}, Mode: orca.ModeAll, Kind: orca.KindBroadcast,
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("want 2 targets, got %v", targets)
	}
	time.Sleep(40 * time.Millisecond)
	if len(rt.Session("a").Sent()) != 1 || len(rt.Session("b").Sent()) != 1 {
		t.Fatalf("broadcast missed: a=%d b=%d", len(rt.Session("a").Sent()), len(rt.Session("b").Sent()))
	}
	if len(rt.Session("c").Sent()) != 0 {
		t.Fatalf("c received a broadcast it shouldn't have")
	}
}

func TestDispatchExcludesSender(t *testing.T) {
	sup, rt, _, done := newTagHarness(t)
	defer done()

	// Both agents share a tag. Sender (a) must not receive its own message
	// when dispatching to that tag.
	spawn(t, sup, "a", "code")
	spawn(t, sup, "b", "code")
	time.Sleep(20 * time.Millisecond)

	for i := 0; i < 3; i++ {
		targets, err := sup.DispatchTagged(context.Background(), orca.Message{
			From: "a", Tags: []string{"code"}, Mode: orca.ModeAny,
		})
		if err != nil {
			t.Fatalf("dispatch %d: %v", i, err)
		}
		if len(targets) != 1 || targets[0] != "b" {
			t.Fatalf("self-exclusion failed: %v", targets)
		}
	}
	time.Sleep(30 * time.Millisecond)
	if len(rt.Session("a").Sent()) != 0 {
		t.Fatalf("self received %d", len(rt.Session("a").Sent()))
	}
}

func TestDispatchNoMatchErrors(t *testing.T) {
	sup, _, _, done := newTagHarness(t)
	defer done()

	spawn(t, sup, "a", "code")
	time.Sleep(10 * time.Millisecond)

	_, err := sup.DispatchTagged(context.Background(), orca.Message{
		From: "cli", Tags: []string{"gtm"}, Mode: orca.ModeAny,
	})
	if err == nil {
		t.Fatal("expected error for unmatched tags")
	}
}

func TestKillRemovesFromTagIndex(t *testing.T) {
	sup, _, _, done := newTagHarness(t)
	defer done()

	spawn(t, sup, "a", "code")
	spawn(t, sup, "b", "code")
	time.Sleep(10 * time.Millisecond)

	if err := sup.Kill("a"); err != nil {
		t.Fatalf("kill: %v", err)
	}
	got := sup.FindByTags([]string{"code"}, false)
	if len(got) != 1 || got[0] != "b" {
		t.Fatalf("expected just b after kill; got %v", got)
	}
}
