package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

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
		Flags: []cli.Flag{socketFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.Args().Len() != 2 {
				return errors.New("usage: fletcher session cp <src> <dst>")
			}
			src, dst := cmd.Args().Get(0), cmd.Args().Get(1)
			srcRef, srcPath, srcRemote := splitRemote(src)
			dstRef, dstPath, dstRemote := splitRemote(dst)

			switch {
			case srcRemote && dstRemote:
				return errors.New("both sides are remote; one of <src>/<dst> must be a local path")
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
