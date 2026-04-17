package claudecode

import (
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// TestShutdownGrace_EnvOverride confirms the grace duration is read from
// ORCA_SHUTDOWN_GRACE when set and falls back to the default otherwise.
func TestShutdownGrace_EnvOverride(t *testing.T) {
	t.Setenv("ORCA_SHUTDOWN_GRACE", "250ms")
	if got := shutdownGrace(); got != 250*time.Millisecond {
		t.Fatalf("env override not honored: got %v", got)
	}

	t.Setenv("ORCA_SHUTDOWN_GRACE", "bogus")
	if got := shutdownGrace(); got != defaultShutdownGrace {
		t.Fatalf("bogus env should fall back to default: got %v", got)
	}

	t.Setenv("ORCA_SHUTDOWN_GRACE", "")
	if got := shutdownGrace(); got != defaultShutdownGrace {
		t.Fatalf("empty env should use default: got %v", got)
	}
}

// TestShutdownSequence_Escalates spawns a process that ignores SIGINT and
// confirms the shutdown sequence eventually SIGKILLs it within the
// expected window. Uses `sh` because `sleep` on some systems is killed
// by SIGINT; sh with a trap explicitly tests the escalation path.
func TestShutdownSequence_Escalates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal-escalation test is unix-only")
	}
	// Trap SIGINT to no-op so only SIGKILL will terminate us. Sleep long
	// enough that the grace window can't accidentally win.
	cmd := exec.Command("sh", "-c", "trap '' INT; sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sh: %v", err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(done)
	}()

	s := &session{
		id:   "test",
		cmd:  cmd,
		done: done,
	}

	start := time.Now()
	s.shutdownSequence(50 * time.Millisecond)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit after shutdown sequence")
	}
	elapsed := time.Since(start)

	// Should have escalated past SIGINT to SIGKILL. Total wall time must
	// be at least grace (50ms) + interruptGrace (2s). Upper bound is
	// grace + interruptGrace + kernel kill latency.
	minExpected := 50*time.Millisecond + interruptGrace
	if elapsed < minExpected {
		t.Fatalf("shutdown too fast: %v (want ≥ %v) — did we skip SIGINT stage?", elapsed, minExpected)
	}
	if waitErr == nil {
		t.Fatal("expected non-nil wait error from killed process")
	}
}

// TestShutdownSequence_EarlyExit confirms a well-behaved process that
// exits during the grace window is not signaled and returns quickly.
func TestShutdownSequence_EarlyExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal test is unix-only")
	}
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sh: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	// Give the shell a moment to exit naturally before we enter the
	// sequence, so Stage 1's select sees s.done already closed.
	time.Sleep(50 * time.Millisecond)

	s := &session{id: "test", cmd: cmd, done: done}
	start := time.Now()
	s.shutdownSequence(5 * time.Second)
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("early-exit path should be near-instant, took %v", elapsed)
	}
}
