package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// brokenWriter fails every Write with a fixed error, mimicking a closed
// stdout pipe when the parent Claude Code process has died.
type brokenWriter struct{ err error }

func (b *brokenWriter) Write(_ []byte) (int, error) { return 0, b.err }

// TestRun_ExitsOnBrokenStdout ensures Run returns (rather than silently
// looping) when the output pipe is broken. Previously encode errors were
// discarded with `_ =` and the shim kept serving requests whose responses
// were lost.
func TestRun_ExitsOnBrokenStdout(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n")
	srv := &Server{
		Name:    "test",
		Version: "0",
		in:      in,
		out:     &brokenWriter{err: io.ErrClosedPipe},
	}

	err := srv.Run(context.Background())
	if err == nil {
		t.Fatal("expected error from Run when stdout is broken, got nil")
	}
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("expected ErrClosedPipe in chain, got %v", err)
	}
}

// TestRun_SkipsMalformedAndContinues verifies malformed input doesn't
// crash the shim or abort the read loop — the server keeps processing
// subsequent valid requests. (The JSON-RPC parse-error response is
// currently elided because nil ids short-circuit respondErr; fixing that
// is tracked separately.)
func TestRun_SkipsMalformedAndContinues(t *testing.T) {
	in := strings.NewReader("not json\n" + `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
	var out bytes.Buffer
	srv := &Server{Name: "test", Version: "0", in: in, out: &out}

	if err := srv.Run(context.Background()); err != nil {
		t.Fatalf("Run errored: %v", err)
	}
	var resp rpcResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("response not valid JSON: %v (%s)", err, out.String())
	}
	if resp.Error != nil {
		t.Fatalf("ping returned error: %+v", resp.Error)
	}
}

// TestRun_ToolsList round-trips a tools/list and confirms the declared
// tool appears.
func TestRun_ToolsList(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"tools/list"}` + "\n")
	var out bytes.Buffer
	srv := &Server{
		Name:    "test",
		Version: "0",
		in:      in,
		out:     &out,
		Tools: []Tool{
			{Name: "greet", Description: "hi", InputSchema: map[string]any{"type": "object"}},
		},
	}
	if err := srv.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), `"greet"`) {
		t.Fatalf("tools/list missing greet: %s", out.String())
	}
}
