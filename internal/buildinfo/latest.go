package buildinfo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// latestReleaseURL is the GitHub API endpoint that returns the most
// recent published release for the Fletcher repo. Hard-coded because
// it is part of the project's identity, not user configuration.
const latestReleaseURL = "https://api.github.com/repos/joshjon/fletcher/releases/latest"

// LatestReleaseCheckTimeout caps how long the daemon waits for the
// GitHub API. A failed check is non-fatal - we just skip the upgrade
// notice rather than blocking startup on network reachability.
const LatestReleaseCheckTimeout = 5 * time.Second

// LatestRelease describes the GitHub release the daemon compares
// itself against.
type LatestRelease struct {
	// TagName is the release's git tag (e.g. "v0.1.0").
	TagName string `json:"tag_name"`
	// HTMLURL is the human-facing release page; useful in upgrade
	// messages so users can click through to the changelog.
	HTMLURL string `json:"html_url"`
}

// CheckLatest fetches the most recent published Fletcher release from
// GitHub. The call is gated by LatestReleaseCheckTimeout; transient
// failures return an error the caller may safely ignore (the daemon
// just skips the upgrade notice).
func CheckLatest(ctx context.Context, client *http.Client) (LatestRelease, error) {
	if client == nil {
		client = &http.Client{Timeout: LatestReleaseCheckTimeout}
	}
	reqCtx, cancel := context.WithTimeout(ctx, LatestReleaseCheckTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, latestReleaseURL, nil)
	if err != nil {
		return LatestRelease{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("accept", "application/vnd.github+json")
	req.Header.Set("user-agent", "fletcher/"+Version)

	resp, err := client.Do(req)
	if err != nil {
		return LatestRelease{}, fmt.Errorf("call github: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		// Repo has no published releases yet; the daemon shouldn't
		// scream about it. Surface as a sentinel.
		return LatestRelease{}, ErrNoReleases
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return LatestRelease{}, fmt.Errorf("github %d", resp.StatusCode)
	}

	var out LatestRelease
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return LatestRelease{}, fmt.Errorf("decode response: %w", err)
	}
	if out.TagName == "" {
		return LatestRelease{}, errors.New("github returned an empty tag_name")
	}
	return out, nil
}

// ErrNoReleases is returned when the GitHub API reports no published
// releases for the repo. Pre-v0.1.0 this is the expected state.
var ErrNoReleases = errors.New("no releases published yet")

// UpgradeAvailable reports whether the locally-running version is older
// than the supplied latest release. Returns false (without error) when
// either version isn't a valid semver tag - that's the case for local
// dev builds (Version = "dev" or a short commit SHA), and we explicitly
// don't want to spam those with upgrade notices.
func UpgradeAvailable(current, latest string) bool {
	cur := normaliseSemver(current)
	lat := normaliseSemver(latest)
	if !semver.IsValid(cur) || !semver.IsValid(lat) {
		return false
	}
	return semver.Compare(cur, lat) < 0
}

func normaliseSemver(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || v == "dev" {
		return ""
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v
}
