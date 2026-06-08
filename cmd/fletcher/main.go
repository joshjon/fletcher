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
	// If the operator was just added to the daemon's group but their shell
	// predates the change, transparently re-exec under that group via sg(1)
	// so client commands work without a logout. No-op in every other case.
	maybeReexecUnderDaemonGroup()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return newApp().Run(ctx, os.Args)
}

func newApp() *cli.Command {
	return &cli.Command{
		Name:    "fletcher",
		Usage:   "private agent compute on hardware you own",
		Version: buildinfo.Version,
		// Root flags are persistent in urfave/cli v3, so subcommands can read
		// --remote/--token to target a daemon over the tunnel.
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "remote",
				Sources: cli.EnvVars("FLETCHER_REMOTE"),
				Usage:   "drive a remote daemon at host:port over the tunnel instead of the local socket",
			},
			&cli.StringFlag{
				Name:    "token",
				Sources: cli.EnvVars("FLETCHER_TOKEN"),
				Usage:   "per-peer API token for --remote (from `fletcher peer pair`)",
			},
		},
		Commands: []*cli.Command{
			// Client commands: drive a daemon over the local socket or a remote
			// tunnel; they run on any OS (this is all a Mac client needs).
			client(healthCmd()),
			client(loginCmd()),
			client(logoutCmd()),
			client(jobCmd()),
			client(sessionCmd()),
			client(modelCmd()),
			client(secretCmd()),
			client(settingsCmd()),
			client(approvalCmd()),
			client(peerCmd()),
			client(versionCmd()),
			// Daemon commands: run on the Linux host that hosts the daemon.
			daemonHost(serveCmd()),
			daemonHost(daemonCmd()),
			daemonHost(doctorCmd()),
			daemonHost(imageCmd()),
			daemonHost(deployCmd()),
			forkRunCmd(), // hidden internal re-exec
		},
	}
}

// Command categories group `fletcher --help` so a client (e.g. on a Mac) can
// tell at a glance which commands are for driving a daemon versus running one.
const (
	catClient = "Client"
	catDaemon = "Daemon (Linux host)"
)

func client(c *cli.Command) *cli.Command     { c.Category = catClient; return c }
func daemonHost(c *cli.Command) *cli.Command { c.Category = catDaemon; return c }

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
