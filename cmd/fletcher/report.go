package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

func reportCmd() *cli.Command {
	return &cli.Command{
		Name:  "report",
		Usage: "read result reports agents posted via the report MCP tool",
		Commands: []*cli.Command{
			reportListCmd(),
			reportGetCmd(),
		},
	}
}

func newReportsClient(cmd *cli.Command) fletcherv1connect.ReportServiceClient {
	hc, base, opts := clientTarget(cmd)
	return fletcherv1connect.NewReportServiceClient(hc, base, opts...)
}

func reportListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list reports, newest first",
		Flags: []cli.Flag{
			socketFlag(),
			outputFlag(),
			&cli.IntFlag{Name: "limit", Usage: "maximum reports to return (default 50)"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			resp, err := newReportsClient(cmd).ListReports(ctx, connect.NewRequest(&fletcherv1.ListReportsRequest{
				Limit: int32(cmd.Int("limit")), //nolint:gosec // bounded by flag use
			}))
			if err != nil {
				return err
			}
			if cmd.String("output") == "json" {
				return writeProtoJSON(os.Stdout, resp.Msg)
			}
			reports := resp.Msg.GetReports()
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSTATUS\tTITLE\tFROM\tCREATED")
			for _, r := range reports {
				from := r.GetSourceName()
				if from == "" {
					from = "-"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
					r.GetId(), r.GetStatus(), r.GetTitle(), from, formatUnix(r.GetCreatedAt()))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			fmt.Printf("\ntotal: %d\n", len(reports))
			return nil
		},
	}
}

func reportGetCmd() *cli.Command {
	return &cli.Command{
		Name:      "get",
		Usage:     "show a report",
		ArgsUsage: "<id>",
		Flags:     []cli.Flag{socketFlag(), outputFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			id := cmd.Args().First()
			if id == "" {
				return errors.New("report id is required")
			}
			resp, err := newReportsClient(cmd).GetReport(ctx, connect.NewRequest(&fletcherv1.GetReportRequest{Id: id}))
			if err != nil {
				return err
			}
			r := resp.Msg.GetReport()
			if cmd.String("output") == "json" {
				return writeProtoJSON(os.Stdout, r)
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(tw, "id:\t%s\n", r.GetId())
			fmt.Fprintf(tw, "title:\t%s\n", r.GetTitle())
			fmt.Fprintf(tw, "status:\t%s\n", r.GetStatus())
			if r.GetSummary() != "" {
				fmt.Fprintf(tw, "summary:\t%s\n", r.GetSummary())
			}
			if r.GetLink() != "" {
				fmt.Fprintf(tw, "link:\t%s\n", r.GetLink())
			}
			if r.GetSourceName() != "" {
				fmt.Fprintf(tw, "from:\t%s (%s)\n", r.GetSourceName(), r.GetSourceType())
			}
			fmt.Fprintf(tw, "created_at:\t%s\n", formatUnix(r.GetCreatedAt()))
			return tw.Flush()
		},
	}
}
