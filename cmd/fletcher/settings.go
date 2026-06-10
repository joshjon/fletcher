package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

// settingsCmd manages runtime-mutable operational settings stored in the
// daemon. Live settings apply via `settings reload`; the rest need a daemon
// restart - no systemctl edit either way.
func settingsCmd() *cli.Command {
	return &cli.Command{
		Name:  "settings",
		Usage: "view and change runtime settings (apply live with `reload`, or on restart)",
		Commands: []*cli.Command{
			settingsListCmd(),
			settingsSetCmd(),
			settingsUnsetCmd(),
			settingsReloadCmd(),
		},
	}
}

func settingsReloadCmd() *cli.Command {
	return &cli.Command{
		Name:  "reload",
		Usage: "apply the live-reloadable settings now, without restarting the daemon",
		Flags: []cli.Flag{socketFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			client := newSettingsClient(cmd)
			resp, err := client.ReloadSettings(ctx, connect.NewRequest(&fletcherv1.ReloadSettingsRequest{}))
			if err != nil {
				return err
			}
			fmt.Printf("reloaded %d live setting(s)\n", len(resp.Msg.GetReloaded()))
			if pending := resp.Msg.GetPendingRestart(); len(pending) > 0 {
				fmt.Printf("changed settings that still need `fletcher daemon restart`: %s\n", strings.Join(pending, ", "))
			}
			return nil
		},
	}
}

func settingsListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list every setting with its value and help",
		Flags: []cli.Flag{socketFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			client := newSettingsClient(cmd)
			resp, err := client.ListSettings(ctx, connect.NewRequest(&fletcherv1.ListSettingsRequest{}))
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "KEY\tVALUE\tAPPLY\tDESCRIPTION")
			for _, s := range resp.Msg.GetSettings() {
				// Show the effective value. Mark it "(default)" when it is not
				// explicitly set; if there is no concrete default (auto/none),
				// show just "(default)".
				value := s.GetValue()
				switch {
				case s.GetSet():
					// explicit value, shown as-is
				case value == "":
					value = "(default)"
				default:
					value += " (default)"
				}
				apply := "live"
				if s.GetRequiresRestart() {
					apply = "restart"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.GetKey(), value, apply, s.GetDescription())
			}
			if err := w.Flush(); err != nil {
				return err
			}
			fmt.Println("\nAPPLY=live: `fletcher settings reload` applies it now. APPLY=restart: needs `fletcher daemon restart`.")
			return nil
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
			client := newSettingsClient(cmd)
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
			client := newSettingsClient(cmd)
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

func newSettingsClient(cmd *cli.Command) fletcherv1connect.SettingsServiceClient {
	hc, base, opts := clientTarget(cmd)
	return fletcherv1connect.NewSettingsServiceClient(hc, base, opts...)
}
