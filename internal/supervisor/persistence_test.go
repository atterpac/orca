package supervisor

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/atterpac/orca/internal/bus"
	"github.com/atterpac/orca/internal/events"
	"github.com/atterpac/orca/internal/storage/sqlite"
	"github.com/atterpac/orca/internal/testutil"
	"github.com/atterpac/orca/pkg/orca"
)

// tmpGitRepo is re-declared in task_test.go; persistence tests reuse
// its lightweight pattern via an inline helper here to avoid coupling.
func tmpRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return dir
}

// TestPersistence_TaskSurvivesRestart stands up a supervisor with a
// sqlite store, opens and closes tasks, tears down the supervisor,
// reopens the store from the same DB path into a fresh supervisor, and
// confirms the tasks reload. This is the crash-recovery guarantee the
// feature exists to provide.
func TestPersistence_TaskSurvivesRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "orca.db")
	repo := tmpRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// --- first lifetime ------------------------------------------------
	store1, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	sup1 := New(bus.NewInProc(), events.NewBus(64))
	sup1.SetStore(store1)
	sup1.RegisterRuntime(testutil.NewRuntime())

	openTask, err := sup1.OpenTask(orca.OpenTaskRequest{RepoRoot: repo})
	if err != nil {
		t.Fatalf("open task: %v", err)
	}
	closingTask, err := sup1.OpenTask(orca.OpenTaskRequest{RepoRoot: repo})
	if err != nil {
		t.Fatalf("open task 2: %v", err)
	}
	if _, err := sup1.CloseTask(closingTask.ID, false); err != nil {
		t.Fatalf("close: %v", err)
	}

	sup1.Shutdown()
	if err := store1.Close(); err != nil {
		t.Fatalf("close store 1: %v", err)
	}

	// --- second lifetime: reload from disk -----------------------------
	store2, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store2.Close()

	sup2 := New(bus.NewInProc(), events.NewBus(64))
	sup2.SetStore(store2)
	sup2.RegisterRuntime(testutil.NewRuntime())
	if err := sup2.LoadTasksFromStore(ctx); err != nil {
		t.Fatalf("load: %v", err)
	}
	defer sup2.Shutdown()

	got, ok := sup2.GetTask(openTask.ID)
	if !ok {
		t.Fatalf("open task did not reload")
	}
	if got.Phase != "open" {
		t.Fatalf("phase lost across restart: %s", got.Phase)
	}

	closedGot, ok := sup2.GetTask(closingTask.ID)
	if !ok {
		t.Fatalf("closed task did not reload")
	}
	if closedGot.Phase != "closed" || closedGot.ClosedAt == nil {
		t.Fatalf("closed task state lost: %+v", closedGot)
	}
}

// TestPersistence_SpawnWithTaskPersists exercises the supervisor.go
// path where Spawn appends to Task.Agents when the spec carries
// TaskID — the persist hook there must fire.
func TestPersistence_SpawnWithTaskPersists(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "orca.db")
	repo := tmpRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	store, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	sup := New(bus.NewInProc(), events.NewBus(64))
	sup.SetStore(store)
	sup.RegisterRuntime(testutil.NewRuntime())
	defer sup.Shutdown()

	task, err := sup.OpenTask(orca.OpenTaskRequest{RepoRoot: repo})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Spawn(ctx, orca.AgentSpec{
		ID: "worker-1", Runtime: "fake", TaskID: task.ID,
	}); err != nil {
		t.Fatalf("spawn: %v", err)
	}

	// Read back directly from the store to confirm the Agents append
	// was persisted, not just applied in-memory.
	persisted, ok, err := store.Tasks().Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("task not in store")
	}
	found := false
	for _, a := range persisted.Agents {
		if a == "worker-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Spawn did not persist Agents append: %v", persisted.Agents)
	}
}

// TestPersistence_NilStoreNoOps confirms SetStore(nil) keeps the
// supervisor fully functional — persistence is opt-in and the
// zero-store path must stay hot.
func TestPersistence_NilStoreNoOps(t *testing.T) {
	repo := tmpRepo(t)
	sup := New(bus.NewInProc(), events.NewBus(64))
	sup.RegisterRuntime(testutil.NewRuntime())
	defer sup.Shutdown()

	// No SetStore call — store is nil.
	if _, err := sup.OpenTask(orca.OpenTaskRequest{RepoRoot: repo}); err != nil {
		t.Fatalf("open task without store: %v", err)
	}
	if err := sup.LoadTasksFromStore(context.Background()); err != nil {
		t.Fatalf("LoadTasksFromStore should no-op with nil store: %v", err)
	}
}
