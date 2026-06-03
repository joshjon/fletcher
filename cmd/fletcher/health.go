package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

func healthCmd() *cli.Command {
	return &cli.Command{
		Name:  "health",
		Usage: "check the running daemon's health",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "socket",
				Usage:   "Unix socket path of the daemon",
				Sources: cli.EnvVars("FLETCHER_SOCKET"),
				Value:   defaultSocketPath(),
			},
			&cli.BoolFlag{
				Name:  "json",
				Usage: "output as JSON",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			client := newAdminClient(cmd.String("socket"))
			resp, err := client.Health(ctx, connect.NewRequest(&fletcherv1.HealthRequest{}))
			if err != nil {
				return fmt.Errorf("call health: %w", err)
			}
			if cmd.Bool("json") {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{
					"status":     resp.Msg.GetStatus(),
					"version":    resp.Msg.GetVersion(),
					"commit":     resp.Msg.GetCommit(),
					"started_at": resp.Msg.GetStartedAt(),
				})
			}
			fmt.Printf("status:     %s\n", resp.Msg.GetStatus())
			fmt.Printf("version:    %s\n", resp.Msg.GetVersion())
			fmt.Printf("commit:     %s\n", resp.Msg.GetCommit())
			fmt.Printf("started_at: %d\n", resp.Msg.GetStartedAt())
			return nil
		},
	}
}

// newAdminClient builds a Connect client that dials the daemon over a Unix
// socket. The base URL is meaningless on Unix transport — Connect just needs
// a syntactically valid URL — so we use http://unix.
func newAdminClient(socket string) fletcherv1connect.AdminServiceClient {
	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}
	return fletcherv1connect.NewAdminServiceClient(httpClient, "http://unix")
}
