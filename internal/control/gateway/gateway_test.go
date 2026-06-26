package gateway

import (
	"context"
	"errors"
	"testing"

	"github.com/sangyi/workspace-brain/internal/control/ingest"
	"github.com/sangyi/workspace-brain/internal/control/jobs"
	"github.com/sangyi/workspace-brain/internal/control/sources"
	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

func TestGatewayCreateProjectUsesGeneratedTenantAndOwner(t *testing.T) {
	t.Parallel()
	core := newFakeCore()
	gw, err := New(core, RoleAuthorizer{}, jobs.NewStore(), WithTenantIDGenerator(staticTenantID("tenant-fixed")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	principal := brainapi.Principal{Source: "slack", ID: "U1", Roles: []string{"admin"}}
	result, err := gw.CreateProject(context.Background(), CreateProjectCommand{BindingKey: "slack:channel:C1", Principal: principal, Metadata: map[string]string{"name": "demo"}})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if result.TenantID != "tenant-fixed" {
		t.Fatalf("tenant = %q", result.TenantID)
	}
	if core.created.OwnerPrincipal.Key() != "slack:U1" || core.created.Metadata["name"] != "demo" {
		t.Fatalf("created request = %+v", core.created)
	}
}

func TestGatewayHidesMissingBindingAndUnauthorized(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		core       *fakeCore
		principal  brainapi.Principal
		wantSafeOp string
	}{
		{"missing binding", &fakeCore{resolveErr: brainapi.E(brainapi.KindNotFound, "resolve", "missing C1", nil)}, brainapi.Principal{ID: "U1", Roles: []string{"member"}}, "gateway_ask"},
		{"unauthorized", newFakeCore(), brainapi.Principal{ID: "U2"}, "gateway_ask"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gw, err := New(tt.core, RoleAuthorizer{}, jobs.NewStore())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = gw.Ask(context.Background(), AskCommand{BindingKey: "slack:channel:C1", Principal: tt.principal, Question: "?"})
			if !brainapi.IsKind(err, brainapi.KindUnauthorized) {
				t.Fatalf("Ask kind = %q err=%v", brainapi.KindOf(err), err)
			}
			if got := err.Error(); got != tt.wantSafeOp+": unauthorized: project is not accessible" {
				t.Fatalf("unsafe error: %q", got)
			}
		})
	}
}

func TestGatewayIngestCreatesJobThenMarksRunning(t *testing.T) {
	t.Parallel()
	core := newFakeCore()
	store := jobs.NewStore()
	gw, err := New(core, RoleAuthorizer{}, store, WithJobIDGenerator(staticJobID("job-fixed")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := gw.Ingest(context.Background(), IngestCommand{
		BindingKey: "slack:channel:C1",
		Principal:  brainapi.Principal{ID: "U1", Roles: []string{"member"}},
		Source:     brainapi.SourceRef{URI: "file://a.pdf", Name: "a.pdf"},
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if result.JobID != "job-fixed" || core.ingested.JobID != "job-fixed" {
		t.Fatalf("job mismatch result=%+v core=%+v", result, core.ingested)
	}
	snapshot, err := store.Snapshot("tenant-1", "job-fixed")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapshot.Status != brainapi.JobRunning {
		t.Fatalf("status = %q", snapshot.Status)
	}
}

func TestGatewayIngestMarksFailedOnCoreError(t *testing.T) {
	t.Parallel()
	core := newFakeCore()
	core.ingestErr = errors.New("queue down")
	store := jobs.NewStore()
	gw, err := New(core, RoleAuthorizer{}, store, WithJobIDGenerator(staticJobID("job-failed")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = gw.Ingest(context.Background(), IngestCommand{BindingKey: "slack:channel:C1", Principal: brainapi.Principal{ID: "U1", Roles: []string{"member"}}, Source: brainapi.SourceRef{URI: "file://a.pdf"}})
	if err == nil {
		t.Fatalf("expected ingest error")
	}
	snapshot, snapErr := store.Snapshot("tenant-1", "job-failed")
	if snapErr != nil {
		t.Fatalf("Snapshot: %v", snapErr)
	}
	if snapshot.Status != brainapi.JobFailed || snapshot.Error != "queue down" {
		t.Fatalf("failed snapshot = %+v", snapshot)
	}
}

func TestGatewayIngestInjectsLoadedContent(t *testing.T) {
	t.Parallel()
	core := newFakeCore()
	store := jobs.NewStore()
	gw, err := New(core, RoleAuthorizer{}, store, WithJobIDGenerator(staticJobID("job-loaded")), WithSourceLoader(fakeSourceLoader{loaded: sources.LoadedSource{
		Source:   brainapi.SourceRef{URI: "file://loaded.txt", Name: "loaded.txt", MimeType: "text/plain"},
		Metadata: map[string]string{"content": "loaded content", "origin": "loader"},
	}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = gw.Ingest(context.Background(), IngestCommand{
		BindingKey: "slack:channel:C1",
		Principal:  brainapi.Principal{ID: "U1", Roles: []string{"member"}},
		Source:     brainapi.SourceRef{URI: "file://source.txt"},
		Metadata:   map[string]string{"origin": "cmd"},
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if core.ingested.Metadata["content"] != "loaded content" || core.ingested.Metadata["origin"] != "loader" {
		t.Fatalf("metadata = %+v", core.ingested.Metadata)
	}
	if core.ingested.Source.Name != "loaded.txt" {
		t.Fatalf("source = %+v", core.ingested.Source)
	}
}

func TestGatewayIngestMarksFailedOnLoaderError(t *testing.T) {
	t.Parallel()
	core := newFakeCore()
	store := jobs.NewStore()
	loaderErr := brainapi.E(brainapi.KindInvalid, "source_load", "bad source", nil)
	gw, err := New(core, RoleAuthorizer{}, store, WithJobIDGenerator(staticJobID("job-loader-failed")), WithSourceLoader(fakeSourceLoader{err: loaderErr}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = gw.Ingest(context.Background(), IngestCommand{BindingKey: "slack:channel:C1", Principal: brainapi.Principal{ID: "U1", Roles: []string{"member"}}, Source: brainapi.SourceRef{URI: "file://bad"}})
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if core.ingested.JobID != "" {
		t.Fatalf("core ingest should not run: %+v", core.ingested)
	}
	snapshot, snapErr := store.Snapshot("tenant-1", "job-loader-failed")
	if snapErr != nil {
		t.Fatalf("Snapshot: %v", snapErr)
	}
	if snapshot.Status != brainapi.JobFailed || snapshot.Error != loaderErr.Error() {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestGatewayIngestWithoutLoaderKeepsMetadataFallback(t *testing.T) {
	t.Parallel()
	core := newFakeCore()
	gw, err := New(core, RoleAuthorizer{}, jobs.NewStore(), WithJobIDGenerator(staticJobID("job-no-loader")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = gw.Ingest(context.Background(), IngestCommand{BindingKey: "slack:channel:C1", Principal: brainapi.Principal{ID: "U1", Roles: []string{"member"}}, Source: brainapi.SourceRef{URI: "file://a.pdf", Name: "a.pdf"}})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if core.ingested.Metadata != nil {
		t.Fatalf("metadata = %+v", core.ingested.Metadata)
	}
}

func TestGatewayStatusCanReconcileCore(t *testing.T) {
	t.Parallel()
	core := newFakeCore()
	store := jobs.NewStore()
	gw, err := New(core, RoleAuthorizer{}, store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	core.status = brainapi.JobSnapshot{TenantID: "tenant-1", JobID: "job-1", Status: brainapi.JobCompleted, ResultRef: "done"}
	snapshot, err := gw.Status(context.Background(), StatusCommand{BindingKey: "slack:channel:C1", Principal: brainapi.Principal{ID: "U1", Roles: []string{"member"}}, JobID: "job-1", Reconcile: true})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if snapshot.Status != brainapi.JobCompleted || snapshot.ResultRef != "done" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestGatewayCallbackRequiresTerminalStatus(t *testing.T) {
	t.Parallel()
	gw, err := New(newFakeCore(), RoleAuthorizer{}, jobs.NewStore())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = gw.HandleJobCompleted(context.Background(), "tenant-1", "job-1", brainapi.JobRunning, "", "")
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("kind = %q err=%v", brainapi.KindOf(err), err)
	}
}

type staticTenantID string

func (s staticTenantID) NewTenantID() brainapi.TenantID { return brainapi.TenantID(s) }

type staticJobID string

func (s staticJobID) NewJobID() brainapi.JobID { return brainapi.JobID(s) }

type fakeCore struct {
	binding     brainapi.TenantID
	resolveErr  error
	created     brainapi.CreateProjectRequest
	createErr   error
	ingested    brainapi.IngestRequest
	ingestErr   error
	status      brainapi.JobSnapshot
	stateTenant brainapi.TenantID
	state       brainapi.ProjectState
	stateErr    error
}

func newFakeCore() *fakeCore { return &fakeCore{binding: "tenant-1"} }

func (f *fakeCore) ResolveBinding(context.Context, brainapi.BindingKey) (brainapi.TenantID, error) {
	if f.resolveErr != nil {
		return "", f.resolveErr
	}
	return f.binding, nil
}
func (f *fakeCore) CreateProject(_ context.Context, req brainapi.CreateProjectRequest) error {
	f.created = req
	return f.createErr
}
func (f *fakeCore) Ingest(_ context.Context, req brainapi.IngestRequest) error {
	f.ingested = req
	return f.ingestErr
}
func (f *fakeCore) JobStatus(context.Context, brainapi.TenantID, brainapi.JobID) (brainapi.JobSnapshot, error) {
	return f.status, nil
}
func (f *fakeCore) Query(context.Context, brainapi.QueryRequest) (brainapi.QueryResponse, error) {
	return brainapi.QueryResponse{Answer: "answer", GroundingAvailable: true}, nil
}
func (f *fakeCore) Discover(context.Context, brainapi.DiscoverRequest) (brainapi.DiscoverResponse, error) {
	return brainapi.DiscoverResponse{}, nil
}
func (f *fakeCore) SetProjectState(_ context.Context, tenantID brainapi.TenantID, state brainapi.ProjectState) error {
	f.stateTenant = tenantID
	f.state = state
	return f.stateErr
}

func TestRoleAuthorizerBranches(t *testing.T) {
	t.Parallel()
	a := RoleAuthorizer{}
	ctx := context.Background()
	if err := a.Authorize(ctx, AuthorizationRequest{}); !brainapi.IsKind(err, brainapi.KindUnauthorized) {
		t.Fatalf("empty principal kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if err := a.Authorize(ctx, AuthorizationRequest{Principal: brainapi.Principal{ID: "U1", Roles: []string{"member"}}, Action: brainapi.ActionCreateProject}); !brainapi.IsKind(err, brainapi.KindUnauthorized) {
		t.Fatalf("member create kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if err := a.Authorize(ctx, AuthorizationRequest{Principal: brainapi.Principal{ID: "U1", Roles: []string{"member"}}, Action: brainapi.ActionQuery}); err != nil {
		t.Fatalf("member query: %v", err)
	}
}

func TestGatewayNewValidationAndDefaultGenerators(t *testing.T) {
	t.Parallel()
	if _, err := New(nil, RoleAuthorizer{}, nil); !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("nil core kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if _, err := New(newFakeCore(), nil, nil); !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("nil authorizer kind=%q err=%v", brainapi.KindOf(err), err)
	}
	gw, err := New(newFakeCore(), RoleAuthorizer{}, nil)
	if err != nil {
		t.Fatalf("New defaults: %v", err)
	}
	created, err := gw.CreateProject(context.Background(), CreateProjectCommand{BindingKey: "slack:channel:C1", Principal: brainapi.Principal{ID: "admin", Roles: []string{"admin"}}})
	if err != nil {
		t.Fatalf("CreateProject default tenant id: %v", err)
	}
	if created.TenantID == "" {
		t.Fatalf("empty generated tenant id")
	}
	result, err := gw.Ingest(context.Background(), IngestCommand{BindingKey: "slack:channel:C1", Principal: brainapi.Principal{ID: "member", Roles: []string{"member"}}, Source: brainapi.SourceRef{URI: "file://a"}})
	if err != nil {
		t.Fatalf("Ingest default job id: %v", err)
	}
	if result.JobID == "" {
		t.Fatalf("empty generated job id")
	}
}

func TestGatewayCreateProjectValidationAndErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	gw, err := New(newFakeCore(), RoleAuthorizer{}, jobs.NewStore(), WithTenantIDGenerator(staticTenantID("tenant-fixed")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = gw.CreateProject(ctx, CreateProjectCommand{BindingKey: "bad", Principal: brainapi.Principal{ID: "admin", Roles: []string{"admin"}}})
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("bad binding kind=%q err=%v", brainapi.KindOf(err), err)
	}
	_, err = gw.CreateProject(ctx, CreateProjectCommand{BindingKey: "slack:channel:C1"})
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("missing principal kind=%q err=%v", brainapi.KindOf(err), err)
	}
	_, err = gw.CreateProject(ctx, CreateProjectCommand{BindingKey: "slack:channel:C1", Principal: brainapi.Principal{ID: "member", Roles: []string{"member"}}})
	if !brainapi.IsKind(err, brainapi.KindUnauthorized) {
		t.Fatalf("unauthorized kind=%q err=%v", brainapi.KindOf(err), err)
	}
	badTenantGW, err := New(newFakeCore(), RoleAuthorizer{}, jobs.NewStore(), WithTenantIDGenerator(staticTenantID("bad tenant")))
	if err != nil {
		t.Fatalf("New bad tenant gw: %v", err)
	}
	_, err = badTenantGW.CreateProject(ctx, CreateProjectCommand{BindingKey: "slack:channel:C1", Principal: brainapi.Principal{ID: "admin", Roles: []string{"admin"}}})
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("bad tenant kind=%q err=%v", brainapi.KindOf(err), err)
	}
	core := newFakeCore()
	core.createErr = brainapi.E(brainapi.KindAlreadyExists, "create", "dup", nil)
	coreErrGW, err := New(core, RoleAuthorizer{}, jobs.NewStore(), WithTenantIDGenerator(staticTenantID("tenant-fixed")))
	if err != nil {
		t.Fatalf("New core err gw: %v", err)
	}
	_, err = coreErrGW.CreateProject(ctx, CreateProjectCommand{BindingKey: "slack:channel:C1", Principal: brainapi.Principal{ID: "admin", Roles: []string{"admin"}}})
	if !brainapi.IsKind(err, brainapi.KindAlreadyExists) {
		t.Fatalf("core create err kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestGatewayAskDiscoverStatusAndCallbacks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	core := newFakeCore()
	gw, err := New(core, RoleAuthorizer{}, jobs.NewStore(), WithJobIDGenerator(staticJobID("job-1")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := gw.Ask(ctx, AskCommand{BindingKey: "slack:channel:C1", Principal: brainapi.Principal{ID: "U", Roles: []string{"member"}}, Question: "q", Options: map[string]string{"a": "b"}}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, err := gw.Discover(ctx, DiscoverCommand{BindingKey: "slack:channel:C1", Principal: brainapi.Principal{ID: "U", Roles: []string{"member"}}, Query: "meta"}); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if _, err := gw.Ingest(ctx, IngestCommand{BindingKey: "slack:channel:C1", Principal: brainapi.Principal{ID: "U", Roles: []string{"member"}}, Source: brainapi.SourceRef{URI: "file://a"}}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if _, err := gw.Status(ctx, StatusCommand{BindingKey: "slack:channel:C1", Principal: brainapi.Principal{ID: "U", Roles: []string{"member"}}, JobID: "job-1"}); err != nil {
		t.Fatalf("Status no reconcile: %v", err)
	}
	completed, err := gw.HandleJobCompleted(ctx, "tenant-1", "job-1", brainapi.JobCompleted, "result", "")
	if err != nil {
		t.Fatalf("Handle completed: %v", err)
	}
	if completed.Status != brainapi.JobCompleted {
		t.Fatalf("completed = %+v", completed)
	}
	failed, err := gw.HandleJobCompleted(ctx, "tenant-1", "job-1", brainapi.JobFailed, "", "boom")
	if err != nil {
		t.Fatalf("Handle failed: %v", err)
	}
	if failed.Status != brainapi.JobFailed || failed.Error != "boom" {
		t.Fatalf("failed = %+v", failed)
	}
}

func TestGatewaySetProjectState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	core := newFakeCore()
	gw, err := New(core, RoleAuthorizer{}, jobs.NewStore())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := gw.SetProjectState(ctx, SetProjectStateCommand{TenantID: "tenant-1", State: brainapi.ProjectOff, Principal: brainapi.Principal{ID: "admin", Roles: []string{"admin"}}})
	if err != nil {
		t.Fatalf("SetProjectState: %v", err)
	}
	if result.TenantID != "tenant-1" || result.State != brainapi.ProjectOff || core.stateTenant != "tenant-1" || core.state != brainapi.ProjectOff {
		t.Fatalf("result=%+v core tenant=%q state=%q", result, core.stateTenant, core.state)
	}
}

func TestGatewaySetProjectStateErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tt := range []struct {
		name string
		core *fakeCore
		cmd  SetProjectStateCommand
		kind brainapi.ErrorKind
	}{
		{"bad tenant", newFakeCore(), SetProjectStateCommand{TenantID: "bad tenant", State: brainapi.ProjectOn, Principal: brainapi.Principal{ID: "admin", Roles: []string{"admin"}}}, brainapi.KindInvalid},
		{"bad state", newFakeCore(), SetProjectStateCommand{TenantID: "tenant-1", State: brainapi.ProjectState("paused"), Principal: brainapi.Principal{ID: "admin", Roles: []string{"admin"}}}, brainapi.KindInvalid},
		{"member", newFakeCore(), SetProjectStateCommand{TenantID: "tenant-1", State: brainapi.ProjectOn, Principal: brainapi.Principal{ID: "member", Roles: []string{"member"}}}, brainapi.KindUnauthorized},
		{"not found safe", &fakeCore{binding: "tenant-1", stateErr: brainapi.E(brainapi.KindNotFound, "state", "missing", nil)}, SetProjectStateCommand{TenantID: "tenant-1", State: brainapi.ProjectOn, Principal: brainapi.Principal{ID: "admin", Roles: []string{"admin"}}}, brainapi.KindUnauthorized},
		{"unauthorized safe", &fakeCore{binding: "tenant-1", stateErr: brainapi.E(brainapi.KindUnauthorized, "state", "denied", nil)}, SetProjectStateCommand{TenantID: "tenant-1", State: brainapi.ProjectOn, Principal: brainapi.Principal{ID: "admin", Roles: []string{"admin"}}}, brainapi.KindUnauthorized},
		{"internal", &fakeCore{binding: "tenant-1", stateErr: errors.New("store down")}, SetProjectStateCommand{TenantID: "tenant-1", State: brainapi.ProjectOn, Principal: brainapi.Principal{ID: "admin", Roles: []string{"admin"}}}, brainapi.KindInternal},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gw, err := New(tt.core, RoleAuthorizer{}, jobs.NewStore())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = gw.SetProjectState(ctx, tt.cmd)
			if !brainapi.IsKind(err, tt.kind) {
				t.Fatalf("kind=%q err=%v want=%q", brainapi.KindOf(err), err, tt.kind)
			}
		})
	}
}

func TestGatewayIngestValidationAndStoreFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	gw, err := New(newFakeCore(), RoleAuthorizer{}, jobs.NewStore(), WithJobIDGenerator(staticJobID("bad job")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = gw.Ingest(ctx, IngestCommand{BindingKey: "slack:channel:C1", Principal: brainapi.Principal{ID: "U", Roles: []string{"member"}}, Source: brainapi.SourceRef{URI: "file://a"}})
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("bad job kind=%q err=%v", brainapi.KindOf(err), err)
	}
	_, err = gw.Ingest(ctx, IngestCommand{BindingKey: "bad", Principal: brainapi.Principal{ID: "U", Roles: []string{"member"}}, Source: brainapi.SourceRef{URI: "file://a"}})
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("bad binding kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestGatewayResolvePropagatesInternalErrors(t *testing.T) {
	t.Parallel()
	core := newFakeCore()
	core.resolveErr = errors.New("database down")
	gw, err := New(core, RoleAuthorizer{}, jobs.NewStore())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = gw.Ask(context.Background(), AskCommand{BindingKey: "slack:channel:C1", Principal: brainapi.Principal{ID: "U", Roles: []string{"member"}}, Question: "q"})
	if err == nil || err.Error() != "database down" {
		t.Fatalf("err=%v", err)
	}
}

func TestGatewayRemainingErrorBranches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	core := newFakeCore()
	gw, err := New(core, RoleAuthorizer{}, jobs.NewStore())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = gw.Discover(ctx, DiscoverCommand{BindingKey: "bad", Principal: brainapi.Principal{ID: "U", Roles: []string{"member"}}, Query: "q"})
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("discover bad binding kind=%q err=%v", brainapi.KindOf(err), err)
	}
	_, err = gw.Ingest(ctx, IngestCommand{BindingKey: "slack:channel:C1", Principal: brainapi.Principal{ID: "U", Roles: []string{"member"}}, Source: brainapi.SourceRef{URI: "file://a"}})
	if err != nil {
		t.Fatalf("baseline ingest: %v", err)
	}
	core.binding = "bad tenant"
	_, err = gw.Ingest(ctx, IngestCommand{BindingKey: "slack:channel:C1", Principal: brainapi.Principal{ID: "U", Roles: []string{"member"}}, Source: brainapi.SourceRef{URI: "file://a"}})
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("put accepted invalid tenant kind=%q err=%v", brainapi.KindOf(err), err)
	}
	_, err = gw.Status(ctx, StatusCommand{BindingKey: "bad", Principal: brainapi.Principal{ID: "U", Roles: []string{"member"}}, JobID: "job"})
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("status bad binding kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

// TestDefaultGeneratorsProduceCollisionResistantIDs is a restart-collision regression
// test. The old sequential generators (atomic counter) reset to 0 on every process
// start and re-emitted "tenant-000001" / "job-000001", which collide with records
// already stored in a persistent core. The new crypto/rand generators must produce
// IDs that are unique across many calls and never match the old sequential format.
func TestDefaultGeneratorsProduceCollisionResistantIDs(t *testing.T) {
	t.Parallel()
	core := newFakeCore()
	// No WithTenantIDGenerator / WithJobIDGenerator overrides: exercises the new defaults.
	gw, err := New(core, RoleAuthorizer{}, jobs.NewStore())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const n = 20
	seenTenants := make(map[brainapi.TenantID]bool, n)
	for i := 0; i < n; i++ {
		result, err := gw.CreateProject(context.Background(), CreateProjectCommand{
			BindingKey: "slack:channel:C1", // fakeCore does not enforce binding uniqueness
			Principal:  brainapi.Principal{ID: "admin", Roles: []string{"admin"}},
		})
		if err != nil {
			t.Fatalf("CreateProject[%d]: %v", i, err)
		}
		if string(result.TenantID) == "tenant-000001" {
			t.Fatalf("generated tenant ID collides with old sequential format: %q", result.TenantID)
		}
		if seenTenants[result.TenantID] {
			t.Fatalf("duplicate tenant ID at iteration %d: %q", i, result.TenantID)
		}
		seenTenants[result.TenantID] = true
	}

	seenJobs := make(map[brainapi.JobID]bool, n)
	for i := 0; i < n; i++ {
		result, err := gw.Ingest(context.Background(), IngestCommand{
			BindingKey: "slack:channel:C1",
			Principal:  brainapi.Principal{ID: "U1", Roles: []string{"member"}},
			Source:     brainapi.SourceRef{URI: "file://a.pdf"},
		})
		if err != nil {
			t.Fatalf("Ingest[%d]: %v", i, err)
		}
		if string(result.JobID) == "job-000001" {
			t.Fatalf("generated job ID collides with old sequential format: %q", result.JobID)
		}
		if seenJobs[result.JobID] {
			t.Fatalf("duplicate job ID at iteration %d: %q", i, result.JobID)
		}
		seenJobs[result.JobID] = true
	}
}

// ── Async ingest-worker tests ─────────────────────────────────────────────────

// fakeCoreCompleter satisfies ingest.CoreCompleter without doing real work.
type fakeCoreCompleter struct{}

func (f *fakeCoreCompleter) CompleteJob(brainapi.TenantID, brainapi.JobID, brainapi.JobStatus, string, string) error {
	return nil
}

// TestGatewayIngestWithQueueReachesCompleted verifies that after a server-mode
// ingest (queue configured), the job transitions to completed once the worker
// has processed the completion task.
func TestGatewayIngestWithQueueReachesCompleted(t *testing.T) {
	t.Parallel()
	core := newFakeCore()
	store := jobs.NewStore()

	gw, err := New(core, RoleAuthorizer{}, store, WithJobIDGenerator(staticJobID("job-async")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Wire the worker: gateway is the GatewayCompleter, fakeCoreCompleter handles
	// the core side (fakeCore does not have CompleteJob).
	worker := ingest.NewWorker(&fakeCoreCompleter{}, gw, 64)
	worker.Start()
	defer worker.Stop()
	gw.SetQueue(worker)

	result, err := gw.Ingest(context.Background(), IngestCommand{
		BindingKey: "slack:channel:C1",
		Principal:  brainapi.Principal{ID: "U1", Roles: []string{"member"}},
		Source:     brainapi.SourceRef{URI: "file://a.pdf", Name: "a.pdf"},
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if result.JobID != "job-async" {
		t.Fatalf("jobID = %q", result.JobID)
	}

	// Block until the worker has processed the completion task.
	worker.WaitForIdle()

	snapshot, err := store.Snapshot("tenant-1", "job-async")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapshot.Status != brainapi.JobCompleted {
		t.Fatalf("status = %q, want completed", snapshot.Status)
	}
}

// alwaysFullQueue is a Queue implementation that always rejects Enqueue calls.
type alwaysFullQueue struct{}

func (alwaysFullQueue) Enqueue(context.Context, ingest.Task) error {
	return brainapi.E(brainapi.KindInternal, "test_queue", "queue full", nil)
}

// TestGatewayIngestWithFullQueueMarksJobFailed verifies that a failed enqueue
// (e.g. queue full or stopped) transitions the job to the failed state and
// returns an error to the caller.
func TestGatewayIngestWithFullQueueMarksJobFailed(t *testing.T) {
	t.Parallel()
	core := newFakeCore()
	store := jobs.NewStore()

	gw, err := New(core, RoleAuthorizer{}, store,
		WithJobIDGenerator(staticJobID("job-queue-fail")),
		WithQueue(alwaysFullQueue{}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = gw.Ingest(context.Background(), IngestCommand{
		BindingKey: "slack:channel:C1",
		Principal:  brainapi.Principal{ID: "U1", Roles: []string{"member"}},
		Source:     brainapi.SourceRef{URI: "file://a.pdf"},
	})
	if err == nil {
		t.Fatal("expected Ingest to return error when queue is full")
	}

	snapshot, snapErr := store.Snapshot("tenant-1", "job-queue-fail")
	if snapErr != nil {
		t.Fatalf("Snapshot: %v", snapErr)
	}
	if snapshot.Status != brainapi.JobFailed {
		t.Fatalf("status = %q, want failed", snapshot.Status)
	}
}

type fakeSourceLoader struct {
	loaded sources.LoadedSource
	err    error
}

func (f fakeSourceLoader) Load(context.Context, brainapi.SourceRef, map[string]string) (sources.LoadedSource, error) {
	if f.err != nil {
		return sources.LoadedSource{}, f.err
	}
	return f.loaded, nil
}
