// Package settings stores runtime-mutable operational settings in the daemon's
// SQLite database. They are edited via `fletcher settings` and applied over the
// flag/env config at daemon startup; changes take effect on the next restart.
// Bootstrap config (database, socket, age key, listen addresses) is not managed
// here. See STANDARDS.md sections 95/98.
package settings

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/joshjon/fletcher/internal/errs"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

// Known setting keys. The same names are used as the stored keys and by the
// daemon when applying settings over its config.
const (
	KeyRuntime        = "runtime"
	KeySnapshot       = "snapshot"
	KeyBtrfsRoot      = "btrfs_root"
	KeyPublicEndpoint = "public_endpoint"
	KeyWireGuardPort  = "wireguard_port"
	KeyLogLevel       = "log_level"
	KeyCredentialsDir = "credentials_dir"
)

// definition describes a settable key: its help text and an optional validator.
type definition struct {
	key         string
	description string
	validate    func(string) error
}

var registry = []definition{
	{KeyRuntime, "runtime driver: mock | runc | firecracker (restart to apply)", oneOf("mock", "runc", "firecracker")},
	{KeySnapshot, "snapshot driver: mock | btrfs | ext4 (restart to apply)", oneOf("mock", "btrfs", "ext4")},
	{KeyBtrfsRoot, "btrfs snapshot root directory (restart to apply)", nil},
	{KeyPublicEndpoint, "host:port peers dial to reach this daemon (restart to apply)", nil},
	{KeyWireGuardPort, "WireGuard UDP listen port, 1-65535 (restart to apply)", portNumber},
	{KeyLogLevel, "log level: debug | info | warn | error (restart to apply)", oneOf("debug", "info", "warn", "error")},
	{KeyCredentialsDir, "host directory for trusted-credential mounts (restart to apply)", nil},
}

// View is one setting's full picture for `fletcher settings list`.
type View struct {
	Key         string
	Value       string
	Description string
	Set         bool
}

// Store persists settings over the generated SQLite queries.
type Store struct {
	q sqliteq.Querier
}

// NewStore builds a Store backed by q.
func NewStore(q sqliteq.Querier) *Store {
	return &Store{q: q}
}

// Set validates and upserts a setting. Unknown keys and invalid values are
// rejected so a typo cannot silently persist.
func (s *Store) Set(ctx context.Context, key, value string) error {
	def, ok := lookup(key)
	if !ok {
		return errs.Newf(errs.CategoryInvalidArgument, "unknown setting %q (known: %s)", key, strings.Join(names(), ", "))
	}
	if def.validate != nil {
		if err := def.validate(value); err != nil {
			return errs.Newf(errs.CategoryInvalidArgument, "invalid value for %q: %s", key, err)
		}
	}
	return s.q.UpsertSetting(ctx, sqliteq.UpsertSettingParams{
		Key:       key,
		Value:     value,
		UpdatedAt: time.Now().Unix(),
	})
}

// Delete removes a setting, reporting whether it existed.
func (s *Store) Delete(ctx context.Context, key string) (bool, error) {
	n, err := s.q.DeleteSetting(ctx, key)
	return n > 0, err
}

// Values returns the explicitly-set settings as a key->value map.
func (s *Store) Values(ctx context.Context) (map[string]string, error) {
	rows, err := s.q.ListSettings(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.Key] = r.Value
	}
	return out, nil
}

// Describe returns every known key with its current value (if set) and help.
func (s *Store) Describe(ctx context.Context) ([]View, error) {
	vals, err := s.Values(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(registry))
	for _, d := range registry {
		v, set := vals[d.key]
		out = append(out, View{Key: d.key, Value: v, Description: d.description, Set: set})
	}
	return out, nil
}

func lookup(key string) (definition, bool) {
	for _, d := range registry {
		if d.key == key {
			return d, true
		}
	}
	return definition{}, false
}

func names() []string {
	out := make([]string, len(registry))
	for i, d := range registry {
		out[i] = d.key
	}
	return out
}

func oneOf(allowed ...string) func(string) error {
	return func(v string) error {
		for _, a := range allowed {
			if v == a {
				return nil
			}
		}
		return fmt.Errorf("must be one of: %s", strings.Join(allowed, ", "))
	}
}

func portNumber(v string) error {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("must be a port number 1-65535")
	}
	return nil
}
