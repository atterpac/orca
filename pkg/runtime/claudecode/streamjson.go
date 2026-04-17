package claudecode

import "encoding/json"

type ccUsage struct {
	InputTokens              uint64  `json:"input_tokens"`
	OutputTokens             uint64  `json:"output_tokens"`
	CacheCreationInputTokens uint64  `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     uint64  `json:"cache_read_input_tokens"`
	CostUSD                  float64 `json:"-"`
}

type ccEnvelope struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	Message   json.RawMessage `json:"message,omitempty"`
	Usage     *ccUsage        `json:"usage,omitempty"`
	Result    string          `json:"result,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	NumTurns  uint64          `json:"num_turns,omitempty"`
	CostUSD   float64         `json:"total_cost_usd,omitempty"`
	DurationMS int64          `json:"duration_ms,omitempty"`
}

type ccAssistantMsg struct {
	Model   string `json:"model"`
	ID      string `json:"id"`
	Content []struct {
		Type string          `json:"type"`
		Text string          `json:"text,omitempty"`
		Name string          `json:"name,omitempty"`
		ID   string          `json:"id,omitempty"`
		Input json.RawMessage `json:"input,omitempty"`
	} `json:"content"`
	StopReason string   `json:"stop_reason"`
	Usage      *ccUsage `json:"usage"`
}

type ccUserInput struct {
	Type    string         `json:"type"`
	Message ccUserMessage  `json:"message"`
}

type ccUserMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func encodeUserInput(text string) ([]byte, error) {
	v := ccUserInput{
		Type:    "user",
		Message: ccUserMessage{Role: "user", Content: text},
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
