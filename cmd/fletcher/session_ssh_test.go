package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestForgetSessionHostKey covers the host-key eviction that lets a session
// recreated under the same ref reconnect without tripping ssh's changed-key
// guard. See forgetSessionHostKey for why this matters.
func TestForgetSessionHostKey(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available")
	}
	t.Setenv("HOME", t.TempDir())

	// A missing known_hosts means there is nothing to forget, not an error.
	require.NoError(t, forgetSessionHostKey(context.Background(), "dev"))

	dir, err := fletcherSSHDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	knownHosts := filepath.Join(dir, "known_hosts")
	require.NoError(t, os.WriteFile(knownHosts,
		[]byte("dev ssh-ed25519 AAAAfakekeydev\nother ssh-ed25519 AAAAfakekeyother\n"), 0o600))

	require.NoError(t, forgetSessionHostKey(context.Background(), "dev"))

	got, err := os.ReadFile(knownHosts)
	require.NoError(t, err)
	require.NotContains(t, string(got), "AAAAfakekeydev", "the target host's pin is evicted")
	require.Contains(t, string(got), "AAAAfakekeyother", "other hosts are left alone")

	// ssh-keygen -R leaves a .old backup; forgetSessionHostKey must clean it up.
	_, err = os.Stat(knownHosts + ".old")
	require.True(t, os.IsNotExist(err), "the .old backup is removed")
}
