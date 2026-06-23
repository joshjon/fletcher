// Package guestagent embeds the fletcher-guest binary (the microVM init) and
// writes it into a rootfs at import time, so the single fletcher binary carries
// everything needed to boot a job - no separate guest install.
//
// The binary is arch-selected via build constraints (embed_linux_*.go), built
// from cmd/fletcher-guest by `make build`, gitignored, and accompanied by a
// committed about.txt so a fresh checkout still compiles. On platforms without
// a Firecracker build, embed_other.go yields an empty set and Available()
// reports false.
package guestagent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
)

// InitPath is where the agent is injected inside the rootfs, and what the
// kernel boots as init (init=/sbin/fletcher-init).
const InitPath = "/sbin/fletcher-init"

// binaryName is the embedded asset's filename.
const binaryName = "fletcher-guest"

// ErrNotBundled is returned when this build does not carry the guest agent.
var ErrNotBundled = fmt.Errorf("guest agent binary not bundled in this build (run `make build`, which builds cmd/fletcher-guest)")

// Available reports whether this build carries the guest agent.
func Available() bool {
	if assetDir == "" {
		return false
	}
	f, err := assetsFS.Open(path.Join(assetDir, binaryName))
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// Bytes returns the embedded guest agent binary.
func Bytes() ([]byte, error) {
	if !Available() {
		return nil, ErrNotBundled
	}
	data, err := assetsFS.ReadFile(path.Join(assetDir, binaryName))
	if err != nil {
		return nil, fmt.Errorf("read embedded guest agent: %w", err)
	}
	return data, nil
}

// Fingerprint is the hex SHA-256 of the embedded guest agent, used to tell
// whether a rootfs already carries this exact init before refreshing it.
func Fingerprint() (string, error) {
	data, err := Bytes()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// WriteTo writes the embedded guest agent to dest with mode 0755.
func WriteTo(dest string) error {
	if !Available() {
		return ErrNotBundled
	}
	data, err := assetsFS.ReadFile(path.Join(assetDir, binaryName))
	if err != nil {
		return fmt.Errorf("read embedded guest agent: %w", err)
	}
	if err := os.WriteFile(dest, data, 0o755); err != nil { //nolint:gosec // the guest init must be executable
		return fmt.Errorf("write guest agent: %w", err)
	}
	return os.Chmod(dest, 0o755) //nolint:gosec // WriteFile honours umask; force the exec bit on the init
}
