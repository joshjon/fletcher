package daemon

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/joshjon/fletcher/internal/errs"
	"github.com/joshjon/fletcher/internal/host"
	"github.com/joshjon/fletcher/internal/image"
	"github.com/joshjon/fletcher/internal/peer"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

// Validate rejects known-invalid stored configuration before a remote restart.
func (r *settingsReloader) Validate(ctx context.Context) error {
	cur := r.flagCfg
	if err := applySettings(ctx, &cur, r.store, r.logger); err != nil {
		return errs.New(errs.CategoryFailedPrecondition, "stored settings are invalid; review configuration before restarting")
	}
	if err := validateConfig(&cur, r.logger); err != nil {
		return errs.New(errs.CategoryFailedPrecondition, "stored configuration cannot start; review settings before restarting")
	}
	return nil
}

func currentImageUpdate(cfg Config, state imageUpdateState) bool {
	if state.available == nil || !state.available.Load() {
		return false
	}
	if state.digest == nil || state.digest.Load() == nil {
		return true
	}
	meta, found, err := image.ReadMeta(filepath.Join(snapshotRootDir(cfg), "images"), cfg.DefaultImage)
	return err == nil && found && meta.Digest == *state.digest.Load()
}

func hostChecks(cfg Config, q *sqliteq.Queries, peers *peer.Service, update imageUpdateState) func(context.Context) []host.Check {
	return func(ctx context.Context) []host.Check {
		checks := []host.Check{{Name: "Database", Status: "ok", Detail: "SQLite is reachable"}}
		if _, err := q.ListSettings(ctx); err != nil {
			checks[0].Status, checks[0].Detail = "error", "SQLite is unavailable; inspect daemon logs"
		}
		runtimeCheck := host.Check{Name: "Runtime", Status: "ok", Detail: driverKind(cfg.RuntimeKind)}
		if cfg.RuntimeKind == "mock" {
			runtimeCheck.Status, runtimeCheck.Detail = "warning", "mock runtime: no VM isolation"
		}
		if cfg.RuntimeKind == "runc" {
			runtimeCheck.Status, runtimeCheck.Detail = "warning", "runc: degraded isolation compared with microVMs"
		}
		checks = append(checks, runtimeCheck)
		templates, err := image.ListTemplates(filepath.Join(snapshotRootDir(cfg), "images"))
		switch {
		case err != nil:
			checks = append(checks, host.Check{Name: "Images", Status: "error", Detail: "cannot read templates; inspect daemon logs"})
		case len(templates) == 0:
			checks = append(checks, host.Check{Name: "Images", Status: "warning", Detail: "no templates imported; open Images to import a registry image"})
		default:
			checks = append(checks, host.Check{Name: "Images", Status: "ok", Detail: fmt.Sprintf("%d imported templates", len(templates))})
		}
		if currentImageUpdate(cfg, update) {
			checks = append(checks, host.Check{Name: "Default image", Status: "warning", Detail: "a newer registry image is available; update it in Images"})
		}
		pairing := host.Check{Name: "Device pairing", Status: "ok", Detail: "public TLS pairing endpoint is configured"}
		if peers.PairingEndpoint() == "" {
			pairing.Status, pairing.Detail = "warning", "public pairing endpoint unavailable; review public endpoint and pairing port, or use a VPN login"
		}
		checks = append(checks, pairing)
		checks = append(checks, host.Check{Name: "Network scope", Status: "ok", Detail: "these checks run on the host; client and router reachability still require a connection test"})
		return checks
	}
}
