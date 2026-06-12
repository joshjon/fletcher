package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

func credentialCmd() *cli.Command {
	return &cli.Command{
		Name:  "credential",
		Usage: "save an agent login once and reuse it across sessions (claude | codex | gemini)",
		Commands: []*cli.Command{
			credentialSaveCmd(),
			credentialListCmd(),
			credentialDeleteCmd(),
		},
	}
}

func newCredentialsClient(cmd *cli.Command) fletcherv1connect.CredentialServiceClient {
	hc, base, opts := clientTarget(cmd)
	return fletcherv1connect.NewCredentialServiceClient(hc, base, opts...)
}

func credentialSaveCmd() *cli.Command {
	return &cli.Command{
		Name:      "save",
		Usage:     "save a running session's agent login as the box default for new sessions",
		ArgsUsage: "<claude|codex|gemini>",
		Flags: []cli.Flag{
			socketFlag(),
			&cli.StringFlag{Name: "from-session", Usage: "session (id or name) you logged into", Required: true},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			name := cmd.Args().First()
			if name == "" {
				return errors.New("name the login to save: claude | codex | gemini")
			}
			client := newCredentialsClient(cmd)
			_, err := client.SaveSessionLogin(ctx, connect.NewRequest(&fletcherv1.SaveSessionLoginRequest{
				SessionRef: cmd.String("from-session"),
				Name:       name,
			}))
			if err != nil {
				return err
			}
			fmt.Printf("saved %s login; create a session with `--credential %s` to reuse it\n", name, name)
			return nil
		},
	}
}

func credentialListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list the box's saved logins",
		Flags: []cli.Flag{socketFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			client := newCredentialsClient(cmd)
			resp, err := client.ListCredentials(ctx, connect.NewRequest(&fletcherv1.ListCredentialsRequest{}))
			if err != nil {
				return err
			}
			names := resp.Msg.GetNames()
			if len(names) == 0 {
				fmt.Println("no saved logins (save one with `fletcher credential save`)")
				return nil
			}
			fmt.Println(strings.Join(names, "\n"))
			return nil
		},
	}
}

func credentialDeleteCmd() *cli.Command {
	return &cli.Command{
		Name:      "rm",
		Usage:     "remove a saved login",
		ArgsUsage: "<claude|codex|gemini>",
		Flags:     []cli.Flag{socketFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			name := cmd.Args().First()
			if name == "" {
				return errors.New("name the login to remove")
			}
			client := newCredentialsClient(cmd)
			_, err := client.DeleteCredential(ctx, connect.NewRequest(&fletcherv1.DeleteCredentialRequest{Name: name}))
			if err != nil {
				return err
			}
			fmt.Printf("removed saved %s login\n", name)
			return nil
		},
	}
}
