package bridge

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/atterpac/orca/pkg/orca"
)

type mockPublisher struct{ got []orca.Message }

func (p *mockPublisher) Publish(ctx context.Context, m orca.Message) error {
	p.got = append(p.got, m)
	return nil
}

func TestBridge_SendDeliversToOutbox(t *testing.T) {
	rt := New(nil)
	sess, err := rt.Start(context.Background(), orca.AgentSpec{ID: "slack"})
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal("hello")
	if err := sess.Send(context.Background(), orca.Message{From: "arch", To: "slack", Body: body}); err != nil {
		t.Fatal(err)
	}

	attached, err := rt.Attach("slack")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-attached.Outbox():
		if m.From != "arch" {
			t.Fatalf("from=%s", m.From)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("outbox empty")
	}
}

func TestBridge_DeliverPublishesViaPublisher(t *testing.T) {
	pub := &mockPublisher{}
	rt := New(pub)
	sess, _ := rt.Start(context.Background(), orca.AgentSpec{ID: "slack"})
	attached, _ := rt.Attach("slack")

	_ = sess // ensure same underlying session
	body, _ := json.Marshal("user said hi")
	if err := attached.Deliver(context.Background(), orca.Message{From: "slack", To: "arch", Body: body}); err != nil {
		t.Fatal(err)
	}
	if len(pub.got) != 1 || pub.got[0].To != "arch" {
		t.Fatalf("publisher got %v", pub.got)
	}
}

func TestBridge_CloseDisallowsFurtherOps(t *testing.T) {
	rt := New(nil)
	sess, _ := rt.Start(context.Background(), orca.AgentSpec{ID: "slack"})
	_ = sess.Close()
	if err := sess.Send(context.Background(), orca.Message{To: "slack"}); err == nil {
		t.Fatal("Send should fail after Close")
	}
	attached, _ := rt.Attach("slack")
	if err := attached.Deliver(context.Background(), orca.Message{}); err == nil {
		t.Fatal("Deliver should fail after Close")
	}
}

func TestBridge_EmitsAgentReadyAndExit(t *testing.T) {
	rt := New(nil)
	sess, _ := rt.Start(context.Background(), orca.AgentSpec{ID: "slack"})
	ch, _ := sess.Events(context.Background())

	select {
	case e := <-ch:
		if e.Kind != orca.EvtAgentReady {
			t.Fatalf("first event kind=%s", e.Kind)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("no ready event")
	}

	_ = sess.Close()
	for e := range ch {
		if e.Kind == orca.EvtAgentExited {
			return
		}
	}
	t.Fatal("no exit event")
}
