package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/atterpac/orca/pkg/orca"
)

// runTrace renders a chronological view of all events touching a task,
// or all recent events for an agent if --agent is passed instead.
//
// Usage:
//   orca trace <task_id>             — task timeline
//   orca trace --agent <id> [--limit 100] [--kinds k1,k2]
func runTrace(args []string) error {
	fs := flag.NewFlagSet("trace", flag.ContinueOnError)
	agent := fs.String("agent", "", "show this agent's transcript instead of a task timeline")
	limit := fs.Int("limit", 100, "max events to show")
	kindsRaw := fs.String("kinds", "", "comma-separated event kinds filter (transcript mode only)")
	noColor := fs.Bool("no-color", false, "disable ANSI colors")
	if err := fs.Parse(args); err != nil {
		return err
	}

	useColor := !*noColor && terminalSupportsColor()

	var url string
	if *agent != "" {
		url = fmt.Sprintf("%s/agents/%s/transcript?limit=%d", daemonAddr(), *agent, *limit)
		if *kindsRaw != "" {
			url += "&kinds=" + *kindsRaw
		}
	} else {
		taskArgs := fs.Args()
		if len(taskArgs) == 0 {
			return fmt.Errorf("usage: orca trace <task_id>  OR  orca trace --agent <id>")
		}
		url = fmt.Sprintf("%s/tasks/%s/timeline?limit=%d", daemonAddr(), taskArgs[0], *limit)
	}

	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("trace failed: %s", string(body))
	}

	var events []orca.Event
	if err := json.Unmarshal(body, &events); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if len(events) == 0 {
		fmt.Fprintln(os.Stderr, "(no events)")
		return nil
	}
	for _, e := range events {
		fmt.Println(formatEvent(e, useColor))
	}
	return nil
}

// formatEvent produces a single-line readable rendering of an event.
//
//   HH:MM:SS.sss  agent           Kind            payload-summary
func formatEvent(e orca.Event, color bool) string {
	ts := e.Timestamp.Format("15:04:05.000")
	agent := e.AgentID
	if agent == "" {
		agent = "-"
	}
	kindStr := string(e.Kind)
	summary := summarizePayload(e)

	if color {
		return fmt.Sprintf("%s  %s  %s  %s",
			dim(ts),
			padRight(agent, 14),
			padRight(colorForKind(kindStr), 50),
			summary)
	}
	return fmt.Sprintf("%s  %-14s  %-22s  %s", ts, agent, kindStr, summary)
}

func summarizePayload(e orca.Event) string {
	if e.Payload == nil {
		return ""
	}
	p, ok := e.Payload.(map[string]any)
	if !ok {
		raw, _ := json.Marshal(e.Payload)
		s := string(raw)
		if len(s) > 120 {
			s = s[:117] + "..."
		}
		return s
	}

	// Pick the most useful 1-3 fields per kind.
	var parts []string
	add := func(k string) {
		if v, ok := p[k]; ok && v != nil {
			parts = append(parts, fmt.Sprintf("%s=%v", k, abbreviate(v, 80)))
		}
	}
	switch e.Kind {
	case orca.EvtToolCallStart, orca.EvtToolCallEnd:
		add("tool")
		if id, ok := p["id"].(string); ok && id != "" {
			parts = append(parts, "id="+abbrev(id, 12))
		}
	case orca.EvtMessageSent, orca.EvtMessageDelivered:
		add("from")
		add("to")
		add("kind")
		add("correlation_id")
	case orca.EvtMessageDropped:
		add("from")
		add("to")
		add("reason")
	case orca.EvtTurnCompleted:
		if cum, ok := p["cumulative"].(map[string]any); ok {
			if c, ok := cum["cost_usd"]; ok {
				parts = append(parts, fmt.Sprintf("cum_cost=$%v", c))
			}
		}
	case orca.EvtTaskOpened:
		add("task_id")
		add("worktree_path")
	case orca.EvtTaskClosed:
		add("task_id")
		add("removed")
	case orca.EvtBudgetWarn, orca.EvtBudgetExceeded:
		add("pct")
	case orca.EvtAgentExited:
		if pe, ok := p["error"].(string); ok && pe != "" {
			parts = append(parts, "error="+abbreviate(pe, 60))
		}
	case orca.EvtError:
		add("scope")
		add("msg")
	default:
		// Fall back to a couple common fields.
		for _, k := range []string{"text", "title", "id", "from", "to"} {
			if _, ok := p[k]; ok {
				add(k)
			}
		}
	}
	out := strings.Join(parts, " ")
	if out == "" {
		raw, _ := json.Marshal(p)
		out = abbreviate(string(raw), 100)
	}
	return out
}

func abbrev(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

func abbreviate(v any, max int) string {
	if s, ok := v.(string); ok {
		return abbrev(s, max)
	}
	raw, _ := json.Marshal(v)
	return abbrev(string(raw), max)
}

func padRight(s string, n int) string {
	if len(stripANSI(s)) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(stripANSI(s)))
}

func stripANSI(s string) string {
	out := make([]byte, 0, len(s))
	skip := false
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			skip = true
			continue
		}
		if skip {
			if s[i] == 'm' {
				skip = false
			}
			continue
		}
		out = append(out, s[i])
	}
	return string(out)
}

// ANSI helpers — minimal, no deps. Only used when stdout is a TTY.

const (
	cReset  = "\033[0m"
	cDim    = "\033[2m"
	cRed    = "\033[31m"
	cGreen  = "\033[32m"
	cYellow = "\033[33m"
	cBlue   = "\033[34m"
	cMagenta = "\033[35m"
	cCyan   = "\033[36m"
	cBold   = "\033[1m"
)

func dim(s string) string  { return cDim + s + cReset }

func colorForKind(kind string) string {
	switch kind {
	case "TaskOpened":
		return cGreen + cBold + kind + cReset
	case "TaskClosed":
		return cGreen + kind + cReset
	case "AgentSpawned", "AgentReady":
		return cBlue + kind + cReset
	case "AgentExited":
		return cMagenta + kind + cReset
	case "Error", "BudgetExceeded", "MessageDropped":
		return cRed + kind + cReset
	case "BudgetWarn":
		return cYellow + kind + cReset
	case "MessageSent":
		return cCyan + kind + cReset
	case "MessageDelivered":
		return cBlue + kind + cReset
	case "ToolCallStart", "ToolCallEnd":
		return cYellow + kind + cReset
	case "TurnCompleted", "UsageSnapshot":
		return cDim + kind + cReset
	case "DecisionRequested", "DecisionAnswered", "DecisionTimedOut":
		return cMagenta + kind + cReset
	case "DiscussionOpened", "DiscussionMessage", "DiscussionClosed":
		return cCyan + kind + cReset
	default:
		return kind
	}
}

func terminalSupportsColor() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
