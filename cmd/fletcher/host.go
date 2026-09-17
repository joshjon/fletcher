package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"strings"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

func newHostClient(cmd *cli.Command) fletcherv1connect.HostServiceClient {
	hc, base, opts := clientTarget(cmd)
	return fletcherv1connect.NewHostServiceClient(hc, base, opts...)
}

func hostDiagnosticsCmd() *cli.Command {
	return &cli.Command{Name: "diagnose", Usage: "run read-only doctor checks on the daemon's host", Flags: []cli.Flag{socketFlag(), outputFlag(), &cli.BoolFlag{Name: "external", Usage: "also contact the public-IP service and model-provider endpoints"}}, Action: func(ctx context.Context, cmd *cli.Command) error {
		r, err := newHostClient(cmd).RunDiagnostics(ctx, connect.NewRequest(&fletcherv1.RunDiagnosticsRequest{IncludeExternalChecks: cmd.Bool("external")}))
		if err != nil {
			return err
		}
		return renderManagement(cmd.String("output"), r.Msg)
	}}
}

func hostCmd() *cli.Command {
	return &cli.Command{Name: "host", Usage: "inspect and maintain the daemon through its management API", Commands: []*cli.Command{
		hostDiagnosticsCmd(),
		{Name: "get", Usage: "show host readiness and restart support", Flags: []cli.Flag{socketFlag(), outputFlag()}, Action: func(ctx context.Context, cmd *cli.Command) error {
			r, err := newHostClient(cmd).GetHost(ctx, connect.NewRequest(&fletcherv1.GetHostRequest{}))
			if err != nil {
				return err
			}
			return renderManagement(cmd.String("output"), r.Msg)
		}},
		{Name: "logs", Usage: "show this daemon process's bounded log tail", Flags: []cli.Flag{socketFlag(), &cli.IntFlag{Name: "lines", Value: 100}}, Action: func(ctx context.Context, cmd *cli.Command) error {
			n := cmd.Int("lines")
			if n < 1 || n > 1000 {
				return errors.New("lines must be 1-1000")
			}
			r, err := newHostClient(cmd).GetDaemonLogs(ctx, connect.NewRequest(&fletcherv1.GetDaemonLogsRequest{Lines: int32(n)}))
			if err != nil {
				return err
			}
			fmt.Println(r.Msg.GetText())
			return nil
		}},
		{Name: "restart", Usage: "request a graceful, supervised daemon restart", Flags: []cli.Flag{socketFlag(), yesFlag()}, Action: func(ctx context.Context, cmd *cli.Command) error {
			c := newHostClient(cmd)
			r, err := c.GetHost(ctx, connect.NewRequest(&fletcherv1.GetHostRequest{}))
			if err != nil {
				return err
			}
			if err := confirmManagement(cmd, "Restart the daemon? Connections will drop and workloads may be interrupted."); err != nil {
				return err
			}
			_, err = c.RestartDaemon(ctx, connect.NewRequest(&fletcherv1.RestartDaemonRequest{RequestId: rand.Text(), ExpectedStartedAt: r.Msg.GetStartedAt()}))
			if err == nil {
				fmt.Println("restart accepted; reconnect in a few seconds")
			}
			return err
		}},
		{Name: "storage", Usage: "show capacity and non-exclusive allocated storage by category", Flags: []cli.Flag{socketFlag(), outputFlag()}, Action: func(ctx context.Context, cmd *cli.Command) error {
			r, err := newHostClient(cmd).GetStorage(ctx, connect.NewRequest(&fletcherv1.GetStorageRequest{}))
			if err != nil {
				return err
			}
			return renderManagement(cmd.String("output"), r.Msg)
		}},
		{Name: "clear-cache", Usage: "delete the idle, regenerable build cache", Flags: []cli.Flag{socketFlag(), yesFlag()}, Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := confirmManagement(cmd, "Clear the build cache? The next build will be cold."); err != nil {
				return err
			}
			_, err := newHostClient(cmd).ClearBuildCache(ctx, connect.NewRequest(&fletcherv1.ClearBuildCacheRequest{}))
			return err
		}},
	}}
}

func yesFlag() cli.Flag {
	return &cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "skip destructive-action confirmation"}
}

func confirmManagement(cmd *cli.Command, message string) error {
	if cmd.Bool("yes") {
		return nil
	}
	fmt.Fprintf(os.Stderr, "%s [y/N] ", message)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil || strings.ToLower(strings.TrimSpace(line)) != "y" {
		return errors.New("cancelled (use --yes for automation)")
	}
	return nil
}
