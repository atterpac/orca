// Package roletemplates holds the framework-provided prompt blocks that
// are composed into every agent's system prompt alongside the user's
// persona-only prompt and the universal Operating Principles.
//
// A role template encodes HOW an agent participates in orca's
// orchestration machinery: task lifecycles, milestone posting,
// correlation handling, bridge relay patterns. Users pick a template
// via AgentSpec.RoleTemplate; they do NOT need to know the mechanics
// the template documents.
//
// Templates are versioned with the orca binary — when you upgrade orca,
// you upgrade the templates. If a deployment needs a custom template,
// fork the source or ship your own composed prompt.
package roletemplates

import (
	"embed"
	"fmt"
	"slices"
)

//go:embed persona.md communicator.md orchestrator.md worker.md reviewer.md
var templatesFS embed.FS

// Known lists every role_template value the framework recognizes.
// Validation at spawn rejects anything not in this list.
var Known = []string{
	"persona",
	"communicator",
	"orchestrator",
	"worker",
	"reviewer",
}

// Load returns the template body for the given name. Returns an error
// with the list of valid names when the template is unknown.
func Load(name string) (string, error) {
	if !slices.Contains(Known, name) {
		return "", fmt.Errorf("unknown role_template %q (valid: %v)", name, Known)
	}
	b, err := templatesFS.ReadFile(name + ".md")
	if err != nil {
		return "", fmt.Errorf("read %s.md: %w", name, err)
	}
	return string(b), nil
}

// List returns all registered template names (same as Known).
func List() []string { return slices.Clone(Known) }
