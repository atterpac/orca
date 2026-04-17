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

// multiRuntime wraps a FakeRuntime but flips MultiSession=true so the
// supervisor wraps it with our per-correlation multiSession.
type multiRuntime struct {
	inner *testutil.FakeRuntime
}

func (r *multiRuntime) Name() string { return "fake-multi" }
func (r *multiRuntime) Capabilities() orca.RuntimeCaps {
	caps := r.inner.Capabilities()
	caps.MultiSession = true
	return caps
}
func (r *multiRuntime) Start(ctx context.Context, spec orca.AgentSpec) (orca.Session, error) {
	return r.inner.Start(ctx, spec)
}

func newMultiHarness(t *testing.T) (*Supervisor, *testutil.FakeRuntime, func()) {
	t.Helper()
	b := bus.NewInProc()
	ev := events.NewBus(64)
	fake := testutil.NewRuntime()
	rt := &multiRuntime{inner: fake}
	sup := New(b, ev)
	sup.RegisterRuntime(rt)
	return sup, fake, func() { sup.Shutdown() }
}

func TestMultiSession_RoutesByCorrelation(t *testing.T) {
	sup, fake, done := newMultiHarness(t)
	defer done()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := sup.Spawn(ctx, orca.AgentSpec{ID: "arch", Runtime: "fake-multi"}); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	// Let deliverInbox's subscribe settle before we publish.
	time.Sleep(50 * time.Millisecond)

	// Two messages on different correlations should spawn two sub-sessions.
	// Third on task-A should reuse the live sub.
	sends := []orca.Message{
		{From: "tester", To: "arch", Kind: orca.KindRequest, CorrelationID: "task-A"},
		{From: "tester", To: "arch", Kind: orca.KindRequest, CorrelationID: "task-B"},
		{From: "tester", To: "arch", Kind: orca.KindRequest, CorrelationID: "task-A"},
	}
	for i, m := range sends {
		if err := sup.DispatchDirect(ctx, m); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	_ = fake // FakeRuntime.Session() returns the last Start'd session;
	// the sub count lives on the wrapper, so we inspect that directly.

	sup.mu.RLock()
	rec := sup.agents["arch"]
	sup.mu.RUnlock()
	ms, ok := rec.session.(*multiSession)
	if !ok {
		t.Fatalf("expected multiSession, got %T", rec.session)
	}

	// Poll subs count to 2.
	ok2 := false
	for range 60 {
		ms.mu.Lock()
		n := len(ms.subs)
		ms.mu.Unlock()
		if n == 2 {
			ok2 = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ok2 {
		ms.mu.Lock()
		n := len(ms.subs)
		ms.mu.Unlock()
		t.Fatalf("expected 2 subs, got %d", n)
	}
}

// TestMultiSession_ConcurrentSendReap exercises the path where the idle
// sweeper races concurrent Send() callers. Before the ensure() refactor a
// sweeper that nil'd sub.live between ensure returning and Send dereffing
// caused a nil-pointer panic under -race. This test must stay clean.
func TestMultiSession_ConcurrentSendReap(t *testing.T) {
	sup, _, done := newMultiHarness(t)
	defer done()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if _, err := sup.Spawn(ctx, orca.AgentSpec{ID: "arch", Runtime: "fake-multi"}); err != nil {
		t.Fatalf("spawn: %v", err)
	}

	sup.mu.RLock()
	rec := sup.agents["arch"]
	sup.mu.RUnlock()
	ms := rec.session.(*multiSession)

	// Aggressive reap: anything idle >1ms gets closed.
	ms.mu.Lock()
	ms.idleTTL = time.Millisecond
	ms.mu.Unlock()

	// Run a hot reap loop against concurrent sends on varied correlations.
	stop := make(chan struct{})
	reaperDone := make(chan struct{})
	go func() {
		defer close(reaperDone)
		for {
			select {
			case <-stop:
				return
			default:
				ms.reapIdle()
			}
		}
	}()

	corrs := []string{"c1", "c2", "c3", "c4"}
	for i := range 200 {
		corr := corrs[i%len(corrs)]
		err := ms.Send(ctx, orca.Message{
			From: "tester", To: "arch", Kind: orca.KindRequest, CorrelationID: corr,
		})
		// Send may legitimately fail if the underlying fake closed between
		// ensure and delivery — we only care that no panic occurs.
		_ = err
	}
	close(stop)
	<-reaperDone
}

func TestMultiSession_DropOnTaskClose(t *testing.T) {
	sup, _, done := newMultiHarness(t)
	defer done()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := sup.Spawn(ctx, orca.AgentSpec{ID: "arch", Runtime: "fake-multi"}); err != nil {
		t.Fatalf("spawn: %v", err)
	}

	// Drive traffic for corr=task-X.
	if err := sup.DispatchDirect(ctx, orca.Message{
		From: "tester", To: "arch", Kind: orca.KindRequest, CorrelationID: "task-X",
	}); err != nil {
		t.Fatal(err)
	}

	sup.mu.RLock()
	rec := sup.agents["arch"]
	sup.mu.RUnlock()
	ms := rec.session.(*multiSession)

	// Wait for sub to materialize.
	for range 60 {
		ms.mu.Lock()
		_, has := ms.subs["task-X"]
		ms.mu.Unlock()
		if has {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	ms.dropSub("task-X")

	ms.mu.Lock()
	_, has := ms.subs["task-X"]
	ms.mu.Unlock()
	if has {
		t.Fatalf("dropSub did not remove entry")
	}
}
