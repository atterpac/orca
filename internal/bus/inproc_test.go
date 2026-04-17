package bus

import (
	"context"
	"testing"
	"time"

	"github.com/atterpac/orca/pkg/orca"
)

func TestPublishDirect(t *testing.T) {
	b := NewInProc()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	ch, _ := b.Subscribe(ctx, Filter{AgentID: "bob"})
	if err := b.Publish(ctx, orca.Message{From: "alice", To: "bob", Kind: orca.KindRequest}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case m := <-ch:
		if m.From != "alice" || m.To != "bob" {
			t.Fatalf("wrong routing: %+v", m)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("no delivery")
	}
}

func TestPublishFiltersOutMismatch(t *testing.T) {
	b := NewInProc()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	ch, _ := b.Subscribe(ctx, Filter{AgentID: "bob"})
	_ = b.Publish(ctx, orca.Message{From: "alice", To: "carol", Kind: orca.KindRequest})

	select {
	case m := <-ch:
		t.Fatalf("should not have delivered: %+v", m)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestRequestResponseCorrelation(t *testing.T) {
	b := NewInProc()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	responderCh, _ := b.Subscribe(ctx, Filter{AgentID: "responder"})
	replyReceived := make(chan orca.Message, 1)

	go func() {
		req, err := b.Request(ctx, orca.Message{From: "asker", To: "responder"})
		if err != nil {
			t.Errorf("request: %v", err)
			return
		}
		replyReceived <- req
	}()

	select {
	case incoming := <-responderCh:
		reply := orca.Message{
			From:          "responder",
			To:            incoming.From,
			Kind:          orca.KindResponse,
			CorrelationID: incoming.CorrelationID,
		}
		if err := b.Publish(ctx, reply); err != nil {
			t.Fatalf("publish reply: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("responder never saw request")
	}

	select {
	case got := <-replyReceived:
		if got.Kind != orca.KindResponse {
			t.Fatalf("expected response, got %s", got.Kind)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("asker did not receive reply")
	}
}

func TestTTLExpiry(t *testing.T) {
	b := NewInProc()
	ctx := context.Background()
	err := b.Publish(ctx, orca.Message{To: "bob", TTL: 1})
	if err != ErrTTLExpired {
		t.Fatalf("expected ErrTTLExpired, got %v", err)
	}
}

func TestSubscribeCancelDrains(t *testing.T) {
	b := NewInProc()
	ctx, cancel := context.WithCancel(context.Background())
	ch, cancelSub := b.Subscribe(ctx, Filter{AgentID: "bob"})
	cancelSub()
	cancel()
	// ch should be closed
	_, ok := <-ch
	if ok {
		t.Fatal("subscribe channel not closed after cancel")
	}
}
