package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultAnthropicEndpoint is the official Anthropic Messages API URL.
const DefaultAnthropicEndpoint = "https://api.anthropic.com/v1/messages"

// AnthropicBackend forwards OpenAI-shaped requests to Anthropic's Messages
// API, translating the wire format both ways.
type AnthropicBackend struct {
	Endpoint   string
	HTTPClient *http.Client
	// APIVersion sets the anthropic-version request header.
	APIVersion string
}

// NewAnthropicBackend returns a Backend pointed at api.anthropic.com.
func NewAnthropicBackend() *AnthropicBackend {
	return &AnthropicBackend{
		Endpoint:   DefaultAnthropicEndpoint,
		HTTPClient: &http.Client{Timeout: 120 * time.Second},
		APIVersion: "2023-06-01",
	}
}

// Complete runs a single non-streaming completion.
func (a *AnthropicBackend) Complete(req OpenAIRequest, apiKey string) (OpenAIResponse, error) {
	return a.CompleteContext(context.Background(), req, apiKey)
}

// CompleteContext is the ctx-aware variant of Complete. Most callers go
// through the Backend interface (Complete) which uses Background; the
// gateway handler uses CompleteContext directly when available.
func (a *AnthropicBackend) CompleteContext(ctx context.Context, req OpenAIRequest, apiKey string) (OpenAIResponse, error) {
	if req.Stream {
		return OpenAIResponse{}, fmt.Errorf("streaming responses are not supported yet")
	}
	anthReq := openAIToAnthropic(req)
	body, err := json.Marshal(anthReq)
	if err != nil {
		return OpenAIResponse{}, fmt.Errorf("marshal anthropic request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Endpoint, bytes.NewReader(body))
	if err != nil {
		return OpenAIResponse{}, fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", apiKey)
	httpReq.Header.Set("anthropic-version", a.APIVersion)

	resp, err := a.HTTPClient.Do(httpReq)
	if err != nil {
		return OpenAIResponse{}, fmt.Errorf("call anthropic: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return OpenAIResponse{}, fmt.Errorf("read anthropic body: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return OpenAIResponse{}, fmt.Errorf("anthropic %d: %s", resp.StatusCode, string(respBody))
	}

	var anthResp anthropicResponse
	if err := json.Unmarshal(respBody, &anthResp); err != nil {
		return OpenAIResponse{}, fmt.Errorf("parse anthropic response: %w", err)
	}
	return anthropicToOpenAI(anthResp, req.Model), nil
}

// --- wire types (Anthropic side, internal) ---

type anthropicRequest struct {
	Model       string             `json:"model"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature *float64           `json:"temperature,omitempty"`
	TopP        *float64           `json:"top_p,omitempty"`
	Metadata    map[string]string  `json:"metadata,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	ID         string                  `json:"id"`
	Type       string                  `json:"type"`
	Role       string                  `json:"role"`
	Model      string                  `json:"model"`
	Content    []anthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason"`
	Usage      anthropicUsage          `json:"usage"`
}

type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// --- translation ---

func openAIToAnthropic(req OpenAIRequest) anthropicRequest {
	systemParts := make([]string, 0, 1)
	convo := make([]anthropicMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		if m.Role == "system" {
			systemParts = append(systemParts, m.Content)
			continue
		}
		convo = append(convo, anthropicMessage(m))
	}
	max := req.MaxTokens
	if max == 0 {
		max = 1024 // Anthropic requires max_tokens
	}
	out := anthropicRequest{
		Model:       req.Model,
		Messages:    convo,
		MaxTokens:   max,
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}
	if len(systemParts) > 0 {
		out.System = joinNonEmpty(systemParts, "\n\n")
	}
	if req.User != "" {
		out.Metadata = map[string]string{"user_id": req.User}
	}
	return out
}

func anthropicToOpenAI(resp anthropicResponse, model string) OpenAIResponse {
	text := ""
	for _, b := range resp.Content {
		if b.Type == "text" {
			text += b.Text
		}
	}
	if model == "" {
		model = resp.Model
	}
	return OpenAIResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []OpenAIChoice{{
			Index:        0,
			Message:      OpenAIMessage{Role: "assistant", Content: text},
			FinishReason: stopReasonToFinishReason(resp.StopReason),
		}},
		Usage: OpenAIUsage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}
}

func stopReasonToFinishReason(r string) string {
	switch r {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	}
	return r
}

func joinNonEmpty(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if p == "" {
			continue
		}
		if i > 0 && out != "" {
			out += sep
		}
		out += p
	}
	return out
}
