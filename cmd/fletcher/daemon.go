package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/urfave/cli/v3"
)

// serviceName is the systemd unit the facade manages.
const serviceName = "fletcher"

// daemonCmd is a thin facade over systemd so operators manage the daemon with
// `fletcher` verbs instead of learning systemctl/journalctl. systemd stays the
// supervisor (boot persistence, crash-restart, the unit sandbox); this only
// shells out to it. On a non-systemd host the subcommands explain that.
func daemonCmd() *cli.Command {
	return &cli.Command{
		Name:  "daemon",
		Usage: "manage the Fletcher service: start, stop, restart, status, logs",
		Commands: []*cli.Command{
			daemonControlCmd("start", "start the daemon now", "start"),
			daemonControlCmd("stop", "stop the daemon", "stop"),
			daemonControlCmd("restart", "restart the daemon (e.g. to apply a config change)", "restart"),
			daemonControlCmd("enable", "start the daemon now and on every boot", "enable", "--now"),
			daemonControlCmd("disable", "stop the daemon and not start it on boot", "disable", "--now"),
			daemonStatusCmd(),
			daemonLogsCmd(),
		},
	}
}

func requireSystemctl() error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("systemctl not found: this host is not running systemd; manage the `fletcher serve` process directly")
	}
	return nil
}

// daemonControlCmd builds a subcommand that runs `systemctl <args> fletcher`
// with sudo (these actions need root).
func daemonControlCmd(name, usage string, systemctlArgs ...string) *cli.Command {
	return &cli.Command{
		Name:  name,
		Usage: usage,
		Action: func(ctx context.Context, _ *cli.Command) error {
			if err := requireSystemctl(); err != nil {
				return err
			}
			args := append([]string{"systemctl"}, systemctlArgs...)
			return runManage(ctx, true, append(args, serviceName)...)
		},
	}
}

func daemonStatusCmd() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "show the daemon's service status",
		Action: func(ctx context.Context, _ *cli.Command) error {
			if err := requireSystemctl(); err != nil {
				return err
			}
			// `systemctl status` exits non-zero when the unit is inactive;
			// that is not a CLI error, so swallow it.
			_ = runManage(ctx, false, "systemctl", "status", serviceName, "--no-pager")
			return nil
		},
	}
}

func daemonLogsCmd() *cli.Command {
	return &cli.Command{
		Name:  "logs",
		Usage: "show the daemon's logs",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "follow", Aliases: []string{"f"}, Usage: "stream new log lines"},
			&cli.IntFlag{Name: "lines", Aliases: []string{"n"}, Value: 80, Usage: "recent lines to show"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if _, err := exec.LookPath("journalctl"); err != nil {
				return errors.New("journalctl not found: this host is not running systemd-journald")
			}
			args := []string{"journalctl", "-u", serviceName, "--no-pager", "-n", fmt.Sprintf("%d", cmd.Int("lines"))}
			if cmd.Bool("follow") {
				args = append(args, "-f")
			}
			return runManage(ctx, false, args...)
		},
	}
}

// runManage runs a management command inheriting the operator's stdio. When
// root is required and we are not already root, it prefixes sudo so the
// operator gets the standard password prompt.
func runManage(ctx context.Context, needsRoot bool, args ...string) error {
	name, rest := args[0], args[1:]
	if needsRoot && os.Geteuid() != 0 {
		rest = append([]string{name}, rest...)
		name = "sudo"
	}
	command := exec.CommandContext(ctx, name, rest...) //nolint:gosec // fixed systemctl/journalctl verbs + the constant unit name
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return cli.Exit("", exitErr.ExitCode())
		}
		return err
	}
	return nil
}
