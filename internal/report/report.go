// Package report stores structured results agents post via the report MCP
// tool ("web app ready", "scrape finished - 3 price drops"). A report becomes
// push-notification content the moment it is created and stays queryable over
// RPC/CLI; the dedicated feed UI is parked until a usage signal (ROADMAP,
// mobile-first wave).
package report

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.jetify.com/typeid"

	"github.com/joshjon/fletcher/internal/errs"
	"github.com/joshjon/fletcher/internal/events"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

// idPrefix is the typeid prefix for report IDs.
const idPrefix = "report"

// Statuses a report can carry (its tone, not a lifecycle).
const (
	StatusInfo    = "info"
	StatusSuccess = "success"
	StatusWarning = "warning"
	StatusError   = "error"
)

// ErrNotFound is returned when a report ID does not exist.
var ErrNotFound = errs.New(errs.CategoryNotFound, "report not found")

// Report is the domain shape of a posted report.
type Report struct {
	ID         string
	SourceType string
	SourceID   string
	SourceName string
	Title      string
	Summary    string
	Status     string
	Link       string
	CreatedAt  time.Time
}

// CreateParams are the inputs to a new report.
type CreateParams struct {
	// SourceType/SourceID/SourceName tie the report to what produced it
	// ("session" or "job"; empty when unattributed).
	SourceType string
	SourceID   string
	SourceName string
	// Title is required; Summary, Status (default "info"), and Link optional.
	Title   string
	Summary string
	Status  string
	Link    string
}

// Service is the reports API.
type Service struct {
	q      sqliteq.Querier
	events events.Sink
}

// NewService wires a Service to its store. events may be nil.
func NewService(q sqliteq.Querier, sink events.Sink) *Service {
	return &Service{q: q, events: sink}
}

// Create stores a report and announces it on the event bus (which is what
// triggers the push notification).
func (s *Service) Create(ctx context.Context, p CreateParams) (Report, error) {
	if strings.TrimSpace(p.Title) == "" {
		return Report{}, errs.New(errs.CategoryInvalidArgument, "title is required")
	}
	status := p.Status
	if status == "" {
		status = StatusInfo
	}
	switch status {
	case StatusInfo, StatusSuccess, StatusWarning, StatusError:
	default:
		return Report{}, errs.Newf(errs.CategoryInvalidArgument,
			"invalid status %q (want info | success | warning | error)", p.Status)
	}

	id, err := typeid.WithPrefix(idPrefix)
	if err != nil {
		return Report{}, fmt.Errorf("generate report id: %w", err)
	}
	row, err := s.q.CreateReport(ctx, sqliteq.CreateReportParams{
		ID:         id.String(),
		SourceType: p.SourceType,
		SourceID:   p.SourceID,
		SourceName: p.SourceName,
		Title:      p.Title,
		Summary:    p.Summary,
		Status:     status,
		Link:       p.Link,
		CreatedAt:  time.Now().Unix(),
	})
	if err != nil {
		return Report{}, fmt.Errorf("create report: %w", err)
	}
	if s.events != nil {
		s.events.Publish(events.Event{Type: events.TypeReport, Action: "created", ID: row.ID, Name: row.Title})
	}
	return fromRow(row), nil
}

// Get returns one report by id.
func (s *Service) Get(ctx context.Context, id string) (Report, error) {
	row, err := s.q.GetReport(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Report{}, ErrNotFound
	}
	if err != nil {
		return Report{}, fmt.Errorf("get report: %w", err)
	}
	return fromRow(row), nil
}

// List returns reports newest-first.
func (s *Service) List(ctx context.Context, limit, offset int32) ([]Report, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.q.ListReports(ctx, sqliteq.ListReportsParams{
		Limit:  int64(limit),
		Offset: int64(offset),
	})
	if err != nil {
		return nil, fmt.Errorf("list reports: %w", err)
	}
	out := make([]Report, len(rows))
	for i, r := range rows {
		out[i] = fromRow(r)
	}
	return out, nil
}

func fromRow(r sqliteq.Report) Report {
	return Report{
		ID:         r.ID,
		SourceType: r.SourceType,
		SourceID:   r.SourceID,
		SourceName: r.SourceName,
		Title:      r.Title,
		Summary:    r.Summary,
		Status:     r.Status,
		Link:       r.Link,
		CreatedAt:  time.Unix(r.CreatedAt, 0),
	}
}
