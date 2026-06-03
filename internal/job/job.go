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
	"strings"
	"time"

	"go.jetify.com/typeid"

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
)

// Trigger describes how a job is invoked.
type Trigger string

// Trigger values. These are the canonical strings stored in the database.
const (
	TriggerEphemeral   Trigger = "ephemeral"
	TriggerCron        Trigger = "cron"
	TriggerLongRunning Trigger = "long_running"
)

// idPrefix is the typeid prefix for job IDs (e.g., "job_01h...").
const idPrefix = "job"

// ErrNotFound is returned when a requested job ID does not exist.
var ErrNotFound = errors.New("job not found")

// Job is the daemon's domain representation of a job. It hides the int64
// epochs sqlc emits and the raw strings used at the SQL boundary.
type Job struct {
	ID           string
	Status       Status
	Trigger      Trigger
	Name         string
	Command      string
	Image        string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	StartedAt    *time.Time
	CompletedAt  *time.Time
	ErrorMessage string
	ExitCode     *int32
}

// CreateParams are the caller-supplied fields for a new job.
type CreateParams struct {
	Trigger Trigger
	Name    string
	Command string
	Image   string
}

// Service is the high-level façade over the jobs storage layer.
type Service struct {
	q sqliteq.Querier
}

// NewService wires a Service to a sqlc-generated querier (anything that
// satisfies sqliteq.Querier — *sqliteq.Queries in prod, a fake in tests).
func NewService(q sqliteq.Querier) *Service {
	return &Service{q: q}
}

// Create validates inputs, generates a typeid, and inserts a new queued job.
func (s *Service) Create(ctx context.Context, p CreateParams) (Job, error) {
	if err := p.validate(); err != nil {
		return Job{}, err
	}
	id, err := typeid.WithPrefix(idPrefix)
	if err != nil {
		return Job{}, fmt.Errorf("generate id: %w", err)
	}
	now := time.Now().Unix()
	row, err := s.q.CreateJob(ctx, sqliteq.CreateJobParams{
		ID:          id.String(),
		Status:      string(StatusQueued),
		TriggerKind: string(p.Trigger),
		Name:        p.Name,
		Command:     p.Command,
		Image:       p.Image,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	if err != nil {
		return Job{}, fmt.Errorf("create job: %w", err)
	}
	return jobFromRow(row), nil
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
	return jobFromRow(row), nil
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
		return mapRows(rows), nil
	}
	rows, err := s.q.ListJobs(ctx, sqliteq.ListJobsParams{
		Limit:  int64(p.Limit),
		Offset: int64(p.Offset),
	})
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	return mapRows(rows), nil
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
// missing entirely.
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
	return n > 0, nil
}

func (p CreateParams) validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("name is required")
	}
	if strings.TrimSpace(p.Command) == "" {
		return errors.New("command is required")
	}
	if strings.TrimSpace(p.Image) == "" {
		return errors.New("image is required")
	}
	switch p.Trigger {
	case TriggerEphemeral, TriggerCron, TriggerLongRunning:
		return nil
	case "":
		return errors.New("trigger is required")
	default:
		return fmt.Errorf("invalid trigger %q", p.Trigger)
	}
}

func mapRows(rows []sqliteq.Job) []Job {
	out := make([]Job, len(rows))
	for i, r := range rows {
		out[i] = jobFromRow(r)
	}
	return out
}

func jobFromRow(r sqliteq.Job) Job {
	return Job{
		ID:           r.ID,
		Status:       Status(r.Status),
		Trigger:      Trigger(r.TriggerKind),
		Name:         r.Name,
		Command:      r.Command,
		Image:        r.Image,
		CreatedAt:    time.Unix(r.CreatedAt, 0).UTC(),
		UpdatedAt:    time.Unix(r.UpdatedAt, 0).UTC(),
		StartedAt:    timePtrFromUnix(r.StartedAt),
		CompletedAt:  timePtrFromUnix(r.CompletedAt),
		ErrorMessage: derefString(r.ErrorMessage),
		ExitCode:     int32PtrFromInt64(r.ExitCode),
	}
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
