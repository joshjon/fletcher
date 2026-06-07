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
		},
	}
}

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
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			client := newSessionsClient(cmd)
			resp, err := client.CreateSession(ctx, connect.NewRequest(&fletcherv1.CreateSessionRequest{
				Name:         cmd.String("name"),
				Image:        cmd.String("image"),
				EgressPolicy: cmd.String("egress"),
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

// --- output rendering ---

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
