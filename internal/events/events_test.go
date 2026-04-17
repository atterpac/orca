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
