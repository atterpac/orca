// Package mcpserver implements a minimal MCP stdio server (JSON-RPC 2.0).
// Used by the orca comms shim to expose inter-agent messaging tools to Claude Code.
package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

const protocolVersion = "2024-11-05"

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Handler     ToolHandler    `json:"-"`
}

type ToolHandler func(ctx context.Context, args map[string]any) (string, error)

type Server struct {
	Name    string
	Version string
	Tools   []Tool
	in      io.Reader
	out     io.Writer
	enc     *json.Encoder
}

func New(name, version string, tools []Tool) *Server {
	return &Server{
		Name:    name,
		Version: version,
		Tools:   tools,
		in:      os.Stdin,
		out:     os.Stdout,
	}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) Run(ctx context.Context) error {
	s.enc = json.NewEncoder(s.out)
	scanner := bufio.NewScanner(s.in)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			if werr := s.respondErr(nil, -32700, "parse error"); werr != nil {
				return fmt.Errorf("mcpserver: write response: %w", werr)
			}
			continue
		}
		if err := s.dispatch(ctx, req); err != nil {
			return fmt.Errorf("mcpserver: dispatch: %w", err)
		}
	}
	return scanner.Err()
}

// dispatch handles a single request. Returns an error only when the
// transport is broken (stdout write failed) so Run can exit rather than
// silently losing responses.
func (s *Server) dispatch(ctx context.Context, req rpcRequest) error {
	switch req.Method {
	case "initialize":
		return s.respond(req.ID, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    s.Name,
				"version": s.Version,
			},
		})
	case "notifications/initialized", "notifications/cancelled":
		// no response for notifications
		return nil
	case "tools/list":
		toolList := make([]map[string]any, 0, len(s.Tools))
		for _, t := range s.Tools {
			toolList = append(toolList, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": t.InputSchema,
			})
		}
		return s.respond(req.ID, map[string]any{"tools": toolList})
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)
		var handler ToolHandler
		for _, t := range s.Tools {
			if t.Name == p.Name {
				handler = t.Handler
				break
			}
		}
		if handler == nil {
			return s.respondErr(req.ID, -32601, "unknown tool: "+p.Name)
		}
		text, err := handler(ctx, p.Arguments)
		if err != nil {
			return s.respond(req.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": err.Error()}},
				"isError": true,
			})
		}
		return s.respond(req.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
			"isError": false,
		})
	case "ping":
		return s.respond(req.ID, map[string]any{})
	default:
		if len(req.ID) > 0 {
			return s.respondErr(req.ID, -32601, "method not found: "+req.Method)
		}
		return nil
	}
}

func (s *Server) respond(id json.RawMessage, result any) error {
	if len(id) == 0 {
		return nil
	}
	return s.enc.Encode(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) respondErr(id json.RawMessage, code int, msg string) error {
	if len(id) == 0 {
		return nil
	}
	return s.enc.Encode(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func MustSchema(j string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(j), &m); err != nil {
		panic(fmt.Errorf("schema: %w", err))
	}
	return m
}
