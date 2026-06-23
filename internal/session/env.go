package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/joshjon/fletcher/internal/errs"
)

// EnvVar is one user-set environment variable injected into a session's
// app/process at boot. Exactly one source is set: Value for a plain literal, or
// SecretName to reference a stored secret resolved from the secret store at
// boot. A secret's value is never persisted in the session row - only its name.
type EnvVar struct {
	Name       string `json:"name"`
	Value      string `json:"value,omitempty"`
	SecretName string `json:"secret_name,omitempty"`
}

// SecretResolver resolves a stored secret's plaintext by name (implemented by
// secrets.Store; kept narrow per "consumers define interfaces").
type SecretResolver interface {
	Get(ctx context.Context, name string) (string, error)
}

// reservedEnvNames are variables the daemon itself sets to wire egress (the
// proxy), the model gateway, and session identity. A user var may not use these
// names: allowing it would let a fork subvert its own sandbox. The FLETCHER_
// prefix is reserved wholesale for the same reason.
var reservedEnvNames = map[string]bool{
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true,
	"OPENAI_BASE_URL": true, "OPENAI_API_KEY": true,
	"ANTHROPIC_BASE_URL": true, "ANTHROPIC_API_KEY": true,
}

// validateEnvVars checks each var has a valid shell-identifier name that is not
// daemon-reserved, with no duplicates, and that a secret var does not also set a
// literal value. It does not resolve secrets (that happens at boot).
func validateEnvVars(vars []EnvVar) error {
	seen := make(map[string]bool, len(vars))
	for _, v := range vars {
		if !validEnvName(v.Name) {
			return errs.Newf(errs.CategoryInvalidArgument,
				"invalid environment variable name %q (use letters, digits, and underscore; not starting with a digit)", v.Name)
		}
		if reservedEnvNames[v.Name] || strings.HasPrefix(v.Name, "FLETCHER_") {
			return errs.Newf(errs.CategoryInvalidArgument,
				"environment variable %q is reserved by the daemon", v.Name)
		}
		if seen[v.Name] {
			return errs.Newf(errs.CategoryInvalidArgument, "duplicate environment variable %q", v.Name)
		}
		seen[v.Name] = true
		if v.SecretName != "" && v.Value != "" {
			return errs.Newf(errs.CategoryInvalidArgument,
				"environment variable %q sets both a value and a secret reference", v.Name)
		}
	}
	return nil
}

// validEnvName reports whether name is a POSIX-ish shell identifier: a letter or
// underscore, then letters/digits/underscores.
func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		letter := r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
		digit := r >= '0' && r <= '9'
		if i == 0 && !letter {
			return false
		}
		if i > 0 && !letter && !digit {
			return false
		}
	}
	return true
}

// prepareEnv validates the requested env vars, resolves their secret references
// to KEY=VALUE strings for boot, and marshals them for persistence. Used by
// Create; a missing secret or bad name fails before a fork is allocated.
func (m *Manager) prepareEnv(ctx context.Context, vars []EnvVar) (userEnv []string, stored string, err error) {
	if err := validateEnvVars(vars); err != nil {
		return nil, "", err
	}
	userEnv, err = m.resolveEnvVars(ctx, vars)
	if err != nil {
		return nil, "", err
	}
	stored, err = marshalEnvVars(vars)
	return userEnv, stored, err
}

// resolveEnvVars turns env vars into KEY=VALUE strings, resolving secret
// references via the secret store. A missing secret (or no store) fails fast so
// a deploy does not boot silently missing a value it expects.
func (m *Manager) resolveEnvVars(ctx context.Context, vars []EnvVar) ([]string, error) {
	out := make([]string, 0, len(vars))
	for _, v := range vars {
		val := v.Value
		if v.SecretName != "" {
			if m.secrets == nil {
				return nil, errs.New(errs.CategoryFailedPrecondition,
					"this daemon has no secret store, so secret environment variables cannot be resolved")
			}
			s, err := m.secrets.Get(ctx, v.SecretName)
			if err != nil {
				return nil, errs.Newf(errs.CategoryFailedPrecondition,
					"environment variable %q references secret %q: %v", v.Name, v.SecretName, err)
			}
			val = s
		}
		out = append(out, v.Name+"="+val)
	}
	return out, nil
}

// marshalEnvVars encodes env vars for the sessions.env_vars JSON column (empty
// string means none). Secret vars keep only their name and secret_name.
func marshalEnvVars(vars []EnvVar) (string, error) {
	if len(vars) == 0 {
		return "", nil
	}
	b, err := json.Marshal(vars)
	if err != nil {
		return "", fmt.Errorf("marshal env vars: %w", err)
	}
	return string(b), nil
}

// parseEnvVars decodes the sessions.env_vars JSON column. A blank or malformed
// column yields no vars rather than failing a boot.
func parseEnvVars(s string) []EnvVar {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var vars []EnvVar
	if err := json.Unmarshal([]byte(s), &vars); err != nil {
		return nil
	}
	return vars
}
