package main

import (
	"context"

	"github.com/urfave/cli/v3"

	"github.com/joshjon/fletcher/internal/runtime/hostdriver"
)

// hostExecutorCmd is the account-scoped service entrypoint, not a remote API operation.
func hostExecutorCmd() *cli.Command {
	return &cli.Command{
		Name:   "host-executor",
		Usage:  "serve the locally authorized Linux account executor",
		Hidden: true,
		Action: func(ctx context.Context, _ *cli.Command) error {
			return hostdriver.ServeActivated(ctx, "fletcher")
		},
	}
}
