// Command fletcher is the Fletcher daemon and client CLI.
//
// "fletcher serve" runs the daemon. Other subcommands are client operations
// that talk to a running daemon over its local Unix socket.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v3"

	"github.com/joshjon/fletcher/internal/buildinfo"
)

func main() {
	if err := run(); err != nil {
		// Colour the "fletcher:" prefix red so the failure stands out
		// against ordinary output, especially when the error message
		// itself is short. The error's own text is left uncoloured -
		// it may already include categorised prefixes from the server
		// (e.g. "invalid_argument: ...") that the user reads as data.
		fmt.Fprintln(os.Stderr, red("fletcher:"), err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return newApp().Run(ctx, os.Args)
}

func newApp() *cli.Command {
	return &cli.Command{
		Name:    "fletcher",
		Usage:   "private agent compute on hardware you own",
		Version: buildinfo.Version,
		Commands: []*cli.Command{
			serveCmd(),
			healthCmd(),
			doctorCmd(),
			jobCmd(),
			modelCmd(),
			secretCmd(),
			approvalCmd(),
			peerCmd(),
			versionCmd(),
		},
	}
}

func versionCmd() *cli.Command {
	return &cli.Command{
		Name:  "version",
		Usage: "print version, commit, and build date",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "json",
				Usage: "output as JSON",
			},
			&cli.BoolFlag{
				Name:  "check-latest",
				Usage: "also check GitHub for the newest published release",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			info := buildinfo.Info()
			var (
				latest      *buildinfo.LatestRelease
				noReleases  bool
				checkErrMsg string
			)
			if cmd.Bool("check-latest") {
				rel, err := buildinfo.CheckLatest(ctx, nil)
				switch {
				case errors.Is(err, buildinfo.ErrNoReleases):
					noReleases = true
				case err != nil:
					checkErrMsg = err.Error()
					fmt.Fprintf(os.Stderr, "could not check latest release: %v\n", err)
				default:
					latest = &rel
				}
			}
			if cmd.Bool("json") {
				out := versionOutput{Information: info}
				if latest != nil {
					out.Latest = latest.TagName
					out.UpgradeAvailable = buildinfo.UpgradeAvailable(info.Version, latest.TagName)
					out.LatestURL = latest.HTMLURL
				}
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}
			fmt.Printf("fletcher %s (commit %s, built %s)\n", info.Version, info.Commit, info.Date)
			switch {
			case latest != nil && buildinfo.UpgradeAvailable(info.Version, latest.TagName):
				fmt.Printf("upgrade available: %s\n  %s\n", latest.TagName, latest.HTMLURL)
			case latest != nil:
				fmt.Printf("up to date (latest released: %s)\n", latest.TagName)
			case noReleases:
				fmt.Println("no releases published yet")
			case checkErrMsg != "":
				// Error already printed to stderr; keep stdout clean.
			}
			return nil
		},
	}
}

// versionOutput is the JSON shape printed by `fletcher version --json`.
// Includes the optional --check-latest fields so consumers can parse
// the same shape in both modes.
type versionOutput struct {
	buildinfo.Information
	Latest           string `json:"latest,omitempty"`
	LatestURL        string `json:"latest_url,omitempty"`
	UpgradeAvailable bool   `json:"upgrade_available,omitempty"`
}
