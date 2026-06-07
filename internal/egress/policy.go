// Package egress is the daemon's forward-proxy for fork-initiated network
// access. A fork has no NIC (DESIGN.md §5); when a session or job is granted
// egress, its HTTP_PROXY/HTTPS_PROXY point - over the loopback->vsock relay -
// at this proxy, which gates each connection on a per-job Policy plus the
// shared netguard LAN guard, then tunnels (CONNECT) or forwards (plain HTTP)
// to the real host. CONNECT keeps TLS end-to-end, so no certificate
// interception is needed and the agent talks to the genuine remote cert.
package egress

import "strings"

// Policy names persisted per session/job and resolved into a Policy value.
const (
	PolicyNone      = "none"
	PolicyAllowlist = "allowlist"
	PolicyOpen      = "open"
)

// Normalize maps a stored policy string to a known value, defaulting empty or
// unrecognised input to the safer "allowlist". Callers that want a different
// default (e.g. the daemon's default_egress_policy setting) resolve that first.
func Normalize(p string) string {
	switch p {
	case PolicyNone, PolicyAllowlist, PolicyOpen:
		return p
	default:
		return PolicyAllowlist
	}
}

// Policy decides whether the fork may reach a given host through the proxy.
// The netguard LAN/metadata guard applies on top of every policy at dial
// time, so even Open can never reach the operator's LAN or the metadata
// endpoint - a policy can only ever narrow what Open would allow.
type Policy interface {
	// Allow reports whether egress to host (hostname only, no port) is
	// permitted by this policy.
	Allow(host string) bool
	// Name is a short label for logs and audit.
	Name() string
}

// Deny refuses all egress. It backs the `none` and `tools` policies: with
// those the fork still has the daemon MCP tools, just no transparent proxy
// (in practice the proxy is simply not wired for them, but Deny is the safe
// default if one is ever pointed at it).
type Deny struct{}

// Allow always returns false: Deny permits no egress.
func (Deny) Allow(string) bool { return false }

// Name identifies the policy in logs and audit.
func (Deny) Name() string { return "none" }

// Open allows any host. The netguard guard still blocks private/LAN/metadata
// addresses at dial time, so Open means "any public host", not "anything".
type Open struct{}

// Allow always returns true: Open permits any host (the netguard guard still
// blocks private/LAN/metadata at dial time).
func (Open) Allow(string) bool { return true }

// Name identifies the policy in logs and audit.
func (Open) Name() string { return "open" }

// Allowlist permits only hosts matching one of its patterns. A pattern is
// either an exact host ("api.anthropic.com") or a wildcard written as
// "*.example.com" or ".example.com", which matches example.com and any
// subdomain. Matching is case-insensitive.
type Allowlist struct {
	exact    map[string]struct{}
	suffixes []string // domains (no leading dot) matched as self-or-subdomain
}

// NewAllowlist normalises patterns: lowercased and trimmed, with "*." / "."
// prefixes treated as self-or-subdomain wildcards. Empty patterns are dropped.
func NewAllowlist(patterns []string) Allowlist {
	a := Allowlist{exact: make(map[string]struct{})}
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		p = strings.TrimSuffix(p, ".")
		if p == "" {
			continue
		}
		switch {
		case strings.HasPrefix(p, "*."):
			a.suffixes = append(a.suffixes, p[2:])
		case strings.HasPrefix(p, "."):
			a.suffixes = append(a.suffixes, p[1:])
		default:
			a.exact[p] = struct{}{}
		}
	}
	return a
}

// Allow reports whether host matches an exact entry or a wildcard suffix.
func (a Allowlist) Allow(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if _, ok := a.exact[host]; ok {
		return true
	}
	for _, base := range a.suffixes {
		if host == base || strings.HasSuffix(host, "."+base) {
			return true
		}
	}
	return false
}

// Name identifies the policy in logs and audit.
func (Allowlist) Name() string { return "allowlist" }

// Patterns is the canonical pattern list this allowlist matches (exact hosts
// plus "*."-prefixed wildcards), for logging and round-tripping a policy.
func (a Allowlist) Patterns() []string {
	out := make([]string, 0, len(a.exact)+len(a.suffixes))
	for h := range a.exact {
		out = append(out, h)
	}
	for _, base := range a.suffixes {
		out = append(out, "*."+base)
	}
	return out
}
