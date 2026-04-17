package slack

import (
	"strings"
	"testing"

	"github.com/atterpac/orca/pkg/orca"
)

func TestParseHumanReply_OptionNumber(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"1", 1},
		{"  2 ", 2},
		{"3", 3},
	}
	for _, c := range cases {
		ans := parseHumanReply(c.text, []string{"a", "b", "c"})
		if ans.Type != orca.AnswerOption {
			t.Errorf("%q: type=%s want option", c.text, ans.Type)
		}
		if ans.Option != c.want {
			t.Errorf("%q: option=%d want %d", c.text, ans.Option, c.want)
		}
	}
}

func TestParseHumanReply_OptionWithNote(t *testing.T) {
	cases := []struct {
		text string
		opt  int
		note string
	}{
		{"1 - but watch X", 1, "but watch X"},
		{"2: ship it", 2, "ship it"},
		{"3. let's do it", 3, "let's do it"},
	}
	for _, c := range cases {
		ans := parseHumanReply(c.text, []string{"a", "b", "c"})
		if ans.Type != orca.AnswerOption {
			t.Errorf("%q: type=%s want option", c.text, ans.Type)
		}
		if ans.Option != c.opt || ans.Note != c.note {
			t.Errorf("%q: opt=%d note=%q want opt=%d note=%q", c.text, ans.Option, ans.Note, c.opt, c.note)
		}
	}
}

func TestParseHumanReply_OutOfRangeNumberFallsThroughToFreeform(t *testing.T) {
	ans := parseHumanReply("9", []string{"a", "b"})
	if ans.Type != orca.AnswerFreeform {
		t.Fatalf("type=%s want freeform", ans.Type)
	}
	if ans.Text != "9" {
		t.Fatalf("text=%q", ans.Text)
	}
}

func TestParseHumanReply_Cancel(t *testing.T) {
	for _, in := range []string{"CANCEL", "cancel", "  Cancel  "} {
		ans := parseHumanReply(in, []string{"a"})
		if ans.Type != orca.AnswerCancel {
			t.Fatalf("%q: type=%s", in, ans.Type)
		}
	}
}

func TestParseHumanReply_Freeform(t *testing.T) {
	cases := []string{
		"hold on, what's the diff size?",
		"let's try a third approach",
		"1 and a half", // "1" prefix but invalid delimiter
		"no thanks",
	}
	for _, in := range cases {
		ans := parseHumanReply(in, []string{"a", "b", "c"})
		if ans.Type != orca.AnswerFreeform {
			t.Errorf("%q: type=%s want freeform", in, ans.Type)
		}
		if ans.Text != strings.TrimSpace(in) {
			t.Errorf("%q: text=%q", in, ans.Text)
		}
	}
}

func TestParseHumanReply_NoOptionsAllFreeform(t *testing.T) {
	// With empty options, a bare number is just freeform text.
	ans := parseHumanReply("1", nil)
	if ans.Type != orca.AnswerFreeform {
		t.Fatalf("type=%s want freeform", ans.Type)
	}
}

func TestRenderDecision_Shape(t *testing.T) {
	d := &orca.Decision{
		ID:            "d7f3a8",
		AgentID:       "architect",
		TaskID:        "861e7e98",
		Question:      "Drop legacy sessions table?",
		Options:       []string{"drop and migrate", "retain with flag", "defer"},
		Context:       []string{"12k rows", "last write 47d ago"},
		Severity:      orca.SevHigh,
		TimeoutSeconds: 1800,
		DefaultOption: 2,
	}
	got := renderDecision(d)

	// Must contain the required structural markers.
	required := []string{
		"[ORCA-DECISION · d7f3a8]",
		"severity=HIGH",
		"agent=architect",
		"task=861e7e98",
		"Q: Drop legacy sessions table?",
		"Context:",
		"• 12k rows",
		"• last write 47d ago",
		"Options:",
		"1. drop and migrate",
		"2. retain with flag",
		"3. defer",
		"Reply in thread",
		"Timeout: 1800s (defaults to option 2).",
	}
	for _, need := range required {
		if !strings.Contains(got, need) {
			t.Errorf("rendered post missing %q — got:\n%s", need, got)
		}
	}

	// Must NOT contain preamble / greetings the framework enforces away.
	forbidden := []string{"Hi,", "Hello", "Hey team", "Thanks", "Please"}
	for _, bad := range forbidden {
		if strings.Contains(got, bad) {
			t.Errorf("rendered post leaked greeting %q", bad)
		}
	}
}

func TestRenderDecision_ContextAndOptionsCapped(t *testing.T) {
	d := &orca.Decision{
		ID:       "abc",
		AgentID:  "a",
		Question: "Q",
		Options: []string{
			strings.Repeat("x", 400),
		},
		Context: []string{
			strings.Repeat("c", 300),
			"b", "c", "d", "e", "f", "g", // > 5 entries
		},
		Severity: orca.SevMedium,
	}
	got := renderDecision(d)
	// Context bullets limited to 5.
	if n := strings.Count(got, "•"); n > 5 {
		t.Fatalf("context bullets not capped, got %d", n)
	}
	// Long strings truncated.
	if strings.Contains(got, strings.Repeat("c", 300)) {
		t.Fatal("context entry not truncated")
	}
}
