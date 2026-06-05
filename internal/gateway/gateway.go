// Package gateway is the daemon's model gateway: a daemon-local HTTP
// endpoint that speaks both the OpenAI Chat Completions wire format
// (for OpenAI-compatible agent CLIs - Codex, Aider, OpenHands, pi) and
// the Anthropic Messages wire format (for Claude Code and other
// Anthropic-native agents). It forwards each call to a real LLM
// provider with the API key stamped from the secrets store; keys
// never enter forks. Agents talk to http://<gateway-addr>/v1 instead
// of a public LLM URL.
//
// See DESIGN.md §6.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/joshjon/fletcher/internal/secrets"
)

// SecretName is the well-known key under which the Anthropic API key is
// looked up in the secrets store.
const SecretName = "anthropic_api_key" //nolint:gosec // identifier, not a credential value

// Gateway serves the gateway HTTP API on the daemon's local listener.
type Gateway struct {
	secrets        *secrets.Store
	upstream       Upstream
	logger         *slog.Logger
	gatewayBaseURL string
}

// Backend translates OpenAI-shaped Chat Completions requests into upstream
// provider calls. The single phase-5 implementation (AnthropicBackend)
// translates to the Messages API; future multi-provider routing slots in
// behind this interface.
type Backend interface {
	Complete(req OpenAIRequest, apiKey string) (OpenAIResponse, error)
}

// MessagesForwarder proxies Anthropic-native Messages requests through to
// the upstream without translation. The caller (the Gateway's /v1/messages
// handler) hands over the raw request body and the client's request headers;
// the forwarder passes the client headers through (so beta features like
// anthropic-beta work), stamps the x-api-key header from the secrets store,
// and returns the upstream response untouched, including streaming SSE bodies.
type MessagesForwarder interface {
	ForwardMessages(ctx context.Context, body []byte, apiKey string, header http.Header) (*http.Response, error)
}

// Upstream is what a Gateway needs from its provider implementation: both
// the OpenAI→Anthropic translating path and the Anthropic-native
// passthrough path. AnthropicBackend implements both today.
type Upstream interface {
	Backend
	MessagesForwarder
}

// New constructs a Gateway. secrets and upstream must be non-nil; in
// production the daemon wires AnthropicBackend as the Upstream. The
// gatewayBaseURL is what BuildCatalog embeds in provider entries so
// agents know what to point at; it may be empty in tests.
func New(secretsStore *secrets.Store, upstream Upstream, gatewayBaseURL string, logger *slog.Logger) *Gateway {
	if logger == nil {
		logger = slog.Default()
	}
	return &Gateway{
		secrets:        secretsStore,
		upstream:       upstream,
		logger:         logger,
		gatewayBaseURL: gatewayBaseURL,
	}
}

// Handler returns the HTTP mux exposing the gateway's routes:
// /v1/chat/completions (OpenAI-compatible), /v1/messages (Anthropic-
// native), and /v1/catalog.json (the discovery surface).
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", g.chatCompletions)
	mux.HandleFunc("POST /v1/messages", g.messages)
	mux.HandleFunc("GET /v1/catalog.json", g.catalog)
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

	resp, err := g.upstream.Complete(req, apiKey)
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

// messages is the Anthropic-native passthrough route. Claude Code (and any
// other agent pointed at ANTHROPIC_BASE_URL=http://daemon-gateway/v1) sends
// a Messages-shaped request; we read the secrets-store key, stamp it on the
// upstream call, and stream the response back untouched. Streaming SSE
// bodies pass through because we copy the response body directly rather
// than JSON-decoding.
func (g *Gateway) messages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "could not read request body")
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

	resp, err := g.upstream.ForwardMessages(r.Context(), body, apiKey, r.Header)
	if err != nil {
		g.logger.ErrorContext(r.Context(), "forward messages", slog.String("err", err.Error()))
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		g.logger.WarnContext(r.Context(), "stream upstream body", slog.String("err", err.Error()))
	}
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
