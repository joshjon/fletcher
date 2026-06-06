package image

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CheckForUpdate reports whether the registry has a newer image than the one a
// template was imported from. It returns (false, ...) without error when the
// template has no recorded registry digest (a local-only build can't be
// checked), so callers can treat any error as "could not check".
func CheckForUpdate(ctx context.Context, imagesDir, name string) (available bool, source string, err error) {
	meta, found, err := ReadMeta(imagesDir, name)
	if err != nil || !found || meta.Digest == "" {
		return false, "", err
	}
	latest, err := LatestDigest(ctx, meta.Source)
	if err != nil {
		return false, meta.Source, err
	}
	return latest != "" && latest != meta.Digest, meta.Source, nil
}

// manifestAccept lists the manifest media types we ask for. The registry
// returns the Docker-Content-Digest of the matching manifest (the multi-arch
// index when present), which is what `docker pull` records in RepoDigests - so
// it is comparable to the digest stored at import.
const manifestAccept = "application/vnd.oci.image.index.v1+json," +
	"application/vnd.docker.distribution.manifest.list.v2+json," +
	"application/vnd.oci.image.manifest.v1+json," +
	"application/vnd.docker.distribution.manifest.v2+json"

// LatestDigest returns the current registry digest ("sha256:...") for a docker
// reference's tag, without pulling the image. It performs the registry v2 auth
// handshake (anonymous bearer token) for registries that require it (ghcr does).
func LatestDigest(ctx context.Context, ref string) (string, error) {
	host, repo, tag := parseRef(ref)
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", host, repo, tag)
	client := &http.Client{Timeout: 10 * time.Second}

	digest, challenge, err := manifestDigest(ctx, client, url, "")
	if err != nil {
		return "", err
	}
	if digest != "" {
		return digest, nil
	}
	// 401: satisfy the bearer challenge with an anonymous token, then retry.
	token, err := fetchToken(ctx, client, challenge)
	if err != nil {
		return "", err
	}
	digest, _, err = manifestDigest(ctx, client, url, token)
	if err != nil {
		return "", err
	}
	if digest == "" {
		return "", errors.New("registry did not return a manifest digest")
	}
	return digest, nil
}

// manifestDigest issues a HEAD for the manifest. On 401 it returns the
// WWW-Authenticate challenge (empty digest, no error) so the caller can get a
// token and retry.
func manifestDigest(ctx context.Context, client *http.Client, url, token string) (digest, challenge string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, http.NoBody)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", manifestAccept)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Header.Get("Docker-Content-Digest"), "", nil
	case http.StatusUnauthorized:
		if token != "" {
			return "", "", fmt.Errorf("registry: unauthorized for %s", url)
		}
		return "", resp.Header.Get("WWW-Authenticate"), nil
	default:
		return "", "", fmt.Errorf("registry returned %d for %s", resp.StatusCode, url)
	}
}

// fetchToken satisfies a `Bearer realm=...,service=...,scope=...` challenge with
// an anonymous token request.
func fetchToken(ctx context.Context, client *http.Client, challenge string) (string, error) {
	realm, params := parseChallenge(challenge)
	if realm == "" {
		return "", errors.New("registry: missing bearer realm in auth challenge")
	}
	url := realm
	if q := params.Encode(); q != "" {
		url += "?" + q
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry token endpoint returned %d", resp.StatusCode)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode registry token: %w", err)
	}
	if body.Token != "" {
		return body.Token, nil
	}
	return body.AccessToken, nil
}

// parseRef splits a docker reference into registry host, repository, and tag,
// defaulting to Docker Hub for a host-less short ref.
func parseRef(ref string) (host, repo, tag string) {
	if i := strings.Index(ref, "@"); i >= 0 { // drop any digest
		ref = ref[:i]
	}
	tag = "latest"
	if i := strings.LastIndex(ref, ":"); i >= 0 && i > strings.LastIndex(ref, "/") {
		tag = ref[i+1:]
		ref = ref[:i]
	}
	if first, rest, ok := strings.Cut(ref, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return first, rest, tag
	}
	repo = ref
	if !strings.Contains(repo, "/") {
		repo = "library/" + repo // Docker Hub official-image namespace
	}
	return "registry-1.docker.io", repo, tag
}

// parseChallenge extracts the realm and the remaining parameters (service,
// scope) from a `Bearer realm="...",service="...",scope="..."` header.
func parseChallenge(header string) (realm string, params url.Values) {
	params = url.Values{}
	header = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(header), "Bearer "))
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"`)
		if k == "realm" {
			realm = v
		} else if k != "" {
			params.Set(k, v)
		}
	}
	return realm, params
}
