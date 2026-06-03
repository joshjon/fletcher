// Command fletcher is the Fletcher daemon and client CLI.
//
// "fletcher serve" runs the daemon. Other subcommands are client operations
// that talk to a running daemon over its local Unix socket.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v3"

	"github.com/joshjon/fletcher/internal/buildinfo"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fletcher:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return newApp().Run(ctx, os.Args)
}

func newApp() *cli.Command {
	return &cli.Command{
		Name:    "fletcher",
		Usage:   "private agent compute on hardware you own",
		Version: buildinfo.Version,
		Commands: []*cli.Command{
			serveCmd(),
			healthCmd(),
			jobCmd(),
			secretCmd(),
			approvalCmd(),
			versionCmd(),
		},
	}
}

func versionCmd() *cli.Command {
	return &cli.Command{
		Name:  "version",
		Usage: "print version, commit, and build date",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "json",
				Usage: "output as JSON",
			},
		},
		Action: func(_ context.Context, cmd *cli.Command) error {
			info := buildinfo.Info()
			if cmd.Bool("json") {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(info)
			}
			fmt.Printf("fletcher %s (commit %s, built %s)\n", info.Version, info.Commit, info.Date)
			return nil
		},
	}
}
