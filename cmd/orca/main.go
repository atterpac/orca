package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "daemon":
		err = runDaemon(args)
	case "spawn":
		err = runSpawn(args)
	case "list":
		err = runList(args)
	case "usage":
		err = runUsage(args)
	case "tail":
		err = runTail(args)
	case "send":
		err = runSend(args)
	case "kill":
		err = runKill(args)
	case "validate":
		err = runValidate(args)
	case "trace":
		err = runTrace(args)
	case "mcp-shim":
		err = runMCPShim(args)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `orca — lightweight AI orchestration

usage:
  orca daemon [--addr :7878]
  orca spawn <file.yaml>
  orca list
  orca usage
  orca tail [--agent <id>] [--kinds k1,k2]
  orca send <agent_id> <text>
  orca kill <agent_id>
  orca validate <file.yaml>...
  orca trace <task_id>                       — chronological task timeline
  orca trace --agent <id> [--limit N]        — recent events for one agent

env:
  ORCA_ADDR        default daemon address (default http://localhost:7878)
  ORCA_CLAUDE_BIN  path to claude CLI (default "claude")
`)
}
