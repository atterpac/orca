package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/atterpac/orca/pkg/orca"
)

// interruptGrace is how long we wait between SIGINT and SIGKILL if the
// child hasn't exited after the initial stdin-close grace period.
const interruptGrace = 2 * time.Second

// defaultShutdownGrace is the time we give claude to exit cleanly after
// stdin is closed, before escalating to SIGINT. Override via
// ORCA_SHUTDOWN_GRACE (any Go duration string, e.g. "10s", "500ms").
const defaultShutdownGrace = 5 * time.Second

func shutdownGrace() time.Duration {
	if v := os.Getenv("ORCA_SHUTDOWN_GRACE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultShutdownGrace
}

type session struct {
	id           string
	spec         orca.AgentSpec
	runtime      *Runtime
	workdir      string
	systemPrompt string

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	events chan orca.Event
	done   chan struct{}

	usageMu sync.RWMutex
	usage   orca.TokenUsage

	closed atomic.Bool
	waitErr error
	waitOnce sync.Once
}

func (s *session) ID() string { return s.id }

func (s *session) Usage() orca.TokenUsage {
	s.usageMu.RLock()
	defer s.usageMu.RUnlock()
	return s.usage
}

func (s *session) start(ctx context.Context) error {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
	}
	if s.systemPrompt != "" {
		args = append(args, "--append-system-prompt", s.systemPrompt)
	}
	if mcpCfg, ok := s.spec.RuntimeOpts["mcp_config"].(string); ok && mcpCfg != "" {
		args = append(args, "--mcp-config", mcpCfg)
	}
	if model, ok := s.spec.RuntimeOpts["model"].(string); ok && model != "" {
		args = append(args, "--model", model)
	}
	if perm, ok := s.spec.RuntimeOpts["permission_mode"].(string); ok && perm != "" {
		args = append(args, "--permission-mode", perm)
	}
	if resume, ok := s.spec.RuntimeOpts["resume_session_id"].(string); ok && resume != "" {
		args = append(args, "--resume", resume)
	}
	args = append(args, s.runtime.ExtraArgs...)

	cmd := exec.CommandContext(ctx, s.runtime.Binary, args...)
	cmd.Dir = s.workdir

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start claude: %w", err)
	}

	s.cmd = cmd
	s.stdin = stdin
	s.stdout = stdout
	s.stderr = stderr

	go s.readStdout()
	go s.drainStderr()
	go s.waitProc()

	s.emit(orca.Event{Kind: orca.EvtAgentSpawned, AgentID: s.id, Payload: map[string]any{
		"runtime": s.runtime.Name(),
		"role":    s.spec.Role,
	}})

	if s.spec.InitialPrompt != "" {
		if err := s.sendText(s.spec.InitialPrompt); err != nil {
			return fmt.Errorf("send initial prompt: %w", err)
		}
	}
	return nil
}

func (s *session) Send(ctx context.Context, m orca.Message) error {
	if s.closed.Load() {
		return errors.New("session closed")
	}
	var text string
	if len(m.Body) > 0 {
		var asString string
		if err := json.Unmarshal(m.Body, &asString); err == nil {
			text = asString
		} else {
			text = string(m.Body)
		}
	}
	from := m.From
	if from == "" {
		from = "unknown"
	}

	// Try to surface the human-readable content cleanly. When the body is
	// JSON with a "text" field (slack message, clarification, etc.), put
	// that text as the primary content and break out other useful metadata
	// (responder_name, decision answer, etc.) into a separate metadata
	// block. Otherwise pass the body through verbatim.
	primary, metadata := parseBodyForDisplay(text)

	var corrLine, contLine, replyHint string
	if m.CorrelationID != "" {
		corrLine = fmt.Sprintf("correlation_id: %s\n", m.CorrelationID)
		contLine = fmt.Sprintf("This message belongs to an ongoing conversation (correlation_id=%s). Earlier messages with the same correlation_id are part of the same exchange — use them for context when interpreting this one.\n\n", m.CorrelationID)
		replyHint = fmt.Sprintf(" Auto-correlation will thread your reply back into this conversation; you don't need to set correlation_id explicitly unless you want to break out into a new thread.")
	}

	wrapped := fmt.Sprintf(
		"--- ORCA INBOUND MESSAGE ---\nfrom: %s\nkind: %s\n%s%s%s--- END MESSAGE ---\n%sReply via mcp__orca_comms__send_message (to=%q) or one of the report_* verbs.%s",
		from, m.Kind, corrLine, metadata, primary, contLine, from, replyHint,
	)
	return s.sendText(wrapped)
}

// parseBodyForDisplay returns (primary, metadata). When body is JSON
// with a recognized shape, primary is the user-readable text and
// metadata is a "key: value\n" block for everything else. When body
// isn't JSON or has no recognizable text field, primary = body and
// metadata is empty.
func parseBodyForDisplay(body string) (primary, metadata string) {
	body = strings.TrimSpace(body)
	if body == "" {
		return "(empty)", ""
	}
	// Only attempt to parse JSON-shaped bodies.
	if !strings.HasPrefix(body, "{") {
		return "text:\n" + body + "\n", ""
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return "text:\n" + body + "\n", ""
	}

	// Find the primary text field. Prefer specific keys.
	var textVal string
	for _, k := range []string{"text", "summary", "question"} {
		if v, ok := m[k].(string); ok && v != "" {
			textVal = v
			delete(m, k)
			break
		}
	}
	// Decision-answer shape has body.answer.text or body.answer.option.
	if textVal == "" {
		if ans, ok := m["answer"].(map[string]any); ok {
			if t, ok := ans["text"].(string); ok && t != "" {
				textVal = t
			} else if opt, ok := ans["option"].(float64); ok {
				textVal = fmt.Sprintf("(picked option %d)", int(opt))
			}
		}
	}

	primary = "text:\n" + textVal + "\n"
	if textVal == "" {
		// Couldn't extract — fall back to JSON dump as the primary.
		raw, _ := json.MarshalIndent(m, "", "  ")
		primary = "body:\n" + string(raw) + "\n"
		return primary, ""
	}

	// Build metadata from remaining keys, skipping noisy/internal fields.
	skip := map[string]bool{
		"slack_channel": true, "slack_thread_ts": true,
		"decision_id": true,
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if skip[k] {
			continue
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return primary, ""
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		v := m[k]
		switch vv := v.(type) {
		case string:
			if vv == "" {
				continue
			}
			b.WriteString(fmt.Sprintf("%s: %s\n", k, vv))
		case nil:
			continue
		default:
			raw, _ := json.Marshal(vv)
			b.WriteString(fmt.Sprintf("%s: %s\n", k, string(raw)))
		}
	}
	return primary, b.String()
}

func (s *session) sendText(text string) error {
	line, err := encodeUserInput(text)
	if err != nil {
		return err
	}
	if _, err := s.stdin.Write(line); err != nil {
		return err
	}
	s.emit(orca.Event{Kind: orca.EvtPromptSubmitted, AgentID: s.id, Payload: map[string]any{
		"bytes": len(line),
	}})
	return nil
}

func (s *session) Events(ctx context.Context) (<-chan orca.Event, error) {
	return s.events, nil
}

func (s *session) Interrupt(ctx context.Context) error {
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	return s.cmd.Process.Signal(interruptSignal())
}

func (s *session) Wait() error {
	<-s.done
	return s.waitErr
}

// Close initiates graceful shutdown and returns immediately. The actual
// shutdown sequence runs in a background goroutine so callers (supervisor
// Shutdown, multiSession sweeper, etc.) don't block on per-agent grace
// windows. Sequence: close stdin → wait grace → SIGINT → wait
// interruptGrace → SIGKILL. Any stage completes early if the process
// exits on its own (s.done closes via waitProc).
func (s *session) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		go s.shutdownSequence(shutdownGrace())
	}
	return nil
}

func (s *session) shutdownSequence(grace time.Duration) {
	// Stage 1: give claude room to flush and exit on its own after
	// stdin EOF. Most well-behaved shutdowns land here.
	select {
	case <-s.done:
		return
	case <-time.After(grace):
	}
	// Stage 2: SIGINT (Ctrl-C equivalent). Claude Code's streaming loop
	// generally honors this.
	_ = s.cmd.Process.Signal(interruptSignal())
	select {
	case <-s.done:
		return
	case <-time.After(interruptGrace):
	}
	// Stage 3: hard kill.
	_ = s.cmd.Process.Kill()
}

func (s *session) emit(e orca.Event) {
	if e.AgentID == "" {
		e.AgentID = s.id
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	select {
	case s.events <- e:
	default:
	}
}

func (s *session) updateUsage(u ccUsage, costUSD float64, turnDelta uint64) (orca.TokenUsage, orca.TokenUsage) {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	delta := orca.TokenUsage{
		InputTokens:         u.InputTokens,
		OutputTokens:        u.OutputTokens,
		CacheCreationTokens: u.CacheCreationInputTokens,
		CacheReadTokens:     u.CacheReadInputTokens,
		Turns:               turnDelta,
		CostUSD:             costUSD,
		LastUpdated:         time.Now(),
	}
	s.usage = s.usage.Add(delta)
	return delta, s.usage
}

func (s *session) readStdout() {
	scanner := bufio.NewScanner(s.stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var env ccEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			continue
		}
		s.handleEnvelope(env, line)
	}
}

func (s *session) handleEnvelope(env ccEnvelope, raw []byte) {
	switch env.Type {
	case "system":
		if env.Subtype == "init" {
			s.emit(orca.Event{Kind: orca.EvtAgentReady, AgentID: s.id, Payload: map[string]any{
				"session_id": env.SessionID,
			}})
		}
	case "assistant":
		var msg ccAssistantMsg
		if err := json.Unmarshal(env.Message, &msg); err == nil {
			for _, c := range msg.Content {
				switch c.Type {
				case "text":
					if c.Text != "" {
						s.emit(orca.Event{Kind: orca.EvtTokenChunk, AgentID: s.id, Payload: map[string]any{
							"text": c.Text,
						}})
					}
				case "tool_use":
					s.emit(orca.Event{Kind: orca.EvtToolCallStart, AgentID: s.id, Payload: map[string]any{
						"tool": c.Name,
						"id":   c.ID,
					}})
				}
			}
			if msg.Usage != nil {
				delta, total := s.updateUsage(*msg.Usage, 0, 1)
				s.emit(orca.Event{Kind: orca.EvtTurnCompleted, AgentID: s.id, Payload: map[string]any{
					"delta":      delta,
					"cumulative": total,
				}})
			}
		}
	case "result":
		if env.Usage != nil {
			s.usageMu.Lock()
			s.usage.CostUSD += env.CostUSD
			s.usage.LastUpdated = time.Now()
			final := s.usage
			s.usageMu.Unlock()
			s.emit(orca.Event{Kind: orca.EvtUsageSnapshot, AgentID: s.id, Payload: map[string]any{
				"usage":       final,
				"is_error":    env.IsError,
				"duration_ms": env.DurationMS,
			}})
		}
	}
	_ = raw
}

func (s *session) drainStderr() {
	scanner := bufio.NewScanner(s.stderr)
	for scanner.Scan() {
		s.emit(orca.Event{Kind: orca.EvtError, AgentID: s.id, Payload: map[string]any{
			"scope": "stderr",
			"msg":   scanner.Text(),
		}})
	}
}

func (s *session) waitProc() {
	s.waitOnce.Do(func() {
		err := s.cmd.Wait()
		s.waitErr = err
		final := s.Usage()
		exitInfo := map[string]any{"final_usage": final}
		if err != nil {
			exitInfo["error"] = err.Error()
		}
		s.emit(orca.Event{Kind: orca.EvtAgentExited, AgentID: s.id, Payload: exitInfo})
		close(s.done)
		close(s.events)
	})
}
