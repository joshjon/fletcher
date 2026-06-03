package main

import (
	"context"

	"github.com/urfave/cli/v3"

	"github.com/joshjon/fletcher/internal/daemon"
)

func serveCmd() *cli.Command {
	return &cli.Command{
		Name:  "serve",
		Usage: "run the fletcher daemon",
		Flags: []cli.Flag{
			socketFlag(),
			&cli.StringFlag{
				Name:    "database",
				Usage:   "SQLite database path",
				Sources: cli.EnvVars("FLETCHER_DATABASE"),
				Value:   defaultDatabasePath(),
			},
			&cli.StringFlag{
				Name:    "log-level",
				Usage:   "log level (debug, info, warn, error)",
				Sources: cli.EnvVars("FLETCHER_LOG_LEVEL"),
				Value:   "info",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return daemon.Run(ctx, daemon.Config{
				SocketPath:   cmd.String("socket"),
				DatabasePath: cmd.String("database"),
				LogLevel:     cmd.String("log-level"),
			})
		},
	}
}
