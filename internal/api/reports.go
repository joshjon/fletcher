package api

import (
	"context"

	"connectrpc.com/connect"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
	"github.com/joshjon/fletcher/internal/report"
)

// ReportsBackend is what the ReportService handler needs from the report
// service.
type ReportsBackend interface {
	Get(ctx context.Context, id string) (report.Report, error)
	List(ctx context.Context, limit, offset int32) ([]report.Report, error)
}

// ReportsService implements fletcherv1connect.ReportServiceHandler.
type ReportsService struct {
	fletcherv1connect.UnimplementedReportServiceHandler
	backend ReportsBackend
}

// NewReportsService wires the service to its backend.
func NewReportsService(backend ReportsBackend) *ReportsService {
	return &ReportsService{backend: backend}
}

// GetReport fetches one report by id.
func (s *ReportsService) GetReport(ctx context.Context, req *connect.Request[fletcherv1.GetReportRequest]) (*connect.Response[fletcherv1.GetReportResponse], error) {
	r, err := s.backend.Get(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.GetReportResponse{Report: reportToProto(r)}), nil
}

// ListReports returns reports, newest first.
func (s *ReportsService) ListReports(ctx context.Context, req *connect.Request[fletcherv1.ListReportsRequest]) (*connect.Response[fletcherv1.ListReportsResponse], error) {
	reports, err := s.backend.List(ctx, req.Msg.GetLimit(), req.Msg.GetOffset())
	if err != nil {
		return nil, err
	}
	out := make([]*fletcherv1.Report, len(reports))
	for i, r := range reports {
		out[i] = reportToProto(r)
	}
	return connect.NewResponse(&fletcherv1.ListReportsResponse{Reports: out}), nil
}

func reportToProto(r report.Report) *fletcherv1.Report {
	return &fletcherv1.Report{
		Id:         r.ID,
		SourceType: r.SourceType,
		SourceId:   r.SourceID,
		SourceName: r.SourceName,
		Title:      r.Title,
		Summary:    r.Summary,
		Status:     r.Status,
		Link:       r.Link,
		CreatedAt:  r.CreatedAt.Unix(),
	}
}
