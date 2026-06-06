package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

func loginCmd() *cli.Command {
	return &cli.Command{
		Name:      "login",
		Usage:     "save credentials to drive a remote daemon over the tunnel",
		ArgsUsage: "<login-token>",
		Description: "Paste the login token printed by `fletcher peer pair` on the server, " +
			"or pass --remote and --token explicitly. The credential is stored at " +
			"~/.config/fletcher/config.toml (mode 0600) and becomes the default target, " +
			"so `fletcher <command>` then drives the remote daemon without flags. Explicit " +
			"--remote/--token flags and the FLETCHER_REMOTE/FLETCHER_TOKEN env vars override it.",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "no-verify", Usage: "save without checking the credential against the daemon first"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			remote, token, err := loginInputs(cmd)
			if err != nil {
				return err
			}
			if !cmd.Bool("no-verify") {
				if verr := verifyRemote(ctx, remote, token); verr != nil {
					return fmt.Errorf("could not reach the daemon at %s with this token: %w\n(pass --no-verify to save anyway)", remote, verr)
				}
			}
			if err := saveClientConfig(clientConfig{Remote: remote, Token: token}); err != nil {
				return err
			}
			fmt.Printf("Logged in to %s; `fletcher` commands now target it by default.\n", remote)
			fmt.Println("Run `fletcher logout` to revert to the local socket.")
			return nil
		},
	}
}

// loginInputs resolves the remote+token from a positional login token, falling
// back to the (persistent) --remote/--token flags.
func loginInputs(cmd *cli.Command) (remote, token string, err error) {
	if blob := cmd.Args().First(); blob != "" {
		decoded, derr := decodeLoginBlob(blob)
		if derr != nil {
			return "", "", derr
		}
		return decoded.Remote, decoded.Token, nil
	}
	remote, token = cmd.String("remote"), cmd.String("token")
	if remote == "" || token == "" {
		return "", "", errors.New("paste the login token from `fletcher peer pair`, or pass both --remote and --token")
	}
	return remote, token, nil
}

func logoutCmd() *cli.Command {
	return &cli.Command{
		Name:  "logout",
		Usage: "remove stored remote credentials (revert to the local socket)",
		Action: func(_ context.Context, _ *cli.Command) error {
			removed, err := clearClientConfig()
			if err != nil {
				return err
			}
			if removed {
				fmt.Println("Logged out; `fletcher` commands now use the local socket.")
			} else {
				fmt.Println("No stored credentials; nothing to do.")
			}
			return nil
		},
	}
}

// verifyRemote checks the remote+token by calling Health, so a bad token or an
// unreachable daemon is caught at login rather than on the next command.
func verifyRemote(ctx context.Context, remote, token string) error {
	hc := &http.Client{Timeout: 10 * time.Second}
	admin := fletcherv1connect.NewAdminServiceClient(hc, "http://"+remote,
		connect.WithInterceptors(bearerAuthInterceptor(token)))
	_, err := admin.Health(ctx, connect.NewRequest(&fletcherv1.HealthRequest{}))
	return err
}
