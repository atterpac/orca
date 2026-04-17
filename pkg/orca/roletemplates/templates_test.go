package roletemplates

import (
	"strings"
	"testing"
)

func TestLoad_KnownTemplates(t *testing.T) {
	for _, name := range Known {
		body, err := Load(name)
		if err != nil {
			t.Errorf("Load(%q): %v", name, err)
			continue
		}
		if body == "" {
			t.Errorf("Load(%q): empty body", name)
		}
		marker := "role_template: " + name
		if !strings.Contains(body, marker) {
			t.Errorf("Load(%q): expected marker %q in body", name, marker)
		}
	}
}

func TestLoad_UnknownRejected(t *testing.T) {
	_, err := Load("does-not-exist")
	if err == nil {
		t.Fatal("expected error for unknown template")
	}
	if !strings.Contains(err.Error(), "unknown role_template") {
		t.Fatalf("error message should flag unknown: %v", err)
	}
}

func TestList(t *testing.T) {
	got := List()
	if len(got) != len(Known) {
		t.Fatalf("List() returned %d, want %d", len(got), len(Known))
	}
	// Must be a clone — mutating result shouldn't corrupt Known.
	got[0] = "mutated"
	if Known[0] == "mutated" {
		t.Fatal("List() leaked the backing slice")
	}
}
