package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/urfave/cli/v3"
)

// imageCmd manages the btrfs rootfs templates the runc/btrfs runtime forks
// from. A job's --image names a template under <btrfs-root>/images/<name>;
// the snapshot driver CoW-snapshots it per run. Until the
// firecracker-containerd OCI pull pipeline lands (DESIGN.md §3/§13), these
// templates are produced by flattening a locally-built OCI image.
func imageCmd() *cli.Command {
	return &cli.Command{
		Name:  "image",
		Usage: "manage base-image rootfs templates for the runc/btrfs runtime",
		Commands: []*cli.Command{
			imageImportCmd(),
			imageListCmd(),
			imageRemoveCmd(),
		},
	}
}

// btrfsRootFlag mirrors the daemon's --btrfs-root so the CLI writes templates
// where the running daemon will look for them.
func btrfsRootFlag() cli.Flag {
	return &cli.StringFlag{
		Name:    "btrfs-root",
		Usage:   "btrfs snapshot root the daemon uses (must match the daemon's FLETCHER_BTRFS_ROOT)",
		Sources: cli.EnvVars("FLETCHER_BTRFS_ROOT"),
	}
}

func imageImportCmd() *cli.Command {
	return &cli.Command{
		Name:      "import",
		Usage:     "flatten a built OCI image into a btrfs rootfs template jobs can run in",
		ArgsUsage: "<docker-image-ref>",
		Description: `Exports a locally-built Docker image (e.g. fletcher-base:dev from
'make image') into a btrfs subvolume at <btrfs-root>/images/<name>, so
'fletcher job create --image <name>' has a real rootfs to run in.

Needs root (btrfs subvolume creation) and a working 'docker' on this
host, so run it with sudo and pass --btrfs-root explicitly (sudo does
not carry FLETCHER_BTRFS_ROOT through by default):

  sudo fletcher image import fletcher-base:dev \
    --btrfs-root /var/lib/fletcher/snapshots --name fletcher-base

This is the interim flatten step until the firecracker-containerd OCI
pull pipeline lands (DESIGN.md §3/§13).`,
		Flags: []cli.Flag{
			btrfsRootFlag(),
			&cli.StringFlag{
				Name:  "name",
				Usage: "template name jobs reference via --image (defaults to the image ref's repository name)",
			},
			&cli.BoolFlag{
				Name:  "force",
				Usage: "replace an existing template of the same name",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if runtime.GOOS != "linux" {
				return errors.New("image import is Linux-only (btrfs)")
			}
			ref := cmd.Args().First()
			if ref == "" {
				return errors.New("a docker image reference is required, e.g. fletcher-base:dev")
			}
			root := cmd.String("btrfs-root")
			if root == "" {
				return errors.New("set --btrfs-root (or FLETCHER_BTRFS_ROOT) to the daemon's snapshot root")
			}
			name := cmd.String("name")
			if name == "" {
				name = defaultImageName(ref)
			}
			return importImage(ctx, root, ref, name, cmd.Bool("force"))
		},
	}
}

func imageListCmd() *cli.Command {
	return &cli.Command{
		Name:  "ls",
		Usage: "list imported rootfs templates",
		Flags: []cli.Flag{btrfsRootFlag()},
		Action: func(_ context.Context, cmd *cli.Command) error {
			root := cmd.String("btrfs-root")
			if root == "" {
				return errors.New("set --btrfs-root (or FLETCHER_BTRFS_ROOT) to the daemon's snapshot root")
			}
			entries, err := os.ReadDir(filepath.Join(root, "images"))
			if errors.Is(err, fs.ErrNotExist) {
				fmt.Println("no images imported yet")
				return nil
			}
			if err != nil {
				return fmt.Errorf("read images dir: %w", err)
			}
			found := false
			for _, e := range entries {
				if e.IsDir() {
					fmt.Println(e.Name())
					found = true
				}
			}
			if !found {
				fmt.Println("no images imported yet")
			}
			return nil
		},
	}
}

func imageRemoveCmd() *cli.Command {
	return &cli.Command{
		Name:      "rm",
		Usage:     "remove an imported rootfs template",
		ArgsUsage: "<name>",
		Flags:     []cli.Flag{btrfsRootFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if runtime.GOOS != "linux" {
				return errors.New("image rm is Linux-only (btrfs)")
			}
			name := cmd.Args().First()
			if name == "" {
				return errors.New("an image name is required")
			}
			root := cmd.String("btrfs-root")
			if root == "" {
				return errors.New("set --btrfs-root (or FLETCHER_BTRFS_ROOT) to the daemon's snapshot root")
			}
			target := filepath.Join(root, "images", name)
			if _, err := os.Stat(target); errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("no such image %q", name)
			}
			if err := runBtrfs(ctx, "subvolume", "delete", target); err != nil {
				return fmt.Errorf("delete subvolume: %w", err)
			}
			fmt.Printf("removed %s\n", name)
			return nil
		},
	}
}

// importImage creates the template subvolume and fills it with the image's
// flattened root filesystem. A failure after the subvolume is created tears
// it back down so a retry starts clean.
func importImage(ctx context.Context, root, ref, name string, force bool) error {
	if err := requireTools("btrfs", "docker", "tar"); err != nil {
		return err
	}
	imagesDir := filepath.Join(root, "images")
	// 0755 so the daemon, which runs as a different user (fletcher), can
	// traverse this directory to read and snapshot the templates within.
	if err := os.MkdirAll(imagesDir, 0o755); err != nil { //nolint:gosec // see comment: cross-user traversal of non-secret base images
		return fmt.Errorf("create images dir: %w", err)
	}
	target := filepath.Join(imagesDir, name)
	if _, err := os.Stat(target); err == nil {
		if !force {
			return fmt.Errorf("template %q already exists at %s (use --force to replace)", name, target)
		}
		if err := runBtrfs(ctx, "subvolume", "delete", target); err != nil {
			return fmt.Errorf("remove existing template: %w", err)
		}
	}
	if err := runBtrfs(ctx, "subvolume", "create", target); err != nil {
		return fmt.Errorf("create subvolume: %w", err)
	}
	if err := exportDockerRootfs(ctx, ref, target); err != nil {
		// Best-effort cleanup of the half-built template; use a fresh context
		// so it still runs when the failure was ctx cancellation.
		_ = runBtrfs(context.Background(), "subvolume", "delete", target) //nolint:contextcheck // cleanup must run even when ctx is cancelled
		return err
	}
	fmt.Printf("imported %s into %s\n", ref, target)
	fmt.Printf("run it with: fletcher job create --image %s --command \"...\"\n", name)
	return nil
}

// exportDockerRootfs pipes `docker export` of a throwaway container created
// from ref into `tar -x` under dest, materialising the flattened rootfs.
func exportDockerRootfs(ctx context.Context, ref, dest string) error {
	create := exec.CommandContext(ctx, "docker", "create", ref) //nolint:gosec // operator-supplied image ref, local admin command
	create.Stderr = os.Stderr
	out, err := create.Output()
	if err != nil {
		return fmt.Errorf("docker create %s: %w", ref, err)
	}
	id := strings.TrimSpace(string(out))
	defer func() { //nolint:contextcheck // cleanup must run even when the export's ctx is cancelled
		// Remove the throwaway container; a fresh context so cleanup runs
		// even if the export was cancelled.
		_ = exec.CommandContext(context.Background(), "docker", "rm", "-f", id).Run() //nolint:gosec // id is docker's own output
	}()

	// Pipe `docker export` into `tar -x` over an explicit os.Pipe. Do NOT use
	// cmd.StdoutPipe(): its Wait closes the pipe, and calling export.Wait()
	// before tar has drained the stream truncates the archive - silently
	// dropping the tail (the agents' install dirs under ~/.local) while tar
	// still exits 0. With an os.Pipe we close our own ends and let tar read to
	// a real EOF (when export exits), so the whole filesystem is extracted.
	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("pipe: %w", err)
	}

	export := exec.CommandContext(ctx, "docker", "export", id) //nolint:gosec // id is docker's own output
	export.Stdout = pw
	export.Stderr = os.Stderr
	extract := exec.CommandContext(ctx, "tar", "-x", "-C", dest) //nolint:gosec // dest is the operator's btrfs root
	extract.Stdin = pr
	extract.Stderr = os.Stderr

	if err := export.Start(); err != nil {
		_ = pw.Close()
		_ = pr.Close()
		return fmt.Errorf("start docker export: %w", err)
	}
	// Our copy of the write end is no longer needed; the export child holds
	// its own, so tar sees EOF only when export actually exits.
	_ = pw.Close()
	if err := extract.Start(); err != nil {
		_ = pr.Close()
		return fmt.Errorf("start tar: %w", err)
	}
	_ = pr.Close()

	errExport := export.Wait()
	errExtract := extract.Wait()
	if errExport != nil {
		return fmt.Errorf("docker export: %w", errExport)
	}
	if errExtract != nil {
		return fmt.Errorf("extract rootfs: %w", errExtract)
	}
	return nil
}

// defaultImageName derives a template name from a docker ref: the repository
// basename without the tag or digest. "registry/foo/bar:tag" -> "bar".
func defaultImageName(ref string) string {
	name := ref
	if i := strings.LastIndex(name, "@"); i >= 0 {
		name = name[:i]
	}
	if i := strings.LastIndex(name, ":"); i >= 0 {
		name = name[:i]
	}
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// requireTools fails early if any external binary the import needs is absent.
func requireTools(names ...string) error {
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			return fmt.Errorf("%q not found in PATH (required for image import): %w", n, err)
		}
	}
	return nil
}

// runBtrfs runs a btrfs subcommand with its output forwarded to stderr,
// keeping our own stdout clean for the import's own status lines.
func runBtrfs(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "btrfs", args...) //nolint:gosec // operator-supplied subvolume paths, local admin command
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
