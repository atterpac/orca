package supervisor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/atterpac/orca/pkg/orca"
)

func TestSpawn_UnknownParentFails(t *testing.T) {
	sup, _, _, _, done := budgetHarness(t)
	defer done()

	_, err := sup.Spawn(context.Background(), orca.AgentSpec{
		ID: "child", Runtime: "fake", ParentID: "ghost",
	})
	if err == nil {
		t.Fatal("expected error for unknown parent")
	}
}

func TestSpawn_ParentWithoutCanSpawnFails(t *testing.T) {
	sup, _, _, _, done := budgetHarness(t)
	defer done()

	// Parent exists but lacks can_spawn permission.
	if _, err := sup.Spawn(context.Background(), orca.AgentSpec{
		ID: "parent", Runtime: "fake",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := sup.Spawn(context.Background(), orca.AgentSpec{
		ID: "child", Runtime: "fake", ParentID: "parent",
	})
	if err == nil {
		t.Fatal("expected error when parent lacks can_spawn")
	}
}

func TestSpawn_ParentChildRegistered(t *testing.T) {
	sup, _, _, _, done := budgetHarness(t)
	defer done()

	if _, err := sup.Spawn(context.Background(), orca.AgentSpec{
		ID: "parent", Runtime: "fake", CanSpawn: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Spawn(context.Background(), orca.AgentSpec{
		ID: "child-a", Runtime: "fake", ParentID: "parent",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Spawn(context.Background(), orca.AgentSpec{
		ID: "child-b", Runtime: "fake", ParentID: "parent",
	}); err != nil {
		t.Fatal(err)
	}

	got := sup.Children("parent")
	if len(got) != 2 || got[0] != "child-a" || got[1] != "child-b" {
		t.Fatalf("children=%v", got)
	}
	if sup.Depth("child-a") != 1 {
		t.Fatalf("depth wrong: %d", sup.Depth("child-a"))
	}
}

func TestSpawn_DepthCap(t *testing.T) {
	sup, _, _, _, done := budgetHarness(t)
	defer done()
	sup.Limits.MaxDepth = 2

	// Build a 3-level chain: a → b → c → d. Depth(a)=0, b=1, c=2, d=3 > cap.
	for _, step := range []struct {
		id, parent string
	}{
		{"a", ""},
		{"b", "a"},
		{"c", "b"},
	} {
		spec := orca.AgentSpec{ID: step.id, Runtime: "fake", ParentID: step.parent, CanSpawn: true}
		if _, err := sup.Spawn(context.Background(), spec); err != nil {
			t.Fatalf("spawn %s: %v", step.id, err)
		}
	}

	_, err := sup.Spawn(context.Background(), orca.AgentSpec{
		ID: "d", Runtime: "fake", ParentID: "c", CanSpawn: true,
	})
	if err == nil {
		t.Fatal("expected depth cap to block spawn at depth 3")
	}
}

func TestSpawn_MaxAgents(t *testing.T) {
	sup, _, _, _, done := budgetHarness(t)
	defer done()
	sup.Limits.MaxAgents = 2

	if _, err := sup.Spawn(context.Background(), orca.AgentSpec{ID: "a", Runtime: "fake"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Spawn(context.Background(), orca.AgentSpec{ID: "b", Runtime: "fake"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Spawn(context.Background(), orca.AgentSpec{ID: "c", Runtime: "fake"}); err == nil {
		t.Fatal("expected max_agents cap to block 3rd spawn")
	}
}

func TestSpawn_CascadeKill(t *testing.T) {
	sup, _, _, _, done := budgetHarness(t)
	defer done()

	if _, err := sup.Spawn(context.Background(), orca.AgentSpec{
		ID: "parent", Runtime: "fake", CanSpawn: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Spawn(context.Background(), orca.AgentSpec{
		ID: "cling", Runtime: "fake", ParentID: "parent", OnParentExit: "kill",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Spawn(context.Background(), orca.AgentSpec{
		ID: "orphan", Runtime: "fake", ParentID: "parent", // default: orphan
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)

	if err := sup.Kill("parent"); err != nil {
		t.Fatal(err)
	}

	// Give cascade goroutine a moment.
	deadline := time.After(300 * time.Millisecond)
	for {
		_, clingAlive := sup.Get("cling")
		_, orphanAlive := sup.Get("orphan")
		if !clingAlive && orphanAlive {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("cascade failed: cling_alive=%v orphan_alive=%v", clingAlive, orphanAlive)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestSpawn_CapabilityNegotiation rejects specs that ask for features
// the runtime can't provide (Skills, ContextFiles, SystemPromptFile,
// Isolation=worktree on a runtime without FileAccess or skill support).
// The fake runtime has SkillFormat=none and no file access, so all four
// surface as spawn errors.
func TestSpawn_CapabilityNegotiation(t *testing.T) {
	cases := []struct {
		name string
		spec orca.AgentSpec
		want string
	}{
		{
			name: "skills on non-skill runtime",
			spec: orca.AgentSpec{ID: "a", Runtime: "fake", Skills: []string{"orca_comms"}},
			want: "skill_format=none",
		},
		{
			name: "worktree without file access",
			spec: orca.AgentSpec{ID: "b", Runtime: "fake", Isolation: "worktree"},
			want: "worktree isolation",
		},
		{
			name: "context_files without file access",
			spec: orca.AgentSpec{ID: "c", Runtime: "fake", ContextFiles: []string{"/tmp/x"}},
			want: "context_files",
		},
		{
			name: "system_prompt_file without file access",
			spec: orca.AgentSpec{ID: "d", Runtime: "fake", SystemPromptFile: "/tmp/p.md"},
			want: "system_prompt_file",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sup, _, _, _, done := budgetHarness(t)
			defer done()
			_, err := sup.Spawn(context.Background(), c.spec)
			if err == nil {
				t.Fatalf("expected capability rejection, got nil")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error should mention %q, got %v", c.want, err)
			}
		})
	}
}

func TestSpawn_KillDetachesFromParent(t *testing.T) {
	sup, _, _, _, done := budgetHarness(t)
	defer done()

	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{ID: "p", Runtime: "fake", CanSpawn: true})
	_, _ = sup.Spawn(context.Background(), orca.AgentSpec{ID: "c", Runtime: "fake", ParentID: "p"})
	if kids := sup.Children("p"); len(kids) != 1 {
		t.Fatalf("children=%v", kids)
	}

	// Kill child first — parent's children set should reflect it.
	if err := sup.Kill("c"); err != nil {
		t.Fatal(err)
	}
	if kids := sup.Children("p"); len(kids) != 0 {
		t.Fatalf("children should be empty; got %v", kids)
	}
}
