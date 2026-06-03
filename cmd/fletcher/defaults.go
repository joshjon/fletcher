package main

import (
	"os"
	"path/filepath"
)

// defaultSocketPath returns the daemon's default Unix-socket path. Prefers
// $XDG_RUNTIME_DIR (set on most modern Linux), falls back to $HOME/.fletcher,
// and finally /tmp for the headless / no-home edge case.
func defaultSocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "fletcher", "fletcher.sock")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".fletcher", "fletcher.sock")
	}
	return "/tmp/fletcher.sock"
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
