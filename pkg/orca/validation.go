package orca

import (
	"fmt"
	"slices"
	"strings"

	"github.com/atterpac/orca/pkg/orca/roletemplates"
)

// ValidationError describes a single problem in an AgentSpec.
// Fatal errors block spawn; warnings are advisory.
type ValidationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
	Fatal   bool   `json:"fatal"`
}

// ValidationContext supplies live state for checks that can't be done
// against the spec alone — e.g. "is this runtime registered?". Empty
// context skips the live checks (offline validation still catches
// schema errors).
type ValidationContext struct {
	// KnownRuntimes is the list of runtime names registered with the
	// supervisor. Empty disables runtime-existence check.
	KnownRuntimes []string
	// AgentExists returns whether an agent with the given id is currently
	// registered. Used for parent_id checks. Nil disables.
	AgentExists func(id string) bool
	// TaskExists returns whether the task is currently open. Nil disables.
	TaskExists func(id string) bool
}

// ValidateSpec returns all problems found with the spec. An empty
// result means the spec is valid in this context. Fatal=false entries
// are warnings — not errors — and shouldn't block spawn.
func ValidateSpec(s AgentSpec, ctx ValidationContext) []ValidationError {
	var errs []ValidationError
	add := func(field, msg string, fatal bool) {
		errs = append(errs, ValidationError{Field: field, Message: msg, Fatal: fatal})
	}

	// Required scalars.
	if strings.TrimSpace(s.ID) == "" {
		add("id", "required", true)
	}
	if strings.TrimSpace(s.Role) == "" {
		add("role", "required (one-line description of the agent's purpose)", true)
	}
	if s.Runtime != "" && len(ctx.KnownRuntimes) > 0 {
		if !slices.Contains(ctx.KnownRuntimes, s.Runtime) {
			add("runtime", fmt.Sprintf("unknown runtime %q (registered: %v)", s.Runtime, ctx.KnownRuntimes), true)
		}
	}

	// Role template — empty is OK (defaults to persona at spawn).
	if s.RoleTemplate != "" {
		if !slices.Contains(roletemplates.Known, s.RoleTemplate) {
			add("role_template", fmt.Sprintf("unknown role_template %q (valid: %v)", s.RoleTemplate, roletemplates.Known), true)
		}
	}

	// Parent must exist if specified, and not be self.
	if s.ParentID != "" {
		if s.ParentID == s.ID {
			add("parent_id", "cannot be self", true)
		} else if ctx.AgentExists != nil && !ctx.AgentExists(s.ParentID) {
			add("parent_id", fmt.Sprintf("parent agent %q does not exist", s.ParentID), true)
		}
	}

	// Task must exist if specified.
	if s.TaskID != "" && ctx.TaskExists != nil {
		if !ctx.TaskExists(s.TaskID) {
			add("task_id", fmt.Sprintf("task %q is not open", s.TaskID), true)
		}
	}

	// Isolation.
	if s.Isolation != "" && s.Isolation != "worktree" {
		add("isolation", fmt.Sprintf("must be empty or %q, got %q", "worktree", s.Isolation), true)
	}

	// Budget on_breach.
	if s.Budget != nil {
		switch s.Budget.OnBreach {
		case "", "warn", "soft_stop", "hard_interrupt":
			// ok
		default:
			add("budget.on_breach", fmt.Sprintf("must be warn|soft_stop|hard_interrupt, got %q", s.Budget.OnBreach), true)
		}
		if s.Budget.MaxInputTokens == 0 && s.Budget.MaxOutputTokens == 0 && s.Budget.MaxCostUSD == 0 {
			add("budget", "all budget caps are zero — set at least one (max_input_tokens, max_output_tokens, max_cost_usd) or omit the budget block", false)
		}
	}

	// ACL selectors.
	if s.ACL != nil {
		for i, sel := range s.ACL.SendsTo {
			if err := validateSelector(sel); err != nil {
				add(fmt.Sprintf("acl.sends_to[%d]", i), err.Error(), true)
			}
		}
		for i, sel := range s.ACL.AcceptsFrom {
			if err := validateSelector(sel); err != nil {
				add(fmt.Sprintf("acl.accepts_from[%d]", i), err.Error(), true)
			}
		}
	}

	// OnParentExit.
	if s.OnParentExit != "" && s.OnParentExit != "orphan" && s.OnParentExit != "kill" {
		add("on_parent_exit", fmt.Sprintf("must be orphan|kill, got %q", s.OnParentExit), true)
	}

	return errs
}

// validateSelector checks one ACL selector string is parseable. Mirrors
// the parser in internal/supervisor/acl.go.
func validateSelector(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("empty selector")
	}
	if raw == "*" {
		return nil
	}
	if strings.HasPrefix(raw, "id:") {
		if strings.TrimSpace(strings.TrimPrefix(raw, "id:")) == "" {
			return fmt.Errorf("id: selector missing value")
		}
		return nil
	}
	if strings.HasPrefix(raw, "tag:") {
		rest := strings.TrimPrefix(raw, "tag:")
		hasOne := false
		for _, t := range strings.Split(rest, ",") {
			if strings.TrimSpace(t) != "" {
				hasOne = true
			}
		}
		if !hasOne {
			return fmt.Errorf("tag: selector missing value")
		}
		return nil
	}
	// Unqualified bare word is treated as id (per supervisor parser).
	return nil
}

// FatalCount counts errors with Fatal=true.
func FatalCount(errs []ValidationError) int {
	n := 0
	for _, e := range errs {
		if e.Fatal {
			n++
		}
	}
	return n
}
