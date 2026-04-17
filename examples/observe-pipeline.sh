#!/usr/bin/env bash
# observe-pipeline.sh — run the three-agent demo and capture clean logs.
#
# Outputs (all under $LOG_DIR):
#   daemon.log          - orca daemon stderr
#   events.jsonl        - one JSON event per line (filtered for noise)
#   timeline.txt        - human-readable interleaved timeline
#   transcript-<id>.md  - per-agent text transcripts (TokenChunk concatenation)
#   summary.md          - the final flow report
#
# Requires: jq, curl, the orca binary at ./bin/orca

set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$PWD"
ORCA="$ROOT/bin/orca"
LOG_DIR="${LOG_DIR:-/tmp/orca-pipeline}"
ADDR="${ORCA_ADDR:-http://localhost:7878}"
TIMEOUT="${TIMEOUT:-360}"   # seconds to wait for FINAL

mkdir -p "$LOG_DIR"
: > "$LOG_DIR/events.jsonl"
: > "$LOG_DIR/timeline.txt"
: > "$LOG_DIR/transcript-architect.md"
: > "$LOG_DIR/transcript-implementer.md"
: > "$LOG_DIR/transcript-qa.md"

export ORCA_BIN="$ORCA"
export ORCA_ADDR="$ADDR"

# 1. Start daemon (detached) if not running. Daemon writes events directly to file.
if ! curl -fsS "$ADDR/healthz" >/dev/null 2>&1; then
  PORT="${ADDR##*:}"
  nohup "$ORCA" daemon --addr ":${PORT}" --events-log "$LOG_DIR/events.jsonl" \
      > "$LOG_DIR/daemon.log" 2>&1 < /dev/null &
  DAEMON_PID=$!
  disown $DAEMON_PID 2>/dev/null || true
  echo "started daemon pid=$DAEMON_PID on :${PORT}"
  for _ in {1..40}; do
    sleep 0.2
    curl -fsS "$ADDR/healthz" >/dev/null 2>&1 && break
  done
  if ! curl -fsS "$ADDR/healthz" >/dev/null 2>&1; then
    echo "daemon failed to come up; logs:"
    cat "$LOG_DIR/daemon.log"
    exit 1
  fi
fi

# 2. Reset demo repo to clean buggy state.
( cd examples/demo-repo && git reset -q --hard HEAD && rm -rf .orca && git clean -qfd )

# 3. Spawn agents (workers first so list_agents shows them).
"$ORCA" spawn examples/agents/implementer.yaml >/dev/null
"$ORCA" spawn examples/agents/qa.yaml >/dev/null
sleep 1
"$ORCA" spawn examples/agents/architect-with-task.yaml >/dev/null

echo "spawned. polling for FINAL (timeout=${TIMEOUT}s)..."

# 4. Poll until architect emits FINAL or timeout.
START=$(date +%s)
while :; do
  if grep -q '"FINAL' "$LOG_DIR/events.jsonl" 2>/dev/null; then
    echo "FINAL detected"
    break
  fi
  NOW=$(date +%s)
  if (( NOW - START > TIMEOUT )); then
    echo "timeout reached"
    break
  fi
  sleep 2
done

sleep 2

# 7. Build derivative artifacts from events.jsonl.

# 7a. timeline.txt — concise human-readable
jq -r '
  select(.kind != "TokenChunk")
  | [
      (.ts | sub("\\.[0-9]+";"") | sub("T"; " ") | sub("Z";"") | sub(":[0-9][0-9]-.*$";"")),
      .agent_id,
      .kind,
      ( .payload // {}
        | if .tool then "tool=\(.tool)"
          elif .to and .kind then "to=\(.to) kind=\(.kind)"
          elif .to then "to=\(.to)"
          elif .from then "from=\(.from)"
          elif .text then "text=\(.text|tostring|.[0:80])"
          else (tostring|.[0:120])
        end )
    ] | @tsv
' "$LOG_DIR/events.jsonl" > "$LOG_DIR/timeline.txt"

# 7b. per-agent transcripts (concat of TokenChunk text)
for who in architect implementer qa; do
  jq -r --arg w "$who" '
    select(.kind=="TokenChunk" and .agent_id==$w) | .payload.text
  ' "$LOG_DIR/events.jsonl" > "$LOG_DIR/transcript-${who}.md"
done

# 8. Print summary.md to stdout AND save it.
SUMMARY="$LOG_DIR/summary.md"
{
  echo "# Orca Pipeline Run — Summary"
  echo
  echo "_log dir: \`$LOG_DIR\`_"
  echo

  echo "## Token Usage"
  echo '```'
  "$ORCA" usage 2>/dev/null || true
  echo '```'

  TASK_DIR=$(ls examples/demo-repo/.orca/ 2>/dev/null | head -1 || true)
  if [ -n "$TASK_DIR" ]; then
    TP="examples/demo-repo/.orca/$TASK_DIR"
    echo
    echo "## Task ID: \`$TASK_DIR\`"
    echo
    if [ -f "$TP/STATUS.json" ]; then
      echo "### STATUS.json"
      echo '```json'
      cat "$TP/STATUS.json"
      echo
      echo '```'
    fi
    if [ -f "$TP/PLAN.md" ]; then
      echo
      echo "### PLAN.md"
      echo '```markdown'
      cat "$TP/PLAN.md"
      echo '```'
    fi
    if [ -d "$TP/diffs" ]; then
      for p in "$TP"/diffs/*.patch; do
        echo
        echo "### $(basename "$p")"
        echo '```diff'
        cat "$p"
        echo '```'
      done
    fi
    if [ -d "$TP/qa" ]; then
      for q in "$TP"/qa/*.md; do
        echo
        echo "### $(basename "$q") (QA)"
        echo '```markdown'
        cat "$q"
        echo '```'
      done
    fi
    if [ -f "$TP/NOTES.md" ]; then
      echo
      echo "### NOTES.md (implementer log)"
      echo '```markdown'
      cat "$TP/NOTES.md"
      echo '```'
    fi
  fi

  echo
  echo "## Inter-Agent Message Flow"
  echo
  jq -r '
    select(.kind=="MessageSent" or .kind=="MessageDelivered" or .kind=="ToolCallStart")
    | [
        (.ts | sub("\\.[0-9]+";"") | sub("T"; " ") | sub("Z";"") | sub(":[0-9][0-9]-.*$";"")),
        .agent_id,
        .kind,
        ( .payload // {}
          | if .tool then "tool=\(.tool)"
            elif .to and .kind then "to=\(.to) kind=\(.kind)"
            elif .to then "to=\(.to)"
            elif .from then "from=\(.from)"
            else (tostring|.[0:80])
          end )
      ] | @tsv
  ' "$LOG_DIR/events.jsonl"

  echo
  echo "## Architect FINAL Statement"
  grep -o '"text":"FINAL[^"]*"' "$LOG_DIR/events.jsonl" | head -1 | sed 's/"text":"//; s/"$//' || echo "(no FINAL emitted)"

  echo
  echo "## Final State of greet/greet.go"
  echo '```go'
  cat examples/demo-repo/greet/greet.go 2>/dev/null || echo "(file missing)"
  echo '```'

  echo
  echo "## Final go test ./..."
  echo '```'
  ( cd examples/demo-repo && go test ./... 2>&1 ) || true
  echo '```'
} > "$SUMMARY"

cat "$SUMMARY"

echo
echo "---"
echo "Artifacts:"
ls -la "$LOG_DIR"
