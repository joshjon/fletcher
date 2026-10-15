//go:build linux

package hostdriver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/creack/pty"

	"github.com/joshjon/fletcher/internal/background"
	"github.com/joshjon/fletcher/internal/runtime"
)

func (s *executor) shellSession(ctx context.Context, conn net.Conn, hs *hostSession, spec runtime.ShellSpec) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	//nolint:contextcheck // bind the inherited stream context to the independent session lifetime as well
	stopSession := context.AfterFunc(hs.ctx, cancel)
	defer stopSession()
	cmd, terminal, err := s.attachTerminal(ctx, hs, spec)
	if err != nil {
		_ = writePacket(conn, resultPacket(err))
		return
	}
	defer terminal.Close()
	stop := context.AfterFunc(ctx, func() { _ = terminal.Close(); _ = conn.Close() })
	defer stop()
	// The terminal namespace has been checked before acknowledging readiness.
	// An attach never creates a replacement server or falls back to another shell.
	if err := writePacket(conn, packet{Kind: "ready"}); err != nil {
		cancel()
		_ = cmd.Wait()
		return
	}
	inputDone := make(chan struct{})
	background.GoNamed(ctx, "host.terminal.input", func(context.Context) {
		defer close(inputDone)
		defer cancel()
		readTerminalInput(conn, terminal)
	})
	buf := make([]byte, 32<<10)
	for {
		n, err := terminal.Read(buf)
		if n > 0 {
			if err := writePacket(conn, packet{Kind: "output", Data: buf[:n]}); err != nil {
				cancel()
				break
			}
		}
		if err != nil {
			break
		}
	}
	exit := int32(0)
	if err := cmd.Wait(); err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			exit = int32(ee.ExitCode()) //nolint:gosec // Unix process exit status is -1 or an 8-bit value
		} else {
			exit = 1
		}
	}
	_ = writePacket(conn, packet{Kind: "exit", Exit: exit})
	cancel()
	_ = conn.Close()
	<-inputDone
}

func (s *executor) attachTerminal(ctx context.Context, hs *hostSession, spec runtime.ShellSpec) (*exec.Cmd, *os.File, error) {
	if err := spec.Validate(); err != nil {
		return nil, nil, err
	}
	if len(spec.Env) != 0 {
		return nil, nil, unavailable("host terminals do not accept injected environment variables")
	}
	name := spec.TerminalName
	if name == "" {
		name = "main"
	}
	dir := spec.WorkDir
	if dir == "" {
		dir = hs.spec.WorkDir
	}
	if spec.Cols == 0 {
		spec.Cols = 80
	}
	if spec.Rows == 0 {
		spec.Rows = 24
	}
	if err := s.ensureTerminal(ctx, hs, name, dir, spec); err != nil {
		return nil, nil, err
	}
	args := []string{"-u", "-S", s.socket(hs.spec.SessionID)}
	if spec.ControlMode {
		args = append(args, "-CC")
	}
	args = append(args, "attach-session", "-t", "="+name)
	//nolint:gosec // validated named terminal on this session's dedicated tmux socket, without a shell
	cmd := exec.CommandContext(ctx, s.tmux, args...)
	cmd.Env = s.env(spec.Term)
	cmd.Dir = s.account.Home
	terminal, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: spec.Cols, Rows: spec.Rows})
	if err != nil {
		return nil, nil, fmt.Errorf("attach host terminal: %w", err)
	}
	return cmd, terminal, nil
}

func readTerminalInput(conn net.Conn, terminal *os.File) {
	for {
		var p packet
		if err := readPacket(conn, &p); err != nil {
			return
		}
		switch p.Kind {
		case "input":
			if _, err := terminal.Write(p.Data); err != nil {
				return
			}
		case "resize":
			if p.Size.Cols == 0 || p.Size.Rows == 0 {
				continue
			}
			if err := pty.Setsize(terminal, &pty.Winsize{Cols: p.Size.Cols, Rows: p.Size.Rows}); err != nil {
				return
			}
		default:
			return
		}
	}
}

func (s *executor) ensureTerminal(ctx context.Context, hs *hostSession, name, dir string, spec runtime.ShellSpec) error {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if err := hs.ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	base := make([]string, 0, 17)
	base = append(base, "-u", "-S", s.socket(hs.spec.SessionID), "-f", filepath.Join(s.dir, "tmux.conf"))
	//nolint:gosec // validated terminal name and dedicated socket passed as separate arguments
	has := exec.CommandContext(ctx, s.tmux, append(append([]string{}, base...), "has-session", "-t", "="+name)...)
	has.Env = s.env(spec.Term)
	if err := has.Run(); err == nil {
		return nil
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return unavailable("terminal directory is unavailable to the selected account")
	}
	// Separate shell arguments avoid handing a composed command to /bin/sh.
	base = append(base, "new-session", "-d", "-s", name, "-c", dir,
		"-x", fmt.Sprint(spec.Cols), "-y", fmt.Sprint(spec.Rows), s.shell, "-l")
	//nolint:gosec // local account shell and directory are separate tmux arguments, not shell text
	cmd := exec.CommandContext(ctx, s.tmux, base...)
	cmd.Env = s.env(spec.Term)
	cmd.Dir = s.account.Home
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("create host terminal: %w", err)
	}
	return nil
}

func (s *executor) list(path string) (runtime.DirListing, error) {
	if path == "" {
		path = s.account.Home
	}
	if err := (runtime.ShellSpec{WorkDir: path, TerminalName: "browse"}).Validate(); err != nil {
		return runtime.DirListing{}, err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return runtime.DirListing{}, fmt.Errorf("resolve host directory: %w", err)
	}
	dir, err := os.Open(resolved)
	if err != nil {
		return runtime.DirListing{}, fmt.Errorf("open host directory: %w", err)
	}
	defer dir.Close()
	entries, err := dir.ReadDir(2049)
	if err != nil && !errors.Is(err, io.EOF) {
		return runtime.DirListing{}, fmt.Errorf("list host directory: %w", err)
	}
	listing := runtime.DirListing{Path: resolved, Truncated: len(entries) > 2048}
	if listing.Truncated {
		entries = entries[:2048]
	}
	listing.Entries = make([]runtime.DirEntry, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return runtime.DirListing{}, fmt.Errorf("inspect host directory entry: %w", err)
		}
		e := runtime.DirEntry{Name: entry.Name(), Size: info.Size(), Mode: uint32(info.Mode().Perm()), IsDir: info.IsDir(), ModTime: info.ModTime().Unix(), IsSymlink: info.Mode()&os.ModeSymlink != 0}
		if e.IsSymlink {
			e.SymlinkTarget, err = os.Readlink(filepath.Join(resolved, e.Name))
			if err != nil {
				return runtime.DirListing{}, fmt.Errorf("read host symlink: %w", err)
			}
			if target, err := os.Stat(filepath.Join(resolved, e.Name)); err == nil {
				e.IsDir = target.IsDir()
			}
		}
		listing.Entries = append(listing.Entries, e)
	}
	sort.Slice(listing.Entries, func(i, j int) bool {
		a, b := listing.Entries[i], listing.Entries[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		return a.Name < b.Name
	})
	return listing, nil
}
