package events

import (
	"context"
	"testing"
	"time"

	"github.com/atterpac/orca/pkg/orca"
)

func TestEmitFanout(t *testing.T) {
	b := NewBus(16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a, _ := b.Subscribe(ctx, Filter{})
	b2, _ := b.Subscribe(ctx, Filter{AgentID: "alice"})

	b.Emit(orca.Event{Kind: orca.EvtAgentReady, AgentID: "alice"})
	b.Emit(orca.Event{Kind: orca.EvtAgentReady, AgentID: "bob"})

	// First subscriber takes both.
	got := 0
	timeout := time.After(100 * time.Millisecond)
loop1:
	for {
		select {
		case <-a:
			got++
			if got == 2 {
				break loop1
			}
		case <-timeout:
			t.Fatalf("subscriber a got only %d of 2", got)
		}
	}

	// Filtered subscriber takes only alice.
	select {
	case e := <-b2:
		if e.AgentID != "alice" {
			t.Fatalf("filtered got %s", e.AgentID)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("filtered subscriber got nothing")
	}
	select {
	case e := <-b2:
		t.Fatalf("filtered got extra: %+v", e)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestEmitDropsOnFullSubscriber(t *testing.T) {
	b := NewBus(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, _ := b.Subscribe(ctx, Filter{})
	// Fill the 1-slot buffer, then emit once more — must not block publisher.
	b.Emit(orca.Event{Kind: orca.EvtAgentReady})
	done := make(chan struct{})
	go func() {
		b.Emit(orca.Event{Kind: orca.EvtAgentReady})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("publisher blocked on full subscriber")
	}
	<-ch
}

// TestEmit_StuckSubscriberDoesNotStarveOthers confirms per-subscriber
// backpressure isolation: one stuck subscriber silently drops events,
// other subscribers keep getting every event. Violating this would let
// a single slow consumer (e.g. a remote TUI over a choked network)
// block the supervisor's delivery loop.
func TestEmit_StuckSubscriberDoesNotStarveOthers(t *testing.T) {
	b := NewBus(0) // 0 → default buffer, but we override per-sub via Subscribe call semantics.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The stuck subscriber never drains. The fast subscriber drains
	// eagerly.
	stuck, _ := b.Subscribe(ctx, Filter{})
	fast, _ := b.Subscribe(ctx, Filter{})

	const n = 500
	fastGot := make(chan int, 1)
	go func() {
		seen := 0
		for range fast {
			seen++
			if seen == n {
				fastGot <- seen
				return
			}
		}
		fastGot <- seen
	}()

	for i := 0; i < n; i++ {
		b.Emit(orca.Event{Kind: orca.EvtAgentReady})
	}

	select {
	case got := <-fastGot:
		if got != n {
			t.Fatalf("fast subscriber saw %d/%d events despite stuck peer", got, n)
		}
	case <-time.After(time.Second):
		t.Fatal("fast subscriber starved by stuck peer")
	}

	// Stuck subscriber should have received at most its buffer worth.
	// We don't assert exact count — whatever landed before the buffer
	// filled is acceptable — just that it didn't block the emit loop.
	drained := 0
drain:
	for {
		select {
		case <-stuck:
			drained++
		default:
			break drain
		}
	}
	if drained >= n {
		t.Fatalf("stuck subscriber somehow got all %d events; backpressure drop not working", n)
	}
}

func TestKindFilter(t *testing.T) {
	b := NewBus(16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, _ := b.Subscribe(ctx, Filter{Kinds: []orca.EventKind{orca.EvtTurnCompleted}})

	b.Emit(orca.Event{Kind: orca.EvtAgentReady})
	b.Emit(orca.Event{Kind: orca.EvtTurnCompleted})
	b.Emit(orca.Event{Kind: orca.EvtAgentExited})

	select {
	case e := <-ch:
		if e.Kind != orca.EvtTurnCompleted {
			t.Fatalf("leaked kind=%s", e.Kind)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("no matching event")
	}
	select {
	case e := <-ch:
		t.Fatalf("leaked extra: %+v", e)
	case <-time.After(20 * time.Millisecond):
	}
}
