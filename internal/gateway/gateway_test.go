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
// returns canned responses. Implements the full gateway.Upstream surface so
// it can be plugged into gateway.New (both /v1/chat/completions and
// /v1/messages routes share one upstream).
type captureBackend struct {
	gotKey  string
	gotReq  gateway.OpenAIRequest
	resp    gateway.OpenAIResponse
	err     error
	gotBody []byte
	fwdResp *http.Response
	fwdErr  error
}

func (c *captureBackend) Complete(req gateway.OpenAIRequest, apiKey string) (gateway.OpenAIResponse, error) {
	c.gotKey = apiKey
	c.gotReq = req
	return c.resp, c.err
}

func (c *captureBackend) ForwardMessages(_ context.Context, body []byte, apiKey string) (*http.Response, error) {
	c.gotKey = apiKey
	c.gotBody = body
	if c.fwdErr != nil {
		return nil, c.fwdErr
	}
	return c.fwdResp, nil
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

func TestGatewayMessagesPassesBodyThroughWithSecretStamped(t *testing.T) {
	store := newSecrets(t, "sk-ant-real")
	backend := &captureBackend{
		fwdResp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"content-type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"msg_1","content":[{"type":"text","text":"hi"}]}`)),
		},
	}
	gw := gateway.New(store, backend, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)

	body := []byte(`{"model":"claude-opus-4-7","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ := io.ReadAll(resp.Body)
	require.Contains(t, string(got), `"id":"msg_1"`)

	require.Equal(t, "sk-ant-real", backend.gotKey, "the secrets-store key must be stamped onto the upstream call")
	require.JSONEq(t, string(body), string(backend.gotBody), "the body must be passed through unchanged")
}

func TestGatewayMessagesReturnsUnauthorizedWhenNoSecret(t *testing.T) {
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

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAnthropicBackendForwardMessagesPassesBodyAndKey(t *testing.T) {
	var captured struct {
		key     string
		version string
		body    []byte
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.key = r.Header.Get("x-api-key")
		captured.version = r.Header.Get("anthropic-version")
		captured.body, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_xyz","content":[{"type":"text","text":"ok"}]}`))
	}))
	t.Cleanup(upstream.Close)

	backend := &gateway.AnthropicBackend{
		Endpoint:   upstream.URL,
		HTTPClient: http.DefaultClient,
		APIVersion: "2023-06-01",
	}

	in := []byte(`{"model":"claude-opus-4-7","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := backend.ForwardMessages(context.Background(), in, "sk-ant-real")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	out, _ := io.ReadAll(resp.Body)
	require.Contains(t, string(out), `"msg_xyz"`)
	require.Equal(t, "sk-ant-real", captured.key)
	require.Equal(t, "2023-06-01", captured.version)
	require.JSONEq(t, string(in), string(captured.body))
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
