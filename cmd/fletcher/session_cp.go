package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
)

// cpChunkSize is how many bytes each upload/download stream message carries.
const cpChunkSize = 256 << 10

func sessionCpCmd() *cli.Command {
	return &cli.Command{
		Name:      "cp",
		Usage:     "copy a file into or out of a session (like scp): one side is <ref>:<path>",
		ArgsUsage: "<src> <dst>",
		Description: "Copy a file between the local machine and a running session.\n" +
			"Exactly one of <src>/<dst> is remote, written as <ref>:<path> (a session\n" +
			"id or name, then a path in the guest; a relative path resolves under the\n" +
			"login user's home). Examples:\n" +
			"  fletcher session cp ./app.env mybox:/home/fletcher/app/.env\n" +
			"  fletcher session cp mybox:/var/log/fletcher-app.log ./app.log",
		Flags: []cli.Flag{
			socketFlag(),
			&cli.BoolFlag{Name: "recursive", Aliases: []string{"r"}, Usage: "for a remote-to-remote copy, copy a directory and its contents"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.Args().Len() != 2 {
				return errors.New("usage: fletcher session cp <src> <dst>")
			}
			src, dst := cmd.Args().Get(0), cmd.Args().Get(1)
			srcRef, srcPath, srcRemote := splitRemote(src)
			dstRef, dstPath, dstRemote := splitRemote(dst)

			switch {
			case srcRemote && dstRemote:
				if srcRef != dstRef {
					return errors.New("a remote-to-remote copy must stay within one session")
				}
				return fileOpCmd(ctx, cmd, srcRef, fletcherv1.FileOpKind_FILE_OP_KIND_COPY, srcPath, dstPath, cmd.Bool("recursive"))
			case !srcRemote && !dstRemote:
				return errors.New("neither side is remote; write the session side as <ref>:<path>")
			case !srcRemote && dstRemote:
				return uploadFile(ctx, cmd, src, dstRef, dstPath)
			default:
				return downloadFile(ctx, cmd, srcRef, srcPath, dst)
			}
		},
	}
}

func sessionRmCmd() *cli.Command {
	return &cli.Command{
		Name:      "rm",
		Usage:     "delete a file or directory in a running session",
		ArgsUsage: "<ref>:<path>",
		Flags: []cli.Flag{
			socketFlag(),
			&cli.BoolFlag{Name: "recursive", Aliases: []string{"r"}, Usage: "delete a directory and its contents"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ref, p, remote := splitRemote(cmd.Args().First())
			if !remote {
				return errors.New("usage: fletcher session rm <ref>:<path>")
			}
			return fileOpCmd(ctx, cmd, ref, fletcherv1.FileOpKind_FILE_OP_KIND_DELETE, p, "", cmd.Bool("recursive"))
		},
	}
}

func sessionMvCmd() *cli.Command {
	return &cli.Command{
		Name:      "mv",
		Usage:     "move or rename a file/directory in a running session",
		ArgsUsage: "<ref>:<src> <ref>:<dst>",
		Flags:     []cli.Flag{socketFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.Args().Len() != 2 {
				return errors.New("usage: fletcher session mv <ref>:<src> <ref>:<dst>")
			}
			srcRef, srcPath, srcRemote := splitRemote(cmd.Args().Get(0))
			dstRef, dstPath, dstRemote := splitRemote(cmd.Args().Get(1))
			if !srcRemote || !dstRemote {
				return errors.New("both <src> and <dst> must be remote (<ref>:<path>)")
			}
			if srcRef != dstRef {
				return errors.New("a move must stay within one session")
			}
			return fileOpCmd(ctx, cmd, srcRef, fletcherv1.FileOpKind_FILE_OP_KIND_MOVE, srcPath, dstPath, false)
		},
	}
}

// fileOpCmd runs a delete/move/copy and prints a short confirmation.
func fileOpCmd(ctx context.Context, cmd *cli.Command, ref string, op fletcherv1.FileOpKind, path, dest string, recursive bool) error {
	client := newSessionsClient(cmd)
	if _, err := client.FileOp(ctx, connect.NewRequest(&fletcherv1.FileOpRequest{
		Ref:       ref,
		Op:        op,
		Path:      path,
		Dest:      dest,
		Recursive: recursive,
	})); err != nil {
		return err
	}
	switch op {
	case fletcherv1.FileOpKind_FILE_OP_KIND_DELETE:
		fmt.Fprintf(os.Stdout, "deleted %s:%s\n", ref, path)
	case fletcherv1.FileOpKind_FILE_OP_KIND_MOVE:
		fmt.Fprintf(os.Stdout, "moved %s:%s -> %s:%s\n", ref, path, ref, dest)
	case fletcherv1.FileOpKind_FILE_OP_KIND_COPY:
		fmt.Fprintf(os.Stdout, "copied %s:%s -> %s:%s\n", ref, path, ref, dest)
	}
	return nil
}

func sessionLsCmd() *cli.Command {
	return &cli.Command{
		Name:      "ls",
		Usage:     "list a directory inside a running session (works on a shell-less image)",
		ArgsUsage: "<ref>[:<path>]",
		Description: "List a directory in a running session's filesystem - including a\n" +
			"mounted volume at /volume. Served by the guest in pure Go (no shell),\n" +
			"so it works even on a distroless image. Example:\n" +
			"  fletcher session ls wc-26-pundit:/volume",
		Flags: []cli.Flag{socketFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			arg := cmd.Args().First()
			if arg == "" {
				return errors.New("usage: fletcher session ls <ref>[:<path>]")
			}
			ref, dir, isRemote := splitRemote(arg)
			if !isRemote {
				// No colon: the whole arg is the ref, list the login user's home.
				ref, dir = arg, ""
			}
			client := newSessionsClient(cmd)
			resp, err := client.ListDir(ctx, connect.NewRequest(&fletcherv1.ListDirRequest{Ref: ref, Path: dir}))
			if err != nil {
				return err
			}
			msg := resp.Msg
			tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			entries := msg.GetEntries()
			sort.SliceStable(entries, func(i, j int) bool {
				if entries[i].GetIsDir() != entries[j].GetIsDir() {
					return entries[i].GetIsDir()
				}
				return entries[i].GetName() < entries[j].GetName()
			})
			for _, e := range entries {
				name := e.GetName()
				if e.GetIsDir() {
					name += "/"
				}
				if e.GetIsSymlink() && e.GetSymlinkTarget() != "" {
					name += " -> " + e.GetSymlinkTarget()
				}
				size := "-"
				if !e.GetIsDir() {
					size = humanBytes(e.GetSize())
				}
				modified := time.Unix(e.GetModifiedAt(), 0).Format("2006-01-02 15:04")
				fmt.Fprintf(tw, "%s\t%8s\t%s\t%s\n", modeString(e.GetMode(), e.GetIsDir(), e.GetIsSymlink()), size, modified, name)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if msg.GetTruncated() {
				fmt.Fprintf(os.Stdout, "... (listing truncated at %d entries)\n", len(entries))
			}
			return nil
		},
	}
}

// modeString renders unix permission bits as an ls-style string (e.g. drwxr-xr-x).
func modeString(mode uint32, isDir, isSymlink bool) string {
	var b strings.Builder
	switch {
	case isSymlink:
		b.WriteByte('l')
	case isDir:
		b.WriteByte('d')
	default:
		b.WriteByte('-')
	}
	const rwx = "rwxrwxrwx"
	for i := 0; i < 9; i++ {
		if mode&(1<<uint(8-i)) != 0 {
			b.WriteByte(rwx[i])
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// splitRemote parses a `<ref>:<path>` argument. It is remote when there is a
// colon whose left side is a non-empty session ref (no slash) - so a local path
// like ./x, /etc/x, or x is never mistaken for remote. Force a local file that
// contains a colon with a leading ./ (e.g. ./weird:name).
func splitRemote(arg string) (ref, p string, remote bool) {
	i := strings.IndexByte(arg, ':')
	if i <= 0 {
		return "", arg, false
	}
	head := arg[:i]
	if strings.ContainsAny(head, "/.") {
		return "", arg, false
	}
	return head, arg[i+1:], true
}

// uploadFile streams a local file into a running session. A remote path that is
// empty or ends in '/' takes the local file's base name.
func uploadFile(ctx context.Context, cmd *cli.Command, localPath, ref, remotePath string) error {
	f, err := os.Open(localPath) //nolint:gosec // localPath is the operator's chosen source file
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory (file transfer is one file at a time)", localPath)
	}
	if remotePath == "" || strings.HasSuffix(remotePath, "/") {
		remotePath += filepath.Base(localPath)
	}

	client := newSessionsClient(cmd)
	stream := client.UploadFile(ctx)
	if err := stream.Send(&fletcherv1.UploadFileRequest{
		Msg: &fletcherv1.UploadFileRequest_Start{Start: &fletcherv1.UploadStart{
			Ref:  ref,
			Path: remotePath,
			Mode: uint32(info.Mode().Perm()),
			Size: info.Size(),
		}},
	}); err != nil {
		return err
	}

	buf := make([]byte, cpChunkSize)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if serr := stream.Send(&fletcherv1.UploadFileRequest{
				Msg: &fletcherv1.UploadFileRequest_Chunk{Chunk: buf[:n]},
			}); serr != nil {
				return serr
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	resp, err := stream.CloseAndReceive()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "uploaded %s -> %s:%s (%d bytes, sha256 %s)\n",
		localPath, ref, remotePath, resp.Msg.GetBytesWritten(), resp.Msg.GetSha256())
	return nil
}

// downloadFile streams a session file to the local machine. A local dst that is
// an existing directory or ends in '/' takes the remote file's base name.
func downloadFile(ctx context.Context, cmd *cli.Command, ref, remotePath, localDst string) error {
	target := localDst
	if strings.HasSuffix(localDst, string(os.PathSeparator)) {
		target = filepath.Join(localDst, path.Base(remotePath))
	} else if fi, err := os.Stat(localDst); err == nil && fi.IsDir() {
		target = filepath.Join(localDst, path.Base(remotePath))
	}

	client := newSessionsClient(cmd)
	stream, err := client.DownloadFile(ctx, connect.NewRequest(&fletcherv1.DownloadFileRequest{
		Ref:  ref,
		Path: remotePath,
	}))
	if err != nil {
		return err
	}

	var out *os.File
	var written int64
	defer func() {
		if out != nil {
			_ = out.Close()
		}
	}()
	for stream.Receive() {
		msg := stream.Msg()
		if info := msg.GetInfo(); info != nil {
			mode := os.FileMode(info.GetMode()).Perm()
			if mode == 0 {
				mode = 0o644
			}
			out, err = os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode) //nolint:gosec // target is the operator's chosen destination
			if err != nil {
				return err
			}
			continue
		}
		if chunk := msg.GetChunk(); len(chunk) > 0 {
			if out == nil {
				return errors.New("download stream sent data before the file header")
			}
			n, werr := out.Write(chunk)
			if werr != nil {
				return werr
			}
			written += int64(n)
		}
	}
	if err := stream.Err(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "downloaded %s:%s -> %s (%d bytes)\n", ref, remotePath, target, written)
	return nil
}
