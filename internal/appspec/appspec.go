// Package appspec is the small launch spec Fletcher captures from a Docker
// image's run config at import time and writes into the rootfs, so the guest
// init can run the image's own app on boot (Milestone 9). It is shared by the
// CLI (which writes it during `image import`) and the guest init (which reads
// it), so it carries no platform-specific code.
package appspec

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Path is where the spec lives inside the rootfs. The guest init reads it when
// the kernel cmdline asks for app mode.
const Path = "/etc/fletcher/app.json"

// Spec is the subset of a Docker image's run config Fletcher needs to launch the
// image's app the way `docker run` would.
type Spec struct {
	// Entrypoint and Cmd combine into the argv, exactly as Docker layers them.
	Entrypoint []string `json:"entrypoint,omitempty"`
	Cmd        []string `json:"cmd,omitempty"`
	// Env are "KEY=value" entries baked into the image.
	Env []string `json:"env,omitempty"`
	// WorkingDir is the image's WORKDIR; empty means "/".
	WorkingDir string `json:"workingDir,omitempty"`
	// User is the image's USER (name or uid[:gid]); empty means root. Captured
	// now; precise mapping is a follow-up.
	User string `json:"user,omitempty"`
}

// Argv is the effective command: Entrypoint followed by Cmd, as Docker forms it.
func (s Spec) Argv() []string {
	argv := make([]string, 0, len(s.Entrypoint)+len(s.Cmd))
	argv = append(argv, s.Entrypoint...)
	argv = append(argv, s.Cmd...)
	return argv
}

// Write marshals the spec to dest (creating parent dirs). dest is the on-disk
// path in the rootfs staging dir, e.g. <staging>/etc/fletcher/app.json.
func Write(s Spec, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil { //nolint:gosec // non-secret app spec dir in the rootfs
		return fmt.Errorf("create app spec dir: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal app spec: %w", err)
	}
	if err := os.WriteFile(dest, data, 0o644); err != nil { //nolint:gosec // non-secret launch spec read by the guest init
		return fmt.Errorf("write app spec: %w", err)
	}
	return nil
}

// Read loads a spec written by Write.
func Read(path string) (Spec, error) {
	data, err := os.ReadFile(path) //nolint:gosec // fixed path inside the guest rootfs
	if err != nil {
		return Spec{}, err
	}
	var s Spec
	if err := json.Unmarshal(data, &s); err != nil {
		return Spec{}, fmt.Errorf("parse app spec: %w", err)
	}
	return s, nil
}
