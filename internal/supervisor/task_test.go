package supervisor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/atterpac/orca/internal/bus"
	"github.com/atterpac/orca/internal/events"
	"github.com/atterpac/orca/internal/testutil"
	"github.com/atterpac/orca/pkg/orca"
)

// tmpGitRepo creates a temporary git repo with one commit and returns its
// absolute path. t.TempDir cleans up on teardown.
func tmpGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", abs}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=orca", "GIT_AUTHOR_EMAIL=orca@test",
			"GIT_COMMITTER_NAME=orca", "GIT_COMMITTER_EMAIL=orca@test",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(abs, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "initial")
	return abs
}

func taskHarness(t *testing.T) (*Supervisor, *testutil.FakeRuntime, *events.Bus, func()) {
	t.Helper()
	b := bus.NewInProc()
	ev := events.NewBus(64)
	rt := testutil.NewRuntime()
	sup := New(b, ev)
	sup.RegisterRuntime(rt)
	return sup, rt, ev, func() { sup.Shutdown() }
}

func TestOpenTask_CreatesWorktreeAndSymlink(t *testing.T) {
	sup, _, _, done := taskHarness(t)
	defer done()

	repo := tmpGitRepo(t)
	task, err := sup.OpenTask(orca.OpenTaskRequest{RepoRoot: repo})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	if task.ID == "" {
		t.Fatal("expected generated task id")
	}
	if task.Phase != "open" {
		t.Fatalf("phase=%s want open", task.Phase)
	}
	// Artifact dir exists under main repo.
	if st, err := os.Stat(task.ArtifactDir); err != nil || !st.IsDir() {
		t.Fatalf("artifact dir missing: %v", err)
	}
	// Worktree exists.
	if st, err := os.Stat(task.WorktreePath); err != nil || !st.IsDir() {
		t.Fatalf("worktree missing: %v", err)
	}
	// Worktree's .orca/<id>/ is a symlink pointing to the main artifact dir.
	linkPath := filepath.Join(task.WorktreePath, ".orca", task.ID)
	target, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("symlink missing: %v", err)
	}
	// Resolve both to absolute and compare.
	targetAbs, _ := filepath.Abs(target)
	wantAbs, _ := filepath.Abs(task.ArtifactDir)
	if targetAbs != wantAbs {
		t.Fatalf("symlink target=%s want=%s", targetAbs, wantAbs)
	}
}

func TestOpenTask_NonGitRepoSkipsWorktree(t *testing.T) {
	sup, _, _, done := taskHarness(t)
	defer done()

	plain := t.TempDir()
	task, err := sup.OpenTask(orca.OpenTaskRequest{RepoRoot: plain})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	if task.WorktreePath != "" {
		t.Fatalf("expected no worktree for non-git repo; got %s", task.WorktreePath)
	}
	if _, err := os.Stat(task.ArtifactDir); err != nil {
		t.Fatalf("artifact dir still expected: %v", err)
	}
}

func TestOpenTask_DuplicateIDErrors(t *testing.T) {
	sup, _, _, done := taskHarness(t)
	defer done()

	repo := tmpGitRepo(t)
	if _, err := sup.OpenTask(orca.OpenTaskRequest{ID: "fixed", RepoRoot: repo}); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.OpenTask(orca.OpenTaskRequest{ID: "fixed", RepoRoot: repo}); err == nil {
		t.Fatal("expected duplicate task open to fail")
	}
}

func TestCloseTask_RemovesWorktreeKeepsArtifacts(t *testing.T) {
	sup, _, _, done := taskHarness(t)
	defer done()

	repo := tmpGitRepo(t)
	task, err := sup.OpenTask(orca.OpenTaskRequest{RepoRoot: repo})
	if err != nil {
		t.Fatal(err)
	}

	// Write an artifact so we can confirm it survives.
	artifact := filepath.Join(task.ArtifactDir, "PLAN.md")
	if err := os.WriteFile(artifact, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := sup.CloseTask(task.ID, true); err != nil {
		t.Fatal(err)
	}

	// Worktree gone.
	if _, err := os.Stat(task.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("expected worktree removed, stat err=%v", err)
	}
	// Artifact survives.
	if _, err := os.Stat(artifact); err != nil {
		t.Fatalf("artifact should survive close: %v", err)
	}
	// Task no longer in open list (but still in registry — see
	// TestCloseTask_KeepsTaskInRegistryForTrace).
	if len(sup.ListTasks()) != 0 {
		t.Fatalf("ListTasks should be empty after close, got %v", sup.ListTasks())
	}
}

func TestSpawn_WithTaskID_UsesWorktreeAsWorkdir(t *testing.T) {
	sup, rt, _, done := taskHarness(t)
	defer done()

	repo := tmpGitRepo(t)
	task, err := sup.OpenTask(orca.OpenTaskRequest{RepoRoot: repo})
	if err != nil {
		t.Fatal(err)
	}

	info, err := sup.Spawn(context.Background(), orca.AgentSpec{
		ID: "impl", Runtime: "fake", TaskID: task.ID,
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if info.Spec.Workdir != task.WorktreePath {
		t.Fatalf("workdir not inherited from task: got %q want %q", info.Spec.Workdir, task.WorktreePath)
	}
	_ = rt // harness hook

	// Task should now list the agent as a participant.
	got, _ := sup.GetTask(task.ID)
	if len(got.Agents) != 1 || got.Agents[0] != "impl" {
		t.Fatalf("task agents = %v", got.Agents)
	}
}

func TestSpawn_WithUnknownTaskID_Fails(t *testing.T) {
	sup, _, _, done := taskHarness(t)
	defer done()
	_, err := sup.Spawn(context.Background(), orca.AgentSpec{
		ID: "x", Runtime: "fake", TaskID: "ghost",
	})
	if err == nil {
		t.Fatal("expected unknown task id to error")
	}
}

func TestSessionID_StoredOnAgentReady(t *testing.T) {
	sup, rt, _, done := taskHarness(t)
	defer done()

	if _, err := sup.Spawn(context.Background(), orca.AgentSpec{ID: "a", Runtime: "fake"}); err != nil {
		t.Fatal(err)
	}
	// FakeSession emits AgentReady automatically without session_id. Push one
	// that mimics the claude-code adapter's behavior.
	time.Sleep(20 * time.Millisecond)
	rt.Session("a").Emit(orca.Event{
		Kind: orca.EvtAgentReady,
		Payload: map[string]any{
			"session_id": "cc-sess-abc123",
		},
	})
	// Give the supervisor pump a chance to apply the event.
	deadline := time.After(300 * time.Millisecond)
	for {
		info, _ := sup.Get("a")
		if info.SessionID == "cc-sess-abc123" {
			return
		}
		select {
		case <-deadline:
			info, _ := sup.Get("a")
			t.Fatalf("session_id not captured; info=%+v", info)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestOpenTask_RejectsNonOrchestratorRoles(t *testing.T) {
	// Regression: a communicator (or worker / reviewer) calling open_task
	// must be rejected at the daemon. Only orchestrators may open tasks.
	// Without this, a literal-minded communicator might "be helpful" and
	// pre-open a task before delegating, resulting in a duplicate task
	// being created downstream by the orchestrator itself.
	sup, _, _, done := taskHarness(t)
	defer done()
	repo := tmpGitRepo(t)

	for _, role := range []string{"communicator", "worker", "reviewer"} {
		id := "agent-" + role
		if _, err := sup.Spawn(context.Background(), orca.AgentSpec{
			ID:           id,
			Runtime:      "fake",
			RoleTemplate: role,
		}); err != nil {
			t.Fatalf("spawn %s: %v", id, err)
		}
		_, err := sup.OpenTask(orca.OpenTaskRequest{
			RepoRoot: repo,
			OpenedBy: id,
			Summary:  "should be rejected",
		})
		if err == nil {
			t.Errorf("role=%s: open_task should have been rejected", role)
		}
	}

	// Sanity: orchestrator IS allowed.
	if _, err := sup.Spawn(context.Background(), orca.AgentSpec{
		ID: "arch", Runtime: "fake", RoleTemplate: "orchestrator",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.OpenTask(orca.OpenTaskRequest{
		RepoRoot: repo, OpenedBy: "arch", Summary: "ok",
	}); err != nil {
		t.Fatalf("orchestrator must be allowed: %v", err)
	}
}

func TestCloseTask_KeepsTaskInRegistryForTrace(t *testing.T) {
	sup, _, _, done := taskHarness(t)
	defer done()
	repo := tmpGitRepo(t)

	task, err := sup.OpenTask(orca.OpenTaskRequest{RepoRoot: repo})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sup.CloseTask(task.ID, true); err != nil {
		t.Fatal(err)
	}

	// Closed tasks must remain queryable.
	got, ok := sup.GetTask(task.ID)
	if !ok {
		t.Fatal("GetTask should return the closed task")
	}
	if got.Phase != "closed" {
		t.Fatalf("phase=%s want closed", got.Phase)
	}
	// ListTasks filters open-only.
	if len(sup.ListTasks()) != 0 {
		t.Fatal("ListTasks should not include closed tasks")
	}
	// ListAllTasks includes closed.
	if len(sup.ListAllTasks()) != 1 {
		t.Fatal("ListAllTasks should include closed tasks")
	}
	// Idempotent close.
	if _, err := sup.CloseTask(task.ID, false); err != nil {
		t.Fatalf("re-close should be idempotent: %v", err)
	}
}

func TestTaskTimeline_IncludesAllAgentsTouchingTask(t *testing.T) {
	// Regression: pre-spawned fleet agents that join a task via
	// correlation_id (rather than spec.TaskID) must show up in the
	// task timeline.
	sup, rt, _, done := taskHarness(t)
	defer done()
	repo := tmpGitRepo(t)

	// Two pre-spawned agents — neither has spec.TaskID set.
	if _, err := sup.Spawn(context.Background(), orca.AgentSpec{ID: "arch", Runtime: "fake"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Spawn(context.Background(), orca.AgentSpec{ID: "impl", Runtime: "fake"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)

	// Open a task as arch.
	task, err := sup.OpenTask(orca.OpenTaskRequest{RepoRoot: repo, OpenedBy: "arch"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)

	// Both agents emit events tagged with correlation_id=task_id.
	rt.Session("arch").Emit(orca.Event{
		Kind: orca.EvtMessageSent, AgentID: "arch",
		Payload: map[string]any{"to": "impl", "correlation_id": task.ID},
	})
	rt.Session("impl").Emit(orca.Event{
		Kind: orca.EvtToolCallStart, AgentID: "impl",
		Payload: map[string]any{"tool": "Edit", "correlation_id": task.ID},
	})
	time.Sleep(80 * time.Millisecond)

	timeline := sup.TaskTimeline(task.ID, 0)
	sawArch, sawImpl := false, false
	for _, e := range timeline {
		if e.AgentID == "arch" {
			sawArch = true
		}
		if e.AgentID == "impl" {
			sawImpl = true
		}
	}
	if !sawArch {
		t.Error("timeline missing events from arch (the task opener)")
	}
	if !sawImpl {
		t.Error("timeline missing events from impl (joined via correlation_id, not spec.TaskID)")
	}
}

func TestOpenTask_SwitchesAutoCorrelationToTaskID(t *testing.T) {
	// Regression: after open_task, the calling agent's auto-correlation
	// must point at the new task_id so subsequent report_* / send_message
	// calls thread under the task announcement, not the user's original
	// discussion thread.
	sup, _, _, done := taskHarness(t)
	defer done()
	repo := tmpGitRepo(t)

	// Spawn the architect-style agent.
	if _, err := sup.Spawn(context.Background(), orca.AgentSpec{ID: "arch", Runtime: "fake"}); err != nil {
		t.Fatal(err)
	}
	// Seed: pretend the architect just received a slack discussion message.
	sup.lastInboundCorr.Store("arch", "slack:C0XXX:T1")
	if got := sup.AutoCorrelationFor("arch"); got != "slack:C0XXX:T1" {
		t.Fatalf("seed failed: got %q", got)
	}

	// Architect opens a task. opened_by names itself.
	task, err := sup.OpenTask(orca.OpenTaskRequest{
		RepoRoot: repo,
		Summary:  "fix the bug",
		OpenedBy: "arch",
		UserCorrelationID: "slack:C0XXX:T1",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Architect's last-inbound should now be the task_id, not the slack thread.
	got := sup.AutoCorrelationFor("arch")
	if got != task.ID {
		t.Fatalf("after open_task, AutoCorrelationFor(arch) = %q; want task_id %q",
			got, task.ID)
	}
}

func TestListTasksSorted(t *testing.T) {
	sup, _, _, done := taskHarness(t)
	defer done()

	repo := tmpGitRepo(t)
	for _, id := range []string{"ccc", "aaa", "bbb"} {
		if _, err := sup.OpenTask(orca.OpenTaskRequest{ID: id, RepoRoot: repo}); err != nil {
			t.Fatal(err)
		}
	}
	got := sup.ListTasks()
	if len(got) != 3 || got[0].ID != "aaa" || got[1].ID != "bbb" || got[2].ID != "ccc" {
		t.Fatalf("unsorted: %v", got)
	}
}
