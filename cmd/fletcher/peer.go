package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

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
device immediately and don't log it elsewhere.

Pass --mobile when pairing a native client (the Fletcher iOS app)
that generates its own WireGuard keypair locally. The daemon
returns a single pairing blob that bundles every field the app
needs in one QR; the app keeps its private key in its secure
enclave. It calls CompletePair over the public pairing endpoint
(a TLS port whose self-signed cert it pins from the blob) to
register its public key, then brings up the tunnel. The daemon's
API token is only released after the app supplies its public key,
so a copied blob does not authorise API access.`,
		Flags: []cli.Flag{
			socketFlag(),
			&cli.BoolFlag{
				Name:  "no-qr",
				Usage: "skip the QR-code render (useful when copying the config to a non-mobile device)",
			},
			&cli.BoolFlag{
				Name:  "mobile",
				Usage: "emit a single pair blob for a native client (e.g. the Fletcher iOS app) that does its own keygen",
			},
			&cli.BoolFlag{
				Name:  "byo-vpn",
				Usage: "pair a client for Mode B: reach the box over a VPN you already run (e.g. Tailscale), no Fletcher tunnel. Requires the daemon's --remote-api-listen.",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			name := cmd.Args().First()
			if name == "" {
				return errors.New("peer name is required")
			}
			if cmd.Bool("mobile") && cmd.Bool("byo-vpn") {
				return errors.New("--mobile and --byo-vpn are mutually exclusive: --mobile sets up Fletcher's tunnel, --byo-vpn reaches the box over a VPN you already run")
			}
			client := newPeersClient(cmd)
			if cmd.Bool("mobile") {
				resp, err := client.BeginPair(ctx, connect.NewRequest(&fletcherv1.BeginPairRequest{Name: name}))
				if err != nil {
					return err
				}
				renderMobilePairResult(os.Stdout, name, resp.Msg, !cmd.Bool("no-qr"))
				return nil
			}
			resp, err := client.PairPeer(ctx, connect.NewRequest(&fletcherv1.PairPeerRequest{Name: name}))
			if err != nil {
				return err
			}
			if cmd.Bool("byo-vpn") {
				return renderByoVPNPairResult(os.Stdout, resp.Msg, !cmd.Bool("no-qr"))
			}
			renderPairResult(os.Stdout, resp.Msg, !cmd.Bool("no-qr"))
			return nil
		},
	}
}

func renderMobilePairResult(w io.Writer, name string, resp *fletcherv1.BeginPairResponse, withQR bool) {
	if resp.GetPairingEndpoint() == "" {
		fmt.Fprintf(w, "pairing slot reserved for %s, but the daemon has no public pairing listener.\n", name)
		fmt.Fprintln(w, "The iOS app cannot complete pairing without it. Restart the daemon so the")
		fmt.Fprintln(w, "pairing listener can bind (it needs the WireGuard tunnel up and a public endpoint),")
		fmt.Fprintln(w, "then run `fletcher peer pair --mobile` again.")
		return
	}
	blob := encodePairBlob(pairBlob{
		PairingCode:         resp.GetPairingCode(),
		ExpiresAt:           resp.GetExpiresAt(),
		ServerPublicKey:     resp.GetServerPublicKey(),
		Endpoint:            resp.GetEndpoint(),
		Address:             resp.GetAddress(),
		AllowedIPs:          resp.GetAllowedIps(),
		APIEndpoint:         resp.GetApiEndpoint(),
		PersistentKeepalive: resp.GetPersistentKeepalive(),
		Name:                name,
		PairingEndpoint:     resp.GetPairingEndpoint(),
		PairingFingerprint:  resp.GetPairingTlsFingerprint(),
	})
	expires := time.Unix(resp.GetExpiresAt(), 0).UTC().Format(time.RFC3339)
	fmt.Fprintf(w, "pairing slot reserved for %s\n", name)
	fmt.Fprintf(w, "  address:  %s\n", resp.GetAddress())
	fmt.Fprintf(w, "  endpoint: %s\n", resp.GetEndpoint())
	fmt.Fprintf(w, "  pairing:  %s\n", resp.GetPairingEndpoint())
	fmt.Fprintf(w, "  expires:  %s (slot released if the app does not complete pairing in time)\n", expires)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "# ===== PAIR BLOB (paste into the Fletcher iOS app, or scan the QR) =====")
	fmt.Fprintln(w, blob)
	if withQR && isTerminal(w) {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Scan with the Fletcher iOS app:")
		renderScanQR(w, blob)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "# The API token is released to the app only after it supplies its WireGuard public key.")
}

// renderByoVPNPairResult renders a Mode B pairing: a {remote, token} login blob
// (and QR) the app reaches over a VPN the operator already runs, with no
// Fletcher tunnel. Errors when the daemon has no --remote-api-listen set, since
// there is then no VPN address to advertise.
func renderByoVPNPairResult(w io.Writer, resp *fletcherv1.PairPeerResponse, withQR bool) error {
	remote := resp.GetRemoteApiEndpoint()
	if remote == "" {
		return errors.New("the daemon has no Mode B address configured: run " +
			"`fletcher settings set remote_api_listen <box-vpn-ip>:11700` (e.g. your Tailscale IP), " +
			"restart with `fletcher daemon restart`, then pair again")
	}
	p := resp.GetPeer()
	blob := encodeLoginBlob(remote, resp.GetApiToken())
	fmt.Fprintf(w, "paired %s for Mode B (reach the box over your own VPN)\n", p.GetName())
	fmt.Fprintf(w, "  id:     %s\n", p.GetId())
	fmt.Fprintf(w, "  remote: %s\n", remote)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "# ===== LOGIN BLOB (paste into the Fletcher iOS app, or scan the QR) =====")
	fmt.Fprintln(w, "# The app dials this address over your active VPN (e.g. Tailscale) - no Fletcher tunnel.")
	fmt.Fprintln(w, blob)
	if withQR && isTerminal(w) {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Scan with the Fletcher iOS app:")
		renderScanQR(w, blob)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "# Or log in from a laptop on the same VPN:")
	fmt.Fprintf(w, "#   fletcher login %s\n", blob)
	return nil
}

// renderScanQR writes a half-block QR of data to w. Half-blocks pack two
// vertical modules per character row, so the code is about a quarter of the
// full-block size - small enough not to swamp the terminal while staying
// scannable.
func renderScanQR(w io.Writer, data string) {
	qrterminal.GenerateWithConfig(data, qrterminal.Config{
		Level:          qrterminal.M,
		Writer:         w,
		HalfBlocks:     true,
		BlackChar:      qrterminal.BLACK_BLACK,
		WhiteBlackChar: qrterminal.WHITE_BLACK,
		WhiteChar:      qrterminal.WHITE_WHITE,
		BlackWhiteChar: qrterminal.BLACK_WHITE,
		QuietZone:      1,
	})
}

func renderPairResult(w io.Writer, resp *fletcherv1.PairPeerResponse, withQR bool) {
	p := resp.GetPeer()
	fmt.Fprintf(w, "paired %s\n", p.GetName())
	fmt.Fprintf(w, "  id:       %s\n", p.GetId())
	fmt.Fprintf(w, "  address:  %s\n", resp.GetAddress())
	fmt.Fprintf(w, "  endpoint: %s\n", resp.GetEndpoint())
	fmt.Fprintln(w)

	// Step 1 first: `fletcher login` (step 2) reaches the daemon over the
	// tunnel, so the tunnel has to be up before it will work.
	fmt.Fprintln(w, "# ===== STEP 1: WIREGUARD TUNNEL (private key shown exactly once) =====")
	fmt.Fprint(w, resp.GetClientConfig())
	if withQR && isTerminal(w) {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Scan with the WireGuard app (the QR encodes the config above):")
		renderScanQR(w, resp.GetClientConfig())
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "# ===== STEP 2: API TOKEN (shown exactly once) =====")
	fmt.Fprintln(w, "# Once the tunnel above is connected, save the credential and just run `fletcher`:")
	fmt.Fprintf(w, "#   fletcher login %s\n", encodeLoginBlob(resp.GetApiEndpoint(), resp.GetApiToken()))
	fmt.Fprintln(w, "# Or pass it per command:")
	fmt.Fprintf(w, "#   fletcher --remote %s --token %s <command>\n", resp.GetApiEndpoint(), resp.GetApiToken())
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
			client := newPeersClient(cmd)
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
			client := newPeersClient(cmd)
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
			client := newPeersClient(cmd)
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
			client := newPeersClient(cmd)
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

func newPeersClient(cmd *cli.Command) fletcherv1connect.PeerServiceClient {
	hc, base, opts := clientTarget(cmd)
	return fletcherv1connect.NewPeerServiceClient(hc, base, opts...)
}
