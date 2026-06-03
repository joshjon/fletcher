package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
)

func modelCmd() *cli.Command {
	return &cli.Command{
		Name:  "model",
		Usage: "inspect the gateway's model catalog",
		Commands: []*cli.Command{
			modelListCmd(),
		},
	}
}

func modelListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list endpoints and models the gateway can route to",
		Description: `The gateway speaks two wire formats. Agents inside Fletcher jobs
have ANTHROPIC_BASE_URL and OPENAI_BASE_URL auto-set; you only need
the URL yourself when running an agent on your host machine or
calling the gateway directly with curl.

Anthropic SDKs (Claude Code) read ANTHROPIC_BASE_URL. OpenAI SDKs
(Codex, Aider, OpenHands, pi) read OPENAI_BASE_URL. Set the env var
to the matching URL.

Model IDs work on either endpoint - same id, two wire formats.`,
		Flags: []cli.Flag{
			socketFlag(),
			outputFlag(),
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			client := newModelsClient(cmd.String("socket"))
			resp, err := client.ListModels(ctx, connect.NewRequest(&fletcherv1.ListModelsRequest{}))
			if err != nil {
				return err
			}
			return renderCatalog(os.Stdout, cmd.String("output"), resp.Msg)
		},
	}
}

const catalogFooter = `Fletcher auto-injects the env vars into every job, so agents inside forks
need no setup. Outside Fletcher, export the env var or hit the URL directly.`

func renderCatalog(w io.Writer, format string, resp *fletcherv1.ListModelsResponse) error {
	if format == "json" {
		return writeProtoJSON(w, resp)
	}
	if err := renderEndpointsSection(w, resp.GetEndpoints()); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	if err := renderModelsSection(w, resp.GetModels()); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, "\n"+catalogFooter)
	return err
}

func renderEndpointsSection(w io.Writer, endpoints []*fletcherv1.Endpoint) error {
	if _, err := fmt.Fprintln(w, "ENDPOINTS"); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SHAPE\tURL\tAGENT ENV VAR")
	for _, e := range endpoints {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", e.GetKind(), e.GetUrl(), e.GetEnvVar())
	}
	return tw.Flush()
}

func renderModelsSection(w io.Writer, models []*fletcherv1.Model) error {
	if _, err := fmt.Fprintln(w, "MODELS"); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tLABEL\tUPSTREAM")
	for _, m := range models {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", m.GetId(), m.GetLabel(), m.GetUpstream())
	}
	return tw.Flush()
}
