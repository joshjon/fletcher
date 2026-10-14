package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/joshjon/fletcher/internal/approval"
	"github.com/joshjon/fletcher/internal/egress"
	"github.com/joshjon/fletcher/internal/forkauth"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

// HTTPWrites coordinates approval-gated, persistent at-most-once HTTP side effects.
type HTTPWrites struct {
	q         sqliteq.Querier
	approvals ApprovalBackend
	client    *http.Client
	allowlist egress.Allowlist
	locks     sync.Map
}

// HTTPWritesOptions supplies the collaborators for HTTP side effects.
type HTTPWritesOptions struct {
	Client    *http.Client
	Approvals ApprovalBackend
	Allowlist egress.Allowlist
}

// NewHTTPWrites marks interrupted sends as unknown rather than automatically repeating them.
func NewHTTPWrites(ctx context.Context, q sqliteq.Querier, opts HTTPWritesOptions) (*HTTPWrites, error) {
	if opts.Approvals == nil {
		return nil, errors.New("http writes require an approval backend")
	}
	if err := q.InterruptEgressOperations(ctx, time.Now().Unix()); err != nil {
		return nil, fmt.Errorf("interrupt egress operations: %w", err)
	}
	if opts.Client == nil {
		opts.Client = NewEgressHTTPClient(30 * time.Second)
	}
	baseClient := *opts.Client
	if transport, ok := baseClient.Transport.(*http.Transport); ok {
		transport = transport.Clone()
		transport.DisableKeepAlives = true
		baseClient.Transport = transport
	}
	client := policyHTTPClient(&baseClient, opts.Allowlist)
	// A write approval is for one destination, not a chain of redirected writes.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPWrites{q: q, approvals: opts.Approvals, client: client, allowlist: opts.Allowlist}, nil
}

// Execute requires a stable key and consent, replaying completed results but refusing ambiguous retries.
func (s *HTTPWrites) Execute(ctx context.Context, req *http.Request, key, justification string) (string, error) {
	req = req.Clone(ctx)
	if !validOperationKey(key) || strings.TrimSpace(justification) == "" {
		return "", errors.New("http writes require an idempotency_key (16-80 letters, digits, '_' or '-') and justification")
	}
	if err := (policyTransport{allowlist: s.allowlist}).check(req); err != nil {
		return "", err
	}
	caller, _ := forkauth.FromContext(ctx)
	hash, err := requestHash(req)
	if err != nil {
		return "", err
	}
	v, _ := s.locks.LoadOrStore(caller.ID+"\x00"+key, make(chan struct{}, 1))
	sem := v.(chan struct{})
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	row, err := s.operation(ctx, caller.ID, key, hash)
	if err != nil {
		return "", err
	}
	switch row.State {
	case "succeeded":
		return row.Result, nil
	case "failed":
		return "", errors.New("this operation was denied or expired; a new attempt needs a new key and approval")
	case "executing", "unknown":
		return "", errors.New("request outcome is unknown; inspect the destination before considering a new operation")
	}
	if err := s.approve(ctx, row, req, justification); err != nil {
		return "", err
	}
	n, err := s.q.ClaimEgressOperation(ctx, sqliteq.ClaimEgressOperationParams{CallerID: caller.ID, OperationKey: key, UpdatedAt: time.Now().Unix()})
	if err != nil || n != 1 {
		return "", errors.New("could not claim egress operation; inspect its result before retrying")
	}
	req.Header.Set("Idempotency-Key", fmt.Sprintf("%x", sha256.Sum256([]byte(caller.ID+"\x00"+key))))
	// The standard transport treats Idempotency-Key as permission to retry.
	// Arbitrary servers need not honor it, so never supply a replayable body.
	req.GetBody = nil
	if req.Body == nil || req.Body == http.NoBody {
		req.Body = io.NopCloser(strings.NewReader(""))
		req.ContentLength = -1
	}
	result, sendErr := performHTTPRequest(s.client, req)
	state := "succeeded"
	if sendErr != nil {
		state, result = "unknown", ""
	}
	if err := s.record(ctx, row, state, result); err != nil {
		return "", errors.New("could not record request outcome; inspect the destination before retrying")
	}
	if sendErr != nil {
		return "", errors.New("request outcome is unknown; inspect the destination before considering a new operation")
	}
	return result, nil
}

func (s *HTTPWrites) operation(ctx context.Context, caller, key, hash string) (sqliteq.McpEgressOperation, error) {
	if err := s.q.CreateEgressOperation(ctx, sqliteq.CreateEgressOperationParams{CallerID: caller, OperationKey: key, RequestHash: hash, UpdatedAt: time.Now().Unix()}); err != nil {
		return sqliteq.McpEgressOperation{}, fmt.Errorf("record egress operation: %w", err)
	}
	row, err := s.q.GetEgressOperation(ctx, sqliteq.GetEgressOperationParams{CallerID: caller, OperationKey: key})
	if err != nil {
		return row, fmt.Errorf("get egress operation: %w", err)
	}
	if row.RequestHash != hash {
		return row, errors.New("idempotency_key already used for a different request")
	}
	return row, nil
}

func (s *HTTPWrites) approve(ctx context.Context, row sqliteq.McpEgressOperation, req *http.Request, justification string) error {
	id := row.ApprovalID
	if id == nil {
		// Never put URL credentials or query tokens on approval cards.
		a, err := s.approvals.Create(ctx, approval.CreateParams{
			Action:        fmt.Sprintf("http %s %s://%s%s", req.Method, req.URL.Scheme, req.URL.Host, req.URL.EscapedPath()),
			Justification: justification, Requester: "fork:" + row.CallerID, TTL: time.Hour,
		})
		if err != nil {
			return fmt.Errorf("create egress approval: %w", err)
		}
		id = &a.ID
		if err := s.q.UpdateEgressApproval(ctx, sqliteq.UpdateEgressApprovalParams{CallerID: row.CallerID, OperationKey: row.OperationKey, ApprovalID: id, UpdatedAt: time.Now().Unix()}); err != nil {
			return fmt.Errorf("record egress approval: %w", err)
		}
	}
	a, err := s.approvals.Wait(ctx, *id)
	if err != nil {
		return fmt.Errorf("wait for egress approval: %w", err)
	}
	if a.Status != approval.StatusApproved {
		if err := s.record(ctx, row, "failed", ""); err != nil {
			return fmt.Errorf("record egress denial: %w", err)
		}
		return errors.New("http write was not approved")
	}
	return nil
}

func (s *HTTPWrites) record(ctx context.Context, row sqliteq.McpEgressOperation, state, result string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return s.q.UpdateEgressResult(ctx, sqliteq.UpdateEgressResultParams{CallerID: row.CallerID, OperationKey: row.OperationKey, State: state, Result: result, UpdatedAt: time.Now().Unix()})
}

func validOperationKey(key string) bool {
	if len(key) < 16 || len(key) > 80 {
		return false
	}
	for _, r := range key {
		valid := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-'
		if !valid {
			return false
		}
	}
	return true
}

func requestHash(req *http.Request) (string, error) {
	var body []byte
	if req.GetBody != nil {
		r, err := req.GetBody()
		if err != nil {
			return "", fmt.Errorf("read request for idempotency: %w", err)
		}
		defer func() { _ = r.Close() }()
		body, err = io.ReadAll(io.LimitReader(r, 1<<20+1))
		if err != nil || len(body) > 1<<20 {
			return "", errors.New("http write body exceeds the request limit")
		}
	}
	data, err := json.Marshal(struct {
		Method  string
		URL     string
		Headers http.Header
		Body    []byte
	}{req.Method, req.URL.String(), req.Header, body})
	if err != nil {
		return "", fmt.Errorf("hash egress request: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func performHTTPRequest(client *http.Client, req *http.Request) (string, error) {
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("perform request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxEgressBody))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	return fmt.Sprintf("status=%d\n%s", resp.StatusCode, string(body)), nil
}
