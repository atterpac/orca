package orca

// UpdatePhase is a coarse bucket the bridge uses to pick an emoji /
// visual accent for an update. Free-form strings are tolerated but
// unknown values fall back to a generic icon.
type UpdatePhase string

const (
	PhasePlanning      UpdatePhase = "planning"
	PhaseImplementing  UpdatePhase = "implementing"
	PhaseTesting       UpdatePhase = "testing"
	PhaseReviewing     UpdatePhase = "reviewing"
	PhaseInvestigating UpdatePhase = "investigating"
	PhaseDeploying     UpdatePhase = "deploying"
	PhaseBlocked       UpdatePhase = "blocked"
	PhaseDone          UpdatePhase = "done"
	PhaseInfo          UpdatePhase = "info"
)

// UpdateStatus indicates where in the phase's own lifecycle this post sits.
// Values (validated at the MCP boundary): "started" | "progress" | "complete" | "failed".
type UpdateStatus string

// UpdateSeverity modulates urgency cues on the bridge (color accents,
// mentions, notification weight). Defaults to "info".
type UpdateSeverity string

const (
	UpdateInfo    UpdateSeverity = "info"
	UpdateSuccess UpdateSeverity = "success"
	UpdateWarn    UpdateSeverity = "warn"
	UpdateError   UpdateSeverity = "error"
)

// Update is the structured payload bridges render for progress posts.
// Agents populate slots via the `post_update` MCP tool; they don't write
// prose. The framework renders consistently across bridges.
type Update struct {
	Phase    UpdatePhase       `json:"phase"`
	Status   UpdateStatus      `json:"status,omitempty"`
	Title    string            `json:"title"`            // one-line headline
	Details  []string          `json:"details,omitempty"` // ≤5 bulleted lines
	Metrics  map[string]string `json:"metrics,omitempty"` // key→value pairs (e.g. files=3, tests="5 passed")
	Severity UpdateSeverity    `json:"severity,omitempty"`
	AgentID  string            `json:"agent_id,omitempty"` // sender id — auto-filled by MCP shim
	Link     string            `json:"link,omitempty"`     // optional URL
	TaskID   string            `json:"task_id,omitempty"`  // optional task context
}
