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
// `deploy` works with no --btrfs-root on a standard install.
const defaultDeployBtrfsRoot = "/var/lib/fletcher/snapshots"

func deployCmd() *cli.Command {
	return &cli.Command{
		Name:      "deploy",
		Usage:     "build (or pull) a Docker image and run it as a published app session",
		ArgsUsage: "<dir-or-image-ref>",
		Description: `One command from a Dockerfile (or a built image) to a running app:

  1. if the argument is a directory with a Dockerfile, 'docker build' it;
     otherwise treat it as a Docker image reference (pulled if needed)
  2. flatten it into a rootfs template ('image import')
  3. create a session that runs the image's own app on boot ('--app')
  4. publish the app's port - over the tunnel, or publicly with --host

Runs the build/import locally, so it needs root and docker (like 'image
import'); the session/publish steps go to the local daemon. Example:

  sudo fletcher deploy ./myapp --host app.example.com

The port defaults to the image's EXPOSE; set --port if the image declares none.`,
		Flags: []cli.Flag{
			socketFlag(),
			btrfsRootFlag(),
			&cli.StringFlag{Name: "name", Usage: "session + template name (default: derived from the image/dir)"},
			&cli.StringFlag{Name: "host", Usage: "public hostname to serve at over HTTPS, e.g. app.example.com (omit for tunnel-only)"},
			&cli.IntFlag{Name: "port", Usage: "container port to publish (default: the image's EXPOSE)"},
			&cli.StringFlag{Name: "egress", Usage: "fork network egress: none | allowlist | open (default: the daemon's setting)"},
			&cli.StringFlag{Name: "gateway", Usage: "model gateway: on | off (default: the daemon's setting)"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			src := cmd.Args().First()
			if src == "" {
				return errors.New("usage: fletcher deploy <dir-or-image-ref> [--host ...]")
			}
			name := cmd.String("name")
			if name == "" {
				name = deployName(src)
			}
			root := cmd.String("btrfs-root")
			if root == "" {
				root = defaultDeployBtrfsRoot
			}

			ref, err := deploySource(ctx, src, name)
			if err != nil {
				return err
			}

			port := cmd.Int("port")
			if port == 0 {
				port = dockerExposedPort(ctx, ref)
			}
			if port < 1 || port > 65535 {
				return errors.New("could not determine the app's port from the image; pass --port")
			}

			fmt.Printf("importing %s as template %q...\n", ref, name)
			if err := importImageExt4(ctx, root, ref, name, true); err != nil {
				return err
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

			resp, err := client.PublishPort(ctx, connect.NewRequest(&fletcherv1.PublishPortRequest{
				Ref:       name,
				GuestPort: uint32(port),
				Public:    cmd.String("host") != "",
				Host:      cmd.String("host"),
			}))
			if err != nil {
				return fmt.Errorf("publish app port %d: %w", port, err)
			}

			fmt.Printf("\ndeployed %q (app on port %d)\n", name, port)
			return renderPublishedPort(ctx, os.Stdout, "table", resp.Msg.GetPort(), tunnelHostHint(cmd), resp.Msg.GetPublicIp())
		},
	}
}

// deploySource resolves the deploy argument to a Docker image reference: a
// directory with a Dockerfile is built; anything else is treated as an image ref
// (which `image import` pulls if it is not present locally).
func deploySource(ctx context.Context, src, name string) (string, error) {
	fi, statErr := os.Stat(src)
	isDir := statErr == nil && fi.IsDir()
	if !isDir {
		return src, nil // not a local directory -> treat as a Docker image reference
	}
	if _, err := os.Stat(filepath.Join(src, "Dockerfile")); err != nil {
		return "", fmt.Errorf("%s is a directory but has no Dockerfile", src)
	}
	ref := "fletcher-deploy/" + name + ":latest"
	fmt.Printf("building %s from %s...\n", ref, src)
	build := exec.CommandContext(ctx, "docker", "build", "-t", ref, src) //nolint:gosec // operator-supplied build context, local admin command
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		return "", fmt.Errorf("docker build %s: %w", src, err)
	}
	return ref, nil
}

// dockerExposedPort returns the lowest TCP port the image EXPOSEs, or 0 if it
// declares none (so the caller asks for --port).
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

// deployName derives a session/template name from the deploy source: the
// directory's base name, or the image ref's repository.
func deployName(src string) string {
	if fi, err := os.Stat(src); err == nil && fi.IsDir() {
		abs, aerr := filepath.Abs(src)
		if aerr == nil {
			return defaultImageName(filepath.Base(abs))
		}
	}
	return defaultImageName(src)
}
