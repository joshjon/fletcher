package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"connectrpc.com/connect"
	"github.com/mdp/qrterminal/v3"
	"github.com/urfave/cli/v3"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

func peerCmd() *cli.Command {
	return &cli.Command{
		Name:  "peer",
		Usage: "manage WireGuard peers and emit wg-quick configs",
		Commands: []*cli.Command{
			peerPairCmd(),
			peerAddCmd(),
			peerListCmd(),
			peerDeleteCmd(),
			peerServerConfigCmd(),
		},
	}
}

func peerPairCmd() *cli.Command {
	return &cli.Command{
		Name:      "pair",
		Usage:     "add a new device (phone, laptop) to the tunnel with one command",
		ArgsUsage: "<name>",
		Description: `Auto-allocates a tunnel IP and renders the wg-quick config the
device needs. The daemon must have been started with
--public-endpoint (or FLETCHER_PUBLIC_ENDPOINT) set to the
host:port your peers will dial; everything else is figured out
for you.

The rendered config is printed in full; if stdout is a terminal,
a QR code is also rendered for scanning with the WireGuard mobile
app. The private key is shown exactly once - paste it into the
device immediately and don't log it elsewhere.`,
		Flags: []cli.Flag{
			socketFlag(),
			&cli.BoolFlag{
				Name:  "no-qr",
				Usage: "skip the QR-code render (useful when copying the config to a non-mobile device)",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			name := cmd.Args().First()
			if name == "" {
				return errors.New("peer name is required")
			}
			client := newPeersClient(cmd.String("socket"))
			resp, err := client.PairPeer(ctx, connect.NewRequest(&fletcherv1.PairPeerRequest{Name: name}))
			if err != nil {
				return err
			}
			renderPairResult(os.Stdout, resp.Msg, !cmd.Bool("no-qr"))
			return nil
		},
	}
}

func renderPairResult(w io.Writer, resp *fletcherv1.PairPeerResponse, withQR bool) {
	p := resp.GetPeer()
	fmt.Fprintf(w, "paired %s\n", p.GetName())
	fmt.Fprintf(w, "  id:       %s\n", p.GetId())
	fmt.Fprintf(w, "  address:  %s\n", resp.GetAddress())
	fmt.Fprintf(w, "  endpoint: %s\n", resp.GetEndpoint())
	fmt.Fprintln(w)
	fmt.Fprintln(w, "# ===== CLIENT CONFIG (private key shown exactly once) =====")
	fmt.Fprint(w, resp.GetClientConfig())
	if withQR && isTerminal(w) {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Scan with the WireGuard app (the QR encodes the config above):")
		qrterminal.GenerateWithConfig(resp.GetClientConfig(), qrterminal.Config{
			Level:     qrterminal.M,
			Writer:    w,
			BlackChar: qrterminal.BLACK,
			WhiteChar: qrterminal.WHITE,
			QuietZone: 1,
		})
	}
}

// isTerminal reports whether w is an os.File attached to a TTY. Used to
// decide whether to render the QR (which looks like noise when piped).
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
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
