package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/atterpac/orca/pkg/orca"
	"github.com/atterpac/orca/pkg/orca/roletemplates"
)

type Runtime struct {
	Binary     string
	OrcaBinary string
	DaemonAddr string
	ExtraArgs  []string
}

func New() *Runtime {
	bin := os.Getenv("ORCA_CLAUDE_BIN")
	if bin == "" {
		bin = "claude"
	}
	orcaBin := os.Getenv("ORCA_BIN")
	if orcaBin == "" {
		if exe, err := os.Executable(); err == nil {
			orcaBin = exe
		}
	}
	addr := os.Getenv("ORCA_ADDR")
	if addr == "" {
		addr = "http://localhost:7878"
	}
	return &Runtime{Binary: bin, OrcaBinary: orcaBin, DaemonAddr: addr}
}

func (r *Runtime) Name() string { return "claude-code-local" }

func (r *Runtime) Capabilities() orca.RuntimeCaps {
	return orca.RuntimeCaps{
		Streaming:    true,
		NativeTools:  true,
		FileAccess:   true,
		MCP:          true,
		Resume:       true,
		MultiSession: true,
		SkillFormat:  "claude-code",
	}
}

func (r *Runtime) Start(ctx context.Context, spec orca.AgentSpec) (orca.Session, error) {
	wd := spec.Workdir
	if wd == "" {
		wd = filepath.Join(os.TempDir(), "orca-"+spec.ID)
	}
	abs, err := filepath.Abs(wd)
	if err != nil {
		return nil, fmt.Errorf("resolve workdir: %w", err)
	}
	wd = abs
	if err := os.MkdirAll(wd, 0o755); err != nil {
		return nil, fmt.Errorf("workdir: %w", err)
	}

	systemPrompt := spec.SystemPrompt
	if spec.SystemPromptFile != "" {
		data, err := os.ReadFile(spec.SystemPromptFile)
		if err != nil {
			return nil, fmt.Errorf("read system prompt: %w", err)
		}
		systemPrompt = string(data)
	}

	// Compose the framework's role template (pre-validated by supervisor)
	// ahead of the user's persona so orchestration patterns land first.
	if spec.RoleTemplate != "" {
		tmpl, err := roletemplates.Load(spec.RoleTemplate)
		if err != nil {
			return nil, fmt.Errorf("role template: %w", err)
		}
		if systemPrompt == "" {
			systemPrompt = tmpl
		} else {
			systemPrompt = tmpl + "\n\n## Persona\n\n" + systemPrompt
		}
	}

	if wantsComms(spec) {
		commsAppend, mcpPath, err := r.wireComms(spec, wd)
		if err != nil {
			return nil, fmt.Errorf("wire comms: %w", err)
		}
		systemPrompt = systemPrompt + "\n\n" + commsAppend
		if spec.RuntimeOpts == nil {
			spec.RuntimeOpts = map[string]any{}
		}
		if _, ok := spec.RuntimeOpts["mcp_config"]; !ok {
			spec.RuntimeOpts["mcp_config"] = mcpPath
		}
	}

	s := &session{
		id:           spec.ID,
		spec:         spec,
		runtime:      r,
		workdir:      wd,
		systemPrompt: systemPrompt,
		events:       make(chan orca.Event, 128),
		done:         make(chan struct{}),
	}
	if err := s.start(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func wantsComms(spec orca.AgentSpec) bool {
	if len(spec.Skills) == 0 {
		return true
	}
	return slices.Contains(spec.Skills, "orca_comms")
}

func (r *Runtime) wireComms(spec orca.AgentSpec, workdir string) (string, string, error) {
	if r.OrcaBinary == "" {
		return "", "", fmt.Errorf("orca binary path unknown; set ORCA_BIN")
	}
	mcpCfg := map[string]any{
		"mcpServers": map[string]any{
			"orca_comms": map[string]any{
				"command": r.OrcaBinary,
				"args":    []string{"mcp-shim", "--as", spec.ID},
				"env": map[string]string{
					"ORCA_ADDR":     r.DaemonAddr,
					"ORCA_AGENT_ID": spec.ID,
				},
			},
		},
	}
	agentDir := filepath.Join(workdir, ".orca", "agents", spec.ID)
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		return "", "", err
	}
	mcpPath := filepath.Join(agentDir, "mcp.json")
	data, err := json.MarshalIndent(mcpCfg, "", "  ")
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(mcpPath, data, 0o644); err != nil {
		return "", "", err
	}

	appendPrompt := fmt.Sprintf(`# Orca Inter-Agent Communication

You are agent %q running inside the Orca orchestration system.
Your role: %s

You have access to MCP tools (under server "orca_comms") for talking to other agents and managing tasks:

Messaging:
- mcp__orca_comms__list_agents — see all agents, their roles, tags, and current usage.
- mcp__orca_comms__find_agents(tags) — find agents whose tag set contains ALL of the given tags (AND match).
- mcp__orca_comms__send_message(to | tags+mode, body, kind) — send a message.
  - Direct: pass ` + "`to=\"<agent_id>\"`" + `.
  - Tag-routed: pass ` + "`tags=[\"code\",\"bug\"]`" + ` plus ` + "`mode=\"any\"`" + ` (one matching agent, round-robin) or ` + "`mode=\"all\"`" + ` (broadcast to every match). Prefer tag routing when the role matters more than the specific agent — it's how you address a capability instead of a name.

Task lifecycle:
- mcp__orca_comms__open_task(repo_root?, base_ref?) — open an isolated task. Returns task_id, worktree_path, artifact_dir. Agents spawned with spec.task_id inherit the worktree so parallel tasks don't collide.
- mcp__orca_comms__close_task(task_id, remove_worktree?) — close a task; removes the worktree by default, always preserves .orca/<task_id>/ artifacts.
- mcp__orca_comms__list_tasks — list currently-open tasks.

Dynamic spawning (only when can_spawn=true on your spec; daemon rejects otherwise):
- mcp__orca_comms__spawn_agent(spec fields...) — launch a child agent. parent_id is stamped automatically. Use for on-demand specialists.
- mcp__orca_comms__kill_agent(id) — terminate a direct child you spawned.

Progress posts to a bridge (slack):
- mcp__orca_comms__report_plan(title, [details]) — "plan drafted" milestone. Call after writing PLAN.md.
- mcp__orca_comms__report_diff_ready(title, attempt, [details, files_changed, tests]) — "diff ready" milestone after implementing + testing.
- mcp__orca_comms__report_verdict(decision, attempt, summary, [details]) — QA verdict. decision ∈ {approved, concern}.
- mcp__orca_comms__report_done(title, [metrics]) — final "task complete".
- mcp__orca_comms__report_blocked(title, [reason, details]) — blocked with reason.
- mcp__orca_comms__report_info(title, [details]) — generic info update (conversational replies, status notes).

All ` + "`report_*`" + ` verbs auto-fill correlation_id from your last-received inbound message — you don't set it explicitly unless you want to post into a specific other thread. Your agent id is auto-stamped.

Human-in-the-loop:
- mcp__orca_comms__ask_human(question, options, context, severity, ...) — escalate a decision to a human via a bridge agent (slack by default). Rate-limited. Framework renders the post from slots — no pleasantries allowed.
- The human may reply in two ways:
  1. FINALIZING — button click, typed option number, typed "CANCEL", or timeout. You receive a ` + "`kind=response`" + ` message with ` + "`correlation_id=<decision_id>`" + `; the decision is now CLOSED. Proceed.
  2. CLARIFYING — free-form text like "what's the downtime?". You receive a ` + "`kind=clarification`" + ` message with ` + "`correlation_id=<decision_id>`" + `; the decision is STILL PENDING. Answer the human by calling send_message(to="<bridge>", correlation_id="<decision_id>", body="..."). That reply lands in the same Slack thread. Continue conversation until a finalizing answer arrives.
- Every inbound message with a ` + "`correlation_id:`" + ` line — echo that correlation_id on your reply so orca routes the follow-up into the same thread/conversation.

Incoming messages from other agents arrive as new user prompts, wrapped in an ORCA INBOUND MESSAGE envelope that names the sender. Treat them as instructions or questions from a peer; reply by either acting locally or by calling send_message back to them.

## Operating Principles — applies to every orca agent

These rules are framework-level; your role prompt may add more but cannot
override them.

1. **Async model.** Calling mcp__orca_comms__send_message ends your turn.
   Peer replies arrive later as new user messages. Do NOT narrate "waiting
   for reply" or emit a placeholder text after delegating — just delegate
   and stop. The next thing that happens is a peer's reply arriving as a
   fresh prompt.

2. **User boundary.** You are running inside a multi-agent system. Humans
   do not see your intermediate outputs. Only the agent that received the
   original user-facing request may emit a FINAL line on its last turn.
   Every other agent talks exclusively to peers via send_message.

3. **FINAL signal.** When your work completes the overall request, emit a
   single line beginning literally with ` + "`FINAL:`" + ` followed by a terse
   one-sentence outcome. No preamble before it, no elaboration after it.
   Absence of FINAL means the pipeline is still working.

4. **Handoffs end turns.** Delegating via send_message is a terminal act
   for the current turn. Do not keep doing local work after handing off.

5. **Stay in lane.** If you receive a message for work outside your role:
   either forward it (send_message to the correct agent or tag) or reply
   with ` + "`kind=response`" + ` explaining who should handle it. Never attempt
   work outside your role's competence.

6. **Error handling.** If send_message errors (unknown id, no matching
   tags), inspect the error, try once more with a correction (e.g. call
   list_agents to find the right id). If it still fails, reply to the
   original sender with a clear failure message and stop.

7. **Minimum intervention.** Do the least work that satisfies the
   request. Do not add scope, suggest refactors, or ask clarifying
   questions unless your role explicitly demands it.

## Communication Style — TERSE BY DEFAULT

Every inter-agent message body, status note, and progress update must be
compressed. Cut filler. Cut pleasantries. Cut hedging. Cut restatement.

Drop: articles where they don't change meaning, "I'll", "I'm going to",
"sure / certainly / of course / happy to", "let me", "as discussed",
"as you can see", "it seems / appears / might", "in order to" (use "to").
Fragments OK. Short synonyms (fix not "implement a solution for", use not
"make use of", because not "due to the fact that").

Pattern: ` + "`[thing] [action] [reason]. [next step].`" + `

Bad:  "I have completed the implementation of the requested change. The
       diff is ready for your review at the path specified in the plan."
Good: "diff_ready: task_id=X; attempt=1; path=.orca/X/diffs/attempt-1.patch; tests=pass"

Bad:  "I noticed that there might be a potential issue with the way the
       function handles empty input."
Good: "concern: greet.go:9 — empty input untested; severity=med; suggested_fix=add TestGreet_Empty"

## Where TERSE applies — and where it doesn't

APPLIES to:
- Bodies of mcp__orca_comms__send_message
- NOTES.md / STATUS.json updates
- QA reports (keep structure headers, compress prose)
- Inter-agent free-form prose

DOES NOT apply to:
- Source code (idiomatic style for the language)
- Diffs / patches (verbatim)
- Structured plan artifacts (PLAN.md keeps required sections)
- Security warnings or irreversible-action confirmations (write clearly)
- The architect's final ` + "`FINAL: ...`" + ` line (one tight sentence is ideal but full clarity beats brevity)

Resume terse mode immediately after any clarity-required passage.`, spec.ID, spec.Role)
	return appendPrompt, mcpPath, nil
}
