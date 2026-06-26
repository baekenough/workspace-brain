package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

func TestCoreCreateResolveAndDuplicateProtection(t *testing.T) {
	t.Parallel()
	core := New()
	req := brainapi.CreateProjectRequest{TenantID: "tenant-a", BindingKey: "slack:channel:C1", OwnerPrincipal: brainapi.Principal{ID: "U1"}}
	if err := core.CreateProject(context.Background(), req); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	tenantID, err := core.ResolveBinding(context.Background(), "slack:channel:C1")
	if err != nil {
		t.Fatalf("ResolveBinding: %v", err)
	}
	if tenantID != "tenant-a" {
		t.Fatalf("tenant = %q", tenantID)
	}
	if err := core.CreateProject(context.Background(), req); !brainapi.IsKind(err, brainapi.KindAlreadyExists) {
		t.Fatalf("duplicate tenant kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if err := core.CreateProject(context.Background(), brainapi.CreateProjectRequest{TenantID: "tenant-b", BindingKey: "slack:channel:C1", OwnerPrincipal: brainapi.Principal{ID: "U1"}}); !brainapi.IsKind(err, brainapi.KindAlreadyExists) {
		t.Fatalf("duplicate binding kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestCoreTenantScopedIngestQueryDiscoverStatus(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)
	core := New(WithClock(func() time.Time { return fixed }))
	ctx := context.Background()
	mustCreate(t, core, "tenant-a", "slack:channel:C1")
	mustCreate(t, core, "tenant-b", "slack:channel:C2")

	if err := core.Ingest(ctx, brainapi.IngestRequest{TenantID: "tenant-a", JobID: "job-1", Source: brainapi.SourceRef{URI: "file://a.pdf", Name: "a.pdf", MimeType: "application/pdf"}}); err != nil {
		t.Fatalf("Ingest tenant-a: %v", err)
	}
	if err := core.Ingest(ctx, brainapi.IngestRequest{TenantID: "tenant-b", JobID: "job-1", Source: brainapi.SourceRef{URI: "file://b.pdf", Name: "b.pdf", MimeType: "application/pdf"}}); err != nil {
		t.Fatalf("Ingest tenant-b: %v", err)
	}

	a, err := core.Query(ctx, brainapi.QueryRequest{TenantID: "tenant-a", Question: "무엇이 있나?"})
	if err != nil {
		t.Fatalf("Query tenant-a: %v", err)
	}
	if len(a.Sources) != 1 || a.Sources[0].URI != "file://a.pdf" || a.Sources[0].TenantID != "tenant-a" {
		t.Fatalf("tenant-a query leaked or missed source: %+v", a.Sources)
	}

	b, err := core.Discover(ctx, brainapi.DiscoverRequest{TenantID: "tenant-b", Query: ""})
	if err != nil {
		t.Fatalf("Discover tenant-b: %v", err)
	}
	if len(b.Results) != 1 || b.Results[0].Title != "b.pdf" || !b.Results[0].FreshAt.Equal(fixed) {
		t.Fatalf("tenant-b discover = %+v", b.Results)
	}

	status, err := core.JobStatus(ctx, "tenant-a", "job-1")
	if err != nil {
		t.Fatalf("JobStatus: %v", err)
	}
	if status.Status != brainapi.JobRunning || status.TenantID != "tenant-a" {
		t.Fatalf("status = %+v", status)
	}
}

func TestCoreOffProjectRejectsServing(t *testing.T) {
	t.Parallel()
	core := New()
	ctx := context.Background()
	mustCreate(t, core, "tenant-a", "slack:channel:C1")
	if err := core.SetProjectState(ctx, "tenant-a", brainapi.ProjectOff); err != nil {
		t.Fatalf("SetProjectState: %v", err)
	}
	_, err := core.Query(ctx, brainapi.QueryRequest{TenantID: "tenant-a", Question: "?"})
	if !brainapi.IsKind(err, brainapi.KindConflict) {
		t.Fatalf("off query kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestCoreCompleteJob(t *testing.T) {
	t.Parallel()
	core := New()
	ctx := context.Background()
	mustCreate(t, core, "tenant-a", "slack:channel:C1")
	if err := core.Ingest(ctx, brainapi.IngestRequest{TenantID: "tenant-a", JobID: "job-1", Source: brainapi.SourceRef{URI: "file://a.pdf"}}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if err := core.CompleteJob("tenant-a", "job-1", brainapi.JobCompleted, "result://1", ""); err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}
	status, err := core.JobStatus(ctx, "tenant-a", "job-1")
	if err != nil {
		t.Fatalf("JobStatus: %v", err)
	}
	if status.Status != brainapi.JobCompleted || status.ResultRef != "result://1" {
		t.Fatalf("status = %+v", status)
	}
}

func mustCreate(t *testing.T, core *Core, tenantID brainapi.TenantID, binding brainapi.BindingKey) {
	t.Helper()
	if err := core.CreateProject(context.Background(), brainapi.CreateProjectRequest{TenantID: tenantID, BindingKey: binding, OwnerPrincipal: brainapi.Principal{ID: "owner"}}); err != nil {
		t.Fatalf("CreateProject(%q): %v", tenantID, err)
	}
}

func TestCoreValidationErrors(t *testing.T) {
	t.Parallel()
	core := New()
	ctx := context.Background()
	if _, err := core.ResolveBinding(ctx, "bad"); !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("resolve invalid kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if _, err := core.ResolveBinding(ctx, "slack:channel:missing"); !brainapi.IsKind(err, brainapi.KindNotFound) {
		t.Fatalf("resolve missing kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{TenantID: "bad tenant", BindingKey: "slack:channel:C1", OwnerPrincipal: brainapi.Principal{ID: "U"}}); !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("create bad tenant kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{TenantID: "tenant-a", BindingKey: "bad", OwnerPrincipal: brainapi.Principal{ID: "U"}}); !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("create bad binding kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{TenantID: "tenant-a", BindingKey: "slack:channel:C1"}); !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("create missing owner kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if err := core.Ingest(ctx, brainapi.IngestRequest{TenantID: "bad tenant", JobID: "job-1", Source: brainapi.SourceRef{URI: "file://a"}}); !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("ingest bad tenant kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if err := core.Ingest(ctx, brainapi.IngestRequest{TenantID: "tenant-a", JobID: "bad job", Source: brainapi.SourceRef{URI: "file://a"}}); !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("ingest bad job kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if err := core.Ingest(ctx, brainapi.IngestRequest{TenantID: "tenant-a", JobID: "job-1"}); !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("ingest missing source kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if err := core.Ingest(ctx, brainapi.IngestRequest{TenantID: "tenant-a", JobID: "job-1", Source: brainapi.SourceRef{URI: "file://a"}}); !brainapi.IsKind(err, brainapi.KindNotFound) {
		t.Fatalf("ingest missing tenant kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestCoreMoreErrorsAndBranches(t *testing.T) {
	t.Parallel()
	core := New()
	ctx := context.Background()
	mustCreate(t, core, "tenant-a", "slack:channel:C1")
	if err := core.Ingest(ctx, brainapi.IngestRequest{TenantID: "tenant-a", JobID: "job-1", Source: brainapi.SourceRef{URI: "file://a"}}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if err := core.Ingest(ctx, brainapi.IngestRequest{TenantID: "tenant-a", JobID: "job-1", Source: brainapi.SourceRef{URI: "file://a"}}); !brainapi.IsKind(err, brainapi.KindAlreadyExists) {
		t.Fatalf("duplicate job kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if err := core.CompleteJob("tenant-a", "job-1", brainapi.JobRunning, "", ""); !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("complete nonterminal kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if err := core.CompleteJob("tenant-a", "missing", brainapi.JobFailed, "", "boom"); !brainapi.IsKind(err, brainapi.KindNotFound) {
		t.Fatalf("complete missing kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if _, err := core.JobStatus(ctx, "bad tenant", "job-1"); !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("status bad tenant kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if _, err := core.JobStatus(ctx, "tenant-a", "bad job"); !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("status bad job kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if _, err := core.JobStatus(ctx, "tenant-a", "missing"); !brainapi.IsKind(err, brainapi.KindNotFound) {
		t.Fatalf("status missing kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if _, err := core.Query(ctx, brainapi.QueryRequest{TenantID: "bad tenant", Question: "q"}); !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("query bad tenant kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if _, err := core.Query(ctx, brainapi.QueryRequest{TenantID: "tenant-a"}); !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("query blank kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if _, err := core.Discover(ctx, brainapi.DiscoverRequest{TenantID: "bad tenant"}); !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("discover bad tenant kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if err := core.SetProjectState(ctx, "bad tenant", brainapi.ProjectOn); !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("state bad tenant kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if err := core.SetProjectState(ctx, "tenant-a", brainapi.ProjectState("weird")); !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("state weird kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if err := core.SetProjectState(ctx, "missing", brainapi.ProjectOn); !brainapi.IsKind(err, brainapi.KindNotFound) {
		t.Fatalf("state missing kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestCoreCloneMapAndUntitledSources(t *testing.T) {
	t.Parallel()
	core := New()
	ctx := context.Background()
	meta := map[string]string{"name": "original"}
	if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{TenantID: "tenant-a", BindingKey: "slack:channel:C1", OwnerPrincipal: brainapi.Principal{ID: "owner"}, Metadata: meta}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	meta["name"] = "mutated"
	if err := core.Ingest(ctx, brainapi.IngestRequest{TenantID: "tenant-a", JobID: "job-1", Source: brainapi.SourceRef{URI: "file://unnamed"}}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	q, err := core.Query(ctx, brainapi.QueryRequest{TenantID: "tenant-a", Question: "q"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(q.Sources) != 1 || q.Sources[0].Title != "file://unnamed" {
		t.Fatalf("sources = %+v", q.Sources)
	}
	d, err := core.Discover(ctx, brainapi.DiscoverRequest{TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(d.Results) != 1 || d.Results[0].Title != "file://unnamed" {
		t.Fatalf("results = %+v", d.Results)
	}
}

func TestCoreDiscoverOffProject(t *testing.T) {
	t.Parallel()
	core := New()
	ctx := context.Background()
	mustCreate(t, core, "tenant-a", "slack:channel:C1")
	if err := core.SetProjectState(ctx, "tenant-a", brainapi.ProjectOff); err != nil {
		t.Fatalf("SetProjectState: %v", err)
	}
	_, err := core.Discover(ctx, brainapi.DiscoverRequest{TenantID: "tenant-a"})
	if !brainapi.IsKind(err, brainapi.KindConflict) {
		t.Fatalf("discover off kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestCoreGroundedRetrievalRankingAndFiltering(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC)
	core := New(WithClock(func() time.Time { return fixed }))
	ctx := context.Background()
	mustCreate(t, core, "tenant-a", "api:space:A")
	mustCreate(t, core, "tenant-b", "api:space:B")
	long := strings.Repeat("alpha beta gamma ", 60) + "needle tenant alpha"
	if err := core.Ingest(ctx, brainapi.IngestRequest{TenantID: "tenant-a", JobID: "job-1", Source: brainapi.SourceRef{URI: "text://alpha", Name: "alpha-doc", MimeType: "text/plain"}, Metadata: map[string]string{"content": long}}); err != nil {
		t.Fatalf("Ingest a1: %v", err)
	}
	if err := core.Ingest(ctx, brainapi.IngestRequest{TenantID: "tenant-a", JobID: "job-2", Source: brainapi.SourceRef{URI: "text://beta", Name: "beta-doc", MimeType: "text/plain"}, Metadata: map[string]string{"content": "delta epsilon zeta"}}); err != nil {
		t.Fatalf("Ingest a2: %v", err)
	}
	if err := core.Ingest(ctx, brainapi.IngestRequest{TenantID: "tenant-b", JobID: "job-1", Source: brainapi.SourceRef{URI: "text://other", Name: "other-doc", MimeType: "text/plain"}, Metadata: map[string]string{"content": "needle from another tenant"}}); err != nil {
		t.Fatalf("Ingest b: %v", err)
	}
	answer, err := core.Query(ctx, brainapi.QueryRequest{TenantID: "tenant-a", Question: "needle alpha"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !answer.GroundingAvailable || len(answer.Sources) == 0 || len(answer.GroundedSpans) == 0 {
		t.Fatalf("answer not grounded: %+v", answer)
	}
	if answer.Sources[0].TenantID != "tenant-a" || answer.Sources[0].URI == "text://other" || !strings.Contains(answer.Answer, "근거 기반 응답") {
		t.Fatalf("answer leaked or malformed: %+v", answer)
	}
	discovered, err := core.Discover(ctx, brainapi.DiscoverRequest{TenantID: "tenant-a", Query: "delta"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(discovered.Results) != 1 || discovered.Results[0].Title != "beta-doc" || !discovered.Results[0].FreshAt.Equal(fixed) {
		t.Fatalf("discover filtered = %+v", discovered.Results)
	}
	none, err := core.Discover(ctx, brainapi.DiscoverRequest{TenantID: "tenant-a", Query: "missing"})
	if err != nil {
		t.Fatalf("Discover missing: %v", err)
	}
	if len(none.Results) != 0 {
		t.Fatalf("unexpected discover results = %+v", none.Results)
	}
}

func TestCoreQueryWithoutSourcesAndRetrievalHelpers(t *testing.T) {
	t.Parallel()
	core := New()
	ctx := context.Background()
	mustCreate(t, core, "tenant-a", "api:space:A")
	answer, err := core.Query(ctx, brainapi.QueryRequest{TenantID: "tenant-a", Question: "anything"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if answer.GroundingAvailable || !strings.Contains(answer.Answer, "수집된 근거가 없습니다") {
		t.Fatalf("empty answer = %+v", answer)
	}
	if got := synthesize("q", nil); !strings.Contains(got, "수집된 근거가 없습니다") {
		t.Fatalf("synthesize empty = %q", got)
	}
	blankDoc := document{source: brainapi.SourceRef{URI: "text://blank", Name: "blank"}, title: "blank", freshAt: time.Unix(0, 0)}
	blankChunks := chunksFrom("tenant-a", 0, blankDoc, 0)
	if len(blankChunks) != 1 || blankChunks[0].text != "blank" {
		t.Fatalf("blank chunks = %+v", blankChunks)
	}
	left := map[string]int{"x": 1, "y": 3}
	right := map[string]int{"x": 2, "y": 1}
	if got := overlap(left, right); got != 2 {
		t.Fatalf("overlap = %d", got)
	}
	if got := cosine(nilVector(), embed("x")); got != 0 {
		t.Fatalf("zero cosine = %f", got)
	}
	tied := rankChunks("nomatch", []chunk{{text: "a", terms: terms("a"), vector: embed("a")}, {text: "b", terms: terms("b"), vector: embed("b")}})
	if len(tied) != 2 || tied[0].order != 0 {
		t.Fatalf("stable rank = %+v", tied)
	}
}

func nilVector() []float64 { return make([]float64, embeddingDimensions) }

func TestCoreReturnsReadyErrorForServingAndMutationMethods(t *testing.T) {
	t.Parallel()
	core := New()
	core.initErr = errors.New("snapshot unavailable")
	ctx := context.Background()

	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "create project",
			call: func() error {
				return core.CreateProject(ctx, brainapi.CreateProjectRequest{TenantID: "tenant-a", BindingKey: "api:space:A", OwnerPrincipal: brainapi.Principal{ID: "owner"}})
			},
		},
		{
			name: "ingest",
			call: func() error {
				return core.Ingest(ctx, brainapi.IngestRequest{TenantID: "tenant-a", JobID: "job-1", Source: brainapi.SourceRef{URI: "text://a"}})
			},
		},
		{
			name: "complete job",
			call: func() error {
				return core.CompleteJob("tenant-a", "job-1", brainapi.JobCompleted, "", "")
			},
		},
		{
			name: "job status",
			call: func() error {
				_, err := core.JobStatus(ctx, "tenant-a", "job-1")
				return err
			},
		},
		{
			name: "query",
			call: func() error {
				_, err := core.Query(ctx, brainapi.QueryRequest{TenantID: "tenant-a", Question: "ready?"})
				return err
			},
		},
		{
			name: "discover",
			call: func() error {
				_, err := core.Discover(ctx, brainapi.DiscoverRequest{TenantID: "tenant-a"})
				return err
			},
		},
		{
			name: "set project state",
			call: func() error {
				return core.SetProjectState(ctx, "tenant-a", brainapi.ProjectOn)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if !brainapi.IsKind(err, brainapi.KindInternal) {
				t.Fatalf("ready error kind=%q err=%v", brainapi.KindOf(err), err)
			}
			if !strings.Contains(err.Error(), "memory core persistence is unavailable") {
				t.Fatalf("ready error did not explain persistence failure: %v", err)
			}
		})
	}
}
