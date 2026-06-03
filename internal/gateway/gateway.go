// Package gateway is the daemon's model gateway: a daemon-local HTTP
// endpoint that speaks the OpenAI Chat Completions wire format and
// forwards calls to a real LLM provider (Anthropic in phase 5). API
// keys live in the secrets store; they never enter forks. Agents and
// jobs talk to http://<gateway-addr>/v1 instead of a public LLM URL.
//
// See DESIGN.md §6.
package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/joshjon/fletcher/internal/secrets"
)

// SecretName is the well-known key under which the Anthropic API key is
// looked up in the secrets store.
const SecretName = "anthropic_api_key" //nolint:gosec // identifier, not a credential value

// Gateway serves the OpenAI-compatible HTTP API.
type Gateway struct {
	secrets *secrets.Store
	backend Backend
	logger  *slog.Logger
}

// Backend is the upstream LLM provider the gateway forwards to. Phase 5
// has one implementation (Anthropic); future providers slot in here.
type Backend interface {
	Complete(req OpenAIRequest, apiKey string) (OpenAIResponse, error)
}

// New constructs a Gateway. secrets must be non-nil; backend defaults to
// nothing — the daemon wires the AnthropicBackend in production.
func New(secretsStore *secrets.Store, backend Backend, logger *slog.Logger) *Gateway {
	if logger == nil {
		logger = slog.Default()
	}
	return &Gateway{secrets: secretsStore, backend: backend, logger: logger}
}

// Handler returns the HTTP mux exposing /v1/chat/completions.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", g.chatCompletions)
	return mux
}

func (g *Gateway) chatCompletions(w http.ResponseWriter, r *http.Request) {
	var req OpenAIRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "could not parse request body")
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "messages is required")
		return
	}

	apiKey, err := g.secrets.Get(r.Context(), SecretName)
	if err != nil {
		if errors.Is(err, secrets.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "no_api_key",
				fmt.Sprintf("no secret %q set; run `fletcher secret set %s <key>`", SecretName, SecretName))
			return
		}
		g.logger.ErrorContext(r.Context(), "load api key", slog.String("err", err.Error()))
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	resp, err := g.backend.Complete(req, apiKey)
	if err != nil {
		g.logger.ErrorContext(r.Context(), "backend call",
			slog.String("model", req.Model),
			slog.String("err", err.Error()),
		)
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}

	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"type":    code,
			"message": message,
		},
	})
}
