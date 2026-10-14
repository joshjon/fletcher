package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"

	"github.com/joshjon/fletcher/internal/background"
	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/runtime"
)

// shellStream is the client side of the interactive-shell bidi RPC.
type shellStream = connect.BidiStreamForClient[fletcherv1.ShellSessionRequest, fletcherv1.ShellSessionResponse]

func sessionShellCmd() *cli.Command {
	return &cli.Command{
		Name:      "shell",
		Usage:     "open or reattach a terminal in a running session",
		ArgsUsage: "<ref>",
		Flags: []cli.Flag{
			socketFlag(),
			&cli.StringFlag{Name: "terminal", Usage: "durable terminal name (empty reattaches the default terminal)"},
			&cli.StringFlag{Name: "workdir", Usage: "initial absolute guest directory for a new named terminal; requires --terminal"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ref := cmd.Args().First()
			if ref == "" {
				return errors.New("session ref (id or name) is required")
			}
			return runSessionShellTarget(ctx, cmd, ref, cmd.String("workdir"), cmd.String("terminal"))
		},
	}
}

// runSessionShellTarget wires the local terminal to a PTY in the session VM: it puts
// stdin in raw mode, streams keystrokes up and terminal output down, and
// forwards window resizes, restoring the terminal when the shell ends.
func runSessionShellTarget(ctx context.Context, cmd *cli.Command, ref, dir, terminalName string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := (runtime.ShellSpec{WorkDir: dir, TerminalName: terminalName}).Validate(); err != nil {
		return err
	}
	stream := newSessionsClient(cmd).ShellSession(ctx)
	defer func() { _ = stream.CloseResponse() }()

	stdinFd := int(os.Stdin.Fd())
	// connect streams are not safe for concurrent Send; serialise stdin,
	// resize, and the opening message behind one mutex.
	var sendMu sync.Mutex
	send := func(msg *fletcherv1.ShellSessionRequest) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(msg)
	}

	start := shellStartMessage(ref, stdinFd)
	start.GetStart().WorkDir = dir
	start.GetStart().TerminalName = terminalName
	if err := send(start); err != nil {
		return err
	}

	if term.IsTerminal(stdinFd) {
		old, err := term.MakeRaw(stdinFd)
		if err != nil {
			return err
		}
		var once sync.Once
		restore := func() { once.Do(func() { _ = term.Restore(stdinFd, old) }) }
		defer restore()

		stopResize := forwardResizes(ctx, stdinFd, send)
		defer stopResize()
	}

	stdin := os.Stdin
	closeRequest := func() error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.CloseRequest()
	}
	background.GoNamed(ctx, "shell-input", func(context.Context) { forwardStdin(stdin, send, closeRequest) })

	code, err := copyShellOutput(stream)
	_ = stream.CloseResponse()
	if err != nil {
		return err
	}
	if code != 0 {
		return cli.Exit("", int(code))
	}
	return nil
}

// shellStartMessage builds the opening message: ref plus the local terminal's
// TERM and current window size.
func shellStartMessage(ref string, stdinFd int) *fletcherv1.ShellSessionRequest {
	cols, rows := 80, 24
	if w, h, err := term.GetSize(stdinFd); err == nil {
		cols, rows = w, h
	}
	termEnv := os.Getenv("TERM")
	if termEnv == "" {
		termEnv = "xterm-256color"
	}
	return &fletcherv1.ShellSessionRequest{
		Msg: &fletcherv1.ShellSessionRequest_Start{Start: &fletcherv1.ShellStart{
			Ref:  ref,
			Term: termEnv,
			Cols: termDim(cols),
			Rows: termDim(rows),
		}},
	}
}

// termDim narrows a terminal dimension (always small and non-negative) to the
// uint32 the proto carries, clamping a nonsensical negative to zero.
func termDim(v int) uint32 {
	if v < 0 {
		return 0
	}
	return uint32(v) //nolint:gosec // guarded non-negative just above
}

// forwardResizes pushes a resize message on each SIGWINCH and returns a stop
// function that tears the watcher down.
func forwardResizes(ctx context.Context, stdinFd int, send func(*fletcherv1.ShellSessionRequest) error) func() {
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	background.GoNamed(ctx, "shell-window-resize", func(ctx context.Context) {
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-winch:
				if !ok {
					return
				}
				w, h, err := term.GetSize(stdinFd)
				if err != nil {
					continue
				}
				if err := send(&fletcherv1.ShellSessionRequest{
					Msg: &fletcherv1.ShellSessionRequest_Resize{Resize: &fletcherv1.ShellResize{
						Cols: termDim(w), Rows: termDim(h),
					}},
				}); err != nil {
					return
				}
			}
		}
	})
	return func() { signal.Stop(winch); close(winch) }
}

// forwardStdin streams local keystrokes to the server until stdin ends, then
// half-closes the send side.
func forwardStdin(input io.Reader, send func(*fletcherv1.ShellSessionRequest) error, closeRequest func() error) {
	buf := make([]byte, 4<<10)
	for {
		n, rerr := input.Read(buf)
		if n > 0 {
			if serr := send(&fletcherv1.ShellSessionRequest{
				Msg: &fletcherv1.ShellSessionRequest_Stdin{Stdin: bytes.Clone(buf[:n])},
			}); serr != nil {
				return
			}
		}
		if rerr != nil {
			_ = closeRequest()
			return
		}
	}
}

// copyShellOutput writes terminal output to stdout until the shell reports its
// exit code (the final message) or the stream ends.
func copyShellOutput(stream *shellStream) (int32, error) {
	for {
		resp, err := stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0, nil
			}
			return 0, err
		}
		switch m := resp.Msg.(type) {
		case *fletcherv1.ShellSessionResponse_Data:
			_, _ = os.Stdout.Write(m.Data)
		case *fletcherv1.ShellSessionResponse_ExitCode:
			return m.ExitCode, nil
		}
	}
}
