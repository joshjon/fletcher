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
	"google.golang.org/protobuf/proto"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

func approvalCmd() *cli.Command {
	return &cli.Command{
		Name:  "approval",
		Usage: "review pending privileged-operation approvals",
		Commands: []*cli.Command{
			approvalListCmd(),
			approvalGetCmd(),
			approvalApproveCmd(),
			approvalDenyCmd(),
		},
	}
}

func approvalListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list approvals (newest first)",
		Flags: []cli.Flag{
			socketFlag(),
			outputFlag(),
			&cli.IntFlag{Name: "limit", Value: 50},
			&cli.IntFlag{Name: "offset", Value: 0},
			&cli.StringFlag{Name: "status", Usage: "filter by status (pending|approved|denied|expired)"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			status, err := approvalStatusFromString(cmd.String("status"))
			if err != nil {
				return err
			}
			client := newApprovalsClient(cmd.String("socket"))
			resp, err := client.ListApprovals(ctx, connect.NewRequest(&fletcherv1.ListApprovalsRequest{
				Limit:        clampInt32(cmd.Int("limit")),
				Offset:       clampInt32(cmd.Int("offset")),
				StatusFilter: status,
			}))
			if err != nil {
				return err
			}
			if cmd.String("output") == "json" {
				return writeProtoJSON(os.Stdout, resp.Msg)
			}
			return writeApprovalsTable(os.Stdout, resp.Msg.GetApprovals())
		},
	}
}

func approvalGetCmd() *cli.Command {
	return &cli.Command{
		Name:      "get",
		Usage:     "fetch a single approval by id",
		ArgsUsage: "<id>",
		Flags:     []cli.Flag{socketFlag(), outputFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			id := cmd.Args().First()
			if id == "" {
				return errors.New("approval id is required")
			}
			client := newApprovalsClient(cmd.String("socket"))
			resp, err := client.GetApproval(ctx, connect.NewRequest(&fletcherv1.GetApprovalRequest{Id: id}))
			if err != nil {
				return err
			}
			return renderApproval(os.Stdout, cmd.String("output"), resp.Msg.GetApproval())
		},
	}
}

func approvalApproveCmd() *cli.Command {
	return &cli.Command{
		Name:      "approve",
		Usage:     "approve a pending request",
		ArgsUsage: "<id>",
		Flags: []cli.Flag{
			socketFlag(),
			&cli.StringFlag{Name: "reason", Usage: "optional decision reason"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			id := cmd.Args().First()
			if id == "" {
				return errors.New("approval id is required")
			}
			client := newApprovalsClient(cmd.String("socket"))
			resp, err := client.ApproveApproval(ctx, connect.NewRequest(&fletcherv1.ApproveApprovalRequest{
				Id: id, Reason: cmd.String("reason"),
			}))
			if err != nil {
				return err
			}
			if resp.Msg.GetDecided() {
				fmt.Printf("approved %s\n", id)
			} else {
				fmt.Printf("%s was not in a pending state\n", id)
			}
			return nil
		},
	}
}

func approvalDenyCmd() *cli.Command {
	return &cli.Command{
		Name:      "deny",
		Usage:     "deny a pending request",
		ArgsUsage: "<id>",
		Flags: []cli.Flag{
			socketFlag(),
			&cli.StringFlag{Name: "reason", Usage: "optional decision reason"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			id := cmd.Args().First()
			if id == "" {
				return errors.New("approval id is required")
			}
			client := newApprovalsClient(cmd.String("socket"))
			resp, err := client.DenyApproval(ctx, connect.NewRequest(&fletcherv1.DenyApprovalRequest{
				Id: id, Reason: cmd.String("reason"),
			}))
			if err != nil {
				return err
			}
			if resp.Msg.GetDecided() {
				fmt.Printf("denied %s\n", id)
			} else {
				fmt.Printf("%s was not in a pending state\n", id)
			}
			return nil
		},
	}
}

func renderApproval(w io.Writer, format string, a *fletcherv1.Approval) error {
	if format == "json" {
		return writeProtoJSON(w, proto.Message(a))
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "id:\t%s\n", a.GetId())
	fmt.Fprintf(tw, "status:\t%s\n", approvalStatusLabel(a.GetStatus()))
	fmt.Fprintf(tw, "action:\t%s\n", a.GetAction())
	fmt.Fprintf(tw, "justification:\t%s\n", a.GetJustification())
	fmt.Fprintf(tw, "requester:\t%s\n", a.GetRequester())
	fmt.Fprintf(tw, "created_at:\t%s\n", formatUnix(a.GetCreatedAt()))
	fmt.Fprintf(tw, "expires_at:\t%s\n", formatUnix(a.GetExpiresAt()))
	if a.DecidedAt != nil {
		fmt.Fprintf(tw, "decided_at:\t%s\n", formatUnix(a.GetDecidedAt()))
	}
	if a.GetDecisionReason() != "" {
		fmt.Fprintf(tw, "decision_reason:\t%s\n", a.GetDecisionReason())
	}
	return tw.Flush()
}

func writeApprovalsTable(w io.Writer, items []*fletcherv1.Approval) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tREQUESTER\tACTION\tCREATED\tEXPIRES")
	for _, a := range items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			a.GetId(),
			approvalStatusLabel(a.GetStatus()),
			a.GetRequester(),
			truncate(a.GetAction(), 40),
			formatUnix(a.GetCreatedAt()),
			formatUnix(a.GetExpiresAt()),
		)
	}
	return tw.Flush()
}

func approvalStatusLabel(s fletcherv1.ApprovalStatus) string {
	switch s {
	case fletcherv1.ApprovalStatus_APPROVAL_STATUS_PENDING:
		return "pending"
	case fletcherv1.ApprovalStatus_APPROVAL_STATUS_APPROVED:
		return "approved"
	case fletcherv1.ApprovalStatus_APPROVAL_STATUS_DENIED:
		return "denied"
	case fletcherv1.ApprovalStatus_APPROVAL_STATUS_EXPIRED:
		return "expired"
	}
	return "unknown"
}

func approvalStatusFromString(s string) (fletcherv1.ApprovalStatus, error) {
	switch s {
	case "":
		return fletcherv1.ApprovalStatus_APPROVAL_STATUS_UNSPECIFIED, nil
	case "pending":
		return fletcherv1.ApprovalStatus_APPROVAL_STATUS_PENDING, nil
	case "approved":
		return fletcherv1.ApprovalStatus_APPROVAL_STATUS_APPROVED, nil
	case "denied":
		return fletcherv1.ApprovalStatus_APPROVAL_STATUS_DENIED, nil
	case "expired":
		return fletcherv1.ApprovalStatus_APPROVAL_STATUS_EXPIRED, nil
	}
	return fletcherv1.ApprovalStatus_APPROVAL_STATUS_UNSPECIFIED, fmt.Errorf("invalid status %q", s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func newApprovalsClient(socket string) fletcherv1connect.ApprovalServiceClient {
	return fletcherv1connect.NewApprovalServiceClient(unixHTTPClient(socket), unixBaseURL)
}
