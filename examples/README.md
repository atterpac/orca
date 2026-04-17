# Orca POC Demo — Two Agents Talking

This walkthrough proves the end-to-end loop: spawn two Claude Code agents, watch
them exchange messages over the orca channel bus via the auto-injected
`orca_comms` MCP tools, while the daemon tracks token usage per session.

## Prereqs

- `claude` CLI installed and authenticated
- Go 1.25+

## Build

```sh
cd /Users/atterpac/projects/atterpac/orca
go build -o ./bin/orca ./cmd/orca
export PATH="$PWD/bin:$PATH"
export ORCA_BIN="$PWD/bin/orca"
```

## Run the daemon (terminal 1)

```sh
orca daemon --addr :7878
```

## Watch events (terminal 2)

```sh
orca tail
```

## Spawn agents (terminal 3)

```sh
# Researcher first — must be discoverable when coordinator boots.
orca spawn examples/agents/researcher.yaml
orca spawn examples/agents/coordinator.yaml
```

The coordinator's `initial_prompt` will fire automatically. Watch terminal 2 for:

- `AgentSpawned` / `AgentReady` for each agent
- `ToolCallStart` for `mcp__orca_comms__list_agents`
- `ToolCallStart` for `mcp__orca_comms__send_message`
- `MessageDelivered` to the researcher
- `TurnCompleted` events with delta token usage on both agents
- Coordinator emitting `FINAL: ...` summary

## Inspect state

```sh
orca list
orca usage
orca tail --kinds TurnCompleted,MessageDelivered
```

## Send a message manually

```sh
orca send researcher "Name three moons of Jupiter."
```

## Tear down

```sh
orca kill coordinator
orca kill researcher
```

## What the POC proves

- Runtime interface boots Claude Code as a subprocess and parses stream-json.
- Token usage per session is captured (input/output/cache) and aggregated.
- Auto-wired MCP server gives every agent inter-agent comms tools.
- Channel bus routes messages between agents; supervisor delivers them as new
  prompts to the receiving session.
- Event bus fans out to SSE consumers — TUIs/web UIs would attach the same way.
