package orca

import (
	"strings"
	"testing"
)

func TestValidate_RequiresIDAndRole(t *testing.T) {
	errs := ValidateSpec(AgentSpec{}, ValidationContext{})
	mustHave := []string{"id", "role"}
	for _, want := range mustHave {
		found := false
		for _, e := range errs {
			if e.Field == want && e.Fatal {
				found = true
			}
		}
		if !found {
			t.Errorf("expected fatal error on field %q; got %v", want, errs)
		}
	}
}

func TestValidate_KnownRuntimeAccepted(t *testing.T) {
	errs := ValidateSpec(
		AgentSpec{ID: "a", Role: "x", Runtime: "fake"},
		ValidationContext{KnownRuntimes: []string{"fake", "claude-code-local"}},
	)
	for _, e := range errs {
		if e.Field == "runtime" {
			t.Fatalf("known runtime should be accepted: %v", e)
		}
	}
}

func TestValidate_UnknownRuntimeRejected(t *testing.T) {
	errs := ValidateSpec(
		AgentSpec{ID: "a", Role: "x", Runtime: "ghost"},
		ValidationContext{KnownRuntimes: []string{"fake"}},
	)
	if FatalCount(errs) == 0 {
		t.Fatal("unknown runtime should be a fatal error")
	}
}

func TestValidate_RoleTemplate(t *testing.T) {
	// Valid template.
	errs := ValidateSpec(AgentSpec{ID: "a", Role: "x", RoleTemplate: "orchestrator"}, ValidationContext{})
	for _, e := range errs {
		if e.Field == "role_template" {
			t.Fatalf("known template should be accepted: %v", e)
		}
	}
	// Invalid template.
	errs = ValidateSpec(AgentSpec{ID: "a", Role: "x", RoleTemplate: "ghost"}, ValidationContext{})
	if FatalCount(errs) == 0 {
		t.Fatal("unknown role_template should be fatal")
	}
}

func TestValidate_ParentMustExist(t *testing.T) {
	exists := func(id string) bool { return id == "alive" }

	errs := ValidateSpec(
		AgentSpec{ID: "child", Role: "x", ParentID: "ghost"},
		ValidationContext{AgentExists: exists},
	)
	if FatalCount(errs) == 0 {
		t.Fatal("missing parent should be fatal")
	}

	errs = ValidateSpec(
		AgentSpec{ID: "child", Role: "x", ParentID: "alive"},
		ValidationContext{AgentExists: exists},
	)
	for _, e := range errs {
		if e.Field == "parent_id" {
			t.Fatalf("present parent should pass: %v", e)
		}
	}
}

func TestValidate_ParentNotSelf(t *testing.T) {
	errs := ValidateSpec(
		AgentSpec{ID: "loop", Role: "x", ParentID: "loop"},
		ValidationContext{AgentExists: func(id string) bool { return true }},
	)
	if FatalCount(errs) == 0 {
		t.Fatal("self-parent should be fatal")
	}
}

func TestValidate_BudgetOnBreach(t *testing.T) {
	for _, v := range []string{"warn", "soft_stop", "hard_interrupt", ""} {
		errs := ValidateSpec(
			AgentSpec{ID: "a", Role: "x", Budget: &Budget{MaxInputTokens: 1, OnBreach: v}},
			ValidationContext{},
		)
		for _, e := range errs {
			if e.Field == "budget.on_breach" {
				t.Errorf("on_breach=%q should pass: %v", v, e)
			}
		}
	}
	errs := ValidateSpec(
		AgentSpec{ID: "a", Role: "x", Budget: &Budget{MaxInputTokens: 1, OnBreach: "explode"}},
		ValidationContext{},
	)
	if FatalCount(errs) == 0 {
		t.Fatal("invalid on_breach should be fatal")
	}
}

func TestValidate_BudgetEmptyWarns(t *testing.T) {
	errs := ValidateSpec(
		AgentSpec{ID: "a", Role: "x", Budget: &Budget{}},
		ValidationContext{},
	)
	hasWarn := false
	for _, e := range errs {
		if e.Field == "budget" && !e.Fatal {
			hasWarn = true
		}
	}
	if !hasWarn {
		t.Fatal("empty budget block should produce a warning")
	}
}

func TestValidate_ACLSelectors(t *testing.T) {
	cases := []struct {
		sel  string
		ok   bool
	}{
		{"*", true},
		{"id:foo", true},
		{"tag:code", true},
		{"tag:code,bug", true},
		{"bareword", true}, // treated as id
		{"id:", false},
		{"tag:", false},
		{"tag:,", false},
		{"", false},
	}
	for _, c := range cases {
		errs := ValidateSpec(
			AgentSpec{ID: "a", Role: "x", ACL: &ACL{SendsTo: []string{c.sel}}},
			ValidationContext{},
		)
		hasErr := false
		for _, e := range errs {
			if strings.HasPrefix(e.Field, "acl.sends_to") {
				hasErr = true
			}
		}
		if c.ok && hasErr {
			t.Errorf("selector %q should be accepted; got errors %v", c.sel, errs)
		}
		if !c.ok && !hasErr {
			t.Errorf("selector %q should be rejected; got no error", c.sel)
		}
	}
}
