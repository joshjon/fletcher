// Package mockdriver is the mock runtime driver: it spawns local /bin/sh
// processes instead of real Firecracker microVMs. Per DESIGN.md §10, this
// is a production-code citizen (not a test hack) - it's what powers
// Fletcher during macOS development where /dev/kvm is unavailable.
package mockdriver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/joshjon/fletcher/internal/runtime"
)

// Driver implements runtime.Driver by exec'ing the job command under a
// configurable shell (default /bin/sh).
type Driver struct {
	Shell string
}

// New returns a Driver using /bin/sh.
func New() *Driver { return &Driver{Shell: "/bin/sh"} }

// Run executes spec.Command via "<shell> -c <command>", returning the
// process's exit code. The command runs in its own process group and
// cancellation kills the whole group, so children forked by the command die
// too instead of holding the output pipes (and the caller's Wait) open.
func (d *Driver) Run(ctx context.Context, spec runtime.Spec, stdout, stderr io.Writer) (runtime.Result, error) {
	shell := d.Shell
	if shell == "" {
		shell = "/bin/sh"
	}
	// G204: the entire purpose of this driver is to run user-supplied
	// commands inside an isolated snapshot. Production runtimes (Firecracker)
	// add the actual isolation; the mock is a dev convenience.
	cmd := exec.CommandContext(ctx, shell, "-c", spec.Command) //nolint:gosec // intentional subprocess of user command
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if spec.WorkDir != "" {
		cmd.Dir = spec.WorkDir
	}
	cmd.Env = append(cmd.Environ(), spec.Env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The default Cancel only signals the direct child; a grandchild would
	// survive and keep the pipes open. Signal the group (negative pid).
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	// A command that exits leaving a forked child attached to the pipes (e.g.
	// "sleep 60 & echo done") would otherwise block Wait until the child
	// exits; bound that wait instead of inheriting the child's lifetime.
	cmd.WaitDelay = 3 * time.Second

	err := cmd.Run()
	if err != nil {
		// Cancellation wins over exec.ExitError: when ctx is cancelled the
		// process exits via SIGKILL, which exec.Cmd surfaces as an
		// *exec.ExitError. Reporting that as "exit 137" would hide the real
		// cause.
		if ctx.Err() != nil {
			return runtime.Result{}, fmt.Errorf("job cancelled: %w", ctx.Err())
		}
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			//nolint:gosec // POSIX exit codes fit in int32
			return runtime.Result{ExitCode: int32(exitErr.ExitCode())}, nil
		}
		return runtime.Result{}, fmt.Errorf("run job: %w", err)
	}
	return runtime.Result{ExitCode: 0}, nil
}
