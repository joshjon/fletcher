package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
)

func sessionCmd() *cli.Command {
	return &cli.Command{
		Name:  "session",
		Usage: "create, inspect, and manage durable sessions (persistent microVMs)",
		Commands: []*cli.Command{
			sessionCreateCmd(),
			sessionGetCmd(),
			sessionListCmd(),
			sessionStartCmd(),
			sessionStopCmd(),
			sessionDeleteCmd(),
			sessionExecCmd(),
			sessionShellCmd(),
			sessionSSHCmd(),
			sessionSSHProxyCmd(),
			sessionPublishCmd(),
			sessionUnpublishCmd(),
			sessionPortsCmd(),
			sessionLogsCmd(),
		},
	}
}

func sessionLogsCmd() *cli.Command {
	return &cli.Command{
		Name:      "logs",
		Usage:     "show the app log of a session created with --app (or via `deploy`)",
		ArgsUsage: "<ref>",
		Flags:     []cli.Flag{socketFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ref := cmd.Args().First()
			if ref == "" {
				return errors.New("session ref (id or name) is required")
			}
			client := newSessionsClient(cmd)
			resp, err := client.ExecSession(ctx, connect.NewRequest(&fletcherv1.ExecSessionRequest{
				Ref:     ref,
				Command: "cat " + appLogPathInVM + " 2>/dev/null || echo '(no app log - is this an --app session?)'",
			}))
			if err != nil {
				return err
			}
			fmt.Fprint(os.Stdout, resp.Msg.GetStdout())
			fmt.Fprint(os.Stderr, resp.Msg.GetStderr())
			return nil
		},
	}
}

// appLogPathInVM is where fletcher-init writes a session app's output; mirrors
// the guest's appLogPath.
const appLogPathInVM = "/var/log/fletcher-app.log"

func sessionCreateCmd() *cli.Command {
	return &cli.Command{
		Name:  "create",
		Usage: "create a session and boot its VM",
		Flags: []cli.Flag{
			socketFlag(),
			outputFlag(),
			&cli.StringFlag{Name: "name", Usage: "unique session name", Required: true},
			&cli.StringFlag{Name: "image", Usage: "image / environment spec (default: the daemon's default_image setting)"},
			&cli.StringFlag{Name: "egress", Usage: "fork network egress: none | allowlist | open (default: the daemon's default_egress_policy setting)"},
			&cli.StringFlag{Name: "gateway", Usage: "model gateway: on (inject ANTHROPIC_/OPENAI_ env) | off (use the agent's own auth, e.g. a subscription login) (default: the daemon's default_gateway setting)"},
			&cli.BoolFlag{Name: "app", Usage: "run the image's own app (its entrypoint) on boot, instead of a bare environment"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			client := newSessionsClient(cmd)
			resp, err := client.CreateSession(ctx, connect.NewRequest(&fletcherv1.CreateSessionRequest{
				Name:         cmd.String("name"),
				Image:        cmd.String("image"),
				EgressPolicy: cmd.String("egress"),
				Gateway:      cmd.String("gateway"),
				RunApp:       cmd.Bool("app"),
			}))
			if err != nil {
				return err
			}
			return renderSession(os.Stdout, cmd.String("output"), resp.Msg.GetSession())
		},
	}
}

func sessionGetCmd() *cli.Command {
	return &cli.Command{
		Name:      "get",
		Usage:     "fetch a session by id or name",
		ArgsUsage: "<ref>",
		Flags:     []cli.Flag{socketFlag(), outputFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ref := cmd.Args().First()
			if ref == "" {
				return errors.New("session ref (id or name) is required")
			}
			client := newSessionsClient(cmd)
			resp, err := client.GetSession(ctx, connect.NewRequest(&fletcherv1.GetSessionRequest{Ref: ref}))
			if err != nil {
				return err
			}
			return renderSession(os.Stdout, cmd.String("output"), resp.Msg.GetSession())
		},
	}
}

func sessionListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list sessions (newest first)",
		Flags: []cli.Flag{socketFlag(), outputFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			client := newSessionsClient(cmd)
			resp, err := client.ListSessions(ctx, connect.NewRequest(&fletcherv1.ListSessionsRequest{}))
			if err != nil {
				return err
			}
			return renderSessionList(os.Stdout, cmd.String("output"), resp.Msg)
		},
	}
}

func sessionStartCmd() *cli.Command {
	return &cli.Command{
		Name:      "start",
		Usage:     "boot a stopped session's VM against its persisted disk",
		ArgsUsage: "<ref>",
		Flags:     []cli.Flag{socketFlag(), outputFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ref := cmd.Args().First()
			if ref == "" {
				return errors.New("session ref (id or name) is required")
			}
			client := newSessionsClient(cmd)
			resp, err := client.StartSession(ctx, connect.NewRequest(&fletcherv1.StartSessionRequest{Ref: ref}))
			if err != nil {
				return err
			}
			return renderSession(os.Stdout, cmd.String("output"), resp.Msg.GetSession())
		},
	}
}

func sessionStopCmd() *cli.Command {
	return &cli.Command{
		Name:      "stop",
		Usage:     "stop a running session's VM, keeping its disk",
		ArgsUsage: "<ref>",
		Flags:     []cli.Flag{socketFlag(), outputFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ref := cmd.Args().First()
			if ref == "" {
				return errors.New("session ref (id or name) is required")
			}
			client := newSessionsClient(cmd)
			resp, err := client.StopSession(ctx, connect.NewRequest(&fletcherv1.StopSessionRequest{Ref: ref}))
			if err != nil {
				return err
			}
			return renderSession(os.Stdout, cmd.String("output"), resp.Msg.GetSession())
		},
	}
}

func sessionDeleteCmd() *cli.Command {
	return &cli.Command{
		Name:      "delete",
		Usage:     "stop a session and destroy its disk",
		ArgsUsage: "<ref>",
		Flags:     []cli.Flag{socketFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ref := cmd.Args().First()
			if ref == "" {
				return errors.New("session ref (id or name) is required")
			}
			client := newSessionsClient(cmd)
			resp, err := client.DeleteSession(ctx, connect.NewRequest(&fletcherv1.DeleteSessionRequest{Ref: ref}))
			if err != nil {
				return err
			}
			if resp.Msg.GetDeleted() {
				fmt.Printf("deleted %s\n", ref)
				// Clear the stale SSH host-key pin so a future session reusing
				// this ref connects cleanly. Cleanup only - never fail a delete.
				if err := forgetSessionHostKey(ctx, ref); err != nil {
					fmt.Fprintf(os.Stderr, "warning: could not evict ssh host key for %s: %v\n", ref, err)
				}
			} else {
				fmt.Printf("%s did not exist\n", ref)
			}
			return nil
		},
	}
}

func sessionExecCmd() *cli.Command {
	return &cli.Command{
		Name:      "exec",
		Usage:     "run a command inside a running session",
		ArgsUsage: "<ref> <command>",
		Flags:     []cli.Flag{socketFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ref := cmd.Args().First()
			command := cmd.Args().Get(1)
			if ref == "" || command == "" {
				return errors.New("usage: fletcher session exec <ref> <command>")
			}
			client := newSessionsClient(cmd)
			resp, err := client.ExecSession(ctx, connect.NewRequest(&fletcherv1.ExecSessionRequest{
				Ref:     ref,
				Command: command,
			}))
			if err != nil {
				return err
			}
			fmt.Fprint(os.Stdout, resp.Msg.GetStdout())
			fmt.Fprint(os.Stderr, resp.Msg.GetStderr())
			// Mirror the command's exit code so scripts can branch on it.
			if code := resp.Msg.GetExitCode(); code != 0 {
				return cli.Exit("", int(code))
			}
			return nil
		},
	}
}

func sessionPublishCmd() *cli.Command {
	return &cli.Command{
		Name:      "publish",
		Usage:     "expose a port the session serves over the tunnel, or publicly with --public",
		ArgsUsage: "<ref> <guest-port>",
		Flags: []cli.Flag{
			socketFlag(),
			outputFlag(),
			&cli.StringFlag{Name: "name", Usage: "label for the published port (default: port-<guest-port>)"},
			&cli.BoolFlag{Name: "public", Usage: "also serve on the public internet over HTTPS (requires `fletcher settings set public_web true`)"},
			&cli.StringFlag{Name: "host", Usage: "public hostname to route to this port, e.g. app.example.com (required with --public)"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ref := cmd.Args().First()
			portArg := cmd.Args().Get(1)
			if ref == "" || portArg == "" {
				return errors.New("usage: fletcher session publish <ref> <guest-port>")
			}
			guestPort, err := strconv.Atoi(portArg)
			if err != nil || guestPort < 1 || guestPort > 65535 {
				return fmt.Errorf("guest port must be a number between 1 and 65535, got %q", portArg)
			}
			client := newSessionsClient(cmd)
			resp, err := client.PublishPort(ctx, connect.NewRequest(&fletcherv1.PublishPortRequest{
				Ref:       ref,
				GuestPort: uint32(guestPort),
				Name:      cmd.String("name"),
				Public:    cmd.Bool("public"),
				Host:      cmd.String("host"),
			}))
			if err != nil {
				return err
			}
			return renderPublishedPort(ctx, os.Stdout, cmd.String("output"), resp.Msg.GetPort(), tunnelHostHint(cmd), resp.Msg.GetPublicIp())
		},
	}
}

func sessionUnpublishCmd() *cli.Command {
	return &cli.Command{
		Name:      "unpublish",
		Usage:     "stop forwarding a session's published port",
		ArgsUsage: "<ref> <guest-port>",
		Flags:     []cli.Flag{socketFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ref := cmd.Args().First()
			portArg := cmd.Args().Get(1)
			if ref == "" || portArg == "" {
				return errors.New("usage: fletcher session unpublish <ref> <guest-port>")
			}
			guestPort, err := strconv.Atoi(portArg)
			if err != nil || guestPort < 1 || guestPort > 65535 {
				return fmt.Errorf("guest port must be a number between 1 and 65535, got %q", portArg)
			}
			client := newSessionsClient(cmd)
			if _, err := client.UnpublishPort(ctx, connect.NewRequest(&fletcherv1.UnpublishPortRequest{
				Ref:       ref,
				GuestPort: uint32(guestPort),
			})); err != nil {
				return err
			}
			fmt.Printf("unpublished port %d for %s\n", guestPort, ref)
			return nil
		},
	}
}

func sessionPortsCmd() *cli.Command {
	return &cli.Command{
		Name:      "ports",
		Usage:     "list a session's published ports",
		ArgsUsage: "<ref>",
		Flags:     []cli.Flag{socketFlag(), outputFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ref := cmd.Args().First()
			if ref == "" {
				return errors.New("session ref (id or name) is required")
			}
			client := newSessionsClient(cmd)
			resp, err := client.ListPorts(ctx, connect.NewRequest(&fletcherv1.ListPortsRequest{Ref: ref}))
			if err != nil {
				return err
			}
			if cmd.String("output") == "json" {
				return writeProtoJSON(os.Stdout, resp.Msg)
			}
			return writePublishedPortsTable(ctx, os.Stdout, resp.Msg.GetPorts(), tunnelHostHint(cmd), resp.Msg.GetPublicIp())
		},
	}
}

// tunnelHostHint is the host a paired client uses to reach a published port: the
// remote daemon's host when targeting one, else empty (the local box reaches it
// at the daemon's own tunnel IP).
func tunnelHostHint(cmd *cli.Command) string {
	remote, _ := resolveRemote(cmd)
	if remote == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(remote); err == nil {
		return host
	}
	return remote
}

// --- output rendering ---

func renderPublishedPort(ctx context.Context, w io.Writer, format string, p *fletcherv1.PublishedPort, tunnelHost, publicIP string) error {
	if format == "json" {
		return writeProtoJSON(w, p)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "id:\t%s\n", p.GetId())
	fmt.Fprintf(tw, "name:\t%s\n", p.GetName())
	fmt.Fprintf(tw, "guest_port:\t%d\n", p.GetGuestPort())
	fmt.Fprintf(tw, "tunnel_port:\t%d\n", p.GetTunnelPort())
	if p.GetPublic() {
		fmt.Fprintf(tw, "public:\t%s\n", green("https://"+p.GetHost()))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w, "\n"+publishedReach(p, tunnelHost))
	if p.GetPublic() {
		writePublicDNSGuidance(ctx, w, p.GetHost(), publicIP)
	}
	return nil
}

// writePublicDNSGuidance tells the operator the exact DNS record to create for a
// public port and immediately checks whether it already points here - so the
// set-and-confirm loop stays inside Fletcher.
func writePublicDNSGuidance(ctx context.Context, w io.Writer, host, publicIP string) {
	if publicIP == "" {
		fmt.Fprintf(w, "\n%s Fletcher could not determine your public IP (no public endpoint).\n  Set one with `fletcher settings set public_endpoint <ip:port>` or check `fletcher doctor`.\n", yellow("!"))
		return
	}
	status, ok := dnsStatus(ctx, host, publicIP)
	fmt.Fprintf(w, "\nTo serve %s publicly, create this DNS record at your provider:\n", bold(host))
	fmt.Fprintf(w, "    %s.\tA\t%s\n", host, publicIP)
	if ok {
		fmt.Fprintf(w, "%s the record already resolves here; https://%s is live (the TLS cert issues on the first request).\n", green("✓"), host)
		return
	}
	fmt.Fprintf(w, "%s %s\n  Re-check after adding it with `fletcher session ports <session>`.\n", yellow("…"), status)
}

// dnsStatus resolves host and reports whether it points at wantIP. The bool is
// true only on a confirmed match.
func dnsStatus(ctx context.Context, host, wantIP string) (string, bool) {
	if wantIP == "" {
		return gray("public IP unknown"), false
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(lookupCtx, host)
	if err != nil {
		return "not resolving yet - add the record above", false
	}
	for _, a := range addrs {
		if a == wantIP {
			return "DNS ✓", true
		}
	}
	return fmt.Sprintf("points to %s, not %s", strings.Join(addrs, ", "), wantIP), false
}

// publishedReach is the human hint for where to reach a published port over the
// tunnel.
func publishedReach(p *fletcherv1.PublishedPort, tunnelHost string) string {
	if p.GetTunnelPort() == 0 {
		return gray("not reachable yet: the WireGuard tunnel is not up (no public endpoint)")
	}
	if tunnelHost != "" {
		return fmt.Sprintf("reachable from a paired client at %s:%d", tunnelHost, p.GetTunnelPort())
	}
	return fmt.Sprintf("reachable from a paired client at the daemon's tunnel IP, port %d", p.GetTunnelPort())
}

func writePublishedPortsTable(ctx context.Context, w io.Writer, ports []*fletcherv1.PublishedPort, tunnelHost, publicIP string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tGUEST PORT\tTUNNEL ADDRESS\tPUBLIC URL\tDNS")
	for _, p := range ports {
		addr := fmt.Sprintf("%d", p.GetTunnelPort())
		if p.GetTunnelPort() == 0 {
			addr = "-"
		} else if tunnelHost != "" {
			addr = fmt.Sprintf("%s:%d", tunnelHost, p.GetTunnelPort())
		}
		public, dns := "-", "-"
		if p.GetPublic() {
			public = "https://" + p.GetHost()
			msg, ok := dnsStatus(ctx, p.GetHost(), publicIP)
			if ok {
				dns = green("ok")
			} else {
				dns = yellow(msg)
			}
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", p.GetName(), p.GetGuestPort(), addr, public, dns)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(w, "\ntotal: %d\n", len(ports))
	return nil
}

// --- session output rendering ---

func renderSession(w io.Writer, format string, s *fletcherv1.Session) error {
	if format == "json" {
		return writeProtoJSON(w, s)
	}
	return writeSessionDetails(w, s)
}

func renderSessionList(w io.Writer, format string, resp *fletcherv1.ListSessionsResponse) error {
	if format == "json" {
		return writeProtoJSON(w, resp)
	}
	return writeSessionsTable(w, resp.GetSessions())
}

func writeSessionDetails(w io.Writer, s *fletcherv1.Session) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "id:\t%s\n", s.GetId())
	fmt.Fprintf(tw, "name:\t%s\n", s.GetName())
	fmt.Fprintf(tw, "state:\t%s\n", coloredSessionState(s.GetState()))
	fmt.Fprintf(tw, "image:\t%s\n", s.GetImage())
	fmt.Fprintf(tw, "egress:\t%s\n", s.GetEgressPolicy())
	fmt.Fprintf(tw, "gateway:\t%s\n", s.GetGateway())
	if s.GetRunApp() {
		fmt.Fprintf(tw, "app:\t%s\n", green("runs the image's entrypoint on boot"))
	}
	fmt.Fprintf(tw, "disk:\t%s\n", humanBytes(s.GetDiskBytes()))
	fmt.Fprintf(tw, "created_at:\t%s\n", formatUnix(s.GetCreatedAt()))
	fmt.Fprintf(tw, "updated_at:\t%s\n", formatUnix(s.GetUpdatedAt()))
	if s.LastUsedAt != nil {
		fmt.Fprintf(tw, "last_used_at:\t%s\n", formatUnix(s.GetLastUsedAt()))
	}
	return tw.Flush()
}

// humanBytes renders a byte count as a compact human-readable size.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func writeSessionsTable(w io.Writer, sessions []*fletcherv1.Session) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATE\tIMAGE\tDISK\tLAST USED")
	for _, s := range sessions {
		lastUsed := "-"
		if s.LastUsedAt != nil {
			lastUsed = formatUnix(s.GetLastUsedAt())
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			s.GetId(),
			s.GetName(),
			coloredSessionState(s.GetState()),
			s.GetImage(),
			humanBytes(s.GetDiskBytes()),
			lastUsed,
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(w, "\ntotal: %d\n", len(sessions))
	return nil
}

func sessionStateLabel(s fletcherv1.SessionState) string {
	switch s {
	case fletcherv1.SessionState_SESSION_STATE_RUNNING:
		return "running"
	case fletcherv1.SessionState_SESSION_STATE_STOPPED:
		return "stopped"
	}
	return "unknown"
}

// coloredSessionState is sessionStateLabel plus an ANSI colour by state.
// Used in human output only; the JSON path goes through the proto enum.
func coloredSessionState(s fletcherv1.SessionState) string {
	label := sessionStateLabel(s)
	switch s {
	case fletcherv1.SessionState_SESSION_STATE_RUNNING:
		return green(label)
	case fletcherv1.SessionState_SESSION_STATE_STOPPED:
		return gray(label)
	}
	return label
}
