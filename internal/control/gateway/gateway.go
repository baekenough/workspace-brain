// Package gateway coordinates surface-neutral requests before they reach the data core.
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	"github.com/sangyi/workspace-brain/internal/control/audit"
	"github.com/sangyi/workspace-brain/internal/control/ingest"
	"github.com/sangyi/workspace-brain/internal/control/jobs"
	"github.com/sangyi/workspace-brain/internal/control/sources"
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
	loader     sources.SourceLoader
	queue      ingest.Queue // optional async completion worker; nil = sync path
	audit      audit.Logger // defaults to audit.NoOp{}
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

// WithSourceLoader loads source content before core ingestion when Metadata["content"] is absent.
func WithSourceLoader(loader sources.SourceLoader) Option {
	return func(g *Gateway) {
		if loader != nil {
			g.loader = loader
		}
	}
}

// WithAuditLogger sets the audit logger used to record authorization decisions.
// Passing nil is a no-op: the default audit.NoOp logger is preserved so that
// existing behaviour and tests remain unchanged.
func WithAuditLogger(logger audit.Logger) Option {
	return func(g *Gateway) {
		if logger != nil {
			g.audit = logger
		}
	}
}

// WithQueue wires an async ingest-completion worker to the gateway.
// When set, Ingest hands a completion task to the queue after synchronously
// indexing the content; the worker then drives the job to a terminal state.
// When nil (the default), Ingest stays on the existing synchronous path.
func WithQueue(q ingest.Queue) Option {
	return func(g *Gateway) {
		if q != nil {
			g.queue = q
		}
	}
}

// SetQueue sets (or replaces) the async ingest-completion queue after
// construction.  It is provided for lifecycle wiring in the server entrypoint
// where the gateway and the worker share a mutual reference.
func (g *Gateway) SetQueue(q ingest.Queue) {
	g.queue = q
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
		tenantIDs:  &randTenantIDGenerator{},
		jobIDs:     &randJobIDGenerator{},
		audit:      audit.NoOp{},
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

// emitAuthAudit records an authorization decision to the gateway's audit logger.
func (g *Gateway) emitAuthAudit(actor string, action brainapi.Action, tenantID brainapi.TenantID, authErr error) {
	decision := audit.DecisionAllow
	reason := ""
	if authErr != nil {
		decision = audit.DecisionDeny
		reason = authErr.Error()
	}
	g.audit.Log(audit.Event{
		Actor:    actor,
		Action:   action,
		TenantID: tenantID,
		Decision: decision,
		Reason:   reason,
	})
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
	authErr := g.authorizer.Authorize(ctx, AuthorizationRequest{Principal: cmd.Principal, BindingKey: cmd.BindingKey, Action: brainapi.ActionCreateProject})
	g.emitAuthAudit(cmd.Principal.Key(), brainapi.ActionCreateProject, "", authErr)
	if authErr != nil {
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

// Ingest creates the control-plane job, indexes the content synchronously,
// and — when an async queue is configured — enqueues a lightweight completion
// task so the job transitions from running to completed in the background.
//
// When no queue is configured (the default), the method returns with the job
// in "running" state, preserving the original behaviour for tests and the demo
// flow.
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
	source := cmd.Source
	metadata := cloneMap(cmd.Metadata)
	if metadata == nil || metadata[sources.MetadataContentKey] == "" {
		if g.loader != nil {
			loaded, err := g.loader.Load(ctx, source, metadata)
			if err != nil {
				_, _ = g.jobs.MarkFailed(tenantID, jobID, err.Error())
				return IngestResult{}, err
			}
			source = loaded.Source
			metadata = cloneMap(loaded.Metadata)
		}
	}
	// Index content synchronously so the data is immediately queryable.
	if err := g.core.Ingest(ctx, brainapi.IngestRequest{TenantID: tenantID, JobID: jobID, Source: source, Metadata: metadata}); err != nil {
		_, _ = g.jobs.MarkFailed(tenantID, jobID, err.Error())
		return IngestResult{}, err
	}
	_, _ = g.jobs.MarkRunning(tenantID, jobID)

	if g.queue != nil {
		// Async path: hand off job-state finalization to the completion worker.
		// The job stays "running" until the worker calls HandleJobCompleted.
		if err := g.queue.Enqueue(ctx, ingest.Task{TenantID: tenantID, JobID: jobID}); err != nil {
			_, _ = g.jobs.MarkFailed(tenantID, jobID, err.Error())
			return IngestResult{}, err
		}
	}
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

// SetProjectStateCommand asks the gateway to change a tenant lifecycle state.
type SetProjectStateCommand struct {
	Principal brainapi.Principal
	TenantID  brainapi.TenantID
	State     brainapi.ProjectState
}

// SetProjectStateResult confirms the updated tenant lifecycle state.
type SetProjectStateResult struct {
	TenantID brainapi.TenantID
	State    brainapi.ProjectState
}

// SetProjectState authorizes an admin principal and updates core tenant state.
func (g *Gateway) SetProjectState(ctx context.Context, cmd SetProjectStateCommand) (SetProjectStateResult, error) {
	const op = "gateway_set_project_state"
	if err := brainapi.ValidateTenantID(cmd.TenantID); err != nil {
		return SetProjectStateResult{}, err
	}
	if cmd.State != brainapi.ProjectOn && cmd.State != brainapi.ProjectOff {
		return SetProjectStateResult{}, brainapi.E(brainapi.KindInvalid, op, "unknown project state", nil)
	}
	authErr := g.authorizer.Authorize(ctx, AuthorizationRequest{Principal: cmd.Principal, TenantID: cmd.TenantID, Action: brainapi.ActionAdmin})
	g.emitAuthAudit(cmd.Principal.Key(), brainapi.ActionAdmin, cmd.TenantID, authErr)
	if authErr != nil {
		return SetProjectStateResult{}, brainapi.SafeAccessError(op)
	}
	if err := g.core.SetProjectState(ctx, cmd.TenantID, cmd.State); err != nil {
		if brainapi.IsKind(err, brainapi.KindNotFound) || brainapi.IsKind(err, brainapi.KindUnauthorized) {
			return SetProjectStateResult{}, brainapi.SafeAccessError(op)
		}
		return SetProjectStateResult{}, err
	}
	return SetProjectStateResult{TenantID: cmd.TenantID, State: cmd.State}, nil
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
	authErr := g.authorizer.Authorize(ctx, AuthorizationRequest{Principal: principal, TenantID: tenantID, BindingKey: binding, Action: action})
	g.emitAuthAudit(principal.Key(), action, tenantID, authErr)
	if authErr != nil {
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

// randTenantIDGenerator produces collision-resistant tenant IDs using 128 bits of
// crypto/rand entropy. Unlike the old sequence-based generator, IDs do not reset
// to zero on process restart, so they never collide with records in a persistent core.
type randTenantIDGenerator struct{}

func (g *randTenantIDGenerator) NewTenantID() brainapi.TenantID {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return brainapi.TenantID("tenant-" + hex.EncodeToString(b[:]))
}

// randJobIDGenerator produces collision-resistant job IDs using 128 bits of
// crypto/rand entropy for the same restart-safety reason as randTenantIDGenerator.
type randJobIDGenerator struct{}

func (g *randJobIDGenerator) NewJobID() brainapi.JobID {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return brainapi.JobID("job-" + hex.EncodeToString(b[:]))
}
