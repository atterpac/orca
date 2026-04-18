package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/atterpac/orca/internal/bus"
	"github.com/atterpac/orca/internal/events"
	"github.com/atterpac/orca/internal/supervisor"
	"github.com/atterpac/orca/internal/testutil"
	"github.com/atterpac/orca/pkg/orca"
	"github.com/atterpac/orca/pkg/runtime/bridge"
)

// TestHTTPSendMessage_AutoFillsCorrelation verifies the HTTP direct-send
// endpoint routes through Supervisor.DispatchDirect (and therefore picks
// up the sender's last-inbound correlation_id when the request omits
// one). Any future handler that bypasses Dispatch* and writes straight
// to the bus will fail this test.
func TestHTTPSendMessage_AutoFillsCorrelation(t *testing.T) {
	b := bus.NewInProc()
	ev := events.NewBus(64)
	rt := testutil.NewRuntime()
	sup := supervisor.New(b, ev)
	sup.RegisterRuntime(rt)
	defer sup.Shutdown()

	srv := httptest.NewServer(New(sup, b, ev).Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := sup.Spawn(ctx, orca.AgentSpec{ID: "a", Runtime: "fake"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Spawn(ctx, orca.AgentSpec{ID: "b", Runtime: "fake"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	// Seed a's last-inbound via a direct bus publish.
	_ = b.Publish(ctx, orca.Message{
		From: "ext", To: "a", Kind: orca.KindRequest, CorrelationID: "CONV-HTTP",
	})
	time.Sleep(50 * time.Millisecond)

	if got := sup.AutoCorrelationFor("a"); got != "CONV-HTTP" {
		t.Fatalf("seed did not stick; got %q", got)
	}

	// a sends to b via HTTP with NO correlation_id. Supervisor must fill.
	body, _ := json.Marshal(map[string]any{
		"from": "a",
		"kind": string(orca.KindRequest),
		"body": json.RawMessage(`"hi"`),
	})
	resp, err := http.Post(srv.URL+"/agents/b/message", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}

	// After delivery, b's last-inbound should equal the seed.
	deadline := time.After(500 * time.Millisecond)
	for {
		if got := sup.AutoCorrelationFor("b"); got == "CONV-HTTP" {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("auto-corr did not propagate: b's last-inbound=%q", sup.AutoCorrelationFor("b"))
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestHTTPDispatchMessage_AutoFillsCorrelation mirrors the above for the
// /messages endpoint (direct-by-To variant).
func TestHTTPDispatchMessage_AutoFillsCorrelation(t *testing.T) {
	b := bus.NewInProc()
	ev := events.NewBus(64)
	rt := testutil.NewRuntime()
	sup := supervisor.New(b, ev)
	sup.RegisterRuntime(rt)
	defer sup.Shutdown()

	srv := httptest.NewServer(New(sup, b, ev).Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = sup.Spawn(ctx, orca.AgentSpec{ID: "a", Runtime: "fake"})
	_, _ = sup.Spawn(ctx, orca.AgentSpec{ID: "b", Runtime: "fake"})
	time.Sleep(50 * time.Millisecond)

	_ = b.Publish(ctx, orca.Message{From: "ext", To: "a", Kind: orca.KindRequest, CorrelationID: "CONV-DISP"})
	time.Sleep(50 * time.Millisecond)

	payload, _ := json.Marshal(map[string]any{
		"from": "a",
		"to":   "b",
		"kind": string(orca.KindRequest),
		"body": json.RawMessage(`"hi"`),
	})
	resp, err := http.Post(srv.URL+"/messages", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}

	deadline := time.After(500 * time.Millisecond)
	for {
		if got := sup.AutoCorrelationFor("b"); got == "CONV-DISP" {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("auto-corr did not propagate via /messages: b's last-inbound=%q", sup.AutoCorrelationFor("b"))
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestBridgeWS_RejectsImpersonation stands up the full registry,
// bridge runtime, and a real websocket client; confirms a sidecar
// trying to Deliver a message with a foreign From is rejected at the
// Deliver trust boundary. Phase 3.3 added the rejection inside
// bridge.Session.Deliver; this test proves it fires through the WS
// endpoint that untrusted sidecars use.
func TestBridgeWS_RejectsImpersonation(t *testing.T) {
	b := bus.NewInProc()
	ev := events.NewBus(64)
	sup := supervisor.New(b, ev)
	defer sup.Shutdown()

	bridgeRT := bridge.New(b)
	sup.RegisterRuntime(bridgeRT)
	sup.RegisterRuntime(testutil.NewRuntime())

	registrySrv := New(sup, b, ev)
	registrySrv.SetBridgeRuntime(bridgeRT)
	srv := httptest.NewServer(registrySrv.Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Spawn bridge and victim agent.
	if _, err := sup.Spawn(ctx, orca.AgentSpec{ID: "slack", Runtime: "bridge"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Spawn(ctx, orca.AgentSpec{ID: "arch", Runtime: "fake"}); err != nil {
		t.Fatal(err)
	}

	// Dial the bridge WebSocket as if we were the slack sidecar.
	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) + "/agents/slack/bridge"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "test done")

	// Send a publish frame with a foreign From — bridge must reject.
	impersonation := bridgeFrame{
		Kind: "publish",
		Message: &orca.Message{
			From: "arch", // NOT slack — impersonation
			To:   "arch",
			Kind: orca.KindRequest,
			Body: json.RawMessage(`"evil"`),
		},
	}
	if err := wsjson.Write(ctx, conn, impersonation); err != nil {
		t.Fatalf("ws write: %v", err)
	}

	// Expect an error frame back.
	var reply bridgeFrame
	if err := wsjson.Read(ctx, conn, &reply); err != nil {
		t.Fatalf("ws read: %v", err)
	}
	if reply.Kind != "error" {
		t.Fatalf("want error frame, got kind=%q: %+v", reply.Kind, reply)
	}
	if !strings.Contains(reply.Error, "may not deliver") {
		t.Fatalf("error should mention impersonation rejection, got %q", reply.Error)
	}
}
