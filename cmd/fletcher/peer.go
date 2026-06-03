package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

func peerCmd() *cli.Command {
	return &cli.Command{
		Name:  "peer",
		Usage: "manage WireGuard peers and emit wg-quick configs",
		Commands: []*cli.Command{
			peerAddCmd(),
			peerListCmd(),
			peerDeleteCmd(),
			peerServerConfigCmd(),
		},
	}
}

func peerAddCmd() *cli.Command {
	return &cli.Command{
		Name:      "add",
		Usage:     "register a new peer (returns its private key once + optional client wg-quick conf)",
		ArgsUsage: "<name>",
		Flags: []cli.Flag{
			socketFlag(),
			&cli.StringFlag{
				Name:     "address",
				Usage:    "peer's WireGuard tunnel address with CIDR (e.g. 10.99.0.2/32)",
				Required: true,
			},
			&cli.StringFlag{
				Name:  "endpoint",
				Usage: "server endpoint host:port the peer will dial (enables client config rendering)",
			},
			&cli.StringSliceFlag{
				Name:  "dns",
				Usage: "DNS servers to push to the peer",
			},
			&cli.StringSliceFlag{
				Name:  "client-allowed-ips",
				Usage: "CIDRs the peer should route through the tunnel (default 10.99.0.0/24)",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			name := cmd.Args().First()
			if name == "" {
				return errors.New("peer name is required")
			}
			req := &fletcherv1.CreatePeerRequest{
				Name:             name,
				AllowedIps:       []string{cmd.String("address")},
				ClientAddress:    cmd.String("address"),
				ClientDns:        cmd.StringSlice("dns"),
				ServerEndpoint:   cmd.String("endpoint"),
				ClientAllowedIps: cmd.StringSlice("client-allowed-ips"),
			}
			client := newPeersClient(cmd.String("socket"))
			resp, err := client.CreatePeer(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			p := resp.Msg.GetPeer()
			fmt.Printf("registered %s (id=%s, public_key=%s)\n", p.GetName(), p.GetId(), p.GetPublicKey())
			fmt.Println()
			fmt.Println("# ===== CLIENT CONFIG (private key shown exactly once) =====")
			if cfg := resp.Msg.GetClientConfig(); cfg != "" {
				fmt.Print(cfg)
			} else {
				fmt.Printf("# [Interface]\n# PrivateKey = %s\n# Address = %s\n\n# (re-run with --endpoint to render the full conf)\n",
					resp.Msg.GetPrivateKey(), cmd.String("address"))
			}
			return nil
		},
	}
}

func peerListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list registered peers",
		Flags: []cli.Flag{socketFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			client := newPeersClient(cmd.String("socket"))
			resp, err := client.ListPeers(ctx, connect.NewRequest(&fletcherv1.ListPeersRequest{Limit: 100}))
			if err != nil {
				return err
			}
			return writePeersTable(os.Stdout, resp.Msg.GetPeers())
		},
	}
}

func peerDeleteCmd() *cli.Command {
	return &cli.Command{
		Name:      "delete",
		Usage:     "remove a peer",
		ArgsUsage: "<id>",
		Flags:     []cli.Flag{socketFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			id := cmd.Args().First()
			if id == "" {
				return errors.New("peer id is required")
			}
			client := newPeersClient(cmd.String("socket"))
			resp, err := client.DeletePeer(ctx, connect.NewRequest(&fletcherv1.DeletePeerRequest{Id: id}))
			if err != nil {
				return err
			}
			if resp.Msg.GetExisted() {
				fmt.Printf("deleted %s\n", id)
			} else {
				fmt.Printf("%s did not exist\n", id)
			}
			return nil
		},
	}
}

func peerServerConfigCmd() *cli.Command {
	return &cli.Command{
		Name:  "server-config",
		Usage: "emit the daemon-side wg-quick config (contains the server private key)",
		Flags: []cli.Flag{
			socketFlag(),
			&cli.StringFlag{
				Name:  "address",
				Usage: "server tunnel address with CIDR (e.g. 10.99.0.1/24)",
				Value: "10.99.0.1/24",
			},
			&cli.IntFlag{
				Name:  "listen-port",
				Usage: "UDP port the server listens on",
				Value: 51820,
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			client := newPeersClient(cmd.String("socket"))
			resp, err := client.ServerConfig(ctx, connect.NewRequest(&fletcherv1.ServerConfigRequest{
				Address:    cmd.String("address"),
				ListenPort: clampInt32(cmd.Int("listen-port")),
			}))
			if err != nil {
				return err
			}
			fmt.Printf("# Server public key: %s\n\n", resp.Msg.GetPublicKey())
			fmt.Print(resp.Msg.GetConfig())
			return nil
		},
	}
}

func writePeersTable(w io.Writer, peers []*fletcherv1.Peer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tALLOWED_IPS\tPUBLIC_KEY\tCREATED")
	for _, p := range peers {
		fmt.Fprintf(tw, "%s\t%s\t%v\t%s\t%s\n",
			p.GetId(),
			p.GetName(),
			p.GetAllowedIps(),
			truncate(p.GetPublicKey(), 16),
			formatUnix(p.GetCreatedAt()),
		)
	}
	return tw.Flush()
}

func newPeersClient(socket string) fletcherv1connect.PeerServiceClient {
	return fletcherv1connect.NewPeerServiceClient(unixHTTPClient(socket), unixBaseURL)
}
