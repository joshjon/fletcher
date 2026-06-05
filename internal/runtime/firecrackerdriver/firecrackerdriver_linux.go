//go:build linux

// Package firecrackerdriver runs a job inside a Firecracker microVM: the
// strong-isolation (KVM) runtime tier. It boots the bundled kernel against the
// per-job ext4 rootfs (cloned by the ext4 snapshot driver), with the
// fletcher-guest agent as init. Host and guest talk over a single vsock
// connection (see guestproto): the host sends the job spec, the guest runs the
// command and streams its output back, then powers the VM off.
//
// The VM has no network interface - only vsock - so an agent inside it can
// reach the daemon's gateway (a later phase wires that over vsock) but has no
// route to the internet. That is the §5/§6 trust boundary, enforced
// structurally by the absence of a NIC.
//
// Per DESIGN.md §10 all KVM/Firecracker calls live behind the runtime.Driver
// interface; this is its implementation.
package firecrackerdriver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/sirupsen/logrus"

	fcruntime "github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver/guestagent"
	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver/guestproto"
)

// guestWorkDir is the in-VM working directory for the job command, matching the
// runc runtime's cwd so jobs behave the same across runtimes.
const guestWorkDir = "/workspace"

// shutdownGrace bounds how long we wait for the VM to power off after the job
// reports its exit code before forcing the VMM down.
const shutdownGrace = 20 * time.Second

// Driver is the Firecracker-backed runtime.Driver.
type Driver struct {
	firecrackerBinary string
	kernelPath        string
	runDir            string
	vcpuCount         int64
	memSizeMib        int64
	log               *logrus.Entry
}

// Options configures a Driver. FirecrackerBinary and KernelPath are the VMM
// assets extracted by the vmm package; RunDir holds per-VM control sockets.
type Options struct {
	FirecrackerBinary string
	KernelPath        string
	RunDir            string
	// VcpuCount defaults to 1, MemSizeMib to 512 when zero.
	VcpuCount  int64
	MemSizeMib int64
}

// New constructs a Driver, validating the VMM assets are present.
func New(opts Options) (*Driver, error) {
	if opts.FirecrackerBinary == "" || opts.KernelPath == "" {
		return nil, errors.New("firecracker: FirecrackerBinary and KernelPath are required")
	}
	for _, p := range []string{opts.FirecrackerBinary, opts.KernelPath} {
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("firecracker: VMM asset %s: %w", p, err)
		}
	}
	if opts.RunDir == "" {
		return nil, errors.New("firecracker: RunDir is required")
	}
	if err := os.MkdirAll(opts.RunDir, 0o750); err != nil {
		return nil, fmt.Errorf("firecracker: create run dir: %w", err)
	}
	vcpu := opts.VcpuCount
	if vcpu == 0 {
		vcpu = 1
	}
	mem := opts.MemSizeMib
	if mem == 0 {
		mem = 512
	}
	// Keep the SDK's own logging quiet; we surface failures via the console
	// capture and wrapped errors instead.
	logger := logrus.New()
	logger.SetOutput(io.Discard)

	return &Driver{
		firecrackerBinary: opts.FirecrackerBinary,
		kernelPath:        opts.KernelPath,
		runDir:            opts.RunDir,
		vcpuCount:         vcpu,
		memSizeMib:        mem,
		log:               logrus.NewEntry(logger),
	}, nil
}

// Run boots a microVM for spec, runs the command, and returns its exit code.
// spec.WorkDir is the host path to the per-job ext4 rootfs (from the snapshot
// driver). Output is streamed to stdout/stderr as the command produces it.
func (d *Driver) Run(ctx context.Context, spec fcruntime.Spec, stdout, stderr io.Writer) (fcruntime.Result, error) {
	if spec.WorkDir == "" {
		return fcruntime.Result{}, errors.New("firecracker: spec.WorkDir is required (the per-job rootfs image)")
	}
	rootfs := spec.WorkDir

	vmDir := filepath.Join(d.runDir, sanitiseID(spec.JobID))
	if err := os.MkdirAll(vmDir, 0o750); err != nil {
		return fcruntime.Result{}, fmt.Errorf("firecracker: create vm dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(vmDir) }()

	apiSock := filepath.Join(vmDir, "fc.sock")
	vsockUDS := filepath.Join(vmDir, "v.sock")

	// The guest dials the host (CID 2) on guestproto.Port; firecracker surfaces
	// that as a connection to "<uds>_<port>" on the host, so we listen there.
	hostConnPath := fmt.Sprintf("%s_%d", vsockUDS, guestproto.Port)
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", hostConnPath)
	if err != nil {
		return fcruntime.Result{}, fmt.Errorf("firecracker: listen vsock: %w", err)
	}
	defer func() { _ = ln.Close() }()

	console := &capWriter{max: 32 << 10}
	cfg := d.machineConfig(apiSock, vsockUDS, rootfs)
	fcCmd := firecracker.VMCommandBuilder{}.
		WithBin(d.firecrackerBinary).
		WithSocketPath(apiSock).
		WithStdout(console).
		WithStderr(console).
		Build(ctx)

	machine, err := firecracker.NewMachine(ctx, cfg,
		firecracker.WithProcessRunner(fcCmd),
		firecracker.WithLogger(d.log),
	)
	if err != nil {
		return fcruntime.Result{}, fmt.Errorf("firecracker: new machine: %w", err)
	}

	if err := machine.Start(ctx); err != nil {
		return fcruntime.Result{}, fmt.Errorf("firecracker: start microVM: %w%s", err, console.suffix())
	}
	// Ensure the VMM is gone on every exit path.
	defer func() { _ = machine.StopVMM() }()

	exitCode, err := d.serveJob(ctx, ln, spec, stdout, stderr)
	if err != nil {
		return fcruntime.Result{}, fmt.Errorf("firecracker: %w%s", err, console.suffix())
	}

	// The guest powers off after sending its exit frame; wait for the VMM to
	// exit so cleanup is clean, but don't hang forever.
	waitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	if werr := machine.Wait(waitCtx); werr != nil && !errors.Is(werr, context.Canceled) {
		// A non-clean shutdown doesn't change the job's result - we already have
		// its exit code - but is worth not masking if Wait surfaced something.
		d.log.WithError(werr).Debug("microVM wait returned")
	}
	return fcruntime.Result{ExitCode: exitCode}, nil
}

// serveJob accepts the guest's vsock connection, sends the spec, and demuxes
// the streamed output back to stdout/stderr until the guest reports its exit
// code. It honours ctx cancellation by closing the connection.
func (d *Driver) serveJob(ctx context.Context, ln net.Listener, spec fcruntime.Spec, stdout, stderr io.Writer) (int32, error) {
	conn, err := acceptCtx(ctx, ln)
	if err != nil {
		return 0, fmt.Errorf("accept guest: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// Close the connection if the job is cancelled so the read loop unblocks.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	gspec := guestproto.Spec{Command: spec.Command, Env: spec.Env, WorkDir: guestWorkDir}
	if err := guestproto.WriteSpec(conn, gspec); err != nil {
		return 0, fmt.Errorf("send spec: %w", err)
	}

	var exitCode int32
	gotExit := false
	for {
		kind, payload, ferr := guestproto.ReadFrame(conn)
		if errors.Is(ferr, io.EOF) {
			break
		}
		if ferr != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			return exitCode, fmt.Errorf("read frame: %w", ferr)
		}
		switch kind {
		case guestproto.KindStdout:
			_, _ = stdout.Write(payload)
		case guestproto.KindStderr:
			_, _ = stderr.Write(payload)
		case guestproto.KindExit:
			code, derr := guestproto.DecodeExit(payload)
			if derr != nil {
				return exitCode, derr
			}
			exitCode, gotExit = code, true
		}
	}
	if !gotExit {
		return 0, errors.New("guest closed connection without reporting an exit code")
	}
	return exitCode, nil
}

// machineConfig assembles the Firecracker VM configuration: bundled kernel, the
// per-job ext4 as the root block device, a vsock device, and no NIC.
func (d *Driver) machineConfig(apiSock, vsockUDS, rootfs string) firecracker.Config {
	return firecracker.Config{
		SocketPath:      apiSock,
		KernelImagePath: d.kernelPath,
		// random.trust_cpu=on seeds the RNG from RDRAND at boot so the guest's
		// getrandom() (Go runtime init) doesn't block for seconds on crng init.
		KernelArgs: "console=ttyS0 reboot=k panic=1 pci=off random.trust_cpu=on root=/dev/vda rw init=" +
			guestagent.InitPath,
		Drives: []models.Drive{{
			DriveID:      ptr("rootfs"),
			PathOnHost:   &rootfs,
			IsRootDevice: ptr(true),
			IsReadOnly:   ptr(false),
		}},
		VsockDevices: []firecracker.VsockDevice{{
			ID:   "vsock0",
			CID:  guestproto.GuestCID,
			Path: vsockUDS,
		}},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  &d.vcpuCount,
			MemSizeMib: &d.memSizeMib,
		},
		// No NetworkInterfaces: the VM has only vsock, hence no egress route.
	}
}

// acceptCtx accepts one connection, returning early if ctx is cancelled.
func acceptCtx(ctx context.Context, ln net.Listener) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		conn, err := ln.Accept()
		ch <- result{conn, err}
	}()
	select {
	case <-ctx.Done():
		_ = ln.Close() // unblock the Accept goroutine
		return nil, ctx.Err()
	case r := <-ch:
		return r.conn, r.err
	}
}

func ptr[T any](v T) *T { return &v }

// sanitiseID keeps a job ID usable as a directory name (typeids are already
// safe, but guard against an empty or path-bearing value).
func sanitiseID(id string) string {
	if id == "" {
		return "job"
	}
	return filepath.Base(id)
}

// capWriter retains only the last max bytes - enough of the VM console to
// diagnose a boot failure without unbounded growth.
type capWriter struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = w.buf[len(w.buf)-w.max:]
	}
	return len(p), nil
}

// suffix renders the captured console for an error message, or "" if empty.
func (w *capWriter) suffix() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) == 0 {
		return ""
	}
	return "\n--- VM console (tail) ---\n" + string(w.buf)
}
