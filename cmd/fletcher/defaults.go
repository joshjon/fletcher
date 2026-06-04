package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// defaultSocketPath returns the daemon's default Unix-socket path. It
// scans the well-known locations in priority order and returns the
// first path that actually has a daemon socket living there - this
// lets a CLI run by the regular user automatically find a daemon
// installed system-wide via systemd (which puts the socket under
// /run/fletcher/) without the operator having to set FLETCHER_SOCKET
// by hand. When no daemon socket exists yet (e.g. first run of
// `fletcher serve`), falls back to the XDG-based path so the daemon
// has a sensible place to create one.
//
// Permission-denied on Stat (typical when the systemd RuntimeDirectory
// is mode 0750 and the operator isn't in the fletcher group yet) is
// treated as "path may exist, try it" - the connect call later gives
// the operator a clearer error than the lookup silently moving on.
func defaultSocketPath() string {
	candidates := socketCandidates()
	for _, c := range candidates {
		_, err := os.Stat(c)
		if err == nil {
			return c
		}
		if errors.Is(err, fs.ErrPermission) {
			return c
		}
	}
	// None of the candidates exist yet - return the first one (the
	// XDG-runtime preferred location) so `fletcher serve` has a path
	// it can create.
	return candidates[0]
}

// socketCandidates returns the prioritised list of socket paths the
// CLI checks at startup. Order matters: a user-level daemon should
// win over a system one when both are running, because the user-level
// instance is whichever one the operator started themselves.
func socketCandidates() []string {
	out := make([]string, 0, 4)
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		out = append(out, filepath.Join(dir, "fletcher", "fletcher.sock"))
	}
	// System path used by init/fletcher.service (RuntimeDirectory=fletcher).
	out = append(out, "/run/fletcher/fletcher.sock")
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".fletcher", "fletcher.sock"))
	}
	out = append(out, "/tmp/fletcher.sock")
	return out
}

// defaultDatabasePath returns the daemon's default SQLite path. Prefers
// $XDG_DATA_HOME, falls back to $HOME/.local/share/fletcher, and finally
// the current directory.
func defaultDatabasePath() string {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "fletcher", "fletcher.db")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "fletcher", "fletcher.db")
	}
	return "fletcher.db"
}

// defaultAgeIdentityPath returns the default location of the daemon's age
// private key. Generated automatically on first boot if missing.
func defaultAgeIdentityPath() string {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "fletcher", "age.key")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "fletcher", "age.key")
	}
	return "age.key"
}

// defaultCredentialsDir returns the operator's $HOME, which is where agent
// CLIs typically store their config (~/.claude, ~/.codex, etc.). An empty
// string is returned if HOME is unset; the supervisor treats that as
// "trusted-credential mode disabled" and fails jobs that request mounts.
func defaultCredentialsDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return ""
}
