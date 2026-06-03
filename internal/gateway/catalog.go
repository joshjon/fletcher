package gateway

import (
	"encoding/json"
	"net/http"
)

// CatalogSchemaVersion is bumped when the catalog wire shape changes
// incompatibly. Consumers (pi extension, fletcher CLI) tolerate newer
// minor versions and ignore unknown fields.
const CatalogSchemaVersion = 2

// Endpoint is one base URL an agent can be configured against, paired
// with the env-var convention the matching SDK family expects.
type Endpoint struct {
	// Kind is the wire format the endpoint speaks
	// ("anthropic-messages" or "openai-chat-completions").
	Kind string `json:"kind"`
	// URL is the exact value to set as EnvVar. Anthropic SDKs expect the
	// bare host (no /v1) and append /v1/messages themselves; OpenAI
	// SDKs expect a URL that already ends in /v1 and append
	// /chat/completions. The catalog reflects each SDK's convention so
	// the value is copy-pastable.
	URL string `json:"url"`
	// EnvVar is the environment variable name SDKs in this family read.
	EnvVar string `json:"env_var"`
}

// Model is one model an agent can name in a request.
type Model struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Upstream string `json:"upstream"`
}

// Catalog is what the daemon publishes on /v1/catalog.json and via the
// ModelService Connect RPC. Endpoints answer "where do I point my
// agent?"; Models answer "what model name do I pass in the request?"
// The two lists are deliberately decoupled so model rows aren't
// duplicated across wire formats.
type Catalog struct {
	SchemaVersion int        `json:"schema_version"`
	Endpoints     []Endpoint `json:"endpoints"`
	Models        []Model    `json:"models"`
}

// BuildCatalog assembles the catalog using gatewayBaseURL (the gateway
// listener's HTTP base URL, e.g. "http://127.0.0.1:11500") as the host
// agents should point at. Phase 14 ships a static catalog covering the
// two wire formats the gateway implements today.
func BuildCatalog(gatewayBaseURL string) Catalog {
	return Catalog{
		SchemaVersion: CatalogSchemaVersion,
		Endpoints: []Endpoint{
			{
				Kind:   "anthropic-messages",
				URL:    gatewayBaseURL,
				EnvVar: "ANTHROPIC_BASE_URL",
			},
			{
				Kind:   "openai-chat-completions",
				URL:    gatewayBaseURL + "/v1",
				EnvVar: "OPENAI_BASE_URL",
			},
		},
		Models: []Model{
			{ID: "claude-opus-4-7", Label: "Claude Opus 4.7", Upstream: "anthropic"},
			{ID: "claude-sonnet-4-6", Label: "Claude Sonnet 4.6", Upstream: "anthropic"},
			{ID: "claude-haiku-4-5", Label: "Claude Haiku 4.5", Upstream: "anthropic"},
		},
	}
}

// catalog is the gateway's GET /v1/catalog.json handler. It serves the
// same data the Connect ModelService.ListModels returns; pi-extension-
// style agents poll this on startup so they can register the daemon's
// endpoints without per-agent env-var fiddling.
func (g *Gateway) catalog(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(BuildCatalog(g.gatewayBaseURL))
}
