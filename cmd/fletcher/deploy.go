package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
)

// defaultDeployBtrfsRoot matches the snapshot root the systemd install uses, so
// a local (Dockerfile) deploy works with no --btrfs-root on a standard install.
const defaultDeployBtrfsRoot = "/var/lib/fletcher/snapshots"

func deployCmd() *cli.Command {
	return &cli.Command{
		Name:      "deploy",
		Usage:     "build (or pull) a Docker image and run it as a published app session",
		ArgsUsage: "<dir-or-image-ref>",
		Description: `One command from a Docker image to a running app:

  1. a registry ref (e.g. ghcr.io/you/app:v1) is pulled and flattened by the
     DAEMON, so this works from a remote client - no local docker needed;
     a local directory with a Dockerfile is built and imported on the host
  2. a session is created that runs the image's own app on boot (--app)
  3. the app's port is published - publicly over HTTPS with --host, else tunnel-only

Examples:

  fletcher deploy ghcr.io/you/app:v1 --host app.example.com           # from anywhere
  fletcher deploy ghcr.io/you/app:v1 --registry-auth user:TOKEN ...   # private registry
  sudo fletcher deploy ./myapp --host app.example.com                 # local Dockerfile (host-only)

The port defaults to the image's EXPOSE; set --port if the image declares none.`,
		Flags: []cli.Flag{
			socketFlag(),
			btrfsRootFlag(),
			&cli.StringFlag{Name: "name", Usage: "session + template name (default: derived from the image/dir)"},
			&cli.StringFlag{Name: "host", Usage: "public hostname to serve at over HTTPS, e.g. app.example.com (omit for tunnel-only)"},
			&cli.IntFlag{Name: "port", Usage: "container port to publish (default: the image's EXPOSE)"},
			&cli.StringFlag{Name: "registry-auth", Usage: "private registry credentials as user:token (for a registry ref)"},
			&cli.StringFlag{Name: "egress", Usage: "fork network egress: none | allowlist | open (default: the daemon's setting)"},
			&cli.StringFlag{Name: "gateway", Usage: "model gateway: on | off (default: the daemon's setting)"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			src := cmd.Args().First()
			if src == "" {
				return errors.New("usage: fletcher deploy <dir-or-image-ref> [--host ...]")
			}

			name, port, err := deployImage(ctx, cmd, src)
			if err != nil {
				return err
			}
			if port < 1 || port > 65535 {
				return errors.New("could not determine the app's port from the image; pass --port")
			}

			client := newSessionsClient(cmd)
			if _, err := client.CreateSession(ctx, connect.NewRequest(&fletcherv1.CreateSessionRequest{
				Name:         name,
				Image:        name,
				EgressPolicy: cmd.String("egress"),
				Gateway:      cmd.String("gateway"),
				RunApp:       true,
			})); err != nil {
				return fmt.Errorf("create session %q (delete an existing one with `fletcher session delete %s`, or pass --name): %w", name, name, err)
			}

			// The session now exists because this deploy created it. If publishing
			// fails (e.g. a host conflict), roll it back so a failed deploy leaves
			// nothing behind rather than an unreachable orphan.
			resp, err := client.PublishPort(ctx, connect.NewRequest(&fletcherv1.PublishPortRequest{
				Ref:       name,
				GuestPort: uint32(port),
				Public:    cmd.String("host") != "",
				Host:      cmd.String("host"),
			}))
			if err != nil {
				if _, derr := client.DeleteSession(ctx, connect.NewRequest(&fletcherv1.DeleteSessionRequest{Ref: name})); derr != nil {
					fmt.Fprintf(os.Stderr, "warning: deploy failed and could not clean up session %q (delete it with `fletcher session delete %s`): %v\n", name, name, derr)
				}
				return fmt.Errorf("publish app port %d: %w", port, err)
			}

			fmt.Printf("\ndeployed %q (app on port %d)\n", name, port)
			return renderPublishedPort(ctx, os.Stdout, "table", resp.Msg.GetPort(), tunnelHostHint(cmd), resp.Msg.GetPublicIp())
		},
	}
}

// deployImage turns the deploy source into an imported template, returning the
// template name and the app's port. A registry ref is imported by the daemon
// (remote-capable); a local directory with a Dockerfile is built and imported on
// the host (needs root + docker).
func deployImage(ctx context.Context, cmd *cli.Command, src string) (name string, port int, err error) {
	if isDeployDir(src) {
		return deployFromDir(ctx, cmd, src)
	}
	return deployFromRef(ctx, cmd, src)
}

// deployFromRef imports a registry ref via the daemon (works from a remote
// client - the daemon pulls and flattens, no local docker).
func deployFromRef(ctx context.Context, cmd *cli.Command, ref string) (string, int, error) {
	user, pass := parseRegistryAuth(cmd.String("registry-auth"))
	fmt.Printf("importing %s (pulled by the daemon)...\n", ref)
	resp, err := newImageClient(cmd).Import(ctx, connect.NewRequest(&fletcherv1.ImportRequest{
		Ref:              ref,
		Name:             cmd.String("name"),
		RegistryUsername: user,
		RegistryPassword: pass,
		Force:            true,
	}))
	if err != nil {
		return "", 0, err
	}
	port := cmd.Int("port")
	if port == 0 {
		port = int(resp.Msg.GetExposedPort())
	}
	return resp.Msg.GetName(), port, nil
}

// deployFromDir builds a directory's Dockerfile and imports it locally (host-only
// - the build needs the working directory, and the local import needs root).
func deployFromDir(ctx context.Context, cmd *cli.Command, dir string) (string, int, error) {
	if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err != nil {
		return "", 0, fmt.Errorf("%s is a directory but has no Dockerfile", dir)
	}
	name := cmd.String("name")
	if name == "" {
		name = deployName(dir)
	}
	ref := "fletcher-deploy/" + name + ":latest"
	fmt.Printf("building %s from %s...\n", ref, dir)
	build := exec.CommandContext(ctx, "docker", "build", "-t", ref, dir) //nolint:gosec // operator-supplied build context, local admin command
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		return "", 0, fmt.Errorf("docker build %s: %w", dir, err)
	}
	root := cmd.String("btrfs-root")
	if root == "" {
		root = defaultDeployBtrfsRoot
	}
	fmt.Printf("importing %s as template %q...\n", ref, name)
	if err := importImageExt4(ctx, root, ref, name, true); err != nil {
		return "", 0, err
	}
	port := cmd.Int("port")
	if port == 0 {
		port = dockerExposedPort(ctx, ref)
	}
	return name, port, nil
}

// isDeployDir reports whether src is a local directory (so it is a build
// context, not a registry image reference).
func isDeployDir(src string) bool {
	fi, err := os.Stat(src)
	return err == nil && fi.IsDir()
}

// parseRegistryAuth splits "user:token" registry credentials.
func parseRegistryAuth(s string) (user, pass string) {
	if s == "" {
		return "", ""
	}
	u, p, ok := strings.Cut(s, ":")
	if !ok {
		return s, ""
	}
	return u, p
}

// dockerExposedPort returns the lowest TCP port the local image EXPOSEs, or 0.
func dockerExposedPort(ctx context.Context, ref string) int {
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{json .Config.ExposedPorts}}", ref).Output() //nolint:gosec // operator-supplied ref, local admin command
	if err != nil {
		return 0
	}
	var ports map[string]struct{}
	if err := json.Unmarshal(out, &ports); err != nil {
		return 0
	}
	best := 0
	for p := range ports { // e.g. "80/tcp"
		numStr, _, _ := strings.Cut(p, "/")
		n, err := strconv.Atoi(numStr)
		if err != nil || n < 1 || n > 65535 {
			continue
		}
		if best == 0 || n < best {
			best = n
		}
	}
	return best
}

// deployName derives a session/template name from a build directory's base name.
func deployName(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		return defaultImageName(filepath.Base(abs))
	}
	return defaultImageName(dir)
}
