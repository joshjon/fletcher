package api

import (
	"context"

	"connectrpc.com/connect"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
	"github.com/joshjon/fletcher/internal/job"
)

// JobsBackend is the consumer-defined interface the JobsService handler
// needs from the domain layer. Production wires it to *job.Service; tests
// can drop in a fake.
type JobsBackend interface {
	Create(ctx context.Context, p job.CreateParams) (job.Job, error)
	Get(ctx context.Context, id string) (job.Job, error)
	List(ctx context.Context, p job.ListParams) ([]job.Job, error)
	Count(ctx context.Context, status job.Status) (int64, error)
	Cancel(ctx context.Context, id string) (bool, error)
}

// JobsService implements fletcherv1connect.JobServiceHandler.
type JobsService struct {
	fletcherv1connect.UnimplementedJobServiceHandler
	svc JobsBackend
}

// NewJobsService wires a JobsService to a domain backend.
func NewJobsService(svc JobsBackend) *JobsService {
	return &JobsService{svc: svc}
}

// CreateJob enqueues a new job. Validation errors are categorised inside
// the domain layer; the ErrorInterceptor maps them to the wire code.
func (s *JobsService) CreateJob(ctx context.Context, req *connect.Request[fletcherv1.CreateJobRequest]) (*connect.Response[fletcherv1.CreateJobResponse], error) {
	m := req.Msg
	j, err := s.svc.Create(ctx, job.CreateParams{
		Trigger:      triggerFromProto(m.GetTrigger()),
		Name:         m.GetName(),
		Command:      m.GetCommand(),
		Image:        m.GetImage(),
		Credentials:  m.GetCredentials(),
		Schedule:     m.GetSchedule(),
		EgressPolicy: m.GetEgressPolicy(),
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.CreateJobResponse{Job: jobToProto(j)}), nil
}

// GetJob fetches a single job by ID. Categorised job.ErrNotFound maps to
// CodeNotFound via the ErrorInterceptor.
func (s *JobsService) GetJob(ctx context.Context, req *connect.Request[fletcherv1.GetJobRequest]) (*connect.Response[fletcherv1.GetJobResponse], error) {
	j, err := s.svc.Get(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.GetJobResponse{Job: jobToProto(j)}), nil
}

// ListJobs returns a page of jobs, newest first, with an optional status filter.
func (s *JobsService) ListJobs(ctx context.Context, req *connect.Request[fletcherv1.ListJobsRequest]) (*connect.Response[fletcherv1.ListJobsResponse], error) {
	m := req.Msg
	filter := statusFromProto(m.GetStatusFilter())
	jobs, err := s.svc.List(ctx, job.ListParams{
		Limit:        m.GetLimit(),
		Offset:       m.GetOffset(),
		StatusFilter: filter,
	})
	if err != nil {
		return nil, err
	}
	total, err := s.svc.Count(ctx, filter)
	if err != nil {
		return nil, err
	}
	protos := make([]*fletcherv1.Job, len(jobs))
	for i, j := range jobs {
		protos[i] = jobToProto(j)
	}
	return connect.NewResponse(&fletcherv1.ListJobsResponse{Jobs: protos, Total: total}), nil
}

// CancelJob transitions a queued or running job to cancelled.
func (s *JobsService) CancelJob(ctx context.Context, req *connect.Request[fletcherv1.CancelJobRequest]) (*connect.Response[fletcherv1.CancelJobResponse], error) {
	ok, err := s.svc.Cancel(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.CancelJobResponse{Cancelled: ok}), nil
}

// --- proto ⇄ domain mapping ---

func triggerFromProto(p fletcherv1.JobTrigger) job.Trigger {
	switch p {
	case fletcherv1.JobTrigger_JOB_TRIGGER_EPHEMERAL:
		return job.TriggerEphemeral
	case fletcherv1.JobTrigger_JOB_TRIGGER_CRON:
		return job.TriggerCron
	case fletcherv1.JobTrigger_JOB_TRIGGER_LONG_RUNNING:
		return job.TriggerLongRunning
	}
	return ""
}

func triggerToProto(t job.Trigger) fletcherv1.JobTrigger {
	switch t {
	case job.TriggerEphemeral:
		return fletcherv1.JobTrigger_JOB_TRIGGER_EPHEMERAL
	case job.TriggerCron:
		return fletcherv1.JobTrigger_JOB_TRIGGER_CRON
	case job.TriggerLongRunning:
		return fletcherv1.JobTrigger_JOB_TRIGGER_LONG_RUNNING
	}
	return fletcherv1.JobTrigger_JOB_TRIGGER_UNSPECIFIED
}

func statusFromProto(p fletcherv1.JobStatus) job.Status {
	switch p {
	case fletcherv1.JobStatus_JOB_STATUS_QUEUED:
		return job.StatusQueued
	case fletcherv1.JobStatus_JOB_STATUS_RUNNING:
		return job.StatusRunning
	case fletcherv1.JobStatus_JOB_STATUS_SUCCEEDED:
		return job.StatusSucceeded
	case fletcherv1.JobStatus_JOB_STATUS_FAILED:
		return job.StatusFailed
	case fletcherv1.JobStatus_JOB_STATUS_CANCELLED:
		return job.StatusCancelled
	}
	return ""
}

func statusToProto(s job.Status) fletcherv1.JobStatus {
	switch s {
	case job.StatusQueued:
		return fletcherv1.JobStatus_JOB_STATUS_QUEUED
	case job.StatusRunning:
		return fletcherv1.JobStatus_JOB_STATUS_RUNNING
	case job.StatusSucceeded:
		return fletcherv1.JobStatus_JOB_STATUS_SUCCEEDED
	case job.StatusFailed:
		return fletcherv1.JobStatus_JOB_STATUS_FAILED
	case job.StatusCancelled:
		return fletcherv1.JobStatus_JOB_STATUS_CANCELLED
	case job.StatusScheduled:
		return fletcherv1.JobStatus_JOB_STATUS_SCHEDULED
	}
	return fletcherv1.JobStatus_JOB_STATUS_UNSPECIFIED
}

func jobToProto(j job.Job) *fletcherv1.Job {
	out := &fletcherv1.Job{
		Id:           j.ID,
		Status:       statusToProto(j.Status),
		Trigger:      triggerToProto(j.Trigger),
		Name:         j.Name,
		Command:      j.Command,
		Image:        j.Image,
		Credentials:  j.Credentials,
		CreatedAt:    j.CreatedAt.Unix(),
		UpdatedAt:    j.UpdatedAt.Unix(),
		ErrorMessage: j.ErrorMessage,
		Schedule:     j.Schedule,
		ParentId:     j.ParentID,
		EgressPolicy: j.EgressPolicy,
	}
	if j.NextRunAt != nil {
		t := j.NextRunAt.Unix()
		out.NextRunAt = &t
	}
	if j.StartedAt != nil {
		t := j.StartedAt.Unix()
		out.StartedAt = &t
	}
	if j.CompletedAt != nil {
		t := j.CompletedAt.Unix()
		out.CompletedAt = &t
	}
	if j.ExitCode != nil {
		v := *j.ExitCode
		out.ExitCode = &v
	}
	return out
}
