// Package registry implements the daemon's HTTP and WebSocket surface.
//
// The Server struct wires together a supervisor, bus, event bus, and
// optional bridge runtime / decisions registry / discussions registry,
// then exposes them via:
//
//   GET  /healthz
//   GET  /agents
//   POST /agents
//   GET  /agents/{id}
//   DELETE /agents/{id}
//   GET  /agents/{id}/usage
//   POST /agents/{id}/message
//   GET  /agents/{id}/events
//   GET  /tasks
//   POST /tasks
//   GET  /tasks/{id}
//   DELETE /tasks/{id}
//   GET  /decisions
//   POST /decisions
//   GET  /decisions/{id}
//   POST /decisions/{id}/answer
//   POST /decisions/{id}/clarify
//   GET  /discussions
//   GET  /discussions/{id}
//   POST /discussions/{id}/close
//   GET  /usage
//   GET  /runtimes
//   GET  /events                      (SSE)
//   POST /messages                    (direct OR tag-routed dispatch)
//   POST /validate/agent-spec
//   WS   /agents/{id}/bridge          (bridge runtime attachment)
//
// The HTTP server is plain net/http with stdlib path patterns. SSE
// streaming uses the standard Flusher interface; the bridge WebSocket
// uses github.com/coder/websocket.
//
// Optional subsystems (bridge, decisions, discussions) attach via
// SetBridgeRuntime / SetDecisions / SetDiscussions. When unset, their
// endpoints respond 501 Not Implemented.
package registry
