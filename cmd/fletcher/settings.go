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

// settingsCmd manages runtime-mutable operational settings stored in the
// daemon. Set a value and restart the daemon to apply it - no systemctl edit.
func settingsCmd() *cli.Command {
	return &cli.Command{
		Name:  "settings",
		Usage: "view and change runtime settings (apply on `fletcher daemon restart`)",
		Commands: []*cli.Command{
			settingsListCmd(),
			settingsSetCmd(),
			settingsUnsetCmd(),
		},
	}
}

func settingsListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list every setting with its value and help",
		Flags: []cli.Flag{socketFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			client := newSettingsClient(cmd.String("socket"))
			resp, err := client.ListSettings(ctx, connect.NewRequest(&fletcherv1.ListSettingsRequest{}))
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "KEY\tVALUE\tDESCRIPTION")
			for _, s := range resp.Msg.GetSettings() {
				value := s.GetValue()
				if !s.GetSet() {
					value = "(default)"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", s.GetKey(), value, s.GetDescription())
			}
			return w.Flush()
		},
	}
}

func settingsSetCmd() *cli.Command {
	return &cli.Command{
		Name:      "set",
		Usage:     "set a setting (restart the daemon to apply)",
		ArgsUsage: "<key> <value>",
		Flags:     []cli.Flag{socketFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.Args().Len() != 2 {
				return errors.New("usage: fletcher settings set <key> <value>")
			}
			key, value := cmd.Args().Get(0), cmd.Args().Get(1)
			client := newSettingsClient(cmd.String("socket"))
			if _, err := client.SetSetting(ctx, connect.NewRequest(&fletcherv1.SetSettingRequest{Key: key, Value: value})); err != nil {
				return err
			}
			fmt.Printf("set %s=%s\n", key, value)
			fmt.Println("run `fletcher daemon restart` to apply")
			return nil
		},
	}
}

func settingsUnsetCmd() *cli.Command {
	return &cli.Command{
		Name:      "unset",
		Usage:     "remove a setting, reverting it to the flag/env default",
		ArgsUsage: "<key>",
		Flags:     []cli.Flag{socketFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			key := cmd.Args().First()
			if key == "" {
				return errors.New("a setting key is required")
			}
			client := newSettingsClient(cmd.String("socket"))
			resp, err := client.DeleteSetting(ctx, connect.NewRequest(&fletcherv1.DeleteSettingRequest{Key: key}))
			if err != nil {
				return err
			}
			if resp.Msg.GetExisted() {
				fmt.Printf("unset %s\n", key)
				fmt.Println("run `fletcher daemon restart` to apply")
			} else {
				fmt.Printf("%s was not set\n", key)
			}
			return nil
		},
	}
}

func newSettingsClient(socket string) fletcherv1connect.SettingsServiceClient {
	return fletcherv1connect.NewSettingsServiceClient(unixHTTPClient(socket), unixBaseURL)
}
