//go:build linux && integration

package firecrackerdriver

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/background"
	fcruntime "github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver/guestagent"
	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver/guestproto"
	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver/vmm"
)

func TestFirecrackerOlderGuestUpgrade(t *testing.T) {
	template, oldGuest := os.Getenv("FLETCHER_TEST_ROOTFS"), os.Getenv("FLETCHER_TEST_OLD_GUEST")
	if template == "" || oldGuest == "" {
		t.Skip("set FLETCHER_TEST_ROOTFS and FLETCHER_TEST_OLD_GUEST to test a pre-named-terminal guest upgrade")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil || !vmm.Available() {
		t.Skip("KVM and bundled VMM assets are required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	legacyInit, err := os.ReadFile(oldGuest)
	require.NoError(t, err)
	oldFP := fingerprintBytes(legacyInit)
	currentFP, err := guestagent.Fingerprint()
	require.NoError(t, err)
	require.NotEqual(t, currentFP, oldFP, "the fixture must contain a historical guest init")
	// Keep Unix socket paths short, independent of the test's name.
	dir, err := os.MkdirTemp("", "fcu-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	rootfs := filepath.Join(dir, "rootfs.ext4")
	copyUpgradeRootfs(t, template, rootfs)
	bundle, err := vmm.Extract(filepath.Join(dir, "vmm"))
	require.NoError(t, err)
	d, err := New(Options{FirecrackerBinary: bundle.FirecrackerPath, KernelPath: bundle.KernelPath, RunDir: filepath.Join(dir, "run")})
	require.NoError(t, err)
	d.repairRootfs(ctx, rootfs)
	require.NoError(t, writeRootfsFile(ctx, rootfs, guestagent.InitPath, legacyInit, "0100755"))
	require.NoError(t, writeRootfsFile(ctx, rootfs, initFingerprintPath, []byte(oldFP), "0100644"))
	require.Equal(t, oldFP, fingerprintBytes(readRootfsFile(ctx, rootfs, guestagent.InitPath)))
	spec := fcruntime.SessionSpec{SessionID: "guest-upgrade", RootfsPath: rootfs, EgressPolicy: "none"}
	legacy := bootUpgradeFixture(t, ctx, d, spec)
	// A historical daemon records its bundled guest fingerprint in snapshot
	// identity. Model that metadata without changing production boot behavior.
	legacy.snapID = strings.Replace(d.snapshotIdentity(spec), currentFP, oldFP, 1)
	execGuest := func(h fcruntime.SessionHandle, command string) string {
		t.Helper()
		var out, stderr bytes.Buffer
		result, err := h.Exec(ctx, fcruntime.Spec{Command: command}, &out, &stderr)
		require.NoError(t, err)
		require.Zero(t, result.ExitCode, stderr.String())
		return strings.TrimSpace(out.String())
	}
	require.Equal(t, oldFP, execGuest(legacy, "sha256sum /sbin/fletcher-init | cut -d ' ' -f 1"))
	oldBootID := execGuest(legacy, "cat /proc/sys/kernel/random/boot_id")
	execGuest(legacy, "mkdir -p '/workspace/upgrade project'; printf guest-upgrade-kept > '/workspace/upgrade project/keep'; sync")
	detach := attachUpgradeShell(t, ctx, legacy, fcruntime.ShellSpec{ControlMode: true})
	detach()
	panes := "tmux -L fletcher list-panes -a -F '#{session_name}|#{pane_id}|#{pane_pid}|#{pane_current_path}'"
	before := execGuest(legacy, panes)
	require.True(t, strings.HasPrefix(before, "main|"))
	var rejected bytes.Buffer
	target := fcruntime.ShellSpec{TerminalName: "project_upgrade-main", WorkDir: "/workspace/upgrade project", ControlMode: true}
	_, err = legacy.Shell(ctx, target, strings.NewReader(""), &rejected, nil)
	require.ErrorContains(t, err, "cold-restart")
	require.Empty(t, rejected.String(), "an unsupported named target must not attach the legacy terminal")
	require.Equal(t, before, execGuest(legacy, panes), "rejection must leave the legacy shell untouched")
	execGuest(legacy, "sync")
	require.NoError(t, legacy.Stop(ctx))
	require.FileExists(t, filepath.Join(legacy.vmDir, "snapshot.state"), "exercise a real old-guest memory snapshot")
	require.False(t, d.hasValidSnapshot(legacy.vmDir, spec), "an old guest snapshot must not bypass the new init")

	upgraded, err := d.StartSession(ctx, spec)
	require.NoError(t, err)
	active := upgraded
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer done()
		if active != nil {
			require.NoError(t, active.Stop(cleanup))
		}
		require.NoError(t, d.DiscardSession(cleanup, spec.SessionID))
	})
	newBootID := execGuest(upgraded, "cat /proc/sys/kernel/random/boot_id")
	require.NotEqual(t, oldBootID, newBootID, "the current driver must cold-boot rather than resume historical guest memory")
	require.Equal(t, currentFP, execGuest(upgraded, "cat /etc/fletcher/init.sha256"))
	require.Equal(t, currentFP, execGuest(upgraded, "sha256sum /sbin/fletcher-init | cut -d ' ' -f 1"))
	require.Equal(t, "guest-upgrade-kept", execGuest(upgraded, "cat '/workspace/upgrade project/keep'"))
	detach = attachUpgradeShell(t, ctx, upgraded, target)
	detach()
	namedPane := execGuest(upgraded, panes)
	require.True(t, strings.HasPrefix(namedPane, target.TerminalName+"|"))
	require.True(t, strings.HasSuffix(namedPane, "|"+target.WorkDir))
	require.NotContains(t, namedPane, "\n", "the named request must create only its requested terminal")
	detach = attachUpgradeShell(t, ctx, upgraded, target)
	detach()
	require.Equal(t, namedPane, execGuest(upgraded, panes), "named reattachment after upgrade must retain the same shell")
	execGuest(upgraded, "sync")
	require.NoError(t, upgraded.Stop(ctx))
	active = nil
	resumed, err := d.StartSession(ctx, spec)
	require.NoError(t, err)
	active = resumed
	require.Equal(t, newBootID, execGuest(resumed, "cat /proc/sys/kernel/random/boot_id"), "the upgraded guest must support ordinary hibernation resume")
	require.Equal(t, namedPane, execGuest(resumed, panes))
	require.Equal(t, "guest-upgrade-kept", execGuest(resumed, "cat '/workspace/upgrade project/keep'"))
	t.Log("verified historical guest rejection, old-snapshot invalidation, in-place init refresh, retained files and upgraded named-shell reconnect/resume")
}

func copyUpgradeRootfs(t *testing.T, src, dst string) {
	t.Helper()
	input, err := os.Open(src)
	require.NoError(t, err)
	defer input.Close()
	output, err := os.Create(dst)
	require.NoError(t, err)
	defer output.Close()
	_, err = io.Copy(output, input)
	require.NoError(t, err)
	require.NoError(t, output.Close())
}

// bootUpgradeFixture boots the historical init without the production refresh
// step, solely to reproduce an already-running VM from an older daemon.
func bootUpgradeFixture(t *testing.T, ctx context.Context, d *Driver, spec fcruntime.SessionSpec) *fcSession {
	t.Helper()
	vmDir := filepath.Join(d.runDir, "session-"+spec.SessionID)
	require.NoError(t, os.MkdirAll(vmDir, 0o750))
	apiSock, vsockUDS := filepath.Join(vmDir, "fc.sock"), filepath.Join(vmDir, "v.sock")
	vmCtx, vmCancel := context.WithCancel(ctx)
	t.Cleanup(vmCancel)
	console := &capWriter{max: 32 << 10}
	command := firecracker.VMCommandBuilder{}.WithBin(d.firecrackerBinary).WithSocketPath(apiSock).WithStdout(console).WithStderr(console).Build(vmCtx)
	machine, err := firecracker.NewMachine(vmCtx, d.machineConfig(apiSock, vsockUDS, spec.RootfsPath, true, false, ""), firecracker.WithProcessRunner(command), firecracker.WithLogger(d.log))
	require.NoError(t, err)
	t.Cleanup(func() { _ = machine.StopVMM() })
	require.NoError(t, machine.Start(vmCtx), console.suffix())
	probe, err := dialGuest(ctx, vsockUDS, guestproto.ControlPort)
	require.NoError(t, err, console.suffix())
	require.NoError(t, probe.Close())
	return &fcSession{machine: machine, vsockUDS: vsockUDS, vmDir: vmDir, vmCancel: vmCancel}
}

type upgradeOutput struct {
	mu      sync.Mutex
	text    strings.Builder
	changed chan struct{}
}

func (w *upgradeOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.text.Write(p)
	select {
	case w.changed <- struct{}{}:
	default:
	}
	return n, err
}

func attachUpgradeShell(t *testing.T, ctx context.Context, h fcruntime.SessionHandle, spec fcruntime.ShellSpec) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	input, writer := io.Pipe()
	output := &upgradeOutput{changed: make(chan struct{}, 1)}
	done := make(chan error, 1)
	background.GoNamed(ctx, "test.guest-upgrade.shell", func(ctx context.Context) {
		defer close(done)
		_, err := h.Shell(ctx, spec, input, output, nil)
		done <- err
	})
	detach := sync.OnceFunc(func() {
		cancel()
		_ = input.Close()
		_ = writer.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("guest upgrade shell did not detach")
		}
	})
	t.Cleanup(detach)
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		output.mu.Lock()
		ready := strings.Contains(output.text.String(), "%session-changed")
		output.mu.Unlock()
		if ready {
			return detach
		}
		select {
		case <-output.changed:
		case err := <-done:
			t.Fatalf("guest upgrade terminal ended before readiness: %v", err)
		case <-timer.C:
			t.Fatal("guest upgrade terminal did not become ready")
		}
	}
}
