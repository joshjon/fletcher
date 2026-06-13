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
		Usage: "save a login once and reuse it across sessions (agent logins, or a git host login)",
		Commands: []*cli.Command{
			credentialSaveCmd(),
			credentialGitCmd(),
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

func credentialGitCmd() *cli.Command {
	return &cli.Command{
		Name:  "git",
		Usage: "save a git host login (host + username + token) for cloning in new sessions",
		Flags: []cli.Flag{
			socketFlag(),
			&cli.StringFlag{Name: "host", Usage: "git host (a bare hostname)", Value: "github.com"},
			&cli.StringFlag{Name: "username", Usage: "account/login for the host", Required: true},
			&cli.StringFlag{Name: "token", Usage: "password or personal access token", Required: true},
			&cli.StringFlag{Name: "name", Usage: "committer name (git user.name)"},
			&cli.StringFlag{Name: "email", Usage: "committer email (git user.email)"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			host := cmd.String("host")
			client := newCredentialsClient(cmd)
			_, err := client.SaveGitCredential(ctx, connect.NewRequest(&fletcherv1.SaveGitCredentialRequest{
				Host:         host,
				Username:     cmd.String("username"),
				Token:        cmd.String("token"),
				GitUserName:  cmd.String("name"),
				GitUserEmail: cmd.String("email"),
			}))
			if err != nil {
				return err
			}
			fmt.Printf("saved git login for %s; create a session with `--credential git` and egress that reaches %s to clone\n", host, host)
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
		ArgsUsage: "<claude|codex|gemini|pi|git>",
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
