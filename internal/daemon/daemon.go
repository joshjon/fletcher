// Package daemon orchestrates the long-running fletcher process: opens
// SQLite, runs migrations, registers Connect handlers, and serves them on
// a local Unix socket inside an oklog/run group.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/oklog/run"

	"github.com/joshjon/fletcher/internal/api"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
	"github.com/joshjon/fletcher/internal/sqlite"
)

// Config holds boot-time daemon settings.
type Config struct {
	SocketPath   string
	DatabasePath string
	LogLevel     string
}

// shutdownTimeout caps how long the daemon waits for in-flight work before
// forcing exit. Matches STANDARDS.md.
const shutdownTimeout = 30 * time.Second

// Run starts the daemon and blocks until ctx is cancelled or a fatal error
// occurs. On shutdown it closes the listener, removes the socket file, and
// closes the database.
func Run(ctx context.Context, cfg Config) error {
	logger := newLogger(cfg.LogLevel)
	logger.Info("starting fletcher daemon",
		slog.String("socket", cfg.SocketPath),
		slog.String("database", cfg.DatabasePath),
	)

	if err := ensureDirs(cfg); err != nil {
		return err
	}

	db, err := sqlite.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := sqlite.Migrate(db); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	logger.Info("migrations up to date")

	ln, err := listenUnix(ctx, cfg.SocketPath)
	if err != nil {
		return err
	}

	srv := newHTTPServer(time.Now().Unix())

	var g run.Group
	// serveActor's interrupt path uses a fresh context for graceful shutdown
	// because the parent ctx is already cancelled by the time interrupt fires.
	//nolint:contextcheck // shutdown must outlive the cancelled parent ctx
	g.Add(serveActor(logger, srv, ln, cfg.SocketPath))
	g.Add(signalActor(ctx))

	if err := g.Run(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	logger.Info("daemon stopped")
	return nil
}

func ensureDirs(cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(cfg.DatabasePath), 0o700); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.SocketPath), 0o700); err != nil {
		return fmt.Errorf("create socket directory: %w", err)
	}
	return nil
}

// listenUnix opens a Unix-domain listener at socketPath, removing any stale
// file left behind by a previous crash. The socket is chmod'd to 0600 so only
// the owning user can talk to the daemon.
func listenUnix(ctx context.Context, socketPath string) (net.Listener, error) {
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen unix %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}
	return ln, nil
}

func newHTTPServer(startedAt int64) *http.Server {
	mux := http.NewServeMux()
	adminSvc := api.NewAdminService(startedAt)
	path, handler := fletcherv1connect.NewAdminServiceHandler(adminSvc)
	mux.Handle(path, handler)
	return &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// serveActor returns the run.Group actor pair that owns the HTTP server.
func serveActor(logger *slog.Logger, srv *http.Server, ln net.Listener, socketPath string) (func() error, func(error)) {
	execute := func() error {
		logger.Info("listening", slog.String("socket", socketPath))
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	}
	interrupt := func(_ error) {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		_ = os.Remove(socketPath)
	}
	return execute, interrupt
}

// signalActor returns the run.Group actor pair that observes ctx (typically
// wired with signal.NotifyContext in main) and triggers group shutdown when
// the signal fires.
func signalActor(ctx context.Context) (func() error, func(error)) {
	cancelCh := make(chan struct{})
	execute := func() error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-cancelCh:
			return nil
		}
	}
	interrupt := func(_ error) { close(cancelCh) }
	return execute, interrupt
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
