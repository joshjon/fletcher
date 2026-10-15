package hostdriver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/joshjon/fletcher/internal/background"
	"github.com/joshjon/fletcher/internal/errs"
	"github.com/joshjon/fletcher/internal/runtime"
)

// Driver delegates only to the account authorized by local boot configuration.
type Driver struct {
	account runtime.HostAccount
	socket  string
}

// HostAccount verifies that the configured executor is reachable and has the expected identity.
func (d *Driver) HostAccount(ctx context.Context) (runtime.HostAccount, error) {
	p, err := d.call(ctx, request{Op: "info"})
	if err != nil {
		return runtime.HostAccount{}, err
	}
	if p.Version != protocolVersion {
		return runtime.HostAccount{}, unavailable("host executor protocol changed; update the daemon and account service together")
	}
	if p.Account != d.account {
		return runtime.HostAccount{}, unavailable("host executor account changed; review local setup before enabling it again")
	}
	return p.Account, nil
}

// StartHostSession registers a terminal namespace without allocating a disk or starting a shell.
func (d *Driver) StartHostSession(ctx context.Context, spec runtime.HostSessionSpec) (runtime.SessionHandle, error) {
	if spec.Account != d.account || !sessionIDPattern.MatchString(spec.SessionID) {
		return nil, unavailable("host session does not match the locally authorized account")
	}
	if _, err := d.HostAccount(ctx); err != nil {
		return nil, err
	}
	if _, err := d.call(ctx, request{Op: "start", Session: spec}); err != nil {
		return nil, err
	}
	return &handle{driver: d, spec: spec}, nil
}

func (d *Driver) dial(ctx context.Context, req request) (net.Conn, packet, error) {
	req.Version = protocolVersion
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", d.socket)
	if err != nil {
		if ctx.Err() != nil {
			return nil, packet{}, ctx.Err()
		}
		return nil, packet{}, unavailable("host executor unavailable; check the locally enabled account service")
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	var reply packet
	if err = writePacket(conn, req); err == nil {
		err = readPacket(conn, &reply)
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil && reply.Error != "" {
		err = unavailable(reply.Error)
	}
	if err != nil {
		_ = conn.Close()
		return nil, packet{}, fmt.Errorf("host executor request: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, reply, nil
}

func (d *Driver) call(ctx context.Context, req request) (packet, error) {
	conn, reply, err := d.dial(ctx, req)
	if err != nil {
		return packet{}, err
	}
	_ = conn.Close()
	return reply, nil
}

func unavailable(message string) error { return errs.New(errs.CategoryFailedPrecondition, message) }

type handle struct {
	driver *Driver
	spec   runtime.HostSessionSpec
}

func (h *handle) Shell(ctx context.Context, spec runtime.ShellSpec, stdin io.Reader, stdout io.Writer, resize <-chan runtime.WinSize) (int32, error) {
	if err := spec.Validate(); err != nil {
		return 0, err
	}
	conn, reply, err := h.driver.dial(ctx, request{Op: "shell", Session: h.spec, Shell: spec})
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	if reply.Kind != "ready" {
		return 0, unavailable("host executor did not acknowledge the requested terminal")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	inputEnded := relayShellInput(ctx, conn, stdin, resize)
	for {
		var p packet
		if err := readPacket(conn, &p); err != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			// An explicit input close detaches the client and can interrupt a frame.
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				select {
				case <-inputEnded:
					return 0, nil
				default:
				}
			}
			return 0, fmt.Errorf("read host terminal: %w", err)
		}
		switch p.Kind {
		case "output":
			if _, err := stdout.Write(p.Data); err != nil {
				return 0, err
			}
		case "exit":
			return p.Exit, nil
		default:
			return 0, unavailable("invalid host terminal response")
		}
	}
}

func relayShellInput(ctx context.Context, conn net.Conn, stdin io.Reader, resize <-chan runtime.WinSize) <-chan struct{} {
	inputEnded := make(chan struct{})
	var sendMu sync.Mutex
	send := func(p packet) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return writePacket(conn, p)
	}
	background.GoNamed(ctx, "host.shell.input", func(context.Context) {
		buf := make([]byte, 32<<10)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				if werr := send(packet{Kind: "input", Data: buf[:n]}); werr != nil {
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					close(inputEnded)
				}
				_ = send(packet{Kind: "close"})
				return
			}
		}
	})
	background.GoNamed(ctx, "host.shell.resize", func(ctx context.Context) {
		for {
			select {
			case <-ctx.Done():
				return
			case size, ok := <-resize:
				if !ok {
					return
				}
				if err := send(packet{Kind: "resize", Size: size}); err != nil {
					return
				}
			}
		}
	})
	return inputEnded
}

func (h *handle) ListDir(ctx context.Context, path string) (runtime.DirListing, error) {
	p, err := h.driver.call(ctx, request{Op: "list", Session: h.spec, Path: path})
	return p.Listing, err
}

func (h *handle) Stop(ctx context.Context) error { return h.driver.StopHostSession(ctx, h.spec) }

// StopHostSession stops a recorded namespace without starting it or changing its account.
func (d *Driver) StopHostSession(ctx context.Context, spec runtime.HostSessionSpec) error {
	if spec.Account != d.account || !sessionIDPattern.MatchString(spec.SessionID) {
		return unavailable("host session does not match the locally authorized account")
	}
	_, err := d.call(ctx, request{Op: "stop", Session: spec})
	return err
}

func (*handle) Exec(context.Context, runtime.Spec, io.Writer, io.Writer) (runtime.Result, error) {
	return runtime.Result{}, unsupported()
}
func (*handle) DialSSH(context.Context) (net.Conn, error)          { return nil, unsupported() }
func (*handle) DialPort(context.Context, uint16) (net.Conn, error) { return nil, unsupported() }
func (*handle) Load(context.Context) (float64, error)              { return 0, unsupported() }
func (*handle) AppRestarts(context.Context) (int64, error)         { return 0, unsupported() }
func (*handle) WriteFile(context.Context, runtime.FileWriteSpec, io.Reader) (runtime.FileWriteResult, error) {
	return runtime.FileWriteResult{}, unsupported()
}

func (*handle) ReadFile(context.Context, string, func(runtime.FileReadResult) error, io.Writer) error {
	return unsupported()
}
func (*handle) FileOp(context.Context, runtime.FileOpSpec) error { return unsupported() }

func unsupported() error {
	return unavailable("host sessions support interactive terminals and directory browsing only; use the terminal for host commands and files")
}

var (
	_ runtime.HostSessionRuntime = (*Driver)(nil)
	_ runtime.SessionHandle      = (*handle)(nil)
)
