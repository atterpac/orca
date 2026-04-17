package orca

import "time"

// Task represents an isolated unit of work. When backed by a git repository it
// owns a dedicated worktree so multiple concurrent tasks never clobber each
// other's edits. Task artifacts (PLAN, NOTES, diffs, QA reports) live in
// ArtifactDir on the main repo and are symlinked into the worktree so agents
// can reference them by the same .orca/<task_id>/ path regardless of which
// side they're operating on.
type Task struct {
	ID           string     `json:"id"`
	Phase        string     `json:"phase"` // open | closed
	RepoRoot     string     `json:"repo_root"`
	WorktreePath string     `json:"worktree_path,omitempty"`
	ArtifactDir  string     `json:"artifact_dir"`
	BaseRef      string     `json:"base_ref,omitempty"`
	Branch       string     `json:"branch,omitempty"`
	OpenedAt     time.Time  `json:"opened_at"`
	ClosedAt     *time.Time `json:"closed_at,omitempty"`
	Agents       []string   `json:"agents,omitempty"`
}

// OpenTaskRequest is the shape accepted by `POST /tasks` and the
// `mcp__orca_comms__open_task` verb.
type OpenTaskRequest struct {
	ID       string `json:"id,omitempty"`
	RepoRoot string `json:"repo_root"`
	BaseRef  string `json:"base_ref,omitempty"`

	// Optional announcement context. When populated, the supervisor emits
	// a task_opened event to the bridge agent so it can post a styled
	// top-level thread-anchor message, and (if UserCorrelationID is set)
	// drop a pointer link into the user's original conversation thread.
	Summary            string `json:"summary,omitempty"`            // one-line description for the announcement
	OpenedBy           string `json:"opened_by,omitempty"`          // agent id that initiated the task
	UserCorrelationID  string `json:"user_correlation_id,omitempty"` // the user's discussion thread, if any
	BridgeAgentID      string `json:"bridge_agent_id,omitempty"`    // defaults to "slack" when a slack agent exists
	AnnounceChannel    string `json:"announce_channel,omitempty"`   // bridge-specific override
}
