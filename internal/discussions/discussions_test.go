package discussions

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/atterpac/orca/pkg/orca"
)

type capEvents struct {
	mu sync.Mutex
	es []orca.Event
}

func (c *capEvents) Emit(e orca.Event) {
	c.mu.Lock()
	c.es = append(c.es, e)
	c.mu.Unlock()
}
func (c *capEvents) snapshot() []orca.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]orca.Event, len(c.es))
	copy(cp, c.es)
	return cp
}

func TestTouch_OpensThenUpdates(t *testing.T) {
	ev := &capEvents{}
	r := NewForTest(ev)

	d1 := r.Touch(TouchInfo{
		ID: "slack:C:1", BridgeAgentID: "slack",
		Channel: "C", ResponderName: "alice",
		Participant: "secretary",
	})
	if d1.Status != orca.DiscOpen || d1.MessageCount != 1 {
		t.Fatalf("first touch: %+v", d1)
	}
	d2 := r.Touch(TouchInfo{
		ID: "slack:C:1", Participant: "architect",
	})
	if d2.MessageCount != 2 {
		t.Fatalf("second touch should bump count to 2; got %d", d2.MessageCount)
	}
	if len(d2.Participants) != 2 {
		t.Fatalf("participants should accumulate: %v", d2.Participants)
	}

	es := ev.snapshot()
	var opened, msg int
	for _, e := range es {
		switch e.Kind {
		case "DiscussionOpened":
			opened++
		case "DiscussionMessage":
			msg++
		}
	}
	if opened != 1 || msg != 2 {
		t.Fatalf("expected 1 opened + 2 message events; got opened=%d msg=%d", opened, msg)
	}
}

func TestClose_Explicit(t *testing.T) {
	ev := &capEvents{}
	r := NewForTest(ev)
	r.Touch(TouchInfo{ID: "x"})

	if err := r.Close("x"); err != nil {
		t.Fatal(err)
	}
	d, _ := r.Get("x")
	if d.Status != orca.DiscClosed {
		t.Fatalf("status=%s", d.Status)
	}
	// Idempotent — closing twice is fine.
	if err := r.Close("x"); err != nil {
		t.Fatalf("re-close should noop, got %v", err)
	}
}

func TestSweep_ExpiresInactive(t *testing.T) {
	ev := &capEvents{}
	r := NewForTest(ev)
	r.Limits.InactivityTimeout = 50 * time.Millisecond

	r.Touch(TouchInfo{ID: "stale"})
	r.Touch(TouchInfo{ID: "fresh"})
	time.Sleep(80 * time.Millisecond)

	// Touch "fresh" again so it's not expired.
	r.Touch(TouchInfo{ID: "fresh"})

	r.SweepForTest(context.Background())

	if d, _ := r.Get("stale"); d.Status != orca.DiscExpired {
		t.Fatalf("stale: status=%s", d.Status)
	}
	if d, _ := r.Get("fresh"); d.Status != orca.DiscOpen {
		t.Fatalf("fresh should still be open: %s", d.Status)
	}

	es := ev.snapshot()
	var closed int
	for _, e := range es {
		if e.Kind == "DiscussionClosed" {
			closed++
		}
	}
	if closed != 1 {
		t.Fatalf("expected 1 close event from sweep; got %d", closed)
	}
}

func TestList_NewestFirst(t *testing.T) {
	r := NewForTest(&capEvents{})
	r.Touch(TouchInfo{ID: "a"})
	time.Sleep(5 * time.Millisecond)
	r.Touch(TouchInfo{ID: "b"})
	time.Sleep(5 * time.Millisecond)
	r.Touch(TouchInfo{ID: "c"})

	got := r.List()
	if len(got) != 3 {
		t.Fatalf("list len=%d", len(got))
	}
	if got[0].ID != "c" || got[2].ID != "a" {
		t.Fatalf("not sorted newest-first: %v", []string{got[0].ID, got[1].ID, got[2].ID})
	}
}

func TestStop_Idempotent(t *testing.T) {
	r := New(&capEvents{})
	r.Stop()
	r.Stop() // must not panic
}
