package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/atterpac/orca/pkg/mcpserver"
	"github.com/atterpac/orca/pkg/orca"
)

func runMCPShim(args []string) error {
	fs := flag.NewFlagSet("mcp-shim", flag.ContinueOnError)
	asAgent := fs.String("as", os.Getenv("ORCA_AGENT_ID"), "agent id this shim represents")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *asAgent == "" {
		return fmt.Errorf("--as <agent_id> required (or ORCA_AGENT_ID)")
	}

	tools := []mcpserver.Tool{
		{
			Name:        "list_agents",
			Description: "List all agents currently registered with orca, including their role, status, skills and token usage.",
			InputSchema: mcpserver.MustSchema(`{"type":"object","properties":{},"additionalProperties":false}`),
			Handler: func(ctx context.Context, args map[string]any) (string, error) {
				resp, err := http.Get(daemonAddr() + "/agents")
				if err != nil {
					return "", err
				}
				defer resp.Body.Close()
				var agents []orca.AgentInfo
				if err := json.NewDecoder(resp.Body).Decode(&agents); err != nil {
					return "", err
				}
				summary := make([]map[string]any, 0, len(agents))
				for _, a := range agents {
					summary = append(summary, map[string]any{
						"id":     a.Spec.ID,
						"role":   a.Spec.Role,
						"status": a.Status,
						"skills": a.Spec.Skills,
						"tags":   a.Spec.Tags,
						"usage": map[string]any{
							"input_tokens":  a.Usage.InputTokens,
							"output_tokens": a.Usage.OutputTokens,
							"turns":         a.Usage.Turns,
						},
					})
				}
				out, _ := json.MarshalIndent(summary, "", "  ")
				return string(out), nil
			},
		},
		{
			Name:        "send_message",
			Description: "Send a message to another agent — either directly by id (`to`), or by tags (`tags` + `mode`). With mode=\"any\" the message is routed to one matching agent (round-robin); with mode=\"all\" it broadcasts to every matching agent. The recipient receives your message as a new prompt and replies autonomously.",
			InputSchema: mcpserver.MustSchema(`{
				"type":"object",
				"properties":{
					"to":{"type":"string","description":"Target agent id (use this OR tags)"},
					"tags":{"type":"array","items":{"type":"string"},"description":"Route by tags (AND match). Use this OR to."},
					"mode":{"type":"string","enum":["any","all"],"default":"any","description":"Tag routing: any=pick one, all=broadcast to every match"},
					"body":{"type":"string","description":"Message text"},
					"kind":{"type":"string","enum":["request","response","handoff","broadcast","concern","question","update"],"default":"request"},
					"correlation_id":{"type":"string","description":"If set, threads this message onto an existing conversation. For human-in-loop clarifications, set this to the decision_id from the inbound message so your reply lands in the same Slack thread."}
				},
				"required":["body"],
				"additionalProperties":false
			}`),
			Handler: func(ctx context.Context, args map[string]any) (string, error) {
				to, _ := args["to"].(string)
				body, _ := args["body"].(string)
				kind, _ := args["kind"].(string)
				mode, _ := args["mode"].(string)
				correlationID, _ := args["correlation_id"].(string)
				var tags []string
				if raw, ok := args["tags"].([]any); ok {
					for _, v := range raw {
						if s, ok := v.(string); ok && s != "" {
							tags = append(tags, s)
						}
					}
				}
				if kind == "" {
					kind = "request"
				}
				if body == "" {
					return "", fmt.Errorf("body required")
				}
				if to == "" && len(tags) == 0 {
					return "", fmt.Errorf("either `to` or `tags` required")
				}
				bodyJSON, _ := json.Marshal(body)

				if len(tags) > 0 {
					payload := map[string]any{
						"from": *asAgent,
						"kind": kind,
						"tags": tags,
						"mode": mode,
						"body": json.RawMessage(bodyJSON),
					}
					if correlationID != "" {
						payload["correlation_id"] = correlationID
					}
					buf, _ := json.Marshal(payload)
					resp, err := http.Post(daemonAddr()+"/messages", "application/json", bytes.NewReader(buf))
					if err != nil {
						return "", err
					}
					defer resp.Body.Close()
					out, _ := io.ReadAll(resp.Body)
					if resp.StatusCode >= 400 {
						return "", fmt.Errorf("dispatch failed: %s", string(out))
					}
					return fmt.Sprintf("dispatched by tags %v mode=%s: %s", tags, mode, strings.TrimSpace(string(out))), nil
				}

				payload := map[string]any{
					"from": *asAgent,
					"kind": kind,
					"body": json.RawMessage(bodyJSON),
				}
				if correlationID != "" {
					payload["correlation_id"] = correlationID
				}
				buf, _ := json.Marshal(payload)
				resp, err := http.Post(daemonAddr()+"/agents/"+to+"/message", "application/json", bytes.NewReader(buf))
				if err != nil {
					return "", err
				}
				defer resp.Body.Close()
				out, _ := io.ReadAll(resp.Body)
				if resp.StatusCode >= 400 {
					return "", fmt.Errorf("send failed: %s", string(out))
				}
				return fmt.Sprintf("delivered to %s", to), nil
			},
		},
		reportTool(asAgent, "report_plan",
			"Post a 'plan drafted' milestone. Call this after writing PLAN.md so the human sees planning is complete.",
			`{
				"type":"object",
				"properties":{
					"title":{"type":"string","description":"One-line synopsis of what the plan does."},
					"details":{"type":"array","items":{"type":"string"},"description":"Up to 5 bullets describing the plan steps."}
				},
				"required":["title"],
				"additionalProperties":false
			}`,
			func(args map[string]any, u *orca.Update) {
				u.Phase = orca.PhasePlanning
				u.Status = "complete"
				u.Title, _ = args["title"].(string)
				u.Details = readStringArray(args, "details")
			}),

		reportTool(asAgent, "report_diff_ready",
			"Post a 'diff ready' milestone. Call this after code edits + tests pass, before handing back to the orchestrator.",
			`{
				"type":"object",
				"properties":{
					"title":{"type":"string","description":"One-line outcome (e.g. 'added Welcome() + tests')."},
					"attempt":{"type":"integer","description":"Attempt number from STATUS.json."},
					"details":{"type":"array","items":{"type":"string"},"description":"Up to 5 bullets describing files touched and their impact."},
					"files_changed":{"type":"integer"},
					"tests":{"type":"string","description":"Test result summary (e.g. '7 passed')."}
				},
				"required":["title","attempt"],
				"additionalProperties":false
			}`,
			func(args map[string]any, u *orca.Update) {
				u.Phase = orca.PhaseImplementing
				u.Status = "complete"
				u.Severity = orca.UpdateSuccess
				u.Title, _ = args["title"].(string)
				u.Details = readStringArray(args, "details")
				u.Metrics = map[string]string{}
				if attempt, ok := args["attempt"].(float64); ok {
					u.Metrics["attempt"] = fmt.Sprintf("%d", int(attempt))
				}
				if files, ok := args["files_changed"].(float64); ok {
					u.Metrics["files_changed"] = fmt.Sprintf("%d", int(files))
				}
				if tests, ok := args["tests"].(string); ok && tests != "" {
					u.Metrics["tests"] = tests
				}
			}),

		reportTool(asAgent, "report_verdict",
			"Post the QA verdict for a review attempt. One post per attempt — approved or concern.",
			`{
				"type":"object",
				"properties":{
					"decision":{"type":"string","enum":["approved","concern"]},
					"attempt":{"type":"integer"},
					"summary":{"type":"string","description":"One-line summary of the verdict."},
					"details":{"type":"array","items":{"type":"string"},"description":"Up to 5 supporting bullets (tests run, concerns cited, etc.)."}
				},
				"required":["decision","attempt","summary"],
				"additionalProperties":false
			}`,
			func(args map[string]any, u *orca.Update) {
				u.Phase = orca.PhaseReviewing
				decision, _ := args["decision"].(string)
				u.Title = args["summary"].(string)
				u.Details = readStringArray(args, "details")
				u.Metrics = map[string]string{}
				if attempt, ok := args["attempt"].(float64); ok {
					u.Metrics["attempt"] = fmt.Sprintf("%d", int(attempt))
				}
				if decision == "approved" {
					u.Status = "complete"
					u.Severity = orca.UpdateSuccess
					u.Title = "approved — " + u.Title
				} else {
					u.Severity = orca.UpdateWarn
					u.Title = "concern — " + u.Title
				}
			}),

		reportTool(asAgent, "report_done",
			"Post the final 'task complete' milestone. Orchestrators call this just before close_task.",
			`{
				"type":"object",
				"properties":{
					"title":{"type":"string","description":"One-line final outcome."},
					"metrics":{"type":"object","additionalProperties":{"type":"string"},"description":"Optional metrics (attempts, files_changed, etc.)."}
				},
				"required":["title"],
				"additionalProperties":false
			}`,
			func(args map[string]any, u *orca.Update) {
				u.Phase = orca.PhaseDone
				u.Status = "complete"
				u.Severity = orca.UpdateSuccess
				u.Title, _ = args["title"].(string)
				u.Metrics = readStringMap(args, "metrics")
			}),

		reportTool(asAgent, "report_blocked",
			"Post a 'blocked' milestone with the reason. Use when a task cannot progress (qa concern, plan ambiguity, external dependency).",
			`{
				"type":"object",
				"properties":{
					"title":{"type":"string","description":"One-line 'what's blocked'."},
					"reason":{"type":"string","description":"Short explanation; included as a detail bullet."},
					"details":{"type":"array","items":{"type":"string"},"description":"Up to 5 additional bullets."}
				},
				"required":["title"],
				"additionalProperties":false
			}`,
			func(args map[string]any, u *orca.Update) {
				u.Phase = orca.PhaseBlocked
				u.Severity = orca.UpdateWarn
				u.Title, _ = args["title"].(string)
				details := readStringArray(args, "details")
				if reason, ok := args["reason"].(string); ok && reason != "" {
					details = append([]string{reason}, details...)
				}
				u.Details = details
			}),

		reportTool(asAgent, "report_info",
			"Post a generic info update. Use for conversational replies to humans and for status notes that don't fit another verb.",
			`{
				"type":"object",
				"properties":{
					"title":{"type":"string"},
					"details":{"type":"array","items":{"type":"string"}}
				},
				"required":["title"],
				"additionalProperties":false
			}`,
			func(args map[string]any, u *orca.Update) {
				u.Phase = orca.PhaseInfo
				u.Title, _ = args["title"].(string)
				u.Details = readStringArray(args, "details")
			}),
		{
			Name: "ask_human",
			Description: `Escalate a decision to a human via a bridge agent (e.g. slack).
The framework renders the post from structured slots — do NOT embed pleasantries; fill only what's asked.

Use ONLY when:
- the decision has irreversible/costly consequences you can't validate locally
- the answer depends on business context not in the code/plan
- the decision falls outside the acceptance criteria of your task

Do NOT use for:
- answers that are in the code or tests
- stylistic choices
- clarifications covered by your role's fallback behavior

Each call counts against rate limits. Framework will reject calls over the limit — do not retry in a loop.`,
			InputSchema: mcpserver.MustSchema(`{
				"type":"object",
				"properties":{
					"question":{"type":"string","description":"One-line question. No greeting, no preamble."},
					"options":{"type":"array","items":{"type":"string"},"description":"2-5 numbered choices. Each ≤120 chars."},
					"context":{"type":"array","items":{"type":"string"},"description":"Up to 5 terse bullets the human needs to decide. Each ≤120 chars."},
					"severity":{"type":"string","enum":["low","medium","high","critical"],"default":"medium"},
					"timeout_seconds":{"type":"integer","description":"How long to wait. Default 1800 (30m)."},
					"default_option":{"type":"integer","description":"1-indexed option applied on timeout. Omit to surface timeout explicitly."},
					"task_id":{"type":"string","description":"Optional task correlation id."},
					"channel":{"type":"string","description":"Bridge-specific channel hint (e.g. slack channel name)."},
					"bridge_agent_id":{"type":"string","description":"Which bridge virtual-agent to route through. Defaults to 'slack'."}
				},
				"required":["question"],
				"additionalProperties":false
			}`),
			Handler: func(ctx context.Context, args map[string]any) (string, error) {
				payload := map[string]any{}
				for k, v := range args {
					payload[k] = v
				}
				payload["agent_id"] = *asAgent
				buf, _ := json.Marshal(payload)
				resp, err := http.Post(daemonAddr()+"/decisions", "application/json", bytes.NewReader(buf))
				if err != nil {
					return "", err
				}
				defer resp.Body.Close()
				body, _ := io.ReadAll(resp.Body)
				if resp.StatusCode >= 400 {
					return "", fmt.Errorf("ask_human failed: %s", string(body))
				}
				return fmt.Sprintf("decision queued — answer will arrive as a correlated response message. raw: %s", string(body)), nil
			},
		},
		{
			Name:        "open_task",
			Description: "Open a new task. Allocates a git worktree off `repo_root` (detached from base_ref, default HEAD) and a shared artifact directory at `.orca/<task_id>/`. When `summary` is provided, the bridge agent (slack by default) will auto-post a styled top-level announcement and all subsequent pipeline updates with `correlation_id=<task_id>` thread beneath it. When `user_correlation_id` is also provided, a pointer link to that announcement drops into the user's original conversation thread.",
			InputSchema: mcpserver.MustSchema(`{
				"type":"object",
				"properties":{
					"id":{"type":"string","description":"Explicit task id. Omit to generate one."},
					"repo_root":{"type":"string","description":"Absolute path to the main repo. Defaults to the current working directory if omitted."},
					"base_ref":{"type":"string","description":"Git ref to base the worktree on. Defaults to HEAD."},
					"summary":{"type":"string","description":"One-line description of the task. Shown on the Slack announcement."},
					"user_correlation_id":{"type":"string","description":"The correlation_id of the user's original conversation thread. A pointer-link post drops here so the user can jump to the task thread."},
					"bridge_agent_id":{"type":"string","description":"Which bridge to announce through. Defaults to 'slack'."},
					"announce_channel":{"type":"string","description":"Bridge-specific channel override. Defaults to the bridge's configured default."}
				},
				"additionalProperties":false
			}`),
			Handler: func(ctx context.Context, args map[string]any) (string, error) {
				id, _ := args["id"].(string)
				repoRoot, _ := args["repo_root"].(string)
				baseRef, _ := args["base_ref"].(string)
				summary, _ := args["summary"].(string)
				userCorr, _ := args["user_correlation_id"].(string)
				bridge, _ := args["bridge_agent_id"].(string)
				announceCh, _ := args["announce_channel"].(string)
				if repoRoot == "" {
					if wd, err := os.Getwd(); err == nil {
						repoRoot = wd
					}
				}
				// Resolve relative paths against the shim's own cwd (the
				// agent's workdir). The daemon would otherwise abs against
				// ITS cwd — usually the orca repo root — which silently
				// opens the task in the wrong repo.
				if repoRoot != "" && !filepath.IsAbs(repoRoot) {
					if abs, err := filepath.Abs(repoRoot); err == nil {
						repoRoot = abs
					}
				}
				payload := map[string]any{
					"id":                  id,
					"repo_root":           repoRoot,
					"base_ref":            baseRef,
					"summary":             summary,
					"opened_by":           *asAgent,
					"user_correlation_id": userCorr,
					"bridge_agent_id":     bridge,
					"announce_channel":    announceCh,
				}
				buf, _ := json.Marshal(payload)
				resp, err := http.Post(daemonAddr()+"/tasks", "application/json", bytes.NewReader(buf))
				if err != nil {
					return "", err
				}
				defer resp.Body.Close()
				body, _ := io.ReadAll(resp.Body)
				if resp.StatusCode >= 400 {
					return "", fmt.Errorf("open_task failed: %s", string(body))
				}
				return string(body), nil
			},
		},
		{
			Name:        "close_task",
			Description: "Close a task. Removes the git worktree by default (remove_worktree=true). Artifact directory at .orca/<task_id>/ is always preserved for post-hoc inspection.",
			InputSchema: mcpserver.MustSchema(`{
				"type":"object",
				"properties":{
					"task_id":{"type":"string"},
					"remove_worktree":{"type":"boolean","default":true}
				},
				"required":["task_id"],
				"additionalProperties":false
			}`),
			Handler: func(ctx context.Context, args map[string]any) (string, error) {
				id, _ := args["task_id"].(string)
				if id == "" {
					return "", fmt.Errorf("task_id required")
				}
				remove := true
				if v, ok := args["remove_worktree"].(bool); ok {
					remove = v
				}
				url := daemonAddr() + "/tasks/" + id
				if !remove {
					url += "?remove_worktree=false"
				}
				req, _ := http.NewRequest("DELETE", url, nil)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					return "", err
				}
				defer resp.Body.Close()
				body, _ := io.ReadAll(resp.Body)
				if resp.StatusCode >= 400 {
					return "", fmt.Errorf("close_task failed: %s", string(body))
				}
				return string(body), nil
			},
		},
		{
			Name:        "spawn_agent",
			Description: "Spawn a child agent with the given spec. The new agent's parent_id is automatically set to this agent. The caller must have can_spawn=true. Returns the new agent's AgentInfo on success.",
			InputSchema: mcpserver.MustSchema(`{
				"type":"object",
				"properties":{
					"id":{"type":"string","description":"Globally unique agent id"},
					"role":{"type":"string"},
					"runtime":{"type":"string","description":"Runtime name. Defaults to claude-code-local."},
					"tags":{"type":"array","items":{"type":"string"}},
					"system_prompt":{"type":"string"},
					"workdir":{"type":"string"},
					"task_id":{"type":"string","description":"If set, the child inherits the task's worktree as workdir."},
					"initial_prompt":{"type":"string"},
					"on_parent_exit":{"type":"string","enum":["orphan","kill"],"default":"orphan"},
					"can_spawn":{"type":"boolean","description":"Whether this child may further spawn. Defaults to false."}
				},
				"required":["id","role"],
				"additionalProperties":true
			}`),
			Handler: func(ctx context.Context, args map[string]any) (string, error) {
				// The shim is running as *asAgent. Stamp parent_id so the daemon
				// validates permission and builds the parent→children edge.
				args["parent_id"] = *asAgent
				buf, _ := json.Marshal(args)
				resp, err := http.Post(daemonAddr()+"/agents", "application/json", bytes.NewReader(buf))
				if err != nil {
					return "", err
				}
				defer resp.Body.Close()
				body, _ := io.ReadAll(resp.Body)
				if resp.StatusCode >= 400 {
					return "", fmt.Errorf("spawn_agent failed: %s", string(body))
				}
				return string(body), nil
			},
		},
		{
			Name:        "kill_agent",
			Description: "Terminate an agent by id. Only permitted when the target is a direct child of this agent. Returns on success.",
			InputSchema: mcpserver.MustSchema(`{
				"type":"object",
				"properties":{"id":{"type":"string"}},
				"required":["id"],
				"additionalProperties":false
			}`),
			Handler: func(ctx context.Context, args map[string]any) (string, error) {
				id, _ := args["id"].(string)
				if id == "" {
					return "", fmt.Errorf("id required")
				}
				// Verify the target is this agent's child before issuing kill.
				resp, err := http.Get(daemonAddr() + "/agents/" + id)
				if err != nil {
					return "", err
				}
				var info orca.AgentInfo
				_ = json.NewDecoder(resp.Body).Decode(&info)
				resp.Body.Close()
				if info.Spec.ParentID != *asAgent {
					return "", fmt.Errorf("not authorized: %s is not your child (parent=%s)", id, info.Spec.ParentID)
				}
				req, _ := http.NewRequest("DELETE", daemonAddr()+"/agents/"+id, nil)
				killResp, err := http.DefaultClient.Do(req)
				if err != nil {
					return "", err
				}
				defer killResp.Body.Close()
				if killResp.StatusCode >= 400 {
					body, _ := io.ReadAll(killResp.Body)
					return "", fmt.Errorf("kill failed: %s", string(body))
				}
				return fmt.Sprintf("killed %s", id), nil
			},
		},
		{
			Name:        "list_tasks",
			Description: "List all currently-open tasks, their phases, worktree paths, and participating agent ids.",
			InputSchema: mcpserver.MustSchema(`{"type":"object","properties":{},"additionalProperties":false}`),
			Handler: func(ctx context.Context, args map[string]any) (string, error) {
				resp, err := http.Get(daemonAddr() + "/tasks")
				if err != nil {
					return "", err
				}
				defer resp.Body.Close()
				body, _ := io.ReadAll(resp.Body)
				if resp.StatusCode >= 400 {
					return "", fmt.Errorf("list_tasks failed: %s", string(body))
				}
				return string(body), nil
			},
		},
		{
			Name:        "find_agents",
			Description: "Find agents by tags. Returns every agent whose tag set contains ALL of the given tags (AND semantics). Pass an empty list or omit `tags` to list all agents. Use this to discover who is available before calling send_message with tag-based routing.",
			InputSchema: mcpserver.MustSchema(`{
				"type":"object",
				"properties":{
					"tags":{"type":"array","items":{"type":"string"},"description":"Tags that matching agents must all possess"}
				},
				"additionalProperties":false
			}`),
			Handler: func(ctx context.Context, args map[string]any) (string, error) {
				var tags []string
				if raw, ok := args["tags"].([]any); ok {
					for _, v := range raw {
						if s, ok := v.(string); ok && s != "" {
							tags = append(tags, s)
						}
					}
				}
				path := "/agents"
				if len(tags) > 0 {
					path += "?tags=" + strings.Join(tags, ",")
				}
				resp, err := http.Get(daemonAddr() + path)
				if err != nil {
					return "", err
				}
				defer resp.Body.Close()
				var agents []orca.AgentInfo
				if err := json.NewDecoder(resp.Body).Decode(&agents); err != nil {
					return "", err
				}
				summary := make([]map[string]any, 0, len(agents))
				for _, a := range agents {
					summary = append(summary, map[string]any{
						"id":     a.Spec.ID,
						"role":   a.Spec.Role,
						"tags":   a.Spec.Tags,
						"status": a.Status,
					})
				}
				out, _ := json.MarshalIndent(summary, "", "  ")
				return string(out), nil
			},
		},
	}

	srv := mcpserver.New("orca-comms", "0.1.0", tools)
	return srv.Run(context.Background())
}
