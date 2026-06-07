// Package job owns the daemon's job model and the high-level operations
// (create, list, cancel) that the API and CLI invoke. It mediates between
// the storage layer (sqlc-generated queries) and consumers, exposing typed
// statuses and triggers and absorbing the int64-epoch ↔ time.Time conversion.
package job

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"go.jetify.com/typeid"

	"github.com/joshjon/fletcher/internal/egress"
	"github.com/joshjon/fletcher/internal/errs"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

// Status is the lifecycle state of a job.
type Status string

// Status values. These are the canonical strings stored in the database
// (constrained by the schema's CHECK clause).
const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
	// StatusScheduled is a cron job definition at rest between runs.
	StatusScheduled Status = "scheduled"
)

// Trigger describes how a job is invoked.
type Trigger string

// Trigger values. These are the canonical strings stored in the database.
const (
	TriggerEphemeral   Trigger = "ephemeral"
	TriggerCron        Trigger = "cron"
	TriggerLongRunning Trigger = "long_running"
)

// idPrefix is the typeid prefix for job IDs (e.g. "job_01h...").
const idPrefix = "job"

// ErrNotFound is returned when a requested job ID does not exist. It is
// categorised so the Connect interceptor maps it to CodeNotFound without
// the API handler needing to do anything.
var ErrNotFound = errs.New(errs.CategoryNotFound, "job not found")

// Job is the daemon's domain representation of a job. It hides the int64
// epochs sqlc emits and the raw strings used at the SQL boundary.
type Job struct {
	ID           string
	Status       Status
	Trigger      Trigger
	Name         string
	Command      string
	Image        string
	Credentials  []string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	StartedAt    *time.Time
	CompletedAt  *time.Time
	ErrorMessage string
	ExitCode     *int32
	// Schedule is the cron expression for a cron job (empty otherwise).
	Schedule string
	// NextRunAt is when a scheduled cron job next fires.
	NextRunAt *time.Time
	// ParentID links a run to the cron definition that spawned it.
	ParentID *string
	// EgressPolicy gates the fork's outbound network: "none" | "allowlist" |
	// "open".
	EgressPolicy string
}

// CreateParams are the caller-supplied fields for a new job.
type CreateParams struct {
	Trigger     Trigger
	Name        string
	Command     string
	Image       string
	Credentials []string
	// Schedule is required when Trigger is cron: a cron expression.
	Schedule string
	// EgressPolicy is "none"|"allowlist"|"open"; empty resolves to the service's
	// configured default.
	EgressPolicy string
}

// Coordinator is what Service needs from the running supervisor: a way to
// nudge it after a new queued job appears, and a way to abort a job that's
// already running. It is optional - passing nil yields a CRUD-only service.
type Coordinator interface {
	Notify()
	CancelRunning(jobID string) bool
}

// Service is the high-level façade over the jobs storage layer.
type Service struct {
	q             sqliteq.Querier
	sup           Coordinator
	defaultImage  string
	defaultEgress string
}

// NewService wires a Service to a sqlc-generated querier (anything that
// satisfies sqliteq.Querier - *sqliteq.Queries in prod, a fake in tests).
// The supervisor argument may be nil for tests that only exercise CRUD.
// defaultImage is used when a job is created with no image (empty makes the
// image required).
func NewService(q sqliteq.Querier, sup Coordinator, defaultImage, defaultEgress string) *Service {
	return &Service{q: q, sup: sup, defaultImage: defaultImage, defaultEgress: defaultEgress}
}

// Create validates inputs, generates a typeid, and inserts a new queued job.
func (s *Service) Create(ctx context.Context, p CreateParams) (Job, error) {
	// Fall back to the configured default image when none is given, before
	// validation (which still rejects an empty image when no default is set).
	if strings.TrimSpace(p.Image) == "" {
		p.Image = s.defaultImage
	}
	if strings.TrimSpace(p.EgressPolicy) == "" {
		p.EgressPolicy = s.defaultEgress
	}
	p.EgressPolicy = egress.Normalize(p.EgressPolicy)
	if err := p.validate(); err != nil {
		return Job{}, err
	}
	// A name is just a human-readable label (the ID is the identifier); default
	// it to the command's program name so `job create --command "claude ..."`
	// needs no --name.
	if strings.TrimSpace(p.Name) == "" {
		p.Name = defaultJobName(p.Command)
	}
	creds, err := normaliseCredentials(p.Credentials)
	if err != nil {
		return Job{}, err
	}
	credsEncoded, err := encodeCredentials(creds)
	if err != nil {
		return Job{}, err
	}
	id, err := typeid.WithPrefix(idPrefix)
	if err != nil {
		return Job{}, fmt.Errorf("generate id: %w", err)
	}

	// A cron job is a definition that rests in "scheduled" until its next fire;
	// every other trigger is queued to run immediately.
	now := time.Now()
	status := StatusQueued
	var nextRun *int64
	if p.Trigger == TriggerCron {
		sched, perr := ParseSchedule(p.Schedule)
		if perr != nil {
			return Job{}, perr
		}
		status = StatusScheduled
		next := sched.Next(now).Unix()
		nextRun = &next
	}

	nowUnix := now.Unix()
	row, err := s.q.CreateJob(ctx, sqliteq.CreateJobParams{
		ID:           id.String(),
		Status:       string(status),
		TriggerKind:  string(p.Trigger),
		Name:         p.Name,
		Command:      p.Command,
		Image:        p.Image,
		Credentials:  credsEncoded,
		CreatedAt:    nowUnix,
		UpdatedAt:    nowUnix,
		Schedule:     p.Schedule,
		NextRunAt:    nextRun,
		EgressPolicy: p.EgressPolicy,
	})
	if err != nil {
		return Job{}, fmt.Errorf("create job: %w", err)
	}
	if s.sup != nil {
		s.sup.Notify()
	}
	return jobFromRow(row)
}

// Get returns the job with the given ID. Returns ErrNotFound if missing.
func (s *Service) Get(ctx context.Context, id string) (Job, error) {
	row, err := s.q.GetJob(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("get job: %w", err)
	}
	return jobFromRow(row)
}

// ListParams paginates the jobs table; StatusFilter is optional ("" = all).
type ListParams struct {
	Limit        int32
	Offset       int32
	StatusFilter Status
}

// List returns jobs newest-first, applying the optional status filter.
func (s *Service) List(ctx context.Context, p ListParams) ([]Job, error) {
	if p.Limit <= 0 {
		p.Limit = 50
	}
	if p.StatusFilter != "" {
		rows, err := s.q.ListJobsByStatus(ctx, sqliteq.ListJobsByStatusParams{
			Status: string(p.StatusFilter),
			Limit:  int64(p.Limit),
			Offset: int64(p.Offset),
		})
		if err != nil {
			return nil, fmt.Errorf("list jobs by status: %w", err)
		}
		return mapRows(rows)
	}
	rows, err := s.q.ListJobs(ctx, sqliteq.ListJobsParams{
		Limit:  int64(p.Limit),
		Offset: int64(p.Offset),
	})
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	return mapRows(rows)
}

// Count returns the total number of jobs (optionally filtered by status).
func (s *Service) Count(ctx context.Context, status Status) (int64, error) {
	if status != "" {
		return s.q.CountJobsByStatus(ctx, string(status))
	}
	return s.q.CountJobs(ctx)
}

// Cancel transitions a queued or running job to cancelled. Returns true if a
// row was updated; false if the job was already in a terminal state or
// missing entirely. When the supervisor is wired and the job is currently
// running, the corresponding process is also signalled to stop.
func (s *Service) Cancel(ctx context.Context, id string) (bool, error) {
	now := time.Now().Unix()
	n, err := s.q.CancelJob(ctx, sqliteq.CancelJobParams{
		CompletedAt: &now,
		UpdatedAt:   now,
		ID:          id,
	})
	if err != nil {
		return false, fmt.Errorf("cancel job: %w", err)
	}
	if n > 0 && s.sup != nil {
		s.sup.CancelRunning(id)
	}
	return n > 0, nil
}

func (p CreateParams) validate() error {
	if strings.TrimSpace(p.Command) == "" {
		return errs.New(errs.CategoryInvalidArgument, "command is required")
	}
	if strings.TrimSpace(p.Image) == "" {
		return errs.New(errs.CategoryInvalidArgument, "image is required")
	}
	switch p.Trigger {
	case TriggerEphemeral, TriggerLongRunning:
		if strings.TrimSpace(p.Schedule) != "" {
			return errs.Newf(errs.CategoryInvalidArgument, "schedule is only valid for a cron job, not %q", p.Trigger)
		}
		return nil
	case TriggerCron:
		if _, err := ParseSchedule(p.Schedule); err != nil {
			return err
		}
		return nil
	case "":
		return errs.New(errs.CategoryInvalidArgument, "trigger is required")
	default:
		return errs.Newf(errs.CategoryInvalidArgument, "invalid trigger %q", p.Trigger)
	}
}

// defaultJobName derives a human-readable name from the command's program name
// (the first token's base name), e.g. "claude -p ..." -> "claude". Falls back to
// "job" for an empty or path-only command.
func defaultJobName(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return "job"
	}
	base := filepath.Base(fields[0])
	if base == "" || base == "." || base == "/" {
		return "job"
	}
	return base
}

// ParseSchedule parses a cron expression (5-field standard form, or a macro
// like @hourly / @daily) into a schedule that can compute its next fire time.
func ParseSchedule(spec string) (cron.Schedule, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, errs.New(errs.CategoryInvalidArgument, "a cron job requires a schedule, e.g. --schedule '*/5 * * * *'")
	}
	sched, err := cron.ParseStandard(spec)
	if err != nil {
		return nil, errs.Newf(errs.CategoryInvalidArgument, "invalid cron schedule %q: %v", spec, err)
	}
	return sched, nil
}

func mapRows(rows []sqliteq.Job) ([]Job, error) {
	out := make([]Job, len(rows))
	for i, r := range rows {
		j, err := jobFromRow(r)
		if err != nil {
			return nil, err
		}
		out[i] = j
	}
	return out, nil
}

func jobFromRow(r sqliteq.Job) (Job, error) {
	creds, err := decodeCredentials(r.Credentials)
	if err != nil {
		return Job{}, err
	}
	return Job{
		ID:           r.ID,
		Status:       Status(r.Status),
		Trigger:      Trigger(r.TriggerKind),
		Name:         r.Name,
		Command:      r.Command,
		Image:        r.Image,
		Credentials:  creds,
		CreatedAt:    time.Unix(r.CreatedAt, 0).UTC(),
		UpdatedAt:    time.Unix(r.UpdatedAt, 0).UTC(),
		StartedAt:    timePtrFromUnix(r.StartedAt),
		CompletedAt:  timePtrFromUnix(r.CompletedAt),
		ErrorMessage: derefString(r.ErrorMessage),
		ExitCode:     int32PtrFromInt64(r.ExitCode),
		Schedule:     r.Schedule,
		NextRunAt:    timePtrFromUnix(r.NextRunAt),
		ParentID:     r.ParentID,
		EgressPolicy: r.EgressPolicy,
	}, nil
}

func timePtrFromUnix(p *int64) *time.Time {
	if p == nil {
		return nil
	}
	t := time.Unix(*p, 0).UTC()
	return &t
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func int32PtrFromInt64(p *int64) *int32 {
	if p == nil {
		return nil
	}
	// Process exit codes fit in int32 (POSIX uses 0-255; SQLite still stores
	// in INTEGER (8 bytes), so we narrow on read).
	v := int32(*p) //nolint:gosec // bounded by POSIX exit-code range
	return &v
}
