package image

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseRef(t *testing.T) {
	cases := []struct {
		ref, host, repo, tag string
	}{
		{"ghcr.io/joshjon/fletcher-base:debian-13", "ghcr.io", "joshjon/fletcher-base", "debian-13"},
		{"ghcr.io/joshjon/fletcher-base", "ghcr.io", "joshjon/fletcher-base", "latest"},
		{"registry:5000/team/app:v1", "registry:5000", "team/app", "v1"},
		{"ubuntu:24.04", "registry-1.docker.io", "library/ubuntu", "24.04"},
		{"someuser/app", "registry-1.docker.io", "someuser/app", "latest"},
		{"ghcr.io/a/b@sha256:deadbeef", "ghcr.io", "a/b", "latest"},
	}
	for _, c := range cases {
		host, repo, tag := parseRef(c.ref)
		require.Equalf(t, c.host, host, "host for %s", c.ref)
		require.Equalf(t, c.repo, repo, "repo for %s", c.ref)
		require.Equalf(t, c.tag, tag, "tag for %s", c.ref)
	}
}

func TestParseChallenge(t *testing.T) {
	realm, params := parseChallenge(`Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:joshjon/fletcher-base:pull"`)
	require.Equal(t, "https://ghcr.io/token", realm)
	require.Equal(t, "ghcr.io", params.Get("service"))
	require.Equal(t, "repository:joshjon/fletcher-base:pull", params.Get("scope"))
}

// TestLatestDigestLive hits ghcr.io for real (the full anonymous-token + manifest
// handshake). It skips on any network error so it doesn't flake offline/CI.
func TestLatestDigestLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	digest, err := LatestDigest(ctx, "ghcr.io/joshjon/fletcher-base:debian-13")
	if err != nil {
		t.Skipf("skipping live registry check: %v", err)
	}
	require.True(t, strings.HasPrefix(digest, "sha256:"), "got %q", digest)
	t.Logf("ghcr fletcher-base:debian-13 digest = %s", digest)
}
