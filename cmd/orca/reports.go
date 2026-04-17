package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/atterpac/orca/pkg/mcpserver"
	"github.com/atterpac/orca/pkg/orca"
)

// reportTool constructs an MCP tool that shapes the caller's args into
// an orca.Update and publishes it via POST /messages. The shaping
// function fills phase/status/severity/title/details/metrics so the
// caller only supplies semantic inputs.
//
// Correlation_id is NOT required on the call — supervisor auto-fills
// it from the agent's last-seen inbound. Callers can pass it via
// `correlation_id` in args to override.
func reportTool(
	asAgent *string,
	name string,
	description string,
	schema string,
	shape func(args map[string]any, u *orca.Update),
) mcpserver.Tool {
	return mcpserver.Tool{
		Name:        name,
		Description: description,
		InputSchema: mcpserver.MustSchema(schema),
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			// shape may set any field; we always overwrite AgentID.
			var u orca.Update
			shape(args, &u)
			u.AgentID = *asAgent
			if u.Title == "" {
				return "", fmt.Errorf("title required")
			}

			body, _ := json.Marshal(u)

			// Optional overrides: bridge_agent_id + explicit correlation_id.
			// Both are off-schema in most verbs but accepted if present so
			// agents have an escape hatch without extra tooling.
			bridge := "slack"
			if s, ok := args["bridge_agent_id"].(string); ok && s != "" {
				bridge = s
			}
			corrID := ""
			if s, ok := args["correlation_id"].(string); ok {
				corrID = s
			}

			payload := map[string]any{
				"from": *asAgent,
				"to":   bridge,
				"kind": "update",
				"body": json.RawMessage(body),
			}
			if corrID != "" {
				payload["correlation_id"] = corrID
			}
			buf, _ := json.Marshal(payload)
			resp, err := http.Post(daemonAddr()+"/messages", "application/json", bytes.NewReader(buf))
			if err != nil {
				return "", err
			}
			defer resp.Body.Close()
			out, _ := io.ReadAll(resp.Body)
			if resp.StatusCode >= 400 {
				return "", fmt.Errorf("%s failed: %s", name, string(out))
			}
			return name + " posted", nil
		},
	}
}

// readStringArray reads args[key] as []string, tolerating []any input.
func readStringArray(args map[string]any, key string) []string {
	raw, ok := args[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// readStringMap reads args[key] as map[string]string, tolerating any json input.
func readStringMap(args map[string]any, key string) map[string]string {
	raw, ok := args[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}
