package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/gateway"
	"github.com/joshjon/fletcher/internal/secrets"
	"github.com/joshjon/fletcher/internal/sqlite"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

// newSecrets builds a Store with a single Anthropic-key secret already set.
func newSecrets(t *testing.T, apiKey string) *secrets.Store {
	t.Helper()
	dir := t.TempDir()
	db, err := sqlite.Open(context.Background(), filepath.Join(dir, "f.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, sqlite.Migrate(db))

	s, err := secrets.Open(sqliteq.New(db), filepath.Join(dir, "age.key"))
	require.NoError(t, err)
	require.NoError(t, s.Set(context.Background(), gateway.SecretName, apiKey))
	return s
}

// captureBackend records the api key and request it was called with, then
// returns a canned response.
type captureBackend struct {
	gotKey string
	gotReq gateway.OpenAIRequest
	resp   gateway.OpenAIResponse
	err    error
}

func (c *captureBackend) Complete(req gateway.OpenAIRequest, apiKey string) (gateway.OpenAIResponse, error) {
	c.gotKey = apiKey
	c.gotReq = req
	return c.resp, c.err
}

func TestGatewayForwardsToBackendWithSecret(t *testing.T) {
	store := newSecrets(t, "sk-ant-abc")
	backend := &captureBackend{
		resp: gateway.OpenAIResponse{
			ID:      "chatcmpl-1",
			Object:  "chat.completion",
			Model:   "claude-opus-4-7",
			Choices: []gateway.OpenAIChoice{{Index: 0, Message: gateway.OpenAIMessage{Role: "assistant", Content: "hi"}, FinishReason: "stop"}},
			Usage:   gateway.OpenAIUsage{PromptTokens: 4, CompletionTokens: 1, TotalTokens: 5},
		},
	}
	gw := gateway.New(store, backend, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)

	body, _ := json.Marshal(gateway.OpenAIRequest{
		Model:    "claude-opus-4-7",
		Messages: []gateway.OpenAIMessage{{Role: "user", Content: "hi"}},
	})
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var got gateway.OpenAIResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, "chatcmpl-1", got.ID)
	require.Equal(t, "hi", got.Choices[0].Message.Content)

	require.Equal(t, "sk-ant-abc", backend.gotKey)
	require.Equal(t, "claude-opus-4-7", backend.gotReq.Model)
}

func TestGatewayReturnsUnauthorizedWhenNoSecret(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(context.Background(), filepath.Join(dir, "f.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, sqlite.Migrate(db))
	store, err := secrets.Open(sqliteq.New(db), filepath.Join(dir, "age.key"))
	require.NoError(t, err)

	gw := gateway.New(store, &captureBackend{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)

	body, _ := json.Marshal(gateway.OpenAIRequest{
		Model:    "claude-opus-4-7",
		Messages: []gateway.OpenAIMessage{{Role: "user", Content: "hi"}},
	})
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	b, _ := io.ReadAll(resp.Body)
	require.Contains(t, string(b), "fletcher secret set")
}

func TestGatewayRejectsMalformedRequest(t *testing.T) {
	store := newSecrets(t, "sk")
	gw := gateway.New(store, &captureBackend{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader("not json"))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestAnthropicBackendTranslatesShapeBothWays(t *testing.T) {
	var captured struct {
		key  string
		body []byte
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.key = r.Header.Get("x-api-key")
		captured.body, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "msg_123",
			"type": "message",
			"role": "assistant",
			"model": "claude-opus-4-7",
			"content": [{"type":"text","text":"hello there"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 11, "output_tokens": 4}
		}`))
	}))
	t.Cleanup(upstream.Close)

	backend := &gateway.AnthropicBackend{
		Endpoint:   upstream.URL,
		HTTPClient: http.DefaultClient,
		APIVersion: "2023-06-01",
	}
	resp, err := backend.Complete(gateway.OpenAIRequest{
		Model: "claude-opus-4-7",
		Messages: []gateway.OpenAIMessage{
			{Role: "system", Content: "you are concise"},
			{Role: "user", Content: "say hi"},
		},
		MaxTokens: 100,
	}, "sk-ant-xyz")
	require.NoError(t, err)
	require.Equal(t, "msg_123", resp.ID)
	require.Equal(t, "hello there", resp.Choices[0].Message.Content)
	require.Equal(t, "stop", resp.Choices[0].FinishReason)
	require.Equal(t, 11, resp.Usage.PromptTokens)
	require.Equal(t, 15, resp.Usage.TotalTokens)

	// Verify the upstream saw system extracted from messages and the key in the header.
	require.Equal(t, "sk-ant-xyz", captured.key)
	require.Contains(t, string(captured.body), `"system":"you are concise"`)
	require.NotContains(t, string(captured.body), `"role":"system"`)
}
