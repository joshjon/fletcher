package image

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"

	registryname "github.com/google/go-containerregistry/pkg/name"

	"github.com/joshjon/fletcher/internal/background"
	"github.com/joshjon/fletcher/internal/errs"
	"github.com/joshjon/fletcher/internal/snapshot"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

var requestID = regexp.MustCompile(`^[a-zA-Z0-9_-]{16,80}$`)

// Manager owns bounded, reconnectable registry imports and guarded deletion.
type Manager struct {
	ctx                       context.Context
	cancel                    context.CancelFunc
	wg                        sync.WaitGroup
	q                         *sqliteq.Queries
	dir, format, defaultImage string
	deleter                   snapshot.TemplateDeleter
	logger                    *slog.Logger
	mu                        sync.Mutex
	busy                      bool
	importFn                  func(context.Context, ImportOptions) (ImportResult, error)
}

// NewManager interrupts stale operation records before accepting new requests.
func NewManager(ctx context.Context, q *sqliteq.Queries, dir, format, defaultImage string, driver snapshot.Driver, logger *slog.Logger) (*Manager, error) {
	if err := q.InterruptImageImports(ctx, time.Now().Unix()); err != nil {
		return nil, fmt.Errorf("interrupt image imports: %w", err)
	}
	deleter, _ := driver.(snapshot.TemplateDeleter)
	ctx, cancel := context.WithCancel(ctx)
	return &Manager{ctx: ctx, cancel: cancel, q: q, dir: dir, format: format, defaultImage: defaultImage, deleter: deleter, logger: logger, importFn: ImportRegistry}, nil
}

// Start starts one import, or returns the existing operation for the same ID.
func (m *Manager) Start(ctx context.Context, id string, opts ImportOptions, update bool) (sqliteq.ImageImport, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !requestID.MatchString(id) {
		return sqliteq.ImageImport{}, errs.New(errs.CategoryInvalidArgument, "request_id must be 16-80 letters, digits, '_' or '-'")
	}
	if opts.Name == "" {
		opts.Name = DefaultName(opts.Ref)
	}
	if !ValidName(opts.Name) {
		return sqliteq.ImageImport{}, errs.New(errs.CategoryInvalidArgument, "invalid image name")
	}
	if update && opts.Ref != "" {
		return sqliteq.ImageImport{}, errs.New(errs.CategoryInvalidArgument, "update takes a template name, not a registry ref")
	}
	old, err := m.q.GetImageImport(ctx, id)
	if err == nil {
		if old.Name != opts.Name || old.IsUpdate != update || (!update && (old.Ref != opts.Ref || old.IsReplacement != opts.Force)) {
			return sqliteq.ImageImport{}, errs.New(errs.CategoryConflict, "request_id already used for a different import")
		}
		return old, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return sqliteq.ImageImport{}, err
	}
	if update {
		meta, found, err := ReadMeta(m.dir, opts.Name)
		if err != nil {
			return sqliteq.ImageImport{}, err
		}
		if !found || meta.Source == "" || meta.Digest == "" {
			return sqliteq.ImageImport{}, errs.New(errs.CategoryFailedPrecondition, "this image has no registry source; rebuild it from its original project")
		}
		opts.Ref, opts.Force = meta.Source, true
	}
	if m.format != "ext4" {
		return sqliteq.ImageImport{}, errs.New(errs.CategoryFailedPrecondition, "registry imports require ext4 snapshots")
	}
	if _, err := registryname.ParseReference(opts.Ref); err != nil {
		return sqliteq.ImageImport{}, errs.New(errs.CategoryInvalidArgument, "a valid registry image reference is required")
	}
	if m.busy {
		return sqliteq.ImageImport{}, errs.New(errs.CategoryConflict, "another image operation is running; wait for it to finish")
	}
	now := time.Now().Unix()
	if err := m.q.CreateImageImport(ctx, sqliteq.CreateImageImportParams{ID: id, Ref: opts.Ref, Name: opts.Name, IsReplacement: opts.Force, IsUpdate: update, CreatedAt: now, UpdatedAt: now}); err != nil {
		return sqliteq.ImageImport{}, err
	}
	m.busy = true
	opts.ImagesDir = m.dir
	m.wg.Add(1)
	//nolint:contextcheck // accepted import belongs to the daemon lifetime, not the request
	background.GoNamed(m.ctx, "image.import", func(ctx context.Context) { defer m.wg.Done(); m.run(ctx, id, opts) })
	return m.q.GetImageImport(ctx, id)
}

func (m *Manager) run(ctx context.Context, id string, opts ImportOptions) {
	state, message := "failed", "import interrupted; inspect the image before retrying"
	defer func() {
		// Record completion even during shutdown, but never persist credentials
		// or raw registry errors in the client-visible operation record.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := m.q.UpdateImageImport(cctx, sqliteq.UpdateImageImportParams{ID: id, State: state, Error: message, UpdatedAt: time.Now().Unix()}); err != nil {
			m.logger.Error("record image import result", "error", err)
		}
		m.mu.Lock()
		m.busy = false
		m.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if _, err := m.importFn(ctx, opts); err != nil {
		message = "import did not complete; check the registry reference, credentials, disk space and daemon logs"
		m.logger.Error("image import did not complete", "operation_id", id, "error", err)
		return
	}
	state, message = "succeeded", ""
}

// List returns the most recent persisted import operations, newest first.
func (m *Manager) List(ctx context.Context) ([]sqliteq.ImageImport, error) {
	return m.q.ListImageImports(ctx)
}

// Delete removes an unused template through the snapshot driver's capability.
func (m *Manager) Delete(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !ValidName(name) {
		return errs.New(errs.CategoryInvalidArgument, "invalid image name")
	}
	if m.busy {
		return errs.New(errs.CategoryConflict, "an image import is running; wait for it to finish")
	}
	if m.deleter == nil {
		return errs.New(errs.CategoryFailedPrecondition, "this snapshot driver cannot delete templates remotely")
	}
	defaults, err := m.q.ListSettings(ctx)
	if err != nil {
		return err
	}
	def := m.defaultImage
	for _, s := range defaults {
		if s.Key == "default_image" {
			def = s.Value
		}
	}
	n, err := m.q.CountImageReferences(ctx, name)
	if err != nil {
		return err
	}
	if name == def || n > 0 {
		return errs.New(errs.CategoryConflict, "image is referenced by the default setting, a session or an active job; remove those references first")
	}
	if err := m.deleter.DeleteTemplate(ctx, name); err != nil {
		return err
	}
	return RemoveMeta(m.dir, name)
}

// Close cancels imports and waits for completion records before SQLite closes.
func (m *Manager) Close() { m.cancel(); m.wg.Wait() }
