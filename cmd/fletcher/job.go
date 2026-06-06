package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
)

func jobCmd() *cli.Command {
	return &cli.Command{
		Name:  "job",
		Usage: "create, inspect, and manage jobs",
		Commands: []*cli.Command{
			jobCreateCmd(),
			jobGetCmd(),
			jobListCmd(),
			jobCancelCmd(),
		},
	}
}

func jobCreateCmd() *cli.Command {
	return &cli.Command{
		Name:  "create",
		Usage: "enqueue a new job",
		Flags: []cli.Flag{
			socketFlag(),
			outputFlag(),
			&cli.StringFlag{Name: "name", Usage: "human-readable job name (default: the command's program name)"},
			&cli.StringFlag{Name: "command", Usage: "command to run inside the job environment", Required: true},
			&cli.StringFlag{Name: "image", Value: "fletcher-base", Usage: "image / environment spec"},
			&cli.StringFlag{
				Name:  "trigger",
				Value: "ephemeral",
				Usage: "trigger kind (ephemeral, cron, long_running)",
			},
			&cli.StringFlag{
				Name:  "schedule",
				Usage: "cron expression for --trigger cron (e.g. '*/5 * * * *' or '@hourly')",
			},
			&cli.StringSliceFlag{
				Name:  "credential",
				Usage: "bind-mount a named credential dir into the fork (repeatable; allowed: claude, codex, gemini)",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			trigger, err := triggerFromString(cmd.String("trigger"))
			if err != nil {
				return err
			}
			client := newJobsClient(cmd)
			resp, err := client.CreateJob(ctx, connect.NewRequest(&fletcherv1.CreateJobRequest{
				Trigger:     trigger,
				Name:        cmd.String("name"),
				Command:     cmd.String("command"),
				Image:       cmd.String("image"),
				Credentials: cmd.StringSlice("credential"),
				Schedule:    cmd.String("schedule"),
			}))
			if err != nil {
				return err
			}
			return renderJob(os.Stdout, cmd.String("output"), resp.Msg.GetJob())
		},
	}
}

func jobGetCmd() *cli.Command {
	return &cli.Command{
		Name:      "get",
		Usage:     "fetch a job by id",
		ArgsUsage: "<id>",
		Flags:     []cli.Flag{socketFlag(), outputFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			id := cmd.Args().First()
			if id == "" {
				return errors.New("job id is required")
			}
			client := newJobsClient(cmd)
			resp, err := client.GetJob(ctx, connect.NewRequest(&fletcherv1.GetJobRequest{Id: id}))
			if err != nil {
				return err
			}
			return renderJob(os.Stdout, cmd.String("output"), resp.Msg.GetJob())
		},
	}
}

func jobListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list jobs (newest first)",
		Flags: []cli.Flag{
			socketFlag(),
			outputFlag(),
			&cli.IntFlag{Name: "limit", Value: 50, Usage: "maximum rows to return"},
			&cli.IntFlag{Name: "offset", Value: 0, Usage: "rows to skip from the newest"},
			&cli.StringFlag{Name: "status", Usage: "filter by status (queued|running|succeeded|failed|cancelled)"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			var status fletcherv1.JobStatus
			if s := cmd.String("status"); s != "" {
				st, err := statusFromString(s)
				if err != nil {
					return err
				}
				status = st
			}
			client := newJobsClient(cmd)
			limit := clampInt32(cmd.Int("limit"))
			offset := clampInt32(cmd.Int("offset"))
			resp, err := client.ListJobs(ctx, connect.NewRequest(&fletcherv1.ListJobsRequest{
				Limit:        limit,
				Offset:       offset,
				StatusFilter: status,
			}))
			if err != nil {
				return err
			}
			return renderJobList(os.Stdout, cmd.String("output"), resp.Msg)
		},
	}
}

func jobCancelCmd() *cli.Command {
	return &cli.Command{
		Name:      "cancel",
		Usage:     "cancel a queued or running job",
		ArgsUsage: "<id>",
		Flags:     []cli.Flag{socketFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			id := cmd.Args().First()
			if id == "" {
				return errors.New("job id is required")
			}
			client := newJobsClient(cmd)
			resp, err := client.CancelJob(ctx, connect.NewRequest(&fletcherv1.CancelJobRequest{Id: id}))
			if err != nil {
				return err
			}
			if resp.Msg.GetCancelled() {
				fmt.Printf("cancelled %s\n", id)
			} else {
				fmt.Printf("%s was not in a cancellable state\n", id)
			}
			return nil
		},
	}
}

// --- shared flags ---

func socketFlag() cli.Flag {
	return &cli.StringFlag{
		Name:    "socket",
		Usage:   "daemon Unix socket path",
		Sources: cli.EnvVars("FLETCHER_SOCKET"),
		Value:   defaultSocketPath(),
	}
}

func outputFlag() cli.Flag {
	return &cli.StringFlag{
		Name:    "output",
		Aliases: []string{"o"},
		Value:   "table",
		Usage:   "output format (table, json)",
	}
}

// --- input parsing ---

func triggerFromString(s string) (fletcherv1.JobTrigger, error) {
	switch strings.ToLower(s) {
	case "ephemeral":
		return fletcherv1.JobTrigger_JOB_TRIGGER_EPHEMERAL, nil
	case "cron":
		return fletcherv1.JobTrigger_JOB_TRIGGER_CRON, nil
	case "long_running", "long-running":
		return fletcherv1.JobTrigger_JOB_TRIGGER_LONG_RUNNING, nil
	}
	return fletcherv1.JobTrigger_JOB_TRIGGER_UNSPECIFIED,
		fmt.Errorf("invalid trigger %q (want: ephemeral, cron, long_running)", s)
}

func statusFromString(s string) (fletcherv1.JobStatus, error) {
	switch strings.ToLower(s) {
	case "queued":
		return fletcherv1.JobStatus_JOB_STATUS_QUEUED, nil
	case "running":
		return fletcherv1.JobStatus_JOB_STATUS_RUNNING, nil
	case "succeeded":
		return fletcherv1.JobStatus_JOB_STATUS_SUCCEEDED, nil
	case "failed":
		return fletcherv1.JobStatus_JOB_STATUS_FAILED, nil
	case "cancelled", "canceled":
		return fletcherv1.JobStatus_JOB_STATUS_CANCELLED, nil
	case "scheduled":
		return fletcherv1.JobStatus_JOB_STATUS_SCHEDULED, nil
	}
	return fletcherv1.JobStatus_JOB_STATUS_UNSPECIFIED,
		fmt.Errorf("invalid status %q", s)
}

// --- output rendering ---

func renderJob(w io.Writer, format string, j *fletcherv1.Job) error {
	if format == "json" {
		return writeProtoJSON(w, j)
	}
	return writeJobDetails(w, j)
}

func renderJobList(w io.Writer, format string, resp *fletcherv1.ListJobsResponse) error {
	if format == "json" {
		return writeProtoJSON(w, resp)
	}
	return writeJobsTable(w, resp.GetJobs(), resp.GetTotal())
}

// writeProtoJSON renders a proto message as canonical JSON: snake_case field
// names and enum string forms. The protobuf JSON marshal already understands
// optional fields and the standard well-known types.
func writeProtoJSON(w io.Writer, msg proto.Message) error {
	opts := protojson.MarshalOptions{
		Multiline:     true,
		Indent:        "  ",
		UseProtoNames: true,
	}
	b, err := opts.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}

func writeJobDetails(w io.Writer, j *fletcherv1.Job) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "id:\t%s\n", j.GetId())
	fmt.Fprintf(tw, "name:\t%s\n", j.GetName())
	fmt.Fprintf(tw, "status:\t%s\n", coloredStatusLabel(j.GetStatus()))
	fmt.Fprintf(tw, "trigger:\t%s\n", triggerLabel(j.GetTrigger()))
	if j.GetSchedule() != "" {
		fmt.Fprintf(tw, "schedule:\t%s\n", j.GetSchedule())
	}
	if j.NextRunAt != nil {
		fmt.Fprintf(tw, "next_run_at:\t%s\n", formatUnix(j.GetNextRunAt()))
	}
	if j.ParentId != nil {
		fmt.Fprintf(tw, "parent:\t%s\n", j.GetParentId())
	}
	fmt.Fprintf(tw, "image:\t%s\n", j.GetImage())
	fmt.Fprintf(tw, "command:\t%s\n", j.GetCommand())
	if creds := j.GetCredentials(); len(creds) > 0 {
		fmt.Fprintf(tw, "credentials:\t%s\n", strings.Join(creds, ", "))
	}
	fmt.Fprintf(tw, "created_at:\t%s\n", formatUnix(j.GetCreatedAt()))
	fmt.Fprintf(tw, "updated_at:\t%s\n", formatUnix(j.GetUpdatedAt()))
	if j.StartedAt != nil {
		fmt.Fprintf(tw, "started_at:\t%s\n", formatUnix(j.GetStartedAt()))
	}
	if j.CompletedAt != nil {
		fmt.Fprintf(tw, "completed_at:\t%s\n", formatUnix(j.GetCompletedAt()))
	}
	if j.ExitCode != nil {
		fmt.Fprintf(tw, "exit_code:\t%d\n", j.GetExitCode())
	}
	if j.GetErrorMessage() != "" {
		fmt.Fprintf(tw, "error:\t%s\n", j.GetErrorMessage())
	}
	return tw.Flush()
}

func writeJobsTable(w io.Writer, jobs []*fletcherv1.Job, total int64) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATUS\tTRIGGER\tCREATED")
	for _, j := range jobs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			j.GetId(),
			j.GetName(),
			coloredStatusLabel(j.GetStatus()),
			triggerLabel(j.GetTrigger()),
			formatUnix(j.GetCreatedAt()),
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(w, "\ntotal: %d\n", total)
	return nil
}

func statusLabel(s fletcherv1.JobStatus) string {
	switch s {
	case fletcherv1.JobStatus_JOB_STATUS_QUEUED:
		return "queued"
	case fletcherv1.JobStatus_JOB_STATUS_RUNNING:
		return "running"
	case fletcherv1.JobStatus_JOB_STATUS_SUCCEEDED:
		return "succeeded"
	case fletcherv1.JobStatus_JOB_STATUS_FAILED:
		return "failed"
	case fletcherv1.JobStatus_JOB_STATUS_CANCELLED:
		return "cancelled"
	case fletcherv1.JobStatus_JOB_STATUS_SCHEDULED:
		return "scheduled"
	}
	return "unknown"
}

// coloredStatusLabel is statusLabel plus an ANSI colour by state.
// Used in human table output only; the JSON path goes through the
// underlying proto enum and never hits this.
func coloredStatusLabel(s fletcherv1.JobStatus) string {
	label := statusLabel(s)
	switch s {
	case fletcherv1.JobStatus_JOB_STATUS_QUEUED:
		return blue(label)
	case fletcherv1.JobStatus_JOB_STATUS_RUNNING:
		return yellow(label)
	case fletcherv1.JobStatus_JOB_STATUS_SUCCEEDED:
		return green(label)
	case fletcherv1.JobStatus_JOB_STATUS_FAILED:
		return red(label)
	case fletcherv1.JobStatus_JOB_STATUS_CANCELLED:
		return gray(label)
	case fletcherv1.JobStatus_JOB_STATUS_SCHEDULED:
		return blue(label)
	}
	return label
}

func triggerLabel(t fletcherv1.JobTrigger) string {
	switch t {
	case fletcherv1.JobTrigger_JOB_TRIGGER_EPHEMERAL:
		return "ephemeral"
	case fletcherv1.JobTrigger_JOB_TRIGGER_CRON:
		return "cron"
	case fletcherv1.JobTrigger_JOB_TRIGGER_LONG_RUNNING:
		return "long_running"
	}
	return "unknown"
}

// clampInt32 saturates a host-sized int (urfave/cli's IntFlag returns int)
// to the int32 range so the proto wire types - designed for sensible page
// sizes - can't overflow.
func clampInt32(v int) int32 {
	const maxInt32 = 1<<31 - 1
	const minInt32 = -1 << 31
	switch {
	case v > maxInt32:
		return int32(maxInt32)
	case v < minInt32:
		return int32(minInt32)
	default:
		return int32(v)
	}
}

func formatUnix(epoch int64) string {
	if epoch == 0 {
		return "-"
	}
	return time.Unix(epoch, 0).UTC().Format(time.RFC3339)
}
