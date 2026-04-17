package supervisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"github.com/atterpac/orca/pkg/orca"
)

// OpenTask creates a new task record, allocating a git worktree and symlinking
// the shared artifact directory into it when RepoRoot points at a git repo.
// Non-git workdirs skip worktree creation and just provision an artifact dir.
//
// Permission: only orchestrator-role agents (or unrestricted "persona" agents)
// may open tasks. Communicators, workers, and reviewers are rejected. This
// closes a gap where a literal-minded communicator agent might "be helpful"
// and pre-open a task when handing off to the orchestrator, resulting in a
// duplicate task being created downstream by the orchestrator itself.
func (s *Supervisor) OpenTask(req orca.OpenTaskRequest) (*orca.Task, error) {
	if req.RepoRoot == "" {
		return nil, errors.New("repo_root required")
	}
	if req.OpenedBy != "" {
		s.mu.RLock()
		caller, ok := s.agents[req.OpenedBy]
		s.mu.RUnlock()
		if ok {
			role := caller.info.Spec.RoleTemplate
			if role == "" {
				role = "persona"
			}
			switch role {
			case "orchestrator", "persona":
				// allowed
			default:
				return nil, fmt.Errorf("agent %q (role_template=%q) is not permitted to open tasks; only orchestrators can. The communicator should hand the request to an orchestrator agent and let it open the task.", req.OpenedBy, role)
			}
		}
	}
	id := req.ID
	if id == "" {
		id = genTaskID()
	}

	repoAbs, err := filepath.Abs(req.RepoRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve repo_root: %w", err)
	}
	if st, err := os.Stat(repoAbs); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("repo_root %s is not a directory", repoAbs)
	}

	s.mu.Lock()
	if s.tasks == nil {
		s.tasks = map[string]*orca.Task{}
	}
	if _, exists := s.tasks[id]; exists {
		s.mu.Unlock()
		return nil, fmt.Errorf("task %s already open", id)
	}
	s.mu.Unlock()

	artifactDir := filepath.Join(repoAbs, ".orca", id)
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		return nil, fmt.Errorf("artifact dir: %w", err)
	}

	var wtPath, branch string
	if isGitRepo(repoAbs) {
		base := req.BaseRef
		if base == "" {
			base = "HEAD"
		}
		wtPath = filepath.Join(repoAbs, ".orca", "worktrees", id)
		// `git worktree add <path> <ref>` creates a detached worktree when
		// <ref> is a commit. For concurrency-friendly tasks we detach; callers
		// can later attach a branch with `git worktree add -b`.
		out, err := exec.Command("git", "-C", repoAbs, "worktree", "add", "--detach", wtPath, base).CombinedOutput()
		if err != nil {
			_ = os.RemoveAll(artifactDir)
			return nil, fmt.Errorf("git worktree add: %w\n%s", err, string(out))
		}

		// Symlink the shared artifact dir into the worktree so agents can
		// access it via the same `.orca/<id>/` path inside or outside.
		wtOrca := filepath.Join(wtPath, ".orca")
		if err := os.MkdirAll(wtOrca, 0o755); err != nil {
			return nil, fmt.Errorf("worktree .orca: %w", err)
		}
		linkPath := filepath.Join(wtOrca, id)
		// Remove pre-existing entry (may appear if .orca is tracked).
		_ = os.Remove(linkPath)
		if err := os.Symlink(artifactDir, linkPath); err != nil {
			return nil, fmt.Errorf("symlink artifact dir: %w", err)
		}
		branch = detectBranch(wtPath)
	}

	t := &orca.Task{
		ID:           id,
		Phase:        "open",
		RepoRoot:     repoAbs,
		WorktreePath: wtPath,
		ArtifactDir:  artifactDir,
		BaseRef:      req.BaseRef,
		Branch:       branch,
		OpenedAt:     time.Now(),
	}

	s.mu.Lock()
	s.tasks[id] = t
	s.mu.Unlock()

	// Make the new task_id the calling agent's "active conversation" so
	// subsequent report_* / send_message calls auto-correlate to the task
	// thread instead of the user's original discussion thread. The agent
	// can still post to the user's thread by passing correlation_id
	// explicitly (e.g. when signaling task completion to the secretary).
	if req.OpenedBy != "" {
		s.lastInboundCorr.Store(req.OpenedBy, t.ID)
		// Record the opener as the first participant so timeline / list
		// queries have something to anchor on even when other agents
		// participate via correlation_id rather than spec.TaskID.
		s.mu.Lock()
		if !slices.Contains(t.Agents, req.OpenedBy) {
			t.Agents = append(t.Agents, req.OpenedBy)
		}
		s.mu.Unlock()
	}

	s.events.Emit(orca.Event{Kind: orca.EvtTaskOpened, Payload: map[string]any{
		"task_id":       t.ID,
		"repo_root":     t.RepoRoot,
		"worktree_path": t.WorktreePath,
		"artifact_dir":  t.ArtifactDir,
		"base_ref":      t.BaseRef,
		"branch":        t.Branch,
	}})

	// Announce to bridge agent (default "slack") so human observers can
	// follow the task. Only fires when the bridge actually exists.
	s.announceTaskOpened(req, t)
	return t, nil
}

func (s *Supervisor) announceTaskOpened(req orca.OpenTaskRequest, t *orca.Task) {
	bridge := req.BridgeAgentID
	if bridge == "" {
		bridge = "slack"
	}
	s.mu.RLock()
	_, bridgeExists := s.agents[bridge]
	s.mu.RUnlock()
	if !bridgeExists {
		return
	}

	summary := req.Summary
	if summary == "" {
		summary = "(no summary provided)"
	}
	payload := map[string]any{
		"type":                "task_opened",
		"task_id":             t.ID,
		"summary":             summary,
		"opened_by":           req.OpenedBy,
		"user_correlation_id": req.UserCorrelationID,
		"announce_channel":    req.AnnounceChannel,
		"worktree_path":       t.WorktreePath,
		"artifact_dir":        t.ArtifactDir,
		"branch":              t.Branch,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	msg := orca.Message{
		From:          "orca",
		To:            bridge,
		Kind:          orca.KindEvent,
		Body:          body,
		CorrelationID: t.ID,
		Timestamp:     time.Now(),
	}
	_ = s.bus.Publish(context.Background(), msg)
}

// CloseTask marks the task closed. When removeWorktree is true the git
// worktree is unlinked via `git worktree remove --force`. The artifact dir
// always remains for post-hoc inspection.
//
// The task is NOT removed from the registry — closed tasks remain
// queryable so trace / timeline endpoints can reconstruct historical
// runs. ListTasks() filters to open by default; GetTask returns any.
func (s *Supervisor) CloseTask(id string, removeWorktree bool) (*orca.Task, error) {
	s.mu.Lock()
	t, ok := s.tasks[id]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("task %s not found", id)
	}
	if t.Phase == "closed" {
		s.mu.Unlock()
		return t, nil // idempotent
	}
	now := time.Now()
	t.ClosedAt = &now
	t.Phase = "closed"
	s.mu.Unlock()

	if removeWorktree && t.WorktreePath != "" {
		// `git worktree remove --force` refuses if the dir is gone; that's
		// fine — just make sure the git bookkeeping stays consistent by
		// pruning whether or not removal succeeded.
		_ = exec.Command("git", "-C", t.RepoRoot, "worktree", "remove", "--force", t.WorktreePath).Run()
		_ = exec.Command("git", "-C", t.RepoRoot, "worktree", "prune").Run()
	}

	// Clear any per-agent active correlation that points at this task
	// so subsequent inbound messages can drive a fresh conversation
	// without auto-correlating to the closed task.
	for _, agentID := range t.Agents {
		if cur, ok := s.lastInboundCorr.Load(agentID); ok {
			if cs, ok := cur.(string); ok && cs == id {
				s.lastInboundCorr.Delete(agentID)
			}
		}
	}

	// Tear down per-task subsessions on every participating multi-session
	// agent. Closed tasks shouldn't leak live claude processes or resume
	// metadata.
	for _, agentID := range t.Agents {
		s.mu.RLock()
		rec, ok := s.agents[agentID]
		s.mu.RUnlock()
		if !ok {
			continue
		}
		if ms, ok := rec.session.(*multiSession); ok {
			ms.dropSub(id)
		}
	}

	s.events.Emit(orca.Event{Kind: orca.EvtTaskClosed, Payload: map[string]any{
		"task_id":       t.ID,
		"artifact_dir":  t.ArtifactDir,
		"worktree_path": t.WorktreePath,
		"removed":       removeWorktree,
	}})
	return t, nil
}

// GetTask returns the task with the given id, or nil if not found.
func (s *Supervisor) GetTask(id string) (*orca.Task, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tasks[id]
	if !ok {
		return nil, false
	}
	// return a copy so callers can't mutate
	cp := *t
	cp.Agents = slices.Clone(t.Agents)
	return &cp, true
}

// ListTasks returns currently-open tasks, sorted by id. Closed tasks
// remain in the registry but are excluded from this default view —
// pass includeClosed=true to ListAllTasks to see them.
func (s *Supervisor) ListTasks() []orca.Task {
	return s.listTasksFiltered(false)
}

// ListAllTasks returns every task ever opened on this daemon, including
// closed ones. Sorted newest-first by OpenedAt for a sensible chronology.
func (s *Supervisor) ListAllTasks() []orca.Task {
	return s.listTasksFiltered(true)
}

func (s *Supervisor) listTasksFiltered(includeClosed bool) []orca.Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]orca.Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		if !includeClosed && t.Phase != "open" {
			continue
		}
		cp := *t
		cp.Agents = slices.Clone(t.Agents)
		out = append(out, cp)
	}
	if includeClosed {
		// Newest-first when showing all (closed history mixed with open).
		slices.SortFunc(out, func(a, b orca.Task) int {
			switch {
			case a.OpenedAt.After(b.OpenedAt):
				return -1
			case a.OpenedAt.Before(b.OpenedAt):
				return 1
			default:
				return 0
			}
		})
	} else {
		slices.SortFunc(out, func(a, b orca.Task) int {
			switch {
			case a.ID < b.ID:
				return -1
			case a.ID > b.ID:
				return 1
			default:
				return 0
			}
		})
	}
	return out
}

func genTaskID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func isGitRepo(path string) bool {
	out, err := exec.Command("git", "-C", path, "rev-parse", "--is-inside-work-tree").CombinedOutput()
	if err != nil {
		return false
	}
	return len(out) > 0 && out[0] == 't'
}

func detectBranch(path string) string {
	out, err := exec.Command("git", "-C", path, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	// Strip trailing newline.
	for len(out) > 0 && (out[len(out)-1] == '\n' || out[len(out)-1] == '\r') {
		out = out[:len(out)-1]
	}
	return string(out)
}

// Shutdown variant that also closes all open tasks. Callers that want to
// preserve worktrees can set remove=false.
func (s *Supervisor) ShutdownWithTasks(ctx context.Context, removeWorktrees bool) {
	s.mu.RLock()
	ids := make([]string, 0, len(s.tasks))
	for id := range s.tasks {
		ids = append(ids, id)
	}
	s.mu.RUnlock()
	for _, id := range ids {
		_, _ = s.CloseTask(id, removeWorktrees)
	}
	s.Shutdown()
}
