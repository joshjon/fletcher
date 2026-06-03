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

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

func secretCmd() *cli.Command {
	return &cli.Command{
		Name:  "secret",
		Usage: "manage the daemon's encrypted secrets",
		Commands: []*cli.Command{
			secretSetCmd(),
			secretListCmd(),
			secretDeleteCmd(),
		},
	}
}

func secretSetCmd() *cli.Command {
	return &cli.Command{
		Name:      "set",
		Usage:     "store or update an encrypted secret",
		ArgsUsage: "<name> [value]",
		Flags: []cli.Flag{
			socketFlag(),
			&cli.BoolFlag{
				Name:  "stdin",
				Usage: "read the value from stdin instead of arg",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			args := cmd.Args().Slice()
			if len(args) == 0 {
				return errors.New("secret name is required")
			}
			name := args[0]

			value, err := readSecretValue(args, cmd.Bool("stdin"))
			if err != nil {
				return err
			}

			client := newSecretsClient(cmd.String("socket"))
			_, err = client.SetSecret(ctx, connect.NewRequest(&fletcherv1.SetSecretRequest{
				Name:  name,
				Value: value,
			}))
			if err != nil {
				return err
			}
			fmt.Printf("stored %s\n", name)
			return nil
		},
	}
}

func secretListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list secret names (values are never returned)",
		Flags: []cli.Flag{socketFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			client := newSecretsClient(cmd.String("socket"))
			resp, err := client.ListSecrets(ctx, connect.NewRequest(&fletcherv1.ListSecretsRequest{}))
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tCREATED\tUPDATED")
			for _, s := range resp.Msg.GetSecrets() {
				fmt.Fprintf(tw, "%s\t%s\t%s\n",
					s.GetName(),
					time.Unix(s.GetCreatedAt(), 0).UTC().Format(time.RFC3339),
					time.Unix(s.GetUpdatedAt(), 0).UTC().Format(time.RFC3339),
				)
			}
			return tw.Flush()
		},
	}
}

func secretDeleteCmd() *cli.Command {
	return &cli.Command{
		Name:      "delete",
		Usage:     "remove a secret",
		ArgsUsage: "<name>",
		Flags:     []cli.Flag{socketFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			name := cmd.Args().First()
			if name == "" {
				return errors.New("secret name is required")
			}
			client := newSecretsClient(cmd.String("socket"))
			resp, err := client.DeleteSecret(ctx, connect.NewRequest(&fletcherv1.DeleteSecretRequest{Name: name}))
			if err != nil {
				return err
			}
			if resp.Msg.GetExisted() {
				fmt.Printf("deleted %s\n", name)
			} else {
				fmt.Printf("%s did not exist\n", name)
			}
			return nil
		},
	}
}

// readSecretValue resolves the secret value from CLI args or stdin. The
// stdin path is the recommended one (no value in shell history).
func readSecretValue(args []string, fromStdin bool) (string, error) {
	if fromStdin {
		data, err := readAllStdin()
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return strings.TrimRight(data, "\n\r"), nil
	}
	if len(args) < 2 {
		return "", errors.New("value is required (pass as second arg or use --stdin)")
	}
	return args[1], nil
}

func readAllStdin() (string, error) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func newSecretsClient(socket string) fletcherv1connect.SecretServiceClient {
	return fletcherv1connect.NewSecretServiceClient(unixHTTPClient(socket), unixBaseURL)
}
