//go:build linux

// Command fletcher-guest is the init process inside a Firecracker microVM. It
// is injected into the rootfs as /sbin/fletcher-init and booted via the kernel
// init= argument. It mounts the basic pseudo-filesystems, dials the daemon over
// vsock, receives the job spec, runs the command while streaming its output
// back, reports the exit code, and powers the VM off.
//
// It is deliberately tiny and dependency-light: it is PID 1, so anything it
// pulls in ships in every rootfs and runs as init.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"

	"github.com/joshjon/fletcher/internal/appspec"
	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver/guestproto"
)

// guestWorkspace is the conventional working directory inside a session VM.
const guestWorkspace = "/workspace"

func main() {
	mountBasics()
	setupLoopback()
	if sessionMode() {
		// A durable session: stay up and serve host-initiated exec/shutdown
		// requests until a shutdown request resets the VM.
		serve()
		shutdown()
		os.Exit(0)
	}
	code := run()
	// PID 1 must not simply return (the kernel panics). Trigger a reboot so the
	// VMM exits and the host's Wait returns. Firecracker has no ACPI, so a
	// power-off would hang; with reboot=k the kernel does a keyboard-controller
	// reset, which Firecracker intercepts and exits on.
	shutdown()
	os.Exit(code) // unreachable once shutdown succeeds
}

// sessionMode reports whether the kernel was booted for a durable session
// (the daemon adds fletcher.session=1 to the cmdline for session VMs).
func sessionMode() bool {
	return cmdlineHas("fletcher.session=1")
}

// appMode reports whether the daemon asked the guest to run the image's own app
// on boot (it adds fletcher.app=1 to the cmdline for a session created --app).
func appMode() bool {
	return cmdlineHas("fletcher.app=1")
}

func cmdlineHas(flag string) bool {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return false
	}
	return strings.Contains(string(data), flag)
}

// appLogPath is where the image app's stdout/stderr are captured inside the VM,
// so a session can be shelled into to read them.
const appLogPath = "/var/log/fletcher-app.log"

// startApp launches the image's own app (captured at import into appspec.Path)
// under a supervisor that restarts it if it exits, logging to appLogPath. The
// control server keeps running alongside it so the session is still shell-able.
// appEnv is the user-set session env vars, layered onto the image's own app env.
func startApp(appEnv []string) {
	// PID 1 boots with an empty environment, so a relative entrypoint (e.g.
	// "cat") would never resolve: exec.Command looks argv[0] up in the parent
	// process's PATH, not the child env. Give init the standard PATH once.
	if os.Getenv("PATH") == "" {
		_ = os.Setenv("PATH", defaultGuestPath)
	}
	spec, err := appspec.Read(appspec.Path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fletcher-guest: app mode on but no app spec (%v); nothing to run\n", err)
		return
	}
	if len(spec.Argv()) == 0 {
		fmt.Fprintln(os.Stderr, "fletcher-guest: app spec has no command")
		return
	}
	go superviseApp(spec, appEnv)
}

// appRestartBackoff is the minimum gap between app restarts, so a crash-looping
// app does not spin the CPU (matches the sshd relaunch backoff).
const appRestartBackoff = time.Second

// appRestarts counts how many times superviseApp has restarted the app after a
// death (the initial start is 0). Reported in Stat so the host can surface a
// deploy's restart count. Resets when the VM boots.
var appRestarts atomic.Int64

// superviseApp runs the app and restarts it whenever it exits (a deployed server
// should stay up). Output for the whole boot goes to one log file; image env and
// working dir are applied, and the image USER if set (else root, what most app
// images expect). appEnv (the user-set session env vars) is layered on top of
// the image's own env, so a user var replaces the image's value for that key.
func superviseApp(spec appspec.Spec, appEnv []string) {
	argv := spec.Argv()
	workDir := spec.WorkingDir
	if workDir == "" {
		workDir = "/"
	}
	env := withDefaults(mergeEnv(spec.Env, appEnv), loginUser{})

	_ = os.MkdirAll("/var/log", 0o755) //nolint:gosec // standard log dir perms inside the VM
	logf, err := os.OpenFile(appLogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fletcher-guest: open app log: %v\n", err)
		return
	}
	defer func() { _ = logf.Close() }()

	for {
		cmd := exec.CommandContext(context.Background(), argv[0], argv[1:]...) //nolint:gosec // running the image's own app is the entire purpose of app mode
		cmd.Dir = workDir
		cmd.Env = env
		cmd.Stdout = logf
		cmd.Stderr = logf
		applyAppUser(cmd, spec.User)

		started := time.Now()
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "fletcher-guest: start app %v: %v\n", argv, err)
			time.Sleep(appRestartBackoff)
			continue
		}
		fmt.Fprintf(os.Stderr, "fletcher-guest: started app pid %d: %v\n", cmd.Process.Pid, argv)
		_ = cmd.Wait()
		appRestarts.Add(1)
		fmt.Fprintf(os.Stderr, "fletcher-guest: app exited; restarting\n")
		// Pace restarts only when the app dies almost immediately (crash loop); a
		// long-lived app that just died restarts without delay.
		if elapsed := time.Since(started); elapsed < appRestartBackoff {
			time.Sleep(appRestartBackoff - elapsed)
		}
	}
}

// applyAppUser runs the app as the image's USER when set and resolvable; an
// empty/root user (or one that cannot be resolved) runs as root, which is what
// most app images expect.
func applyAppUser(cmd *exec.Cmd, u string) {
	u = strings.TrimSpace(u)
	if u == "" || u == "root" || u == "0" {
		return
	}
	uid, gid, ok := resolveUserGroup(u)
	if !ok {
		fmt.Fprintf(os.Stderr, "fletcher-guest: image USER %q unresolved; running app as root\n", u)
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uid, Gid: gid}
}

// resolveUserGroup parses an image USER ("name", "uid", "name:group", or
// "uid:gid") to numeric ids using the rootfs's passwd/group databases.
func resolveUserGroup(spec string) (uid, gid uint32, ok bool) {
	userPart, groupPart, hasGroup := strings.Cut(spec, ":")
	u, err := lookupUser(userPart)
	if err != nil {
		return 0, 0, false
	}
	uid = u.uid
	gid = u.gid
	if hasGroup {
		g, gerr := lookupGroupID(groupPart)
		if gerr != nil {
			return 0, 0, false
		}
		gid = g
	}
	return uid, gid, true
}

type resolvedUser struct{ uid, gid uint32 }

// lookupUser resolves a username or numeric uid to uid/gid.
func lookupUser(name string) (resolvedUser, error) {
	if n, err := strconv.Atoi(name); err == nil && n >= 0 && n <= math.MaxUint32 {
		// Numeric uid: try to find its primary gid, else mirror the uid.
		if u, lerr := user.LookupId(name); lerr == nil {
			if gid, gerr := strconv.Atoi(u.Gid); gerr == nil {
				return resolvedUser{uid: uint32(n), gid: uint32(gid)}, nil //nolint:gosec // bounded above
			}
		}
		return resolvedUser{uid: uint32(n), gid: uint32(n)}, nil
	}
	u, err := user.Lookup(name)
	if err != nil {
		return resolvedUser{}, err
	}
	uid, uerr := strconv.Atoi(u.Uid)
	gid, gerr := strconv.Atoi(u.Gid)
	if uerr != nil || gerr != nil || uid < 0 || gid < 0 || uid > math.MaxUint32 || gid > math.MaxUint32 {
		return resolvedUser{}, fmt.Errorf("bad uid/gid for %q", name)
	}
	return resolvedUser{uid: uint32(uid), gid: uint32(gid)}, nil
}

// lookupGroupID resolves a group name or numeric gid.
func lookupGroupID(name string) (uint32, error) {
	if n, err := strconv.Atoi(name); err == nil && n >= 0 && n <= math.MaxUint32 {
		return uint32(n), nil
	}
	g, err := user.LookupGroup(name)
	if err != nil {
		return 0, err
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil || gid < 0 || gid > math.MaxUint32 {
		return 0, fmt.Errorf("bad gid for %q", name)
	}
	return uint32(gid), nil
}

// serve is the session control loop: listen on vsock and serve each
// host-initiated connection. It returns only if the listener fails.
func serve() {
	// Bring up sshd (brokered SSH / IDE attach) and its vsock relay before
	// accepting control connections; both are best-effort so a session is still
	// usable via exec/shell if sshd cannot start.
	startSSHD()
	go serveSSHRelay()
	go servePortRelay()
	// The run_app session's app is launched from applySetup (not here), so it
	// starts with the user env vars the host delivers in that same setup.

	ln, err := vsock.Listen(guestproto.ControlPort, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fletcher-guest: listen control: %v\n", err)
		return
	}
	defer func() { _ = ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go serveControl(conn)
	}
}

// startSSHD generates host keys on first boot (the image ships none) and runs
// sshd in the foreground, relaunching it if it exits. sshd listens on loopback;
// serveSSHRelay bridges the daemon's vsock connections to it.
func startSSHD() {
	if _, err := os.Stat("/usr/sbin/sshd"); err != nil {
		return // image without an SSH server; brokered SSH is simply unavailable
	}
	// ssh-keygen -A creates any missing host keys under /etc/ssh.
	if out, err := exec.CommandContext(context.Background(), "/usr/bin/ssh-keygen", "-A").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "fletcher-guest: ssh-keygen: %v: %s\n", err, out)
	}
	_ = os.MkdirAll("/run/sshd", 0o755) //nolint:gosec // sshd privilege-separation dir, standard perms
	go func() {
		for {
			cmd := exec.CommandContext(context.Background(), "/usr/sbin/sshd", "-D", "-e")
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "fletcher-guest: sshd exited: %v\n", err)
			}
			time.Sleep(time.Second) // avoid a hot loop if sshd cannot start
		}
	}()
}

// serveSSHRelay accepts the daemon's vsock connections on SSHPort and splices
// each to the loopback sshd, so SSH reaches the VM without it having a NIC.
func serveSSHRelay() {
	ln, err := vsock.Listen(guestproto.SSHPort, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fletcher-guest: listen ssh relay: %v\n", err)
		return
	}
	defer func() { _ = ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go relayToSSHD(conn)
	}
}

func relayToSSHD(conn net.Conn) {
	// sshd may still be coming up just after a cold boot; retry briefly so the
	// first connection after wake does not fail the race.
	var upstream net.Conn
	for range 50 {
		c, err := net.Dial("tcp", "127.0.0.1:22") //nolint:noctx // lifetime is the spliced connection
		if err == nil {
			upstream = c
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if upstream == nil {
		_ = conn.Close()
		return
	}
	splice(conn, upstream)
}

// servePortRelay accepts the daemon's vsock connections on PortForwardPort,
// reads the 2-byte target loopback port, and splices each to that port inside
// the VM. It is the generic form of serveSSHRelay (which is fixed to sshd):
// the daemon uses it to broker a published session port (a preview port)
// without the VM having any network route.
func servePortRelay() {
	ln, err := vsock.Listen(guestproto.PortForwardPort, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fletcher-guest: listen port relay: %v\n", err)
		return
	}
	defer func() { _ = ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go relayToPort(conn)
	}
}

// relayToPort reads the target port header from conn, dials that loopback port
// inside the VM, and splices the two. The published service may still be coming
// up just after a wake, so it retries the dial briefly (as relayToSSHD does).
func relayToPort(conn net.Conn) {
	port, err := guestproto.ReadDialPort(conn)
	if err != nil {
		_ = conn.Close()
		return
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	var upstream net.Conn
	for range 50 {
		c, derr := net.Dial("tcp", addr) //nolint:noctx // lifetime is the spliced connection
		if derr == nil {
			upstream = c
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if upstream == nil {
		_ = conn.Close()
		return
	}
	splice(conn, upstream)
}

// serveControl handles one host control connection: run a command, or shut down.
func serveControl(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	req, err := guestproto.ReadRequest(conn)
	if err != nil {
		// A clean disconnect (e.g. the host's readiness probe) is not an error.
		if !errors.Is(err, io.EOF) {
			fmt.Fprintf(os.Stderr, "fletcher-guest: read request: %v\n", err)
		}
		return
	}
	switch req.Kind {
	case guestproto.RequestSetup:
		applySetup(conn, req.Spec)
	case guestproto.RequestExec:
		runExec(conn, req.Spec)
	case guestproto.RequestShell:
		runShell(conn, req.Shell)
	case guestproto.RequestStat:
		if err := guestproto.WriteStat(conn, guestproto.Stat{Load1: loadAvg1(), AppRestarts: appRestarts.Load()}); err != nil {
			fmt.Fprintf(os.Stderr, "fletcher-guest: write stat: %v\n", err)
		}
	case guestproto.RequestWriteFile:
		writeUpload(conn, req.File)
	case guestproto.RequestReadFile:
		readDownload(conn, req.File)
	case guestproto.RequestListDir:
		listDir(conn, req.File)
	case guestproto.RequestFileOp:
		fileOp(conn, req.FileOp)
	case guestproto.RequestShutdown:
		shutdown() // resets the VM; does not return
	default:
		fmt.Fprintf(os.Stderr, "fletcher-guest: unknown request kind %q\n", req.Kind)
	}
}

// resolveGuestPath makes a transfer path absolute: an absolute path is used
// as-is; a relative one resolves under the login user's home (where the agent
// lives), or /root when there is no login user.
func resolveGuestPath(p string, lu loginUser) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	home := "/root"
	if lu.ok {
		home = lu.home
	}
	return filepath.Join(home, p)
}

// writeUpload handles a RequestWriteFile: ack readiness, stream spec.Size bytes
// into a temp file in the destination directory, atomically rename it into
// place, and hand it to the login user. The two-phase ack (FileResult before the
// bytes) lets the host abort cleanly on a bad path without streaming the upload.
func writeUpload(conn net.Conn, spec guestproto.FileSpec) {
	lu := lookupLoginUser()
	dest := resolveGuestPath(spec.Path, lu)
	dir := filepath.Dir(dest)

	tmp, err := func() (*os.File, error) {
		// Check the destination up front (before streaming): a directory is never
		// replaced, and an existing file only when overwrite is set.
		if fi, lerr := os.Lstat(dest); lerr == nil {
			if fi.IsDir() {
				return nil, fmt.Errorf("a directory named %q already exists", filepath.Base(dest))
			}
			if !spec.Overwrite {
				return nil, fmt.Errorf("a file named %q already exists", filepath.Base(dest))
			}
		}
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // a writable workspace dir inside the fork
			return nil, err
		}
		return os.CreateTemp(dir, ".fletcher-upload-*")
	}()
	if err != nil {
		_ = guestproto.WriteFileResult(conn, guestproto.FileResult{Error: err.Error()})
		return
	}
	tmpName := tmp.Name()
	// From here a failure must clean up the temp file.
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpName) }

	// Ack readiness so the host starts streaming.
	if err := guestproto.WriteFileResult(conn, guestproto.FileResult{}); err != nil {
		cleanup()
		return
	}

	hash := sha256.New()
	n, copyErr := io.CopyN(io.MultiWriter(tmp, hash), conn, spec.Size)
	if copyErr != nil {
		cleanup()
		_ = guestproto.WriteFileResult(conn, guestproto.FileResult{Error: fmt.Sprintf("receive upload: %v", copyErr)})
		return
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		_ = guestproto.WriteFileResult(conn, guestproto.FileResult{Error: fmt.Sprintf("flush upload: %v", err)})
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		_ = guestproto.WriteFileResult(conn, guestproto.FileResult{Error: fmt.Sprintf("close upload: %v", err)})
		return
	}

	mode := os.FileMode(spec.Mode)
	if mode == 0 {
		mode = 0o644
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		_ = os.Remove(tmpName)
		_ = guestproto.WriteFileResult(conn, guestproto.FileResult{Error: fmt.Sprintf("set mode: %v", err)})
		return
	}
	if lu.ok {
		_ = os.Chown(tmpName, int(lu.uid), int(lu.gid))
	}
	if err := os.Rename(tmpName, dest); err != nil {
		_ = os.Remove(tmpName)
		_ = guestproto.WriteFileResult(conn, guestproto.FileResult{Error: fmt.Sprintf("install upload: %v", err)})
		return
	}
	_ = guestproto.WriteFileResult(conn, guestproto.FileResult{
		BytesWritten: n,
		Sha256:       hex.EncodeToString(hash.Sum(nil)),
	})
}

// readDownload handles a RequestReadFile: reply with the file's size and mode,
// then stream its bytes. A missing file or a directory is reported in the
// FileResult error rather than streamed.
func readDownload(conn net.Conn, spec guestproto.FileSpec) {
	lu := lookupLoginUser()
	src := resolveGuestPath(spec.Path, lu)

	f, err := os.Open(src) //nolint:gosec // src is an operator-driven path inside the fork (the sandbox)
	if err != nil {
		_ = guestproto.WriteFileResult(conn, guestproto.FileResult{Error: err.Error()})
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		_ = guestproto.WriteFileResult(conn, guestproto.FileResult{Error: err.Error()})
		return
	}
	if info.IsDir() {
		_ = guestproto.WriteFileResult(conn, guestproto.FileResult{Error: fmt.Sprintf("%s is a directory", src)})
		return
	}
	if err := guestproto.WriteFileResult(conn, guestproto.FileResult{
		Size: info.Size(),
		Mode: uint32(info.Mode().Perm()),
	}); err != nil {
		return
	}
	if _, err := io.CopyN(conn, f, info.Size()); err != nil {
		fmt.Fprintf(os.Stderr, "fletcher-guest: stream %s: %v\n", src, err)
	}
}

// fileOp handles a RequestFileOp: a delete, move, or copy in the fork, in pure
// Go (so it works on an image with no shell). It replies with a FileResult whose
// Error is set on failure.
func fileOp(conn net.Conn, spec guestproto.FileOpSpec) {
	lu := lookupLoginUser()
	src := resolveGuestPath(spec.Path, lu)

	var err error
	switch spec.Op {
	case guestproto.FileOpDelete:
		err = deletePath(src, spec.Recursive)
	case guestproto.FileOpMove:
		err = movePath(src, destPath(resolveGuestPath(spec.Dest, lu), src), lu)
	case guestproto.FileOpCopy:
		err = copyPath(src, destPath(resolveGuestPath(spec.Dest, lu), src), spec.Recursive, lu)
	default:
		err = fmt.Errorf("unknown file operation %q", spec.Op)
	}

	res := guestproto.FileResult{}
	if err != nil {
		res.Error = err.Error()
	}
	_ = guestproto.WriteFileResult(conn, res)
}

// destPath resolves a move/copy destination: when dst is an existing directory,
// the source's base name is placed inside it (mirroring `mv`/`cp`).
func destPath(dst, src string) string {
	if fi, err := os.Stat(dst); err == nil && fi.IsDir() {
		return filepath.Join(dst, filepath.Base(src))
	}
	return dst
}

// deletePath removes a file or directory. A directory needs recursive; without
// it, os.Remove refuses a non-empty directory. Refuses "/" as a guard.
func deletePath(path string, recursive bool) error {
	if path == "/" || path == "" {
		return fmt.Errorf("refusing to delete %q", path)
	}
	if recursive {
		return os.RemoveAll(path)
	}
	return os.Remove(path)
}

// movePath renames src to dst, falling back to copy-then-delete across mounts
// (os.Rename returns EXDEV when src and dst are on different filesystems, e.g. a
// volume at /volume vs the root fork).
func movePath(src, dst string, lu loginUser) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := copyPath(src, dst, true, lu); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

// copyPath copies a file or (with recursive) a directory tree from src to dst,
// owning the result as the login user.
func copyPath(src, dst string, recursive bool, lu loginUser) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if !recursive {
			return fmt.Errorf("%s is a directory (use recursive)", src)
		}
		return copyTree(src, dst, lu)
	}
	return copyFile(src, dst, info.Mode().Perm(), lu)
}

// copyFile copies one regular file's contents and mode, then hands it to the
// login user.
func copyFile(src, dst string, mode os.FileMode, lu loginUser) error {
	in, err := os.Open(src) //nolint:gosec // src is an operator-driven path inside the fork
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { //nolint:gosec // a dir inside the fork
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode) //nolint:gosec // dst is an operator-driven path inside the fork
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if lu.ok {
		_ = os.Chown(dst, int(lu.uid), int(lu.gid))
	}
	return nil
}

// copyTree recursively copies the directory at src to dst.
func copyTree(src, dst string, lu loginUser) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			info, ierr := d.Info()
			mode := os.FileMode(0o755)
			if ierr == nil {
				mode = info.Mode().Perm()
			}
			if mkerr := os.MkdirAll(target, mode); mkerr != nil {
				return mkerr
			}
			if lu.ok {
				_ = os.Chown(target, int(lu.uid), int(lu.gid))
			}
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		// Skip non-regular files (sockets, devices); copy regular files.
		if !info.Mode().IsRegular() {
			return nil
		}
		return copyFile(p, target, info.Mode().Perm(), lu)
	})
}

// maxDirEntries caps a single directory listing so a pathological directory
// cannot make the reply unbounded. The listing reports Truncated when it hits it.
const maxDirEntries = 10000

// listDir handles a RequestListDir: read the directory in pure Go (no shell, so
// it works on an image with no /bin/sh) and reply with the entries, directories
// first then by name. A missing path or a non-directory is reported in the
// listing's error.
func listDir(conn net.Conn, spec guestproto.FileSpec) {
	lu := lookupLoginUser()
	dir := resolveGuestPath(spec.Path, lu)

	info, err := os.Stat(dir)
	if err != nil {
		_ = guestproto.WriteDirListing(conn, guestproto.DirListing{Error: err.Error()})
		return
	}
	if !info.IsDir() {
		_ = guestproto.WriteDirListing(conn, guestproto.DirListing{Error: fmt.Sprintf("%s is not a directory", dir)})
		return
	}
	raw, err := os.ReadDir(dir)
	if err != nil {
		_ = guestproto.WriteDirListing(conn, guestproto.DirListing{Path: dir, Error: err.Error()})
		return
	}
	truncated := false
	if len(raw) > maxDirEntries {
		raw = raw[:maxDirEntries]
		truncated = true
	}

	entries := make([]guestproto.DirEntry, 0, len(raw))
	for _, e := range raw {
		full := filepath.Join(dir, e.Name())
		de := guestproto.DirEntry{Name: e.Name(), IsDir: e.IsDir()}
		if fi, ierr := e.Info(); ierr == nil {
			de.Size = fi.Size()
			de.Mode = uint32(fi.Mode().Perm())
			de.ModTime = fi.ModTime().Unix()
			if fi.Mode()&os.ModeSymlink != 0 {
				de.IsSymlink = true
				if target, lerr := os.Readlink(full); lerr == nil {
					de.SymlinkTarget = target
				}
				// Resolve through the link so the client can descend a dir symlink.
				if ti, terr := os.Stat(full); terr == nil {
					de.IsDir = ti.IsDir()
					if !ti.IsDir() {
						de.Size = ti.Size()
					}
				}
			}
		}
		entries = append(entries, de)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir // directories first
		}
		return entries[i].Name < entries[j].Name
	})

	if err := guestproto.WriteDirListing(conn, guestproto.DirListing{
		Path:      dir,
		Entries:   entries,
		Truncated: truncated,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "fletcher-guest: write listing %s: %v\n", dir, err)
	}
}

// loadAvg1 reads the 1-minute load average from /proc/loadavg; 0 if unreadable.
func loadAvg1() float64 {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return v
}

// runShell opens a PTY running the durable session shell and bridges it to the
// host over conn: the guest streams terminal output back as KindStdout frames
// while reading KindStdin/KindResize frames from the host. It returns when the
// shell exits (user typed exit) or the host closes the connection (client
// detached). When tmux is present the PTY backs a tmux client, so a detach
// leaves the shell - and any agent running in it - alive to reattach; see
// shellCommand.
func runShell(conn net.Conn, spec guestproto.ShellSpec) {
	lu := lookupLoginUser()
	cmd := shellCommand(spec, lu)
	applyLoginUser(cmd, lu) // before pty.Start, which augments SysProcAttr

	ptmx, err := pty.Start(cmd)
	if err != nil {
		fc := &frameConn{w: conn}
		_ = fc.write(guestproto.KindStderr, []byte(fmt.Sprintf("fletcher-guest: start shell: %v\n", err)))
		_ = fc.write(guestproto.KindExit, guestproto.EncodeExit(1))
		return
	}
	defer func() { _ = ptmx.Close() }()
	if spec.Cols > 0 && spec.Rows > 0 {
		_ = pty.Setsize(ptmx, &pty.Winsize{Cols: spec.Cols, Rows: spec.Rows})
	}

	fc := &frameConn{w: conn}
	// Pump PTY output to the host until the shell closes the terminal.
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		buf := make([]byte, 32<<10)
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				if werr := fc.write(guestproto.KindStdout, buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()
	// Pump host input (keystrokes, resizes) to the PTY. When the host closes the
	// connection, ReadFrame errors; closing the PTY hangs up the shell's
	// controlling terminal so it exits and cmd.Wait below returns.
	go func() {
		for {
			kind, payload, rerr := guestproto.ReadFrame(conn)
			if rerr != nil {
				_ = ptmx.Close()
				return
			}
			switch kind {
			case guestproto.KindStdin:
				_, _ = ptmx.Write(payload)
			case guestproto.KindResize:
				if cols, rows, derr := guestproto.DecodeResize(payload); derr == nil {
					_ = pty.Setsize(ptmx, &pty.Winsize{Cols: cols, Rows: rows})
				}
			}
		}
	}()

	code := waitCode(cmd)
	<-outDone                                                             // drain any output the shell wrote on its way out
	_ = fc.write(guestproto.KindExit, guestproto.EncodeExit(int32(code))) //nolint:gosec // exit codes are 0-255
}

const (
	// tmuxSocket names the per-VM tmux server's socket (-L); a fixed name means
	// every attach reaches the same server. tmuxSession is the single durable
	// session every shell attaches to.
	tmuxSocket  = "fletcher"
	tmuxSession = "main"
)

// shellCommand builds the interactive shell process. When tmux is on PATH it
// attaches to (or creates) one durable session per VM, so the REPL survives a
// client detach: the host PTY backs a tmux client, and hanging up the
// connection detaches that client rather than killing the shell - the server
// and its windows keep running in the VM until the session is stopped. Without
// tmux it falls back to a plain login shell whose lifetime is the connection
// (the pre-durability behaviour, kept for a minimal rootfs and the mock
// driver). Either way cmd carries the login user's env and start directory; the
// caller applies the login-user credential before pty.Start.
func shellCommand(spec guestproto.ShellSpec, lu loginUser) *exec.Cmd {
	shell := loginShell()
	dir := shellDir()

	var cmd *exec.Cmd
	if tmux, ok := tmuxPath(); ok {
		// new-session -A attaches to tmuxSession when it exists, else creates it
		// in dir running `shell -l`. The trailing command is ignored on attach,
		// so a reattach lands in the existing session untouched.
		// -u forces UTF-8 I/O: tmux interprets the byte stream (unlike the old
		// raw-passthrough shell), so without this it splits multibyte runes and
		// miscounts cell widths, mangling rich TUIs like Claude Code. The env
		// also carries a UTF-8 LANG so the inner programs agree (see withDefaults).
		args := []string{"-u", "-L", tmuxSocket}
		// -CC puts the tmux client in control mode: the PTY then carries the
		// tmux control protocol (%output, %begin/%end, ...) instead of a
		// rendered terminal, so a client that speaks it renders panes natively
		// with its own scrollback while tmux keeps the session durable. The same
		// new-session -A reaches the same durable session as the plain client.
		if spec.ControlMode {
			args = append(args, "-CC")
		}
		args = append(args, "new-session", "-A", "-s", tmuxSession, "-c", dir, shell, "-l")
		cmd = exec.CommandContext(context.Background(), tmux, args...) //nolint:gosec // launching the user's shell is the entire purpose
	} else {
		cmd = exec.CommandContext(context.Background(), shell, "-l") //nolint:gosec // launching a shell is the entire purpose
	}
	cmd.Dir = dir
	cmd.Env = shellEnv(spec, lu)
	return cmd
}

// tmuxPath resolves the tmux binary. PID 1 boots with an empty environment, so
// exec.LookPath (which consults $PATH) usually fails in the guest; fall back to
// the absolute paths a packaged tmux installs to. Returns ok=false when tmux is
// genuinely absent (minimal rootfs, mock driver), so the caller runs a bare
// login shell.
func tmuxPath() (string, bool) {
	if p, err := exec.LookPath("tmux"); err == nil {
		return p, true
	}
	for _, p := range []string{"/usr/bin/tmux", "/bin/tmux", "/usr/local/bin/tmux"} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, true
		}
	}
	return "", false
}

// loginShell prefers bash, falling back to sh on a minimal rootfs.
func loginShell() string {
	if _, err := os.Stat("/bin/bash"); err == nil {
		return "/bin/bash"
	}
	return "/bin/sh"
}

// shellDir is the shell's working directory: /workspace if present, else /.
func shellDir() string {
	if fi, err := os.Stat(guestWorkspace); err == nil && fi.IsDir() {
		return guestWorkspace
	}
	return "/"
}

// loginUserName is the unprivileged account agent commands run as, so session
// shell/exec (and jobs) match brokered SSH - all run as the image's login user,
// sharing one home and agent config - instead of root.
const loginUserName = "fletcher"

// loginUser is the resolved login account; ok is false on an image without it
// (then commands stay as root, the historical behaviour).
type loginUser struct {
	uid, gid uint32
	home     string
	ok       bool
}

// lookupLoginUser resolves loginUserName from the rootfs (/etc/passwd, parsed by
// pure-Go os/user under CGO_ENABLED=0).
func lookupLoginUser() loginUser {
	u, err := user.Lookup(loginUserName)
	if err != nil {
		return loginUser{}
	}
	uid, uerr := strconv.Atoi(u.Uid)
	gid, gerr := strconv.Atoi(u.Gid)
	if uerr != nil || gerr != nil || uid < 0 || gid < 0 || uid > math.MaxUint32 || gid > math.MaxUint32 {
		return loginUser{}
	}
	return loginUser{uid: uint32(uid), gid: uint32(gid), home: u.HomeDir, ok: true}
}

// applyLoginUser makes cmd run as the login user when resolved. For a PTY shell,
// call it before pty.Start so creack/pty adds Setctty/Setsid to the same
// SysProcAttr rather than replacing the credential.
func applyLoginUser(cmd *exec.Cmd, lu loginUser) {
	if !lu.ok {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: lu.uid, Gid: lu.gid}
}

// shellEnv builds the login shell's environment: the spec's TERM plus the
// usual PATH/HOME defaults (for the login user) and any extra entries the host
// passed.
func shellEnv(spec guestproto.ShellSpec, lu loginUser) []string {
	env := withDefaults(spec.Env, lu)
	term := spec.Term
	if term == "" {
		term = "xterm-256color"
	}
	if !hasKey(env, "TERM") {
		env = append(env, "TERM="+term)
	}
	return env
}

// waitCode waits for cmd and maps its result to an exit code.
func waitCode(cmd *exec.Cmd) int {
	err := cmd.Wait()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ee.ExitCode() >= 0 {
			return ee.ExitCode()
		}
		return 1 // killed by signal (e.g. the host hung up the terminal)
	}
	return 1
}

// run dials the host, executes the job, and returns the exit code to report.
func run() int {
	conn, err := dialHost()
	if err != nil {
		// Nothing to report to; surface on the console for debugging.
		fmt.Fprintf(os.Stderr, "fletcher-guest: dial host: %v\n", err)
		return 1
	}
	defer func() { _ = conn.Close() }()

	spec, err := guestproto.ReadSpec(conn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fletcher-guest: read spec: %v\n", err)
		return 1
	}

	// Bring up the loopback service relays (gateway, MCP) before running, so the
	// agent's calls to them succeed. The VM has no NIC, so this vsock relay is
	// its only path off-box - and it reaches only the daemon, never the internet.
	startForwards(spec.Forwards)

	return runExec(conn, spec)
}

// runExec runs spec and frames its stdout/stderr and exit code back over conn.
// Shared by the ephemeral path and the session control loop.
func runExec(conn net.Conn, spec guestproto.Spec) int {
	// Cancel the command when the host disconnects. For exec the host sends only
	// the request, so a read returning (EOF on close) means it went away - which
	// is how a long-running command like `tail -f` (the follow-log stream) gets
	// killed instead of leaking when the client closes the stream.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		var b [1]byte
		_, _ = conn.Read(b[:])
		cancel()
	}()

	fc := &frameConn{w: conn}
	code := runCommand(ctx, spec, fc)
	if err := fc.write(guestproto.KindExit, guestproto.EncodeExit(int32(code))); err != nil { //nolint:gosec // exit codes are 0-255
		fmt.Fprintf(os.Stderr, "fletcher-guest: send exit: %v\n", err)
	}
	return code
}

// runCommand runs the job command with its stdout/stderr framed back over fc.
func runCommand(ctx context.Context, spec guestproto.Spec, fc *frameConn) int {
	workDir := spec.WorkDir
	if workDir == "" {
		workDir = "/"
	}
	// Ensure the working directory exists (e.g. /workspace on a minimal rootfs);
	// fall back to / if it cannot be created.
	if err := os.MkdirAll(workDir, 0o755); err != nil { //nolint:gosec // standard rootfs dir perms inside the VM
		workDir = "/"
	}
	// ctx is cancelled when the host disconnects, so a long-running command is
	// killed rather than left writing to a dead connection.
	lu := lookupLoginUser()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", spec.Command) //nolint:gosec // running the job is the entire purpose
	cmd.Dir = workDir
	cmd.Env = withDefaults(spec.Env, lu)
	applyLoginUser(cmd, lu)
	cmd.Stdout = streamWriter{fc: fc, kind: guestproto.KindStdout}
	cmd.Stderr = streamWriter{fc: fc, kind: guestproto.KindStderr}

	err := cmd.Run()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ee.ExitCode() >= 0 {
			return ee.ExitCode()
		}
		return 1 // killed by signal
	}
	// Couldn't start the command at all (e.g. no /bin/sh).
	_ = fc.write(guestproto.KindStderr, []byte(fmt.Sprintf("fletcher-guest: %v\n", err)))
	return 127
}

// dialHost connects to the daemon over vsock, retrying while the VM finishes
// bringing up its vsock device.
func dialHost() (net.Conn, error) {
	var lastErr error
	for range 50 {
		conn, err := vsock.Dial(guestproto.HostCID, guestproto.Port, nil)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	return nil, lastErr
}

// frameConn serialises frame writes from the concurrent stdout/stderr streams.
type frameConn struct {
	mu sync.Mutex
	w  net.Conn
}

func (c *frameConn) write(kind byte, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return guestproto.WriteFrame(c.w, kind, payload)
}

// streamWriter frames each Write as one output frame of its kind.
type streamWriter struct {
	fc   *frameConn
	kind byte
}

func (s streamWriter) Write(p []byte) (int, error) {
	if err := s.fc.write(s.kind, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// defaultGuestPath is the PATH commands get when the image's spec sets none.
const defaultGuestPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// withDefaults ensures the command has a usable PATH, HOME, and USER even if the
// spec did not set them. HOME/USER track the login user (root when unresolved)
// so an agent finds its config in the same home it uses over SSH.
func withDefaults(env []string, lu loginUser) []string {
	home, name := "/root", "root"
	if lu.ok {
		home, name = lu.home, loginUserName
	}
	out := append([]string(nil), env...)
	if !hasKey(out, "PATH") {
		out = append(out, "PATH="+defaultGuestPath)
	}
	if !hasKey(out, "HOME") {
		out = append(out, "HOME="+home)
	}
	if !hasKey(out, "USER") {
		out = append(out, "USER="+name)
	}
	if !hasKey(out, "LANG") {
		// A UTF-8 locale so tmux and the agent TUIs compute character widths
		// correctly. C.UTF-8 is built into glibc, so no locale-gen is needed.
		out = append(out, "LANG=C.UTF-8")
	}
	return out
}

// mergeEnv layers override entries on top of base, so a key set in both is taken
// from override (override wins) while base-only keys are kept. Used to inject the
// session's user env vars over the image's own app env.
func mergeEnv(base, override []string) []string {
	if len(override) == 0 {
		return base
	}
	overridden := make(map[string]bool, len(override))
	for _, kv := range override {
		if k, _, ok := strings.Cut(kv, "="); ok {
			overridden[k] = true
		}
	}
	out := make([]string, 0, len(base)+len(override))
	for _, kv := range base {
		if k, _, ok := strings.Cut(kv, "="); ok && overridden[k] {
			continue // replaced by an override entry
		}
		out = append(out, kv)
	}
	return append(out, override...)
}

func hasKey(env []string, key string) bool {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// setupOnce guards the session setup (service forwards + the global env file) so
// it runs exactly once per guest lifetime. A hibernation restore resumes the
// same guest process (forwards already listening, env file already written), so
// a resent RequestSetup is a no-op rather than a double-bind.
var setupOnce sync.Once

// sessionEnvFile is sourced by every login shell (sshd, console), so an agent
// started over brokered SSH or an IDE terminal - not just the guestproto exec
// and shell paths - inherits the gateway/MCP env.
const sessionEnvFile = "/etc/profile.d/fletcher.sh"

// applySetup handles a RequestSetup: once per guest lifetime it brings up the
// session's service forwards, writes the gateway/MCP env where login shells pick
// it up, seeds any credentials, and launches a run_app session's app (with the
// user env vars), then acks with a single Exit frame so the host knows the
// loopback listeners are up before it reports the session ready.
func applySetup(conn net.Conn, spec guestproto.Spec) {
	setupOnce.Do(func() {
		startForwards(spec.Forwards)
		writeSessionEnv(spec.Env)
		writeCredentials(spec.Credentials)
		// Launch the run_app session's app here, not at boot, so it starts with
		// the user env vars (spec.AppEnv) the host delivers in this same setup.
		if appMode() {
			startApp(spec.AppEnv)
		}
	})
	if err := guestproto.WriteFrame(conn, guestproto.KindExit, guestproto.EncodeExit(0)); err != nil {
		fmt.Fprintf(os.Stderr, "fletcher-guest: ack setup: %v\n", err)
	}
}

// writeSessionEnv writes the session env as a profile.d snippet so login shells
// (brokered SSH, IDE terminals) export the gateway/MCP variables. The guestproto
// exec/shell paths inject the same env directly; this covers sshd, which spawns
// its login shells with only the init environment.
func writeSessionEnv(env []string) {
	if len(env) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString("# Fletcher session environment (model gateway + MCP); written by the guest init.\n")
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "export %s=%s\n", k, shellSingleQuote(v))
	}
	if err := os.WriteFile(sessionEnvFile, []byte(b.String()), 0o644); err != nil { //nolint:gosec // sourced by login shells; non-secret gateway URLs + placeholder keys
		fmt.Fprintf(os.Stderr, "fletcher-guest: write session env: %v\n", err)
	}
}

// shellSingleQuote wraps a value in single quotes for safe sourcing, escaping any
// embedded single quotes the POSIX way ('\”).
func shellSingleQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// writeCredentials seeds a freshly created session with the box's saved agent
// login (e.g. ~/.claude): each file is written as root, then handed to the login
// user since the agent runs as that user and refreshes the token in place.
// Best-effort - a bad credential logs and is skipped rather than blocking boot.
func writeCredentials(creds []guestproto.CredentialFile) {
	if len(creds) == 0 {
		return
	}
	lu := lookupLoginUser()
	for _, c := range creds {
		if !filepath.IsAbs(c.Path) {
			fmt.Fprintf(os.Stderr, "fletcher-guest: skip non-absolute credential path %q\n", c.Path)
			continue
		}
		if err := writeCredentialFile(c, lu); err != nil {
			fmt.Fprintf(os.Stderr, "fletcher-guest: seed credential %s: %v\n", c.Path, err)
		}
	}
}

// writeCredentialFile writes one seeded credential file and gives it (and the
// credential directories under the user's home) to the login user.
func writeCredentialFile(c guestproto.CredentialFile, lu loginUser) error {
	mode := os.FileMode(c.Mode)
	if mode == 0 {
		mode = 0o600
	}
	dir := filepath.Dir(c.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(c.Path, c.Data, mode); err != nil {
		return err
	}
	if lu.ok {
		_ = os.Chown(c.Path, int(lu.uid), int(lu.gid))
		// Hand the credential directories (which MkdirAll may have created
		// root-owned) up to but not including the home to the login user, so the
		// agent can refresh tokens in place.
		for d := dir; d != lu.home && d != "/" && d != "." && strings.HasPrefix(d, lu.home); d = filepath.Dir(d) {
			_ = os.Chown(d, int(lu.uid), int(lu.gid))
		}
	}
	return nil
}

// startForwards launches a TCP->vsock relay for each forward. The agent inside
// the VM connects to f.ListenAddr (loopback); each connection is relayed to the
// host on f.VsockPort, where the daemon proxies it to the matching unix socket.
func startForwards(forwards []guestproto.Forward) {
	for _, f := range forwards {
		if err := startForward(f); err != nil {
			fmt.Fprintf(os.Stderr, "fletcher-guest: forward %s: %v\n", f.ListenAddr, err)
		}
	}
}

func startForward(f guestproto.Forward) error {
	ln, err := net.Listen("tcp", f.ListenAddr) //nolint:noctx // lifetime is the VM; closed at poweroff
	if err != nil {
		return err
	}
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			go relayToVsock(client, f.VsockPort)
		}
	}()
	return nil
}

func relayToVsock(client net.Conn, vsockPort uint32) {
	upstream, err := vsock.Dial(guestproto.HostCID, vsockPort, nil)
	if err != nil {
		_ = client.Close()
		return
	}
	splice(client, upstream)
}

// splice copies bidirectionally between two connections, closing both when
// either direction ends so half-closed HTTP/SSE streams terminate.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}

// setupLoopback brings the loopback interface up so the agent (and the forward
// relays) can use 127.0.0.1. Firecracker gives the guest a lo device but does
// not bring it up.
func setupLoopback() {
	if err := ifup("lo"); err != nil {
		fmt.Fprintf(os.Stderr, "fletcher-guest: bring up lo: %v\n", err)
	}
}

func ifup(name string) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP | unix.IFF_RUNNING)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

// mountBasics mounts the pseudo-filesystems most programs expect. Best-effort:
// a missing mount point or an already-mounted fs should not stop the job.
func mountBasics() {
	type m struct{ source, target, fstype string }
	for _, mt := range []m{
		{"proc", "/proc", "proc"},
		{"sysfs", "/sys", "sysfs"},
		{"tmpfs", "/tmp", "tmpfs"},
		{"devpts", "/dev/pts", "devpts"},
	} {
		_ = os.MkdirAll(mt.target, 0o755) //nolint:gosec // standard mountpoint perms inside the VM
		if err := unix.Mount(mt.source, mt.target, mt.fstype, 0, ""); err != nil {
			fmt.Fprintf(os.Stderr, "fletcher-guest: mount %s: %v\n", mt.target, err)
		}
	}
	mountVolume()
}

// volumeMountPoint is where a session's persistent volume (the second virtio
// disk, when the host attached one) appears in the guest.
const volumeMountPoint = "/volume"

// mountVolume mounts the persistent volume at /volume when the host attached
// one. A session without a volume has no /dev/vdb and skips this.
func mountVolume() {
	const dev = "/dev/vdb"
	if _, err := os.Stat(dev); err != nil {
		return
	}
	if err := os.MkdirAll(volumeMountPoint, 0o755); err != nil { //nolint:gosec // standard mountpoint perms inside the VM
		fmt.Fprintf(os.Stderr, "fletcher-guest: create %s: %v\n", volumeMountPoint, err)
		return
	}
	if err := unix.Mount(dev, volumeMountPoint, "ext4", 0, ""); err != nil {
		fmt.Fprintf(os.Stderr, "fletcher-guest: mount volume: %v\n", err)
		return
	}
	// Writable by the unprivileged login user: the volume exists to hold the
	// workspace's durable data.
	if lu := lookupLoginUser(); lu.ok {
		_ = os.Chown(volumeMountPoint, int(lu.uid), int(lu.gid))
	}
}

// shutdown flushes and resets the VM so Firecracker exits. As PID 1 we hold
// CAP_SYS_BOOT. RESTART (with the kernel's reboot=k) does a keyboard-controller
// reset that Firecracker intercepts; POWER_OFF would need ACPI, which
// Firecracker does not provide, and would hang.
func shutdown() {
	unix.Sync()
	if err := unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART); err != nil {
		fmt.Fprintf(os.Stderr, "fletcher-guest: reboot: %v\n", err)
		for {
			_ = unix.Reboot(unix.LINUX_REBOOT_CMD_HALT)
			time.Sleep(time.Second)
		}
	}
}
