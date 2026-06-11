// Package volume manages persistent volumes: first-class disks with their own
// lifecycle, attached to a session as a second drive. A session's fork dies
// with redeploy/delete; its volume does not - repos, databases, and uploads
// live there and reattach to fresh forks (DESIGN §5: storage on metal the
// user owns; an attachment, not a fourth trigger).
package volume

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"syscall"
	"time"

	"go.jetify.com/typeid"

	"github.com/joshjon/fletcher/internal/errs"
	"github.com/joshjon/fletcher/internal/snapshot"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

// idPrefix is the typeid prefix for volume IDs.
const idPrefix = "vol"

// DefaultSizeBytes is the provisioned capacity when a create names none.
// Sparse-backed, so it costs only the blocks data lands on.
const DefaultSizeBytes = 10 << 30

// ErrNotFound is returned when a volume ref matches nothing.
var ErrNotFound = errs.New(errs.CategoryNotFound, "volume not found")

// Volume is the domain view of a persistent volume.
type Volume struct {
	ID        string
	Name      string
	Path      string
	SizeBytes int64
	// UsedBytes is the real disk the sparse backing file occupies.
	UsedBytes int64
	// AttachedSession is the name of the session this volume is attached to,
	// empty when detached.
	AttachedSession string
	CreatedAt       time.Time
}

// Manager owns volume lifecycle. The provisioner is the snapshot driver's
// volume capability (nil when the configured driver has none, in which case
// creates fail with a clear error).
type Manager struct {
	q           sqliteq.Querier
	provisioner snapshot.VolumeProvisioner
	logger      *slog.Logger
}

// NewManager constructs a Manager. provisioner may be nil.
func NewManager(q sqliteq.Querier, provisioner snapshot.VolumeProvisioner, logger *slog.Logger) *Manager {
	return &Manager{q: q, provisioner: provisioner, logger: logger}
}

// Create provisions a new blank volume. sizeBytes of 0 uses the default.
func (m *Manager) Create(ctx context.Context, name string, sizeBytes int64) (Volume, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Volume{}, errs.New(errs.CategoryInvalidArgument, "name is required")
	}
	if !validVolumeName(name) {
		return Volume{}, errs.Newf(errs.CategoryInvalidArgument,
			"invalid volume name %q (use lowercase letters, digits, '.', '_', '-')", name)
	}
	if m.provisioner == nil {
		return Volume{}, errs.New(errs.CategoryFailedPrecondition,
			"the configured snapshot driver cannot provision volumes (requires the firecracker runtime's ext4 snapshots)")
	}
	if sizeBytes == 0 {
		sizeBytes = DefaultSizeBytes
	}

	id, err := typeid.WithPrefix(idPrefix)
	if err != nil {
		return Volume{}, fmt.Errorf("generate volume id: %w", err)
	}
	path, err := m.provisioner.CreateVolume(ctx, id.String(), sizeBytes)
	if err != nil {
		return Volume{}, fmt.Errorf("provision volume: %w", err)
	}

	now := time.Now().Unix()
	row, err := m.q.CreateVolume(ctx, sqliteq.CreateVolumeParams{
		ID:        id.String(),
		Name:      name,
		Path:      path,
		SizeBytes: sizeBytes,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		_ = m.provisioner.DeleteVolume(context.WithoutCancel(ctx), id.String())
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Volume{}, errs.Newf(errs.CategoryConflict, "a volume named %q already exists", name)
		}
		return Volume{}, fmt.Errorf("record volume: %w", err)
	}
	return m.toDomain(ctx, row), nil
}

// Get returns the volume matching ref (an ID or name).
func (m *Manager) Get(ctx context.Context, ref string) (Volume, error) {
	row, err := m.lookup(ctx, ref)
	if err != nil {
		return Volume{}, err
	}
	return m.toDomain(ctx, row), nil
}

// List returns all volumes, newest first.
func (m *Manager) List(ctx context.Context) ([]Volume, error) {
	rows, err := m.q.ListVolumes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list volumes: %w", err)
	}
	out := make([]Volume, len(rows))
	for i, r := range rows {
		out[i] = m.toDomain(ctx, r)
	}
	return out, nil
}

// Delete destroys a volume and its data. Refused while attached to a session:
// disk holding real work is never deleted out from under anything (the
// storage asymmetry, DESIGN/M6) - detach by deleting the session first.
func (m *Manager) Delete(ctx context.Context, ref string) error {
	row, err := m.lookup(ctx, ref)
	if err != nil {
		return err
	}
	attached, err := m.q.ListSessionsByVolume(ctx, &row.ID)
	if err != nil {
		return fmt.Errorf("check volume attachment: %w", err)
	}
	if len(attached) > 0 {
		return errs.Newf(errs.CategoryFailedPrecondition,
			"volume %q is attached to session %q; delete that session first (its disk dies, the volume's data would too)",
			row.Name, attached[0].Name)
	}
	if m.provisioner != nil {
		if derr := m.provisioner.DeleteVolume(context.WithoutCancel(ctx), row.ID); derr != nil {
			return fmt.Errorf("delete volume storage: %w", derr)
		}
	}
	if _, derr := m.q.DeleteVolume(ctx, row.ID); derr != nil {
		return fmt.Errorf("delete volume: %w", derr)
	}
	return nil
}

// ResolveAttachable resolves ref to an attachable volume, refusing one already
// attached to another session (single-writer: one ext4 volume is mounted by at
// most one session).
func (m *Manager) ResolveAttachable(ctx context.Context, ref string) (id, path string, err error) {
	row, err := m.lookup(ctx, ref)
	if err != nil {
		return "", "", err
	}
	attached, err := m.q.ListSessionsByVolume(ctx, &row.ID)
	if err != nil {
		return "", "", fmt.Errorf("check volume attachment: %w", err)
	}
	if len(attached) > 0 {
		return "", "", errs.Newf(errs.CategoryConflict,
			"volume %q is already attached to session %q", row.Name, attached[0].Name)
	}
	return row.ID, row.Path, nil
}

// PathFor returns the backing path for a volume id (a session boot resolving
// its attached volume).
func (m *Manager) PathFor(ctx context.Context, id string) (string, error) {
	row, err := m.lookup(ctx, id)
	if err != nil {
		return "", err
	}
	return row.Path, nil
}

// NameFor returns the display name for a volume id.
func (m *Manager) NameFor(ctx context.Context, id string) (string, error) {
	row, err := m.lookup(ctx, id)
	if err != nil {
		return "", err
	}
	return row.Name, nil
}

func (m *Manager) lookup(ctx context.Context, ref string) (sqliteq.Volume, error) {
	row, err := m.q.GetVolumeByRef(ctx, ref)
	if errors.Is(err, sql.ErrNoRows) {
		return sqliteq.Volume{}, ErrNotFound
	}
	if err != nil {
		return sqliteq.Volume{}, fmt.Errorf("get volume: %w", err)
	}
	return row, nil
}

func (m *Manager) toDomain(ctx context.Context, r sqliteq.Volume) Volume {
	v := Volume{
		ID:        r.ID,
		Name:      r.Name,
		Path:      r.Path,
		SizeBytes: r.SizeBytes,
		UsedBytes: usedBytes(r.Path),
		CreatedAt: time.Unix(r.CreatedAt, 0),
	}
	if attached, err := m.q.ListSessionsByVolume(ctx, &r.ID); err == nil && len(attached) > 0 {
		v.AttachedSession = attached[0].Name
	}
	return v
}

// usedBytes is the real disk a sparse backing file occupies (allocated
// blocks), not its apparent size; 0 when it cannot be stat-ed.
func usedBytes(path string) int64 {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0
	}
	return st.Blocks * 512
}

// validVolumeName reports whether name is safe as a backing-file name.
func validVolumeName(name string) bool {
	if name == "" || len(name) > 64 || name[0] == '.' || name[0] == '-' {
		return false
	}
	for _, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}
