package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"

	"github.com/urfave/cli/v3"
)

// forkRunCmd is the in-fork entrypoint the runc driver wraps a job's command
// with. The fork has no network except loopback, so this sets up TCP->unix
// forwarders (one per --forward) that relay the agent's loopback calls to the
// daemon's gateway/MCP unix sockets bind-mounted into the fork, then runs the
// job command. The fork therefore reaches only the daemon - no egress.
//
// Hidden: it is an internal contract between the daemon and the bind-mounted
// `fletcher` binary, not an operator-facing command.
func forkRunCmd() *cli.Command {
	return &cli.Command{
		Name:      "fork-run",
		Hidden:    true,
		Usage:     "internal: run a job command with TCP->unix forwarders to the daemon",
		ArgsUsage: "-- <command> [args...]",
		Flags: []cli.Flag{
			&cli.StringSliceFlag{
				Name:  "forward",
				Usage: "<tcp-listen>=<unix-socket> to proxy, e.g. 127.0.0.1:11500=/run/.fletcher-fwd-0.sock",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			for _, f := range cmd.StringSlice("forward") {
				listen, socket, ok := strings.Cut(f, "=")
				if !ok {
					return fmt.Errorf("invalid --forward %q (want listen=socket)", f)
				}
				if err := startForwarder(listen, socket); err != nil {
					return fmt.Errorf("start forwarder %s: %w", f, err)
				}
			}

			args := cmd.Args().Slice()
			if len(args) == 0 {
				return cli.Exit("fork-run: no command given", 2)
			}
			child := exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // the daemon supplies the job command to run in the fork
			child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
			err := child.Run()
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				return cli.Exit("", exitErr.ExitCode())
			}
			return err
		},
	}
}

// startForwarder listens on listenAddr (loopback inside the fork) and proxies
// each accepted connection to the unix socket at socketPath.
func startForwarder(listenAddr, socketPath string) error {
	ln, err := net.Listen("tcp", listenAddr) //nolint:noctx // lifetime is the container process; closed on exit
	if err != nil {
		return err
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed on process exit
			}
			go proxyToUnix(conn, socketPath)
		}
	}()
	return nil
}

// proxyToUnix splices a client connection to a unix socket, closing both ends
// when either direction finishes so half-closed HTTP/SSE streams terminate.
func proxyToUnix(client net.Conn, socketPath string) {
	upstream, err := net.Dial("unix", socketPath) //nolint:noctx // short-lived proxy dial; the conn lifetime governs it
	if err != nil {
		_ = client.Close()
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
	_ = client.Close()
	_ = upstream.Close()
	<-done
}
