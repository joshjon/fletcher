package job

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/joshjon/fletcher/internal/errs"
)

// Credential names accepted by the trusted-credential mode.
const (
	CredentialClaude = "claude"
	CredentialCodex  = "codex"
	CredentialGemini = "gemini"
)

// AllowedCredential describes one mountable credential directory.
type AllowedCredential struct {
	// Name is the user-facing identifier on the wire and CLI.
	Name string
	// HostRelPath is the credential directory's path relative to the daemon's
	// configured credentials root (typically the operator's $HOME).
	HostRelPath string
	// GuestPath is the fixed mount point inside the fletcher-base image,
	// matching the paths the bundled agent CLIs read by default.
	GuestPath string
}

// AllowedCredentials lists every credential the trusted-credential mode
// supports. The supervisor resolves names → AllowedCredential entries at
// job-start time.
var AllowedCredentials = map[string]AllowedCredential{
	CredentialClaude: {Name: CredentialClaude, HostRelPath: ".claude", GuestPath: "/home/fletcher/.claude"},
	CredentialCodex:  {Name: CredentialCodex, HostRelPath: ".codex", GuestPath: "/home/fletcher/.codex"},
	CredentialGemini: {Name: CredentialGemini, HostRelPath: ".config/gemini", GuestPath: "/home/fletcher/.config/gemini"},
}

// normaliseCredentials validates each name against AllowedCredentials,
// removes duplicates, and returns the result sorted for stable storage.
func normaliseCredentials(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(in))
	for _, name := range in {
		if _, ok := AllowedCredentials[name]; !ok {
			return nil, errs.Newf(errs.CategoryInvalidArgument, "unknown credential %q (allowed: %s)", name, allowedCredentialNames())
		}
		seen[name] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// encodeCredentials serialises the (already-validated, sorted) list to the
// JSON form stored in SQLite. An empty list maps to the empty string so the
// SQL DEFAULT for the column is the canonical "none" value.
func encodeCredentials(creds []string) (string, error) {
	if len(creds) == 0 {
		return "", nil
	}
	b, err := json.Marshal(creds)
	if err != nil {
		return "", fmt.Errorf("encode credentials: %w", err)
	}
	return string(b), nil
}

// decodeCredentials parses the JSON form stored in SQLite into a slice.
// The empty string round-trips to a nil slice.
func decodeCredentials(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("decode credentials: %w", err)
	}
	return out, nil
}

func allowedCredentialNames() string {
	names := make([]string, 0, len(AllowedCredentials))
	for name := range AllowedCredentials {
		names = append(names, name)
	}
	sort.Strings(names)
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}
