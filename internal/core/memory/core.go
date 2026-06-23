// Package memory provides an in-memory data core for tests and local demos.
package memory

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

// Core is a thread-safe in-memory implementation of brainapi.Core.
type Core struct {
	mu       sync.RWMutex
	now      func() time.Time
	bindings map[brainapi.BindingKey]brainapi.TenantID
	projects map[brainapi.TenantID]project
	jobs     map[string]brainapi.JobSnapshot
	sources  map[brainapi.TenantID][]brainapi.SourceRef
}

type project struct {
	owner    brainapi.Principal
	metadata map[string]string
	state    brainapi.ProjectState
}

// Option configures Core.
type Option func(*Core)

// WithClock injects a deterministic clock.
func WithClock(clock func() time.Time) Option {
	return func(c *Core) {
		if clock != nil {
			c.now = clock
		}
	}
}

// New creates an empty in-memory data core.
func New(opts ...Option) *Core {
	c := &Core{
		now:      time.Now,
		bindings: make(map[brainapi.BindingKey]brainapi.TenantID),
		projects: make(map[brainapi.TenantID]project),
		jobs:     make(map[string]brainapi.JobSnapshot),
		sources:  make(map[brainapi.TenantID][]brainapi.SourceRef),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// ResolveBinding returns tenant_id for a surface-neutral binding key.
func (c *Core) ResolveBinding(ctx context.Context, binding brainapi.BindingKey) (brainapi.TenantID, error) {
	_ = ctx
	if err := brainapi.ValidateBindingKey(binding); err != nil {
		return "", err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	tenantID, ok := c.bindings[binding]
	if !ok {
		return "", brainapi.E(brainapi.KindNotFound, "resolve_binding", "binding not found", nil)
	}
	return tenantID, nil
}

// CreateProject atomically provisions a tenant and binding.
func (c *Core) CreateProject(ctx context.Context, req brainapi.CreateProjectRequest) error {
	_ = ctx
	if err := brainapi.ValidateTenantID(req.TenantID); err != nil {
		return err
	}
	if err := brainapi.ValidateBindingKey(req.BindingKey); err != nil {
		return err
	}
	if req.OwnerPrincipal.ID == "" {
		return brainapi.E(brainapi.KindInvalid, "create_project", "owner principal is required", nil)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.projects[req.TenantID]; ok {
		return brainapi.E(brainapi.KindAlreadyExists, "create_project", "tenant already exists", nil)
	}
	if _, ok := c.bindings[req.BindingKey]; ok {
		return brainapi.E(brainapi.KindAlreadyExists, "create_project", "binding already exists", nil)
	}
	c.projects[req.TenantID] = project{owner: req.OwnerPrincipal, metadata: cloneMap(req.Metadata), state: brainapi.ProjectOn}
	c.bindings[req.BindingKey] = req.TenantID
	return nil
}

// Ingest stores Bronze metadata and records a running job.
func (c *Core) Ingest(ctx context.Context, req brainapi.IngestRequest) error {
	_ = ctx
	if err := brainapi.ValidateTenantID(req.TenantID); err != nil {
		return err
	}
	if err := brainapi.ValidateJobID(req.JobID); err != nil {
		return err
	}
	if strings.TrimSpace(req.Source.URI) == "" {
		return brainapi.E(brainapi.KindInvalid, "ingest", "source uri is required", nil)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.requireProjectLocked(req.TenantID, "ingest"); err != nil {
		return err
	}
	key := jobKey(req.TenantID, req.JobID)
	if _, ok := c.jobs[key]; ok {
		return brainapi.E(brainapi.KindAlreadyExists, "ingest", "job already exists", nil)
	}
	c.sources[req.TenantID] = append(c.sources[req.TenantID], req.Source)
	c.jobs[key] = brainapi.JobSnapshot{TenantID: req.TenantID, JobID: req.JobID, Status: brainapi.JobRunning, UpdatedAt: c.now().UTC()}
	return nil
}

// CompleteJob marks a job terminal. It simulates worker callback state for tests and demos.
func (c *Core) CompleteJob(tenantID brainapi.TenantID, jobID brainapi.JobID, status brainapi.JobStatus, resultRef, message string) error {
	if !status.Final() {
		return brainapi.E(brainapi.KindInvalid, "complete_job", "status must be terminal", nil)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := jobKey(tenantID, jobID)
	snapshot, ok := c.jobs[key]
	if !ok {
		return brainapi.E(brainapi.KindNotFound, "complete_job", "job not found", nil)
	}
	snapshot.Status = status
	snapshot.ResultRef = resultRef
	snapshot.Error = message
	snapshot.UpdatedAt = c.now().UTC()
	c.jobs[key] = snapshot
	return nil
}

// JobStatus returns tenant-scoped job state.
func (c *Core) JobStatus(ctx context.Context, tenantID brainapi.TenantID, jobID brainapi.JobID) (brainapi.JobSnapshot, error) {
	_ = ctx
	if err := brainapi.ValidateTenantID(tenantID); err != nil {
		return brainapi.JobSnapshot{}, err
	}
	if err := brainapi.ValidateJobID(jobID); err != nil {
		return brainapi.JobSnapshot{}, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	snapshot, ok := c.jobs[jobKey(tenantID, jobID)]
	if !ok {
		return brainapi.JobSnapshot{}, brainapi.E(brainapi.KindNotFound, "job_status", "job not found", nil)
	}
	return snapshot, nil
}

// Query returns a deterministic answer over tenant-scoped sources.
func (c *Core) Query(ctx context.Context, req brainapi.QueryRequest) (brainapi.QueryResponse, error) {
	_ = ctx
	if err := brainapi.ValidateTenantID(req.TenantID); err != nil {
		return brainapi.QueryResponse{}, err
	}
	if strings.TrimSpace(req.Question) == "" {
		return brainapi.QueryResponse{}, brainapi.E(brainapi.KindInvalid, "query", "question is required", nil)
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if err := c.requireProjectLocked(req.TenantID, "query"); err != nil {
		return brainapi.QueryResponse{}, err
	}
	sources := make([]brainapi.Source, 0, len(c.sources[req.TenantID]))
	for i, src := range c.sources[req.TenantID] {
		title := src.Name
		if title == "" {
			title = src.URI
		}
		sources = append(sources, brainapi.Source{ID: string(req.TenantID) + ":source:" + string(rune('a'+i%26)), Title: title, URI: src.URI, TenantID: req.TenantID})
	}
	return brainapi.QueryResponse{
		Answer:             "아직 실제 RAG 합성은 연결되지 않았습니다. walking skeleton 응답입니다: " + req.Question,
		Sources:            sources,
		GroundedSpans:      []string{"walking skeleton"},
		SupplementedSpans:  nil,
		GroundingAvailable: len(sources) > 0,
	}, nil
}

// Discover returns tenant-scoped metadata only.
func (c *Core) Discover(ctx context.Context, req brainapi.DiscoverRequest) (brainapi.DiscoverResponse, error) {
	_ = ctx
	if err := brainapi.ValidateTenantID(req.TenantID); err != nil {
		return brainapi.DiscoverResponse{}, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if err := c.requireProjectLocked(req.TenantID, "discover"); err != nil {
		return brainapi.DiscoverResponse{}, err
	}
	results := make([]brainapi.MetadataResult, 0, len(c.sources[req.TenantID]))
	for i, src := range c.sources[req.TenantID] {
		title := src.Name
		if title == "" {
			title = src.URI
		}
		results = append(results, brainapi.MetadataResult{ID: string(req.TenantID) + ":metadata:" + string(rune('a'+i%26)), Title: title, Kind: src.MimeType, FreshAt: c.now().UTC()})
	}
	return brainapi.DiscoverResponse{Results: results}, nil
}

// SetProjectState freezes or enables a tenant.
func (c *Core) SetProjectState(ctx context.Context, tenantID brainapi.TenantID, state brainapi.ProjectState) error {
	_ = ctx
	if err := brainapi.ValidateTenantID(tenantID); err != nil {
		return err
	}
	if state != brainapi.ProjectOn && state != brainapi.ProjectOff {
		return brainapi.E(brainapi.KindInvalid, "set_project_state", "unknown project state", nil)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	proj, ok := c.projects[tenantID]
	if !ok {
		return brainapi.E(brainapi.KindNotFound, "set_project_state", "tenant not found", nil)
	}
	proj.state = state
	c.projects[tenantID] = proj
	return nil
}

func (c *Core) requireProjectLocked(tenantID brainapi.TenantID, op string) error {
	proj, ok := c.projects[tenantID]
	if !ok {
		return brainapi.E(brainapi.KindNotFound, op, "tenant not found", nil)
	}
	if proj.state == brainapi.ProjectOff {
		return brainapi.E(brainapi.KindConflict, op, "project is off", nil)
	}
	return nil
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

func jobKey(tenantID brainapi.TenantID, jobID brainapi.JobID) string {
	return string(tenantID) + "\x00" + string(jobID)
}
