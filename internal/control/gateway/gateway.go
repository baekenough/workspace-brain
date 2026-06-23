// Package gateway coordinates surface-neutral requests before they reach the data core.
package gateway

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/sangyi/workspace-brain/internal/control/jobs"
	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

// Authorizer decides whether a principal can perform an action in a tenant scope.
type Authorizer interface {
	Authorize(ctx context.Context, req AuthorizationRequest) error
}

// AuthorizationRequest is the gateway authorization input.
type AuthorizationRequest struct {
	Principal  brainapi.Principal
	TenantID   brainapi.TenantID
	BindingKey brainapi.BindingKey
	Action     brainapi.Action
}

// TenantIDGenerator creates tenant IDs for project creation.
type TenantIDGenerator interface {
	NewTenantID() brainapi.TenantID
}

// JobIDGenerator creates job IDs for asynchronous work.
type JobIDGenerator interface {
	NewJobID() brainapi.JobID
}

// Gateway is the control-plane boundary. It knows surfaces and identities, while Core does not.
type Gateway struct {
	core       brainapi.Core
	authorizer Authorizer
	jobs       *jobs.Store
	tenantIDs  TenantIDGenerator
	jobIDs     JobIDGenerator
}

// Option configures a Gateway.
type Option func(*Gateway)

// WithTenantIDGenerator overrides tenant ID generation.
func WithTenantIDGenerator(gen TenantIDGenerator) Option {
	return func(g *Gateway) {
		if gen != nil {
			g.tenantIDs = gen
		}
	}
}

// WithJobIDGenerator overrides job ID generation.
func WithJobIDGenerator(gen JobIDGenerator) Option {
	return func(g *Gateway) {
		if gen != nil {
			g.jobIDs = gen
		}
	}
}

// New creates a Gateway.
func New(core brainapi.Core, authorizer Authorizer, jobStore *jobs.Store, opts ...Option) (*Gateway, error) {
	if core == nil {
		return nil, brainapi.E(brainapi.KindInvalid, "gateway_new", "core is required", nil)
	}
	if authorizer == nil {
		return nil, brainapi.E(brainapi.KindInvalid, "gateway_new", "authorizer is required", nil)
	}
	if jobStore == nil {
		jobStore = jobs.NewStore()
	}
	g := &Gateway{
		core:       core,
		authorizer: authorizer,
		jobs:       jobStore,
		tenantIDs:  &sequenceTenantIDGenerator{},
		jobIDs:     &sequenceJobIDGenerator{},
	}
	for _, opt := range opts {
		opt(g)
	}
	return g, nil
}

// CreateProjectCommand is the surface-normalized project creation request.
type CreateProjectCommand struct {
	BindingKey brainapi.BindingKey
	Principal  brainapi.Principal
	Metadata   map[string]string
}

// CreateProjectResult is returned after provisioning succeeds.
type CreateProjectResult struct {
	TenantID brainapi.TenantID
}

// CreateProject authorizes creation, generates tenant_id, and calls core provisioning.
func (g *Gateway) CreateProject(ctx context.Context, cmd CreateProjectCommand) (CreateProjectResult, error) {
	const op = "gateway_create_project"
	if err := brainapi.ValidateBindingKey(cmd.BindingKey); err != nil {
		return CreateProjectResult{}, err
	}
	if cmd.Principal.ID == "" {
		return CreateProjectResult{}, brainapi.E(brainapi.KindInvalid, op, "principal is required", nil)
	}
	if err := g.authorizer.Authorize(ctx, AuthorizationRequest{Principal: cmd.Principal, BindingKey: cmd.BindingKey, Action: brainapi.ActionCreateProject}); err != nil {
		return CreateProjectResult{}, brainapi.SafeAccessError(op)
	}
	tenantID := g.tenantIDs.NewTenantID()
	if err := brainapi.ValidateTenantID(tenantID); err != nil {
		return CreateProjectResult{}, err
	}
	err := g.core.CreateProject(ctx, brainapi.CreateProjectRequest{
		TenantID:       tenantID,
		BindingKey:     cmd.BindingKey,
		OwnerPrincipal: cmd.Principal,
		Metadata:       cloneMap(cmd.Metadata),
	})
	if err != nil {
		return CreateProjectResult{}, err
	}
	return CreateProjectResult{TenantID: tenantID}, nil
}

// AskCommand is a surface-normalized question.
type AskCommand struct {
	BindingKey brainapi.BindingKey
	Principal  brainapi.Principal
	Question   string
	Options    map[string]string
}

// Ask resolves binding, authorizes, and calls the core query contract.
func (g *Gateway) Ask(ctx context.Context, cmd AskCommand) (brainapi.QueryResponse, error) {
	tenantID, err := g.resolveAndAuthorize(ctx, cmd.BindingKey, cmd.Principal, brainapi.ActionQuery, "gateway_ask")
	if err != nil {
		return brainapi.QueryResponse{}, err
	}
	return g.core.Query(ctx, brainapi.QueryRequest{TenantID: tenantID, Question: cmd.Question, Options: cloneMap(cmd.Options)})
}

// DiscoverCommand is a surface-normalized metadata-only query.
type DiscoverCommand struct {
	BindingKey brainapi.BindingKey
	Principal  brainapi.Principal
	Query      string
}

// Discover resolves binding, authorizes, and calls metadata-only discovery.
func (g *Gateway) Discover(ctx context.Context, cmd DiscoverCommand) (brainapi.DiscoverResponse, error) {
	tenantID, err := g.resolveAndAuthorize(ctx, cmd.BindingKey, cmd.Principal, brainapi.ActionDiscover, "gateway_discover")
	if err != nil {
		return brainapi.DiscoverResponse{}, err
	}
	return g.core.Discover(ctx, brainapi.DiscoverRequest{TenantID: tenantID, Query: cmd.Query})
}

// IngestCommand is a surface-normalized ingestion request.
type IngestCommand struct {
	BindingKey brainapi.BindingKey
	Principal  brainapi.Principal
	Source     brainapi.SourceRef
	Metadata   map[string]string
}

// IngestResult returns the gateway-generated job ID.
type IngestResult struct {
	TenantID brainapi.TenantID
	JobID    brainapi.JobID
}

// Ingest creates the control-plane job before enqueueing core work.
func (g *Gateway) Ingest(ctx context.Context, cmd IngestCommand) (IngestResult, error) {
	const op = "gateway_ingest"
	tenantID, err := g.resolveAndAuthorize(ctx, cmd.BindingKey, cmd.Principal, brainapi.ActionIngest, op)
	if err != nil {
		return IngestResult{}, err
	}
	jobID := g.jobIDs.NewJobID()
	if err := brainapi.ValidateJobID(jobID); err != nil {
		return IngestResult{}, err
	}
	if _, err := g.jobs.PutAccepted(tenantID, jobID); err != nil {
		return IngestResult{}, err
	}
	if err := g.core.Ingest(ctx, brainapi.IngestRequest{TenantID: tenantID, JobID: jobID, Source: cmd.Source, Metadata: cloneMap(cmd.Metadata)}); err != nil {
		_, _ = g.jobs.MarkFailed(tenantID, jobID, err.Error())
		return IngestResult{}, err
	}
	_, _ = g.jobs.MarkRunning(tenantID, jobID)
	return IngestResult{TenantID: tenantID, JobID: jobID}, nil
}

// StatusCommand asks for a tenant-scoped job status.
type StatusCommand struct {
	BindingKey brainapi.BindingKey
	Principal  brainapi.Principal
	JobID      brainapi.JobID
	Reconcile  bool
}

// Status returns control-plane status and optionally reconciles stale state from core.
func (g *Gateway) Status(ctx context.Context, cmd StatusCommand) (brainapi.JobSnapshot, error) {
	const op = "gateway_status"
	tenantID, err := g.resolveAndAuthorize(ctx, cmd.BindingKey, cmd.Principal, brainapi.ActionStatus, op)
	if err != nil {
		return brainapi.JobSnapshot{}, err
	}
	if cmd.Reconcile {
		return g.jobs.Reconcile(ctx, g.core, tenantID, cmd.JobID)
	}
	return g.jobs.Snapshot(tenantID, cmd.JobID)
}

// HandleJobCompleted applies a core callback to the control-plane store.
func (g *Gateway) HandleJobCompleted(ctx context.Context, tenantID brainapi.TenantID, jobID brainapi.JobID, status brainapi.JobStatus, resultRef string, message string) (brainapi.JobSnapshot, error) {
	_ = ctx
	if !status.Final() {
		return brainapi.JobSnapshot{}, brainapi.E(brainapi.KindInvalid, "gateway_job_completed", "callback status must be terminal", nil)
	}
	if status == brainapi.JobCompleted {
		return g.jobs.MarkCompleted(tenantID, jobID, resultRef)
	}
	return g.jobs.MarkFailed(tenantID, jobID, message)
}

func (g *Gateway) resolveAndAuthorize(ctx context.Context, binding brainapi.BindingKey, principal brainapi.Principal, action brainapi.Action, op string) (brainapi.TenantID, error) {
	if err := brainapi.ValidateBindingKey(binding); err != nil {
		return "", err
	}
	tenantID, err := g.core.ResolveBinding(ctx, binding)
	if err != nil {
		if brainapi.IsKind(err, brainapi.KindNotFound) || brainapi.IsKind(err, brainapi.KindUnauthorized) {
			return "", brainapi.SafeAccessError(op)
		}
		return "", err
	}
	if err := g.authorizer.Authorize(ctx, AuthorizationRequest{Principal: principal, TenantID: tenantID, BindingKey: binding, Action: action}); err != nil {
		return "", brainapi.SafeAccessError(op)
	}
	return tenantID, nil
}

func cloneMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

type sequenceTenantIDGenerator struct{ n atomic.Uint64 }

func (g *sequenceTenantIDGenerator) NewTenantID() brainapi.TenantID {
	return brainapi.TenantID(fmt.Sprintf("tenant-%06d", g.n.Add(1)))
}

type sequenceJobIDGenerator struct{ n atomic.Uint64 }

func (g *sequenceJobIDGenerator) NewJobID() brainapi.JobID {
	return brainapi.JobID(fmt.Sprintf("job-%06d", g.n.Add(1)))
}
