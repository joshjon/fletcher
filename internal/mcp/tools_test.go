package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/approval"
)

func TestValidateEgressURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"http ok", "http://example.com/x", false},
		{"https ok", "https://example.com", false},
		{"ip literal ok at this layer", "http://127.0.0.1:8080", false}, // blocked later, at dial
		{"ftp scheme", "ftp://example.com", true},
		{"file scheme", "file:///etc/passwd", true},
		{"no scheme", "example.com/x", true},
		{"no host", "http://", true},
		{"garbage", "://nope", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateEgressURL(tc.url)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestEgressHTTPClientBlocksLoopback proves the SSRF guard refuses to dial a
// loopback target even when handed its URL directly (the httptest server binds
// 127.0.0.1), so an agent cannot reach the daemon's own surface.
func TestEgressHTTPClientBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("should not be reachable"))
	}))
	defer srv.Close()

	client := NewEgressHTTPClient(5 * time.Second)
	resp, err := client.Get(srv.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err)
	require.Contains(t, err.Error(), "blocked")
}

// stubApprovals records creations and resolves every Wait with the configured
// terminal status, standing in for the human decision.
type stubApprovals struct {
	created []approval.CreateParams
	status  approval.Status
	reason  string
}

func (s *stubApprovals) Create(_ context.Context, p approval.CreateParams) (approval.Approval, error) {
	s.created = append(s.created, p)
	return approval.Approval{ID: "appr_test", Status: approval.StatusPending, Action: p.Action}, nil
}

func (s *stubApprovals) Get(_ context.Context, id string) (approval.Approval, error) {
	return approval.Approval{ID: id, Status: s.status, DecisionReason: s.reason}, nil
}

func (s *stubApprovals) Wait(_ context.Context, id string) (approval.Approval, error) {
	return approval.Approval{ID: id, Status: s.status, DecisionReason: s.reason}, nil
}

// stubPublisher records what it is asked to publish.
type stubPublisher struct {
	commits []CommitImage
	imports []string
}

func (s *stubPublisher) CommitSessionImage(_ context.Context, p CommitImage) (string, error) {
	s.commits = append(s.commits, p)
	return p.Name, nil
}

func (s *stubPublisher) ImportRegistryImage(_ context.Context, ref, name string, _ bool) (string, error) {
	s.imports = append(s.imports, ref)
	return name, nil
}

func callPublish(t *testing.T, pub ImagePublisher, appr ApprovalBackend, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	tool := publishImageTool(pub, appr)
	res, err := tool.Handler(context.Background(), mcpgo.CallToolRequest{
		Params: mcpgo.CallToolParams{Name: "publish_image", Arguments: args},
	})
	require.NoError(t, err)
	return res
}

func resultText(t *testing.T, res *mcpgo.CallToolResult) string {
	t.Helper()
	require.NotEmpty(t, res.Content)
	tc, ok := mcpgo.AsTextContent(res.Content[0])
	require.True(t, ok)
	return tc.Text
}

func TestPublishImageCommitApprovedPublishes(t *testing.T) {
	pub := &stubPublisher{}
	appr := &stubApprovals{status: approval.StatusApproved}
	res := callPublish(t, pub, appr, map[string]any{
		"name":            "webapp",
		"justification":   "the web app is ready to deploy",
		"session":         "dev-1",
		"entrypoint_json": `["node","/workspace/server.js"]`,
		"exposed_port":    float64(3000),
		"force":           true,
	})

	require.False(t, res.IsError)
	require.Contains(t, resultText(t, res), `published image "webapp"`)
	require.Len(t, pub.commits, 1)
	require.Equal(t, CommitImage{
		SessionRef:  "dev-1",
		Name:        "webapp",
		Entrypoint:  []string{"node", "/workspace/server.js"},
		ExposedPort: 3000,
		Force:       true,
	}, pub.commits[0])
	require.Len(t, appr.created, 1)
	require.Contains(t, appr.created[0].Action, `committing session "dev-1"`)
	require.Equal(t, "agent:dev-1", appr.created[0].Requester)
}

func TestPublishImageDeniedDoesNotPublish(t *testing.T) {
	pub := &stubPublisher{}
	appr := &stubApprovals{status: approval.StatusDenied, reason: "not now"}
	res := callPublish(t, pub, appr, map[string]any{
		"name":          "webapp",
		"justification": "ready",
		"session":       "dev-1",
	})

	require.False(t, res.IsError)
	text := resultText(t, res)
	require.Contains(t, text, "image not published")
	require.Contains(t, text, "denied")
	require.Contains(t, text, "not now")
	require.Empty(t, pub.commits)
	require.Empty(t, pub.imports)
}

func TestPublishImageRegistryMode(t *testing.T) {
	pub := &stubPublisher{}
	appr := &stubApprovals{status: approval.StatusApproved}
	res := callPublish(t, pub, appr, map[string]any{
		"name":          "app",
		"justification": "built in CI",
		"source_ref":    "ghcr.io/example/app:v2",
	})

	require.False(t, res.IsError)
	require.Equal(t, []string{"ghcr.io/example/app:v2"}, pub.imports)
	require.Contains(t, appr.created[0].Action, `registry ref "ghcr.io/example/app:v2"`)
}

func TestPublishImageRequiresExactlyOneSource(t *testing.T) {
	pub := &stubPublisher{}
	appr := &stubApprovals{status: approval.StatusApproved}

	res := callPublish(t, pub, appr, map[string]any{
		"name": "x", "justification": "j",
	})
	require.True(t, res.IsError)

	res = callPublish(t, pub, appr, map[string]any{
		"name": "x", "justification": "j", "session": "a", "source_ref": "b",
	})
	require.True(t, res.IsError)
	require.Empty(t, appr.created)
}

func TestPublishImageRejectsEntrypointForRegistry(t *testing.T) {
	pub := &stubPublisher{}
	appr := &stubApprovals{status: approval.StatusApproved}
	res := callPublish(t, pub, appr, map[string]any{
		"name":            "x",
		"justification":   "j",
		"source_ref":      "ghcr.io/example/app:v2",
		"entrypoint_json": `["node"]`,
	})
	require.True(t, res.IsError)
	require.Empty(t, appr.created)
}
