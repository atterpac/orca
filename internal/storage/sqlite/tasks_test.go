package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/atterpac/orca/pkg/orca"
)

// openTestStore creates a fresh sqlite file under t.TempDir so each
// test gets an isolated migrated database.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "orca.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestTasks_UpsertAndGet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	repo := s.Tasks()

	opened := time.Now().UTC().Truncate(time.Microsecond)
	in := orca.Task{
		ID:           "task-1",
		Phase:        "open",
		RepoRoot:     "/tmp/r",
		WorktreePath: "/tmp/r-wt",
		ArtifactDir:  "/tmp/r/.orca/task-1",
		Branch:       "orca/task-1",
		OpenedAt:     opened,
		Agents:       []string{"arch", "impl"},
	}
	if err := repo.Upsert(ctx, in); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, ok, err := repo.Get(ctx, "task-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !ok {
		t.Fatal("task should exist")
	}
	if got.Phase != "open" || got.RepoRoot != "/tmp/r" {
		t.Fatalf("mismatched fields: %+v", got)
	}
	if len(got.Agents) != 2 || got.Agents[0] != "arch" || got.Agents[1] != "impl" {
		t.Fatalf("agents round-trip broken: %v", got.Agents)
	}
	if !got.OpenedAt.Equal(opened) {
		t.Fatalf("opened_at round-trip: want %v, got %v", opened, got.OpenedAt)
	}
}

// TestTasks_UpsertIsIdempotent confirms a second Upsert with the same
// ID updates phase + agents without creating a duplicate row.
func TestTasks_UpsertIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	repo := s.Tasks()

	base := orca.Task{ID: "t", Phase: "open", RepoRoot: "/r", OpenedAt: time.Now().UTC()}
	if err := repo.Upsert(ctx, base); err != nil {
		t.Fatal(err)
	}

	closed := time.Now().UTC()
	updated := base
	updated.Phase = "closed"
	updated.ClosedAt = &closed
	updated.Agents = []string{"arch"}
	if err := repo.Upsert(ctx, updated); err != nil {
		t.Fatal(err)
	}

	all, err := repo.ListAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 row after idempotent upsert, got %d", len(all))
	}
	if all[0].Phase != "closed" || all[0].ClosedAt == nil {
		t.Fatalf("second upsert did not take: %+v", all[0])
	}
}

// TestTasks_ListOpenFiltersClosed guards the WHERE phase != 'closed'
// clause — closed tasks must not show up in the open-only listing.
func TestTasks_ListOpenFiltersClosed(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	repo := s.Tasks()

	now := time.Now().UTC()
	_ = repo.Upsert(ctx, orca.Task{ID: "open-a", Phase: "open", RepoRoot: "/r", OpenedAt: now})
	_ = repo.Upsert(ctx, orca.Task{ID: "closed-a", Phase: "closed", RepoRoot: "/r", OpenedAt: now, ClosedAt: &now})

	open, err := repo.ListOpen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].ID != "open-a" {
		t.Fatalf("ListOpen returned %v", open)
	}

	all, err := repo.ListAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("ListAll should include closed; got %d rows", len(all))
	}
}

// TestTasks_ReopenAfterRestart exercises the core persistence guarantee:
// write, close, reopen the DB file, read. The rows must survive the
// close/open cycle — this is what lets a crashed daemon recover.
func TestTasks_ReopenAfterRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "orca.db")
	ctx := context.Background()

	s1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	if err := s1.Tasks().Upsert(ctx, orca.Task{
		ID: "persisted", Phase: "open", RepoRoot: "/r", OpenedAt: time.Now().UTC(),
		Agents: []string{"arch"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}

	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	defer s2.Close()

	got, ok, err := s2.Tasks().Get(ctx, "persisted")
	if err != nil || !ok {
		t.Fatalf("row lost across restart: ok=%v err=%v", ok, err)
	}
	if got.Agents[0] != "arch" {
		t.Fatalf("agents list lost across restart: %v", got.Agents)
	}
}
