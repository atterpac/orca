package supervisor

import (
	"context"
	"testing"
	"time"

	"github.com/atterpac/orca/pkg/orca"
)

func TestTranscript_RingBufferEvictsOldest(t *testing.T) {
	tr := newTranscript(3)
	for i := 0; i < 5; i++ {
		tr.add(orca.Event{Kind: orca.EvtAgentReady, Payload: map[string]any{"i": i}})
	}
	got := tr.snapshot(0, nil)
	if len(got) != 3 {
		t.Fatalf("ring should cap at 3, got %d", len(got))
	}
	// Oldest two evicted; remaining are i=2,3,4.
	for k, expect := range []int{2, 3, 4} {
		p, _ := got[k].Payload.(map[string]any)
		if int(p["i"].(int)) != expect {
			t.Errorf("position %d: got i=%v want %d", k, p["i"], expect)
		}
	}
}

func TestTranscript_KindFilter(t *testing.T) {
	tr := newTranscript(10)
	tr.add(orca.Event{Kind: orca.EvtAgentReady})
	tr.add(orca.Event{Kind: orca.EvtToolCallStart})
	tr.add(orca.Event{Kind: orca.EvtTurnCompleted})
	tr.add(orca.Event{Kind: orca.EvtAgentExited})

	got := tr.snapshot(0, []orca.EventKind{orca.EvtToolCallStart, orca.EvtAgentExited})
	if len(got) != 2 {
		t.Fatalf("kind filter: want 2, got %d", len(got))
	}
}

func TestTranscript_LimitTakesNewest(t *testing.T) {
	tr := newTranscript(10)
	for i := 0; i < 6; i++ {
		tr.add(orca.Event{Kind: orca.EvtAgentReady, Payload: map[string]any{"i": i}})
	}
	got := tr.snapshot(3, nil)
	if len(got) != 3 {
		t.Fatalf("limit: want 3, got %d", len(got))
	}
	p, _ := got[2].Payload.(map[string]any)
	if int(p["i"].(int)) != 5 {
		t.Fatalf("last item should be newest (i=5), got %v", p["i"])
	}
}

func TestTranscript_EmptyAgentReturnsNil(t *testing.T) {
	sup, _, _, _, done := budgetHarness(t)
	defer done()
	got := sup.Transcript("nobody", 0, nil)
	if got != nil {
		t.Fatalf("expected nil for unknown agent, got %v", got)
	}
}

func TestTranscript_CollectorCapturesEvents(t *testing.T) {
	sup, rt, _, _, done := budgetHarness(t)
	defer done()

	if _, err := sup.Spawn(context.Background(), orca.AgentSpec{ID: "a", Runtime: "fake"}); err != nil {
		t.Fatal(err)
	}
	// Let the collector pick up AgentSpawned + AgentReady.
	time.Sleep(80 * time.Millisecond)

	rt.Session("a").Emit(orca.Event{Kind: orca.EvtToolCallStart, AgentID: "a"})
	rt.Session("a").Emit(orca.Event{Kind: orca.EvtTurnCompleted, AgentID: "a"})
	time.Sleep(80 * time.Millisecond)

	got := sup.Transcript("a", 0, nil)
	kinds := map[orca.EventKind]bool{}
	for _, e := range got {
		kinds[e.Kind] = true
	}
	// FakeRuntime emits AgentReady on session start (not AgentSpawned —
	// that's emitted by the claudecode adapter only).
	for _, want := range []orca.EventKind{
		orca.EvtAgentReady,
		orca.EvtToolCallStart, orca.EvtTurnCompleted,
	} {
		if !kinds[want] {
			t.Errorf("transcript missing kind %s; saw kinds=%v", want, kinds)
		}
	}
}

func TestTranscript_TokenChunkSkipped(t *testing.T) {
	sup, rt, _, _, done := budgetHarness(t)
	defer done()

	if _, err := sup.Spawn(context.Background(), orca.AgentSpec{ID: "a", Runtime: "fake"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)

	for i := 0; i < 5; i++ {
		rt.Session("a").Emit(orca.Event{Kind: orca.EvtTokenChunk, AgentID: "a"})
	}
	time.Sleep(60 * time.Millisecond)

	got := sup.Transcript("a", 0, nil)
	for _, e := range got {
		if e.Kind == orca.EvtTokenChunk {
			t.Fatal("TokenChunk should not be recorded in transcript")
		}
	}
}
