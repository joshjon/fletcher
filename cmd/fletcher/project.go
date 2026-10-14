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
	"github.com/joshjon/fletcher/internal/runtime"
)

func projectCmd() *cli.Command {
	return &cli.Command{
		Name: "project", Usage: "save and reopen project directories in existing development VMs",
		Commands: []*cli.Command{projectCreateCmd(), projectGetCmd(), projectListCmd(), projectUpdateCmd(), projectDeleteCmd(), projectOpenCmd()},
	}
}

func newProjectsClient(cmd *cli.Command) fletcherv1connect.ProjectServiceClient {
	hc, base, opts := clientTarget(cmd)
	return fletcherv1connect.NewProjectServiceClient(hc, base, opts...)
}

func projectCreateCmd() *cli.Command {
	return &cli.Command{
		Name: "create", Usage: "register a guest directory without creating a VM or changing files",
		Flags: []cli.Flag{
			socketFlag(), outputFlag(),
			&cli.StringFlag{Name: "name", Usage: "unique project name", Required: true},
			&cli.StringFlag{Name: "session", Usage: "existing development session (id or name)", Required: true},
			&cli.StringFlag{Name: "workdir", Usage: "absolute directory inside the guest", Value: "/workspace"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			resp, err := newProjectsClient(cmd).CreateProject(ctx, connect.NewRequest(&fletcherv1.CreateProjectRequest{
				Name: cmd.String("name"), SessionRef: cmd.String("session"), WorkingDir: cmd.String("workdir"),
			}))
			if err != nil {
				return err
			}
			return renderProject(cmd.String("output"), resp.Msg.GetProject())
		},
	}
}

func projectGetCmd() *cli.Command {
	return &cli.Command{
		Name: "get", Usage: "show a saved project without waking its VM", ArgsUsage: "<ref>",
		Flags: []cli.Flag{socketFlag(), outputFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ref, err := projectRef(cmd)
			if err != nil {
				return err
			}
			resp, err := newProjectsClient(cmd).GetProject(ctx, connect.NewRequest(&fletcherv1.GetProjectRequest{Ref: ref}))
			if err != nil {
				return err
			}
			return renderProject(cmd.String("output"), resp.Msg.GetProject())
		},
	}
}

func projectListCmd() *cli.Command {
	return &cli.Command{
		Name: "list", Usage: "list saved projects without waking their VMs",
		Flags: []cli.Flag{socketFlag(), outputFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			resp, err := newProjectsClient(cmd).ListProjects(ctx, connect.NewRequest(&fletcherv1.ListProjectsRequest{}))
			if err != nil {
				return err
			}
			if cmd.String("output") == "json" {
				return writeProtoJSON(os.Stdout, resp.Msg)
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tSESSION\tGUEST DIRECTORY")
			for _, p := range resp.Msg.GetProjects() {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.GetId(), p.GetName(), p.GetSessionId(), p.GetWorkingDir())
			}
			return tw.Flush()
		},
	}
}

func projectUpdateCmd() *cli.Command {
	return &cli.Command{
		Name: "update", Usage: "rename a project or change its initial directory (files and live terminals stay unchanged)", ArgsUsage: "<ref>",
		Flags: []cli.Flag{
			socketFlag(), outputFlag(),
			&cli.StringFlag{Name: "name", Usage: "new project name"},
			&cli.StringFlag{Name: "workdir", Usage: "new absolute guest directory for newly created terminals"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ref, err := projectRef(cmd)
			if err != nil {
				return err
			}
			resp, err := newProjectsClient(cmd).UpdateProject(ctx, connect.NewRequest(&fletcherv1.UpdateProjectRequest{
				Ref: ref, Name: cmd.String("name"), WorkingDir: cmd.String("workdir"),
			}))
			if err != nil {
				return err
			}
			return renderProject(cmd.String("output"), resp.Msg.GetProject())
		},
	}
}

func projectDeleteCmd() *cli.Command {
	return &cli.Command{
		Name: "delete", Usage: "remove only a saved registration (VM, files and terminals are untouched)", ArgsUsage: "<ref>",
		Flags: []cli.Flag{socketFlag(), yesFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ref, err := projectRef(cmd)
			if err != nil {
				return err
			}
			if err := confirmManagement(cmd, "Remove the project registration? The VM, files and terminals will stay unchanged."); err != nil {
				return err
			}
			if _, err := newProjectsClient(cmd).DeleteProject(ctx, connect.NewRequest(&fletcherv1.DeleteProjectRequest{Ref: ref})); err != nil {
				return err
			}
			fmt.Printf("removed project registration %s\n", ref)
			return nil
		},
	}
}

func projectOpenCmd() *cli.Command {
	return &cli.Command{
		Name: "open", Usage: "wake the existing VM and open or reattach a durable project terminal", ArgsUsage: "<ref>",
		Flags: []cli.Flag{
			socketFlag(),
			&cli.StringFlag{Name: "terminal", Usage: "terminal label (same label reattaches; another label starts independent work in this VM)", Value: "main"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ref, err := projectRef(cmd)
			if err != nil {
				return err
			}
			label := cmd.String("terminal")
			if label == "" || len(label) > 64 {
				return errors.New("project terminal label must be 1-64 characters")
			}
			if err := (runtime.ShellSpec{TerminalName: label}).Validate(); err != nil {
				return err
			}
			resp, err := newProjectsClient(cmd).OpenProject(ctx, connect.NewRequest(&fletcherv1.OpenProjectRequest{Ref: ref}))
			if err != nil {
				return err
			}
			p := resp.Msg.GetProject()
			return runSessionShellTarget(ctx, cmd, p.GetSessionId(), p.GetWorkingDir(), p.GetId()+"-"+label)
		},
	}
}

func projectRef(cmd *cli.Command) (string, error) {
	if cmd.Args().Len() != 1 {
		return "", errors.New("exactly one project ref (id or name) is required")
	}
	return cmd.Args().First(), nil
}

func renderProject(format string, p *fletcherv1.Project) error {
	if format == "json" {
		return writeProtoJSON(os.Stdout, p)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "id:\t%s\nname:\t%s\nsession:\t%s\nguest directory:\t%s\n",
		p.GetId(), p.GetName(), p.GetSessionId(), p.GetWorkingDir())
	return tw.Flush()
}
