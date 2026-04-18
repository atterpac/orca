package main

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/atterpac/orca/internal/bus"
	"github.com/atterpac/orca/internal/decisions"
	"github.com/atterpac/orca/internal/events"
	"github.com/atterpac/orca/internal/supervisor"
	"github.com/atterpac/orca/pkg/orca"
)

// failOnceWriter returns an error on the first N writes, then passes
// through to inner. Mimics a transient disk hiccup so we can verify the
// events-log drain keeps going and eventually lands output.
type failOnceWriter struct {
	fails    int
	attempts int
	inner    io.Writer
	err      error
}

func (w *failOnceWriter) Write(p []byte) (int, error) {
	w.attempts++
	if w.attempts <= w.fails {
		return 0, w.err
	}
	return w.inner.Write(p)
}

// TestWriteEventsLog_DrainsOnTransientError confirms a write error does
// not abort the loop — later events still land once the writer
// recovers.
func TestWriteEventsLog_DrainsOnTransientError(t *testing.T) {
	var buf bytes.Buffer
	w := &failOnceWriter{fails: 2, inner: &buf, err: errors.New("disk hiccup")}

	ch := make(chan orca.Event, 4)
	ch <- orca.Event{Kind: orca.EvtAgentSpawned, AgentID: "a"}
	ch <- orca.Event{Kind: orca.EvtAgentSpawned, AgentID: "b"}
	ch <- orca.Event{Kind: orca.EvtAgentSpawned, AgentID: "c"}
	close(ch)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	writeEventsLog(w, ch, logger)

	// First two events failed, third succeeded → one JSON line for "c".
	got := buf.String()
	if !strings.Contains(got, `"agent_id":"c"`) {
		t.Fatalf("expected c to be written after recovery, got %q", got)
	}
	if strings.Contains(got, `"agent_id":"a"`) || strings.Contains(got, `"agent_id":"b"`) {
		t.Fatalf("events that failed to write should not appear: %q", got)
	}
}

// slackHarness builds a minimal supervisor + bus + decisions triple for
// slack-init tests. No slack runtime is registered by the harness
// itself — the test-under-test calls maybeRegisterSlack.
func slackHarness(t *testing.T) (*supervisor.Supervisor, bus.Bus, *decisions.Registry, *slog.Logger) {
	t.Helper()
	b := bus.NewInProc()
	ev := events.NewBus(64)
	sup := supervisor.New(b, ev)
	dec := decisions.New(b, ev, func(id string) bool { _, ok := sup.Get(id); return ok })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Cleanup(func() { sup.Shutdown() })
	return sup, b, dec, logger
}

// TestMaybeRegisterSlack_NoTokenSkips confirms the slack runtime is NOT
// registered when SLACK_BOT_TOKEN is absent — daemon runs headless.
func TestMaybeRegisterSlack_NoTokenSkips(t *testing.T) {
	sup, b, dec, logger := slackHarness(t)
	t.Setenv("SLACK_BOT_TOKEN", "")

	if err := maybeRegisterSlack(sup, b, dec, logger); err != nil {
		t.Fatalf("expected nil when no token, got %v", err)
	}
	for _, n := range sup.RuntimeNames() {
		if n == "slack" {
			t.Fatal("slack runtime must not be registered when SLACK_BOT_TOKEN unset")
		}
	}
}

// TestMaybeRegisterSlack_ConfigFailureFatal confirms a present token
// combined with bad config returns an error — the daemon must refuse
// to start rather than silently skip slack.
func TestMaybeRegisterSlack_ConfigFailureFatal(t *testing.T) {
	sup, b, dec, logger := slackHarness(t)
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-fake")
	t.Setenv("SLACK_SIGNING_SECRET", "")
	t.Setenv("SLACK_APP_TOKEN", "")
	t.Setenv("ORCA_SLACK_OPTIONAL", "")

	err := maybeRegisterSlack(sup, b, dec, logger)
	if err == nil {
		t.Fatal("expected fatal error for missing signing secret")
	}
	if !strings.Contains(err.Error(), "ORCA_SLACK_OPTIONAL") {
		t.Fatalf("error should hint at escape hatch: %v", err)
	}
}

// TestMaybeRegisterSlack_OptionalEscapeHatch confirms
// ORCA_SLACK_OPTIONAL=1 downgrades init failures to a warning so dev
// loops with intermittent slack credentials still start the daemon.
func TestMaybeRegisterSlack_OptionalEscapeHatch(t *testing.T) {
	sup, b, dec, logger := slackHarness(t)
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-fake")
	t.Setenv("SLACK_SIGNING_SECRET", "")
	t.Setenv("SLACK_APP_TOKEN", "")
	t.Setenv("ORCA_SLACK_OPTIONAL", "1")

	if err := maybeRegisterSlack(sup, b, dec, logger); err != nil {
		t.Fatalf("optional escape hatch should swallow error, got %v", err)
	}
	for _, n := range sup.RuntimeNames() {
		if n == "slack" {
			t.Fatal("slack runtime should not register when init skipped")
		}
	}
}

// TestWriteEventsLog_ExitsOnChannelClose verifies the loop terminates
// when the subscription channel closes (normal shutdown path).
func TestWriteEventsLog_ExitsOnChannelClose(t *testing.T) {
	var buf bytes.Buffer
	ch := make(chan orca.Event)
	close(ch)

	done := make(chan struct{})
	go func() {
		writeEventsLog(&buf, ch, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("writeEventsLog did not exit on closed channel")
	}
}
