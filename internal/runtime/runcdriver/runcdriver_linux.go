//go:build linux

// Package runcdriver is the Linux runc-backed runtime driver. It writes
// a minimal OCI bundle (config.json plus a rootfs pointer to the
// snapshot path) into a working directory and invokes the runc binary
// to execute the job's command inside Linux namespaces.
//
// Real isolation depends on the operator's runc + kernel config:
// rootless-runc with user namespaces gives meaningful isolation on a
// home server; running as root unlocks the full set of capabilities
// the OCI spec supports. The driver is deliberately conservative - it
// drops all capabilities and disables the network namespace by default.
//
// This package compiles only on Linux; runcdriver_other.go provides a
// "not supported" stub for cross-compilation.
package runcdriver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/joshjon/fletcher/internal/runtime"
)

// Driver is a runtime.Driver that runs jobs as OCI containers via runc.
type Driver struct {
	binary       string
	bundleDir    string
	stateDir     string
	forwarderBin string
	forwards     []Forward
	counter      atomic.Uint64
}

// Forward describes one TCP-listen -> unix-socket relay the fork runs so an
// agent can reach a daemon service (gateway, MCP). The fork has only loopback,
// so this is its single path to the daemon - and thus no general egress.
type Forward struct {
	// Listen is the loopback address the in-fork forwarder binds, matching the
	// base-URL injected into the job (e.g. "127.0.0.1:11500").
	Listen string
	// HostSocket is the daemon-side unix socket the relay connects to.
	HostSocket string
}

// Options configures a Driver.
type Options struct {
	// Binary is the path to the runc executable; defaults to "runc"
	// (resolved via $PATH).
	Binary string
	// BundleDir is the directory under which transient OCI bundles are
	// materialised before each run. Defaults to a fresh subdir of os.TempDir.
	BundleDir string
	// ForwarderBinary is the host path to a `fletcher` binary bind-mounted
	// into the fork to run the in-fork forwarders (`fletcher fork-run`). When
	// empty, or Forwards is empty, the job command runs without forwarders.
	ForwarderBinary string
	// Forwards are the daemon services the fork should reach.
	Forwards []Forward
}

// New constructs a Driver. Returns an error if the runc binary is not
// reachable.
func New(opts Options) (*Driver, error) {
	binary := opts.Binary
	if binary == "" {
		binary = "runc"
	}
	if _, err := exec.LookPath(binary); err != nil {
		return nil, fmt.Errorf("runc: %s not found in PATH: %w", binary, err)
	}
	bundleDir := opts.BundleDir
	if bundleDir == "" {
		var err error
		bundleDir, err = os.MkdirTemp("", "fletcher-runc-")
		if err != nil {
			return nil, fmt.Errorf("create bundle dir: %w", err)
		}
	}
	if err := os.MkdirAll(bundleDir, 0o700); err != nil {
		return nil, fmt.Errorf("ensure bundle dir: %w", err)
	}
	// runc's default state root (/run/runc) is root-only; the unprivileged
	// daemon needs a writable one of its own for rootless containers.
	stateDir := filepath.Join(bundleDir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("ensure runc state dir: %w", err)
	}
	return &Driver{
		binary:       binary,
		bundleDir:    bundleDir,
		stateDir:     stateDir,
		forwarderBin: opts.ForwarderBinary,
		forwards:     opts.Forwards,
	}, nil
}

// Run materialises a bundle for spec, executes 'runc run', and returns
// the process exit code.
func (d *Driver) Run(ctx context.Context, spec runtime.Spec, stdout, stderr io.Writer) (runtime.Result, error) {
	id := fmt.Sprintf("fletcher-%d-%d", time.Now().UnixNano(), d.counter.Add(1))
	bundle := filepath.Join(d.bundleDir, id)
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		return runtime.Result{}, fmt.Errorf("create bundle: %w", err)
	}
	defer func() { _ = os.RemoveAll(bundle) }()

	args, mounts := d.jobArgsAndMounts(spec)
	if err := writeOCIConfig(bundle, spec, args, mounts); err != nil {
		return runtime.Result{}, fmt.Errorf("write config: %w", err)
	}

	//nolint:gosec // d.binary is the configured runc; state/bundle/id are daemon-generated
	cmd := exec.CommandContext(ctx, d.binary, "--root", d.stateDir, "run", "--bundle", bundle, id)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			return runtime.Result{}, fmt.Errorf("job cancelled: %w", ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			//nolint:gosec // POSIX exit codes fit in int32
			return runtime.Result{ExitCode: int32(exitErr.ExitCode())}, nil
		}
		return runtime.Result{}, fmt.Errorf("runc run: %w", err)
	}
	return runtime.Result{ExitCode: 0}, nil
}

// forkBinPath is where the daemon's `fletcher` binary is bind-mounted inside
// the fork to run the forwarders. fwdSocketPath returns the in-fork path the
// i-th daemon socket is bind-mounted at; both live under dirs the base image
// already has so runc can create the mountpoints.
const forkBinPath = "/usr/local/bin/fletcher-forward"

func fwdSocketPath(i int) string {
	return fmt.Sprintf("/run/.fletcher-fwd-%d.sock", i)
}

// jobArgsAndMounts builds the container process args and bind-mount list. When
// forwarding is configured, the command is wrapped with `fletcher fork-run`
// (bind-mounted in) plus the daemon sockets, so the agent reaches the daemon
// over loopback with no egress; otherwise the command runs directly.
func (d *Driver) jobArgsAndMounts(spec runtime.Spec) ([]string, []runtime.Mount) {
	command := []string{"/bin/sh", "-c", spec.Command}
	mounts := spec.Mounts

	if d.forwarderBin == "" || len(d.forwards) == 0 {
		return command, mounts
	}

	mounts = append(mounts, runtime.Mount{Source: d.forwarderBin, Destination: forkBinPath, ReadOnly: true})
	args := []string{forkBinPath, "fork-run"}
	for i, f := range d.forwards {
		sock := fwdSocketPath(i)
		mounts = append(mounts, runtime.Mount{Source: f.HostSocket, Destination: sock, ReadOnly: false})
		args = append(args, "--forward", f.Listen+"="+sock)
	}
	args = append(args, "--")
	args = append(args, command...)
	return args, mounts
}

// writeOCIConfig produces a minimal-but-valid config.json inside bundle that
// runs args inside spec.WorkDir as rootfs with the given bind mounts.
func writeOCIConfig(bundle string, spec runtime.Spec, args []string, mounts []runtime.Mount) error {
	if spec.WorkDir == "" {
		return errors.New("spec.WorkDir is required (must point at the snapshot rootfs)")
	}
	cfg := ociConfig(spec, args, mounts)
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(bundle, "config.json"), b, 0o600)
}

// minimalOCIConfig builds a small but valid OCI runtime spec for runc.
// We drop the network namespace and all capabilities by default - jobs
// reach out via the daemon-mediated MCP/gateway, not directly.
//
// The daemon runs unprivileged (the `fletcher` user), so runc is rootless and
// needs a user namespace. We use the simplest mapping that needs no
// /etc/subuid or newuidmap: container uid/gid 0 maps to the daemon's own
// uid/gid (a single-ID self-map runc can write directly). The job process runs
// as container root, which is the confined unprivileged daemon user on the
// host. For that to own the rootfs, `fletcher image import` chowns the
// template to the daemon user; HOME=/home/fletcher and cwd /workspace match
// the fletcher-base image so the agent launchers resolve their install.
func ociConfig(spec runtime.Spec, args []string, mounts []runtime.Mount) map[string]any {
	env := append([]string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/home/fletcher",
	}, spec.Env...)

	hostUID := os.Geteuid()
	hostGID := os.Getegid()

	return map[string]any{
		"ociVersion": "1.0.2",
		"process": map[string]any{
			"terminal": false,
			"user":     map[string]any{"uid": 0, "gid": 0},
			"args":     args,
			"env":      env,
			"cwd":      "/workspace",
			"capabilities": map[string]any{
				"bounding":  []string{},
				"effective": []string{},
				"permitted": []string{},
				"ambient":   []string{},
			},
			"noNewPrivileges": true,
		},
		"root": map[string]any{
			"path":     spec.WorkDir,
			"readonly": false,
		},
		"hostname": "fletcher-job",
		"mounts":   buildMounts(mounts),
		"linux": map[string]any{
			"namespaces": []map[string]any{
				{"type": "pid"},
				{"type": "ipc"},
				{"type": "uts"},
				{"type": "mount"},
				{"type": "user"},
				{"type": "network"},
			},
			"uidMappings": []map[string]any{
				{"containerID": 0, "hostID": hostUID, "size": 1},
			},
			"gidMappings": []map[string]any{
				{"containerID": 0, "hostID": hostGID, "size": 1},
			},
		},
	}
}

// buildMounts assembles the OCI mount list: the standard filesystem
// pseudo-mounts every container needs plus any caller-supplied bind
// mounts (trusted-credential dirs, per DESIGN.md §5 Phase 12).
func buildMounts(extra []runtime.Mount) []map[string]any {
	//nolint:prealloc // small fixed base list; readability over a capacity hint
	out := []map[string]any{
		{"destination": "/proc", "type": "proc", "source": "proc"},
		{"destination": "/dev", "type": "tmpfs", "source": "tmpfs", "options": []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{"destination": "/dev/pts", "type": "devpts", "source": "devpts", "options": []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
		{"destination": "/dev/shm", "type": "tmpfs", "source": "shm", "options": []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
		{"destination": "/dev/mqueue", "type": "mqueue", "source": "mqueue", "options": []string{"nosuid", "noexec", "nodev"}},
		{"destination": "/sys", "type": "sysfs", "source": "sysfs", "options": []string{"nosuid", "noexec", "nodev", "ro"}},
	}
	for _, m := range extra {
		opts := []string{"rbind"}
		if m.ReadOnly {
			opts = append(opts, "ro")
		} else {
			opts = append(opts, "rw")
		}
		out = append(out, map[string]any{
			"destination": m.Destination,
			"type":        "bind",
			"source":      m.Source,
			"options":     opts,
		})
	}
	return out
}
