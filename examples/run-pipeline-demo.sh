#!/usr/bin/env bash
# Drives the architect → implementer → qa pipeline end-to-end against the
# demo repo at examples/demo-repo. Assumes:
#   - bin/orca is built (see PLAN.md)
#   - claude CLI is on PATH and authenticated
#   - daemon is running on :7878 (start with `bin/orca daemon &` if not)

set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$PWD"
ORCA="$ROOT/bin/orca"
export ORCA_BIN="$ORCA"
export ORCA_ADDR="${ORCA_ADDR:-http://localhost:7878}"

if ! curl -fsS "$ORCA_ADDR/healthz" >/dev/null 2>&1; then
  echo "orca daemon not reachable at $ORCA_ADDR — start it with: $ORCA daemon &" >&2
  exit 1
fi

# Reset demo repo to clean buggy state.
( cd examples/demo-repo && git reset -q --hard HEAD && rm -rf .orca && git clean -qfd )

# Spawn workers first so they show up in architect's list_agents call.
"$ORCA" spawn examples/agents/implementer.yaml >/dev/null
"$ORCA" spawn examples/agents/qa.yaml >/dev/null
sleep 1
"$ORCA" spawn examples/agents/architect-with-task.yaml >/dev/null

echo "spawned: architect, implementer, qa"
echo "tail with: $ORCA tail"
echo "usage with: $ORCA usage"
echo "kill all with: $ORCA kill architect; $ORCA kill implementer; $ORCA kill qa"
