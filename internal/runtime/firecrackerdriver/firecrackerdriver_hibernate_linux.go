//go:build linux

package firecrackerdriver

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"

	fcruntime "github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver/guestproto"
)

// Hibernation (Firecracker snapshot/restore) is the instant-wake layer on top
// of cold boot. It is a UX optimisation, never load-bearing for durability:
// disk is always the source of truth (DESIGN.md §5/§11), so a snapshot that
// cannot be restored just falls back to a cold boot from the fork.

// snapshotPaths returns the memory, VM-state, and metadata file paths a
// hibernated session keeps in its vmDir.
func snapshotPaths(vmDir string) (mem, state, meta string) {
	return filepath.Join(vmDir, "snapshot.mem"),
		filepath.Join(vmDir, "snapshot.state"),
		filepath.Join(vmDir, "snapshot.meta")
}

// snapshotIdentity fingerprints the VMM binary and guest kernel a snapshot is
// tied to. Firecracker snapshots are only restorable by the same VMM + kernel,
// so a Fletcher upgrade (new bundled assets) changes this and invalidates any
// stale snapshot.
func (d *Driver) snapshotIdentity() string {
	return fileFingerprint(d.firecrackerBinary) + "|" + fileFingerprint(d.kernelPath)
}

func fileFingerprint(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return "?"
	}
	return fmt.Sprintf("%d:%d", fi.Size(), fi.ModTime().UnixNano())
}

// hasValidSnapshot reports whether vmDir holds a hibernation snapshot this
// daemon can restore (present, and tied to the current VMM + kernel).
func (d *Driver) hasValidSnapshot(vmDir string) bool {
	_, statePath, metaPath := snapshotPaths(vmDir)
	if _, err := os.Stat(statePath); err != nil {
		return false
	}
	meta, err := os.ReadFile(metaPath) //nolint:gosec // metaPath is derived from the daemon's own run dir
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(meta)) == d.snapshotIdentity()
}

// restoreSession resumes a hibernated VM from its snapshot, reusing the same
// vmDir (so the stored vsock UDS path is recreated at the path the host dials).
func (d *Driver) restoreSession(ctx context.Context, spec fcruntime.SessionSpec, vmDir string) (fcruntime.SessionHandle, error) {
	apiSock := filepath.Join(vmDir, "fc.sock")
	vsockUDS := filepath.Join(vmDir, "v.sock")
	memPath, statePath, _ := snapshotPaths(vmDir)

	// Firecracker refuses to bind over a stale API socket and recreates the
	// vsock UDS itself; clear sockets but keep the snapshot files.
	clearSockets(vmDir)

	vmCtx, vmCancel := context.WithCancel(context.WithoutCancel(ctx))

	console := &capWriter{max: 32 << 10}
	cfg := d.machineConfig(apiSock, vsockUDS, spec.RootfsPath, true)
	// On restore the snapshot already carries the vsock device (Firecracker
	// recreates its UDS listener at the stored path). The SDK's snapshot handler
	// list still runs AddVsocks afterwards, which the VMM rejects post-boot, so
	// drop the device here to make that a no-op. The host still dials vsockUDS.
	cfg.VsockDevices = nil
	fcCmd := firecracker.VMCommandBuilder{}.
		WithBin(d.firecrackerBinary).
		WithSocketPath(apiSock).
		WithStdout(console).
		WithStderr(console).
		Build(vmCtx)

	machine, err := firecracker.NewMachine(vmCtx, cfg,
		firecracker.WithProcessRunner(fcCmd),
		firecracker.WithLogger(d.log),
		firecracker.WithSnapshot(memPath, statePath, func(c *firecracker.SnapshotConfig) {
			c.ResumeVM = true
		}),
	)
	if err != nil {
		vmCancel()
		return nil, fmt.Errorf("firecracker: new restore machine: %w", err)
	}
	if err := machine.Start(vmCtx); err != nil {
		_ = machine.StopVMM()
		vmCancel()
		return nil, fmt.Errorf("firecracker: load snapshot: %w%s", err, console.suffix())
	}

	// Confirm the resumed guest's control server answers, so the session is
	// exec-ready when we return.
	probeCtx, cancel := context.WithTimeout(vmCtx, sessionStartGrace)
	defer cancel()
	probe, derr := dialGuest(probeCtx, vsockUDS, guestproto.ControlPort)
	if derr != nil {
		_ = machine.StopVMM()
		vmCancel()
		return nil, fmt.Errorf("firecracker: restored guest not ready: %w%s", derr, console.suffix())
	}
	_ = probe.Close()

	// The snapshot is now superseded by the running VM. Remove it so a crash
	// falls back to a clean cold boot, not a stale memory image, and so the
	// memory file stops costing disk while the session runs.
	removeSnapshotFiles(vmDir)

	// The restored guest's loopback forwards are live again (resumed from memory),
	// but their host-side proxy sockets were cleared with the other sockets, so
	// recreate them. The guest's forwardsOnce already fired at its original cold
	// boot, so the resent setup is a no-op there - it just rebuilds the host side.
	effEnv := envForPolicy(spec.EgressPolicy, spec.Env)
	forwardLns, ferr := d.startSessionForwards(vmCtx, vsockUDS, d.forwardsForPolicy(spec.EgressPolicy), effEnv)
	if ferr != nil {
		d.logger.Warn("session service forwards not fully re-established after restore; model gateway/MCP may be unreachable in this session",
			slog.String("session_id", spec.SessionID), slog.String("err", ferr.Error()))
	}

	return &fcSession{
		machine:    machine,
		vsockUDS:   vsockUDS,
		vmDir:      vmDir,
		vmCancel:   vmCancel,
		env:        effEnv,
		forwardLns: forwardLns,
		snapID:     d.snapshotIdentity(),
	}, nil
}

// hibernate pauses the VM, snapshots its memory and state to the vmDir, and
// exits the VMM. The vmDir is kept so the next Start can restore from it.
func (s *fcSession) hibernate(ctx context.Context) error {
	memPath, statePath, _ := snapshotPaths(s.vmDir)
	if err := s.machine.PauseVM(ctx); err != nil {
		return fmt.Errorf("pause vm: %w", err)
	}
	if err := s.machine.CreateSnapshot(ctx, memPath, statePath); err != nil {
		_ = s.machine.ResumeVM(ctx) // un-pause so the caller's fallback can shut down cleanly
		return fmt.Errorf("create snapshot: %w", err)
	}
	if err := writeSnapshotMeta(s.vmDir, s.snapID); err != nil {
		removeSnapshotFiles(s.vmDir)
		_ = s.machine.ResumeVM(ctx)
		return fmt.Errorf("write snapshot meta: %w", err)
	}
	_ = s.machine.StopVMM()
	s.closeForwards()
	s.vmCancel()
	// Keep vmDir: it holds the snapshot the next Start restores from.
	return nil
}

func writeSnapshotMeta(vmDir, identity string) error {
	_, _, metaPath := snapshotPaths(vmDir)
	return os.WriteFile(metaPath, []byte(identity), 0o600)
}

func removeSnapshotFiles(vmDir string) {
	mem, state, meta := snapshotPaths(vmDir)
	for _, p := range []string{mem, state, meta} {
		_ = os.Remove(p)
	}
}

// clearSockets removes the VMM API socket and any vsock UDS files, leaving
// snapshot files intact, so a restore can recreate them fresh.
func clearSockets(vmDir string) {
	_ = os.Remove(filepath.Join(vmDir, "fc.sock"))
	matches, _ := filepath.Glob(filepath.Join(vmDir, "v.sock*"))
	for _, m := range matches {
		_ = os.Remove(m)
	}
}
