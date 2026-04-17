package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/atterpac/orca/pkg/orca"
	"github.com/atterpac/orca/pkg/runtime/bridge"
)

// bridgeRuntime is set when the daemon registered a bridge.Runtime. When nil
// the /bridge endpoint returns 501.
type bridgeEndpoint struct {
	rt *bridge.Runtime
}

// SetBridgeRuntime lets the daemon wire its bridge.Runtime instance into the
// registry so the /agents/{id}/bridge endpoint can service sidecar attachments.
func (s *Server) SetBridgeRuntime(rt *bridge.Runtime) {
	s.bridge = &bridgeEndpoint{rt: rt}
	s.mux.HandleFunc("GET /agents/{id}/bridge", s.bridgeConnect)
}

// bridgeFrame is the JSON envelope exchanged over the websocket.
type bridgeFrame struct {
	// "publish" (sidecar → orca) or "deliver" (orca → sidecar)
	Kind     string           `json:"kind"`
	Message  *orca.Message    `json:"message,omitempty"`
	Event    *orca.Event      `json:"event,omitempty"`
	Error    string           `json:"error,omitempty"`
}

func (s *Server) bridgeConnect(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.bridge == nil || s.bridge.rt == nil {
		http.Error(w, "bridge runtime not configured", http.StatusNotImplemented)
		return
	}
	sess, err := s.bridge.rt.Attach(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // sidecar is typically localhost; TLS-terminated upstream if anywhere
	})
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// orca → sidecar pump.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-sess.Outbox():
				if !ok {
					_ = conn.Close(websocket.StatusNormalClosure, "session closed")
					return
				}
				frame := bridgeFrame{Kind: "deliver", Message: &msg}
				writeCtx, cancelWrite := context.WithTimeout(ctx, 10*time.Second)
				if err := wsjson.Write(writeCtx, conn, frame); err != nil {
					cancelWrite()
					cancel()
					return
				}
				cancelWrite()
			}
		}
	}()

	// sidecar → orca pump.
	for {
		var frame bridgeFrame
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			break
		}
		switch frame.Kind {
		case "publish":
			if frame.Message == nil {
				writeError(ctx, conn, "publish frame missing message")
				continue
			}
			if err := sess.Deliver(ctx, *frame.Message); err != nil {
				writeError(ctx, conn, fmt.Sprintf("deliver: %v", err))
			}
		case "event":
			if frame.Event == nil {
				continue
			}
			sess.Emit(*frame.Event)
		case "ping":
			// no-op; wsjson keeps the socket alive on either side writing
		default:
			writeError(ctx, conn, "unknown frame kind: "+frame.Kind)
		}
	}

	_ = conn.CloseNow()
}

func writeError(ctx context.Context, c *websocket.Conn, msg string) {
	_ = wsjson.Write(ctx, c, bridgeFrame{Kind: "error", Error: msg})
}

// Suppress unused-import warning when this file is compiled standalone during refactors.
var _ = json.Marshal
