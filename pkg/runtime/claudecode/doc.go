// Package claudecode implements the Runtime adapter that drives a
// Claude Code subprocess as an agent's LLM session.
//
// One subprocess per agent. The CLI is invoked with -p (non-interactive),
// --input-format stream-json, --output-format stream-json, --verbose, and
// --append-system-prompt carrying the composed framework + role-template
// + user-persona prompt. The adapter:
//
//   - Composes the system prompt (operating principles → role template
//     → user persona)
//   - Auto-wires the orca_comms MCP server (the orca binary's mcp-shim
//     subcommand) so agents have access to send_message, report_*,
//     ask_human, open_task, etc. without per-agent setup
//   - Resolves workdir to absolute and writes the per-agent MCP config
//     to .orca/agents/<id>/mcp.json so multiple agents in the same
//     workdir don't clobber each other
//   - Pipes stdin/stdout JSONL to/from the subprocess
//   - Parses incoming stream-json events into orca Events
//   - Tracks token usage from the assistant message envelopes and the
//     terminal result event
//
// Outbound user input is wrapped in a parsed envelope (see Send +
// parseBodyForDisplay) so the model receives the user's text as
// primary content with metadata broken out as readable key:value lines
// — important for context retention across multi-turn discussions.
//
// Concurrency: Session is safe to use from multiple goroutines. The
// stdout reader and stderr drainer each run in their own goroutine.
package claudecode
