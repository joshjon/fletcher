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
				Name:    "proxy-listen",
				Usage:   "in-fork loopback address agents point HTTP_PROXY at (relayed to the daemon egress proxy)",
				Sources: cli.EnvVars("FLETCHER_PROXY_LISTEN"),
				Value:   "127.0.0.1:11700",
			},
			&cli.StringFlag{
				Name:    "age-key",
				Usage:   "age identity file path (auto-generated if missing)",
				Sources: cli.EnvVars("FLETCHER_AGE_KEY"),
				Value:   defaultAgeIdentityPath(),
			},
			&cli.StringFlag{
				Name:    "runtime",
				Usage:   "runtime driver: mock, runc, firecracker (default: auto-select firecracker on a KVM host, else mock)",
				Sources: cli.EnvVars("FLETCHER_RUNTIME"),
				// Empty means "not chosen": the daemon auto-selects by capability.
				Value: "",
			},
			&cli.StringFlag{
				Name:    "snapshot",
				Usage:   "snapshot driver: mock, btrfs, ext4 (default: follows the selected runtime)",
				Sources: cli.EnvVars("FLETCHER_SNAPSHOT"),
				Value:   "",
			},
			&cli.StringFlag{
				Name:    "btrfs-root",
				Usage:   "root directory for btrfs snapshots (must be on a btrfs FS)",
				Sources: cli.EnvVars("FLETCHER_BTRFS_ROOT"),
			},
			&cli.StringFlag{
				Name:    "runc-binary",
				Usage:   "path to the runc executable (defaults to $PATH lookup)",
				Sources: cli.EnvVars("FLETCHER_RUNC_BINARY"),
			},
			&cli.StringFlag{
				Name:    "credentials-dir",
				Usage:   "host directory holding agent credential dirs (.claude, .codex, .pi, .gemini) for trusted-credential mode",
				Sources: cli.EnvVars("FLETCHER_CREDENTIALS_DIR"),
				Value:   defaultCredentialsDir(),
			},
			&cli.StringFlag{
				Name:    "public-endpoint",
				Usage:   "host:port peers dial to reach this daemon from outside the LAN (auto-detected via UPnP when unset; required if UPnP is unavailable)",
				Sources: cli.EnvVars("FLETCHER_PUBLIC_ENDPOINT"),
			},
			&cli.IntFlag{
				Name:    "wireguard-port",
				Usage:   "UDP port the daemon's WireGuard tunnel listens on (and asks UPnP to forward)",
				Sources: cli.EnvVars("FLETCHER_WIREGUARD_PORT"),
				Value:   51820,
			},
			&cli.IntFlag{
				Name:    "pairing-port",
				Usage:   "public TCP port the iOS app dials to complete pairing over TLS (and asks UPnP to forward)",
				Sources: cli.EnvVars("FLETCHER_PAIRING_PORT"),
				Value:   51821,
			},
			&cli.BoolFlag{
				Name:    "no-upnp",
				Usage:   "skip the automatic router-port-forward attempt at startup",
				Sources: cli.EnvVars("FLETCHER_NO_UPNP"),
			},
			&cli.StringFlag{
				Name:    "remote-api-listen",
				Usage:   "extra host:port to expose the token-gated API on, beyond the WireGuard tunnel (e.g. a Tailscale IP for the iOS app to reach over your own VPN)",
				Sources: cli.EnvVars("FLETCHER_REMOTE_API_LISTEN"),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return daemon.Run(ctx, daemon.Config{
				SocketPath:          cmd.String("socket"),
				DatabasePath:        cmd.String("database"),
				LogLevel:            cmd.String("log-level"),
				GatewayListenAddr:   cmd.String("gateway-listen"),
				MCPListenAddr:       cmd.String("mcp-listen"),
				ProxyListenAddr:     cmd.String("proxy-listen"),
				AgeIdentityPath:     cmd.String("age-key"),
				RuntimeKind:         cmd.String("runtime"),
				SnapshotKind:        cmd.String("snapshot"),
				BtrfsRoot:           cmd.String("btrfs-root"),
				RuncBinary:          cmd.String("runc-binary"),
				CredentialsDir:      cmd.String("credentials-dir"),
				PublicEndpoint:      cmd.String("public-endpoint"),
				WireGuardListenPort: cmd.Int("wireguard-port"),
				PairingPort:         cmd.Int("pairing-port"),
				DisableUPnP:         cmd.Bool("no-upnp"),
				RemoteAPIListen:     cmd.String("remote-api-listen"),
			})
		},
	}
}
