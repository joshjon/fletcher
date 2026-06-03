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
			&cli.StringFlag{
				Name:    "gateway-listen",
				Usage:   "model gateway TCP listen address",
				Sources: cli.EnvVars("FLETCHER_GATEWAY_LISTEN"),
				Value:   "127.0.0.1:11500",
			},
			&cli.StringFlag{
				Name:    "mcp-listen",
				Usage:   "MCP server TCP listen address",
				Sources: cli.EnvVars("FLETCHER_MCP_LISTEN"),
				Value:   "127.0.0.1:11600",
			},
			&cli.StringFlag{
				Name:    "age-key",
				Usage:   "age identity file path (auto-generated if missing)",
				Sources: cli.EnvVars("FLETCHER_AGE_KEY"),
				Value:   defaultAgeIdentityPath(),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return daemon.Run(ctx, daemon.Config{
				SocketPath:        cmd.String("socket"),
				DatabasePath:      cmd.String("database"),
				LogLevel:          cmd.String("log-level"),
				GatewayListenAddr: cmd.String("gateway-listen"),
				MCPListenAddr:     cmd.String("mcp-listen"),
				AgeIdentityPath:   cmd.String("age-key"),
			})
		},
	}
}
