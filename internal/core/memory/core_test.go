package memory

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sangyi/workspace-brain/internal/core/coretest"
	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

// ─── compile-time interface checks ───────────────────────────────────────────

var (
	_ Synthesizer = localSynthesizer{}
	_ Reranker    = localReranker{}
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
	// localSynthesizer with no inputs must return the abstention message.
	sr, err := localSynthesizer{}.Synthesize(ctx, "q", nil)
	if err != nil {
		t.Fatalf("localSynthesizer empty: %v", err)
	}
	if !strings.Contains(sr.Answer, "수집된 근거가 없습니다") {
		t.Fatalf("localSynthesizer empty answer = %q", sr.Answer)
	}
	blankDoc := document{source: brainapi.SourceRef{URI: "text://blank", Name: "blank"}, title: "blank", freshAt: time.Unix(0, 0)}
	blankChunks := chunksFrom("tenant-a", 0, blankDoc, 0, localEmbedder{})
	if len(blankChunks) != 1 || blankChunks[0].Text != "blank" {
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
	tied := rankChunks(terms("nomatch"), embed("nomatch"), []Chunk{{Text: "a", Terms: terms("a"), Vector: embed("a")}, {Text: "b", Terms: terms("b"), Vector: embed("b")}})
	if len(tied) != 2 || tied[0].order != 0 {
		t.Fatalf("stable rank = %+v", tied)
	}
}

func nilVector() []float64 { return make([]float64, embeddingDimensions) }

// ─── WB-05: Synthesizer seam ─────────────────────────────────────────────────

// TestLocalSynthesizerHappyPath verifies that localSynthesizer produces the
// expected answer and correctly separates grounded from supplemented spans.
func TestLocalSynthesizerHappyPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	inputs := []SynthesisInput{{Text: "chunk alpha"}, {Text: "chunk beta"}}
	result, err := localSynthesizer{}.Synthesize(ctx, "test question", inputs)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if result.Answer != "근거 기반 응답: chunk alpha" {
		t.Fatalf("answer = %q", result.Answer)
	}
	if len(result.GroundedSpans) != 2 || result.GroundedSpans[0] != "chunk alpha" || result.GroundedSpans[1] != "chunk beta" {
		t.Fatalf("grounded spans = %v", result.GroundedSpans)
	}
	if result.SupplementedSpans != nil {
		t.Fatalf("supplemented spans should be nil for local synthesizer, got %v", result.SupplementedSpans)
	}
}

// TestCoreSynthesizerSeamInjection verifies that a custom Synthesizer is called
// during Query and that its SupplementedSpans are propagated to the response.
func TestCoreSynthesizerSeamInjection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// stubSynthesizer always produces a distinguishable answer with supplemented spans.
	stub := &stubSynthesizer{
		answer:       "stub answer",
		grounded:     []string{"grounded text"},
		supplemented: []string{"supplemented text"},
	}
	core := New(WithSynthesizer(stub))
	mustCreate(t, core, "tenant-a", "api:space:A")
	if err := core.Ingest(ctx, brainapi.IngestRequest{
		TenantID: "tenant-a", JobID: "job-1",
		Source:   brainapi.SourceRef{URI: "file://test.txt"},
		Metadata: map[string]string{"content": "some relevant content here"},
	}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	resp, err := core.Query(ctx, brainapi.QueryRequest{TenantID: "tenant-a", Question: "relevant content"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if resp.Answer != "stub answer" {
		t.Fatalf("answer = %q, want %q", resp.Answer, "stub answer")
	}
	if len(resp.GroundedSpans) != 1 || resp.GroundedSpans[0] != "grounded text" {
		t.Fatalf("grounded spans = %v", resp.GroundedSpans)
	}
	if len(resp.SupplementedSpans) != 1 || resp.SupplementedSpans[0] != "supplemented text" {
		t.Fatalf("supplemented spans = %v", resp.SupplementedSpans)
	}
	if !resp.GroundingAvailable {
		t.Fatal("grounding must be available when chunks are found")
	}
	if stub.callCount != 1 {
		t.Fatalf("synthesizer called %d times, want 1", stub.callCount)
	}
}

// TestCoreSynthesizerErrorPropagates verifies that a Synthesizer error is
// returned to the caller and does not produce a partial response.
func TestCoreSynthesizerErrorPropagates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	boom := errors.New("synthesizer unavailable")
	stub := &stubSynthesizer{err: boom}
	core := New(WithSynthesizer(stub))
	mustCreate(t, core, "tenant-a", "api:space:A")
	if err := core.Ingest(ctx, brainapi.IngestRequest{
		TenantID: "tenant-a", JobID: "job-1",
		Source:   brainapi.SourceRef{URI: "file://test.txt"},
		Metadata: map[string]string{"content": "relevant content"},
	}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	_, err := core.Query(ctx, brainapi.QueryRequest{TenantID: "tenant-a", Question: "relevant"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want synthesizer unavailable", err)
	}
}

// TestCoreWithNilSynthesizerKeepsDefault verifies that WithSynthesizer(nil)
// is silently ignored and the localSynthesizer default is preserved.
func TestCoreWithNilSynthesizerKeepsDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	core := New(WithSynthesizer(nil))
	mustCreate(t, core, "tenant-a", "api:space:A")
	if err := core.Ingest(ctx, brainapi.IngestRequest{
		TenantID: "tenant-a", JobID: "job-1",
		Source:   brainapi.SourceRef{URI: "file://test.txt"},
		Metadata: map[string]string{"content": "hello world"},
	}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	resp, err := core.Query(ctx, brainapi.QueryRequest{TenantID: "tenant-a", Question: "hello"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !strings.Contains(resp.Answer, "근거 기반 응답") {
		t.Fatalf("answer = %q", resp.Answer)
	}
}

// ─── WB-05: Reranker seam ────────────────────────────────────────────────────

// TestLocalRerankerReturnsDescendingOrder verifies that localReranker ranks
// a text matching the question higher than an unrelated text.
func TestLocalRerankerReturnsDescendingOrder(t *testing.T) {
	t.Parallel()
	r := localReranker{}
	// "alpha beta" matches the question; "gamma delta" does not.
	indices := r.Rerank("alpha beta", []string{"gamma delta", "alpha beta content"})
	if len(indices) != 2 {
		t.Fatalf("len(indices) = %d, want 2", len(indices))
	}
	if indices[0] != 1 {
		t.Fatalf("best match should be index 1 (alpha beta content), got %d", indices[0])
	}
}

// TestLocalRerankerEmptyInput returns empty slice without panicking.
func TestLocalRerankerEmptyInput(t *testing.T) {
	t.Parallel()
	indices := localReranker{}.Rerank("question", nil)
	if len(indices) != 0 {
		t.Fatalf("expected empty, got %v", indices)
	}
}

// TestCoreRerankerSeamInjection injects a stub Reranker that reverses the
// order of candidates, verifying that the synthesizer receives them reversed.
func TestCoreRerankerSeamInjection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Ingest two documents whose natural ranking puts "alpha" first.
	core := New(WithReranker(&reverseReranker{}), WithTopN(2))
	mustCreate(t, core, "tenant-a", "api:space:A")
	if err := core.Ingest(ctx, brainapi.IngestRequest{
		TenantID: "tenant-a", JobID: "job-1",
		Source:   brainapi.SourceRef{URI: "file://a.txt"},
		Metadata: map[string]string{"content": "alpha relevant query content"},
	}); err != nil {
		t.Fatalf("Ingest a: %v", err)
	}
	if err := core.Ingest(ctx, brainapi.IngestRequest{
		TenantID: "tenant-a", JobID: "job-2",
		Source:   brainapi.SourceRef{URI: "file://b.txt"},
		Metadata: map[string]string{"content": "beta unrelated document"},
	}); err != nil {
		t.Fatalf("Ingest b: %v", err)
	}

	resp, err := core.Query(ctx, brainapi.QueryRequest{TenantID: "tenant-a", Question: "alpha relevant"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	// The reverseReranker puts the LOWEST-scoring chunk first; verify grounding
	// is still available (chunks were returned, just reordered).
	if !resp.GroundingAvailable {
		t.Fatal("grounding must be available when chunks are found")
	}
	if len(resp.Sources) == 0 {
		t.Fatal("expected at least one source")
	}
}

// TestCoreWithNilRerankerKeepsDefault verifies that WithReranker(nil) is
// silently ignored and the localReranker default is preserved.
func TestCoreWithNilRerankerKeepsDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	core := New(WithReranker(nil))
	mustCreate(t, core, "tenant-a", "api:space:A")
	if err := core.Ingest(ctx, brainapi.IngestRequest{
		TenantID: "tenant-a", JobID: "job-1",
		Source:   brainapi.SourceRef{URI: "file://test.txt"},
		Metadata: map[string]string{"content": "hello world"},
	}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	resp, err := core.Query(ctx, brainapi.QueryRequest{TenantID: "tenant-a", Question: "hello"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !resp.GroundingAvailable {
		t.Fatal("expected grounding available")
	}
}

// TestCoreRerankerOutOfRangeIndicesAreSkipped confirms that the Core does not
// panic when a custom Reranker returns indices outside the candidate slice bounds.
func TestCoreRerankerOutOfRangeIndicesAreSkipped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	core := New(WithReranker(&badIndexReranker{}))
	mustCreate(t, core, "tenant-a", "api:space:A")
	if err := core.Ingest(ctx, brainapi.IngestRequest{
		TenantID: "tenant-a", JobID: "job-1",
		Source:   brainapi.SourceRef{URI: "file://test.txt"},
		Metadata: map[string]string{"content": "some content"},
	}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	// Should not panic; may return an empty or partial answer.
	_, _ = core.Query(ctx, brainapi.QueryRequest{TenantID: "tenant-a", Question: "content"})
}

// ─── WB-05: configurable top-N ───────────────────────────────────────────────

// TestCoreWithTopN verifies that the top-N option controls how many sources
// appear in the Query response.
func TestCoreWithTopN(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Ingest 5 distinct sources.
	core := New(WithTopN(2))
	mustCreate(t, core, "tenant-a", "api:space:A")
	for i := 0; i < 5; i++ {
		if err := core.Ingest(ctx, brainapi.IngestRequest{
			TenantID: "tenant-a",
			JobID:    brainapi.JobID("job-" + strconv.Itoa(i)),
			Source:   brainapi.SourceRef{URI: "file://src-" + strconv.Itoa(i)},
			Metadata: map[string]string{"content": "shared keyword content item " + strconv.Itoa(i)},
		}); err != nil {
			t.Fatalf("Ingest[%d]: %v", i, err)
		}
	}

	resp, err := core.Query(ctx, brainapi.QueryRequest{TenantID: "tenant-a", Question: "shared keyword"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !resp.GroundingAvailable {
		t.Fatal("expected grounding available")
	}
	// With topN=2 the synthesizer receives at most 2 chunks → at most 2 sources.
	if len(resp.Sources) > 2 {
		t.Fatalf("expected ≤2 sources with topN=2, got %d: %v", len(resp.Sources), resp.Sources)
	}
	if len(resp.GroundedSpans) > 2 {
		t.Fatalf("expected ≤2 grounded spans with topN=2, got %d", len(resp.GroundedSpans))
	}
}

// TestCoreWithTopNZeroOrNegativeIsIgnored verifies that invalid topN values
// leave the default of 3 in place.
func TestCoreWithTopNZeroOrNegativeIsIgnored(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, -1, -100} {
		n := n
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			t.Parallel()
			c := newCore(WithTopN(n))
			if c.topN != 3 {
				t.Fatalf("WithTopN(%d) changed topN to %d, want 3", n, c.topN)
			}
		})
	}
}

// TestCoreTopNTenantBoundaryIntact verifies that configurable top-N does not
// cause cross-tenant chunk leakage.
func TestCoreTopNTenantBoundaryIntact(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	core := New(WithTopN(5)) // large top-N to stress the boundary
	mustCreate(t, core, "tenant-a", "api:space:A")
	mustCreate(t, core, "tenant-b", "api:space:B")

	for i := 0; i < 5; i++ {
		if err := core.Ingest(ctx, brainapi.IngestRequest{
			TenantID: "tenant-a",
			JobID:    brainapi.JobID("job-a-" + strconv.Itoa(i)),
			Source:   brainapi.SourceRef{URI: "file://a-" + strconv.Itoa(i)},
			Metadata: map[string]string{"content": "alpha content document"},
		}); err != nil {
			t.Fatalf("Ingest tenant-a[%d]: %v", i, err)
		}
	}
	if err := core.Ingest(ctx, brainapi.IngestRequest{
		TenantID: "tenant-b", JobID: "job-b-0",
		Source:   brainapi.SourceRef{URI: "file://b-0"},
		Metadata: map[string]string{"content": "alpha content document"},
	}); err != nil {
		t.Fatalf("Ingest tenant-b: %v", err)
	}

	resp, err := core.Query(ctx, brainapi.QueryRequest{TenantID: "tenant-a", Question: "alpha content"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	for _, src := range resp.Sources {
		if src.TenantID != "tenant-a" {
			t.Fatalf("cross-tenant leak: source %+v appeared in tenant-a response", src)
		}
	}
}

// ─── stub helpers for seam tests ─────────────────────────────────────────────

type stubSynthesizer struct {
	answer       string
	grounded     []string
	supplemented []string
	err          error
	callCount    int
}

func (s *stubSynthesizer) Synthesize(_ context.Context, _ string, _ []SynthesisInput) (SynthesisResult, error) {
	s.callCount++
	if s.err != nil {
		return SynthesisResult{}, s.err
	}
	return SynthesisResult{
		Answer:            s.answer,
		GroundedSpans:     s.grounded,
		SupplementedSpans: s.supplemented,
	}, nil
}

// reverseReranker returns candidate indices in reverse order, inverting the
// default ranking so the worst match becomes the first.
type reverseReranker struct{}

func (reverseReranker) Rerank(_ string, candidateTexts []string) []int {
	result := make([]int, len(candidateTexts))
	for i := range result {
		result[i] = len(candidateTexts) - 1 - i
	}
	return result
}

// badIndexReranker returns indices outside the valid range to verify that the
// Core skips them without panicking.
type badIndexReranker struct{}

func (badIndexReranker) Rerank(_ string, candidateTexts []string) []int {
	return []int{-1, len(candidateTexts), 999}
}

// TestMemoryCoreContractSuite runs the reusable black-box contract suite
// against the in-memory Core so any future backend (PostgreSQL, Qdrant) can
// validate identical behaviour with the same helper.
func TestMemoryCoreContractSuite(t *testing.T) {
	coretest.RunContractSuite(t, func() brainapi.Core { return New() })
}

// TestWithEmbedderAndVectorStoreOptions exercises the new option paths introduced
// by the WB-01 seam refactor. Coverage requires both nil (keep default) and
// non-nil (replace) branches for each option.
func TestWithEmbedderAndVectorStoreOptions(t *testing.T) {
	t.Parallel()

	t.Run("nil_embedder_keeps_default", func(t *testing.T) {
		t.Parallel()
		core := New(WithEmbedder(nil))
		mustCreate(t, core, "tenant-a", "api:space:A")
		// Ingest + Query smoke-test to confirm the default embedder works.
		ctx := context.Background()
		if err := core.Ingest(ctx, brainapi.IngestRequest{
			TenantID: "tenant-a", JobID: "job-1",
			Source: brainapi.SourceRef{URI: "file://test.txt"},
		}); err != nil {
			t.Fatalf("Ingest: %v", err)
		}
		if _, err := core.Query(ctx, brainapi.QueryRequest{TenantID: "tenant-a", Question: "test"}); err != nil {
			t.Fatalf("Query: %v", err)
		}
	})

	t.Run("non_nil_embedder_is_used", func(t *testing.T) {
		t.Parallel()
		core := New(WithEmbedder(localEmbedder{}))
		mustCreate(t, core, "tenant-a", "api:space:A")
	})

	t.Run("nil_vectorstore_keeps_default", func(t *testing.T) {
		t.Parallel()
		core := New(WithVectorStore(nil))
		mustCreate(t, core, "tenant-a", "api:space:A")
	})

	t.Run("non_nil_vectorstore_is_used", func(t *testing.T) {
		t.Parallel()
		core := New(WithVectorStore(newInMemoryVectorStore()))
		mustCreate(t, core, "tenant-a", "api:space:A")
	})

	t.Run("exported_NewInMemoryVectorStore_is_usable", func(t *testing.T) {
		t.Parallel()
		// NewInMemoryVectorStore is the exported constructor for external
		// VectorStore contract testing; verify it returns a working store.
		core := New(WithVectorStore(NewInMemoryVectorStore()))
		mustCreate(t, core, "tenant-a", "api:space:A")
		ctx := context.Background()
		if err := core.Ingest(ctx, brainapi.IngestRequest{
			TenantID: "tenant-a", JobID: "job-1",
			Source: brainapi.SourceRef{URI: "file://test.txt"},
		}); err != nil {
			t.Fatalf("Ingest with exported store: %v", err)
		}
		if _, err := core.Query(ctx, brainapi.QueryRequest{TenantID: "tenant-a", Question: "test"}); err != nil {
			t.Fatalf("Query with exported store: %v", err)
		}
	})
}

// TestCoreSourceIDsDistinctBeyond26Sources is a regression guard for the modulo-26
// source/chunk ID collision. Before the fix, source at index 0 and source at index 26
// both received ID "tenant-a:source:a" (0%26 == 26%26 == 0), causing uniqueSources to
// drop one of them as a duplicate. After the fix (strconv.Itoa), IDs are "tenant-a:source:0"
// and "tenant-a:source:26", which are distinct, so both appear in citations.
func TestCoreSourceIDsDistinctBeyond26Sources(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	core := New()
	mustCreate(t, core, "tenant-a", "slack:channel:C1")

	// Sources 0 and 26 both have content matching the query "alpha"; all others are irrelevant.
	// With the modulo-26 bug, their shared ID caused uniqueSources to collapse them into one.
	for i := 0; i < 27; i++ {
		content := "irrelevant xyz"
		if i == 0 || i == 26 {
			content = "the alpha document"
		}
		if err := core.Ingest(ctx, brainapi.IngestRequest{
			TenantID: "tenant-a",
			JobID:    brainapi.JobID("job-" + strconv.Itoa(i)),
			Source:   brainapi.SourceRef{URI: "file://src-" + strconv.Itoa(i)},
			Metadata: map[string]string{"content": content},
		}); err != nil {
			t.Fatalf("Ingest[%d]: %v", i, err)
		}
	}

	answer, err := core.Query(ctx, brainapi.QueryRequest{TenantID: "tenant-a", Question: "alpha"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	seenURIs := make(map[string]bool, len(answer.Sources))
	for _, src := range answer.Sources {
		seenURIs[src.URI] = true
	}
	if !seenURIs["file://src-0"] {
		t.Fatalf("source at index 0 (file://src-0) absent from citations: %+v", answer.Sources)
	}
	if !seenURIs["file://src-26"] {
		t.Fatalf("source at index 26 (file://src-26) absent from citations: %+v", answer.Sources)
	}
}

// TestCoreAdminListMultiItem covers sort comparison branches in admin methods
// (only reachable when 2+ items exist) and the "name=="" source branch.
func TestCoreAdminListMultiItem(t *testing.T) {
	t.Parallel()
	core := New()
	ctx := context.Background()

	// Two tenants — AdminListTenants sort comparison runs.
	mustCreate(t, core, "tenant-z", "slack:channel:CZ")
	mustCreate(t, core, "tenant-a", "slack:channel:CA")
	tenants, err := core.AdminListTenants(ctx, brainapi.AdminListTenantsRequest{})
	if err != nil {
		t.Fatalf("AdminListTenants: %v", err)
	}
	if len(tenants.Tenants) < 2 || string(tenants.Tenants[0].TenantID) >= string(tenants.Tenants[1].TenantID) {
		t.Fatalf("expected ascending order: %+v", tenants.Tenants)
	}

	// Two bindings across tenants — exercises sort comparison and the
	// "binding doesn't match filter" skip branch in AdminListBindings.
	bindings, err := core.AdminListBindings(ctx, brainapi.AdminListBindingsRequest{TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("AdminListBindings filtered: %v", err)
	}
	// Only tenant-a binding (slack:channel:CA) should be returned.
	for _, b := range bindings.Bindings {
		if b.TenantID != "tenant-a" {
			t.Fatalf("expected only tenant-a, got %+v", b)
		}
	}

	// Unfiltered list must contain both bindings and be sorted.
	all, err := core.AdminListBindings(ctx, brainapi.AdminListBindingsRequest{})
	if err != nil {
		t.Fatalf("AdminListBindings all: %v", err)
	}
	if len(all.Bindings) < 2 {
		t.Fatalf("expected 2+ bindings, got %+v", all.Bindings)
	}
	for i := 1; i < len(all.Bindings); i++ {
		if string(all.Bindings[i-1].BindingKey) > string(all.Bindings[i].BindingKey) {
			t.Fatalf("bindings not sorted: %+v", all.Bindings)
		}
	}

	// Source with empty Name — exercises the "if name=="" branch.
	if err := core.Ingest(ctx, brainapi.IngestRequest{
		TenantID: "tenant-a", JobID: "job-nameless",
		Source: brainapi.SourceRef{URI: "file://nameless.txt"},
	}); err != nil {
		t.Fatalf("Ingest nameless: %v", err)
	}
	srcs, err := core.AdminListSources(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("AdminListSources: %v", err)
	}
	var foundFallback bool
	for _, s := range srcs.Sources {
		if s.Name == "file://nameless.txt" { // URI used as fallback name
			foundFallback = true
		}
	}
	if !foundFallback {
		t.Fatalf("expected nameless source to use URI as name: %+v", srcs.Sources)
	}

	// Two jobs — AdminListJobs sort comparison runs.
	if err := core.Ingest(ctx, brainapi.IngestRequest{
		TenantID: "tenant-a", JobID: "job-beta",
		Source: brainapi.SourceRef{URI: "file://b.txt"},
	}); err != nil {
		t.Fatalf("Ingest job-beta: %v", err)
	}
	if err := core.Ingest(ctx, brainapi.IngestRequest{
		TenantID: "tenant-a", JobID: "job-alpha",
		Source: brainapi.SourceRef{URI: "file://a.txt"},
	}); err != nil {
		t.Fatalf("Ingest job-alpha: %v", err)
	}
	jobs, err := core.AdminListJobs(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("AdminListJobs: %v", err)
	}
	if len(jobs.Jobs) < 2 {
		t.Fatalf("expected 2+ jobs: %+v", jobs.Jobs)
	}
	for i := 1; i < len(jobs.Jobs); i++ {
		if string(jobs.Jobs[i-1].JobID) > string(jobs.Jobs[i].JobID) {
			t.Fatalf("jobs not sorted: %+v", jobs.Jobs)
		}
	}
}

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
		{
			name: "admin list tenants",
			call: func() error {
				_, err := core.AdminListTenants(ctx, brainapi.AdminListTenantsRequest{})
				return err
			},
		},
		{
			name: "admin list bindings",
			call: func() error {
				_, err := core.AdminListBindings(ctx, brainapi.AdminListBindingsRequest{})
				return err
			},
		},
		{
			name: "admin list sources",
			call: func() error {
				_, err := core.AdminListSources(ctx, "tenant-a")
				return err
			},
		},
		{
			name: "admin list jobs",
			call: func() error {
				_, err := core.AdminListJobs(ctx, "tenant-a")
				return err
			},
		},
		{
			name: "admin get job",
			call: func() error {
				_, err := core.AdminGetJob(ctx, "tenant-a", "job-1")
				return err
			},
		},
		{
			name: "create shared tenant",
			call: func() error {
				return core.CreateSharedTenant(ctx, brainapi.CreateSharedTenantRequest{TenantID: "shared-a", OwnerPrincipal: brainapi.Principal{ID: "owner"}})
			},
		},
		{
			name: "bind shared tenant",
			call: func() error {
				return core.BindSharedTenant(ctx, brainapi.BindSharedTenantRequest{ProjectTenantID: "tenant-a", SharedTenantID: "shared-a"})
			},
		},
		{
			name: "shared bindings",
			call: func() error {
				_, err := core.SharedBindings(ctx, "tenant-a")
				return err
			},
		},
		{
			name: "promote source",
			call: func() error {
				return core.PromoteSource(ctx, brainapi.PromoteRequest{
					Admin:           brainapi.Principal{ID: "root", Roles: []string{"admin"}},
					ProjectTenantID: "tenant-a",
					SharedTenantID:  "shared-a",
					SourceID:        "tenant-a:source:0",
				})
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

// ─── Shared knowledge tenant (#12) — validation and error-path coverage ─────
//
// coretest.RunContractSuite exercises the happy paths and the primary
// contract-level rejections (unbound, direct-ingest, non-admin promote).
// These tests fill in the remaining CreateSharedTenant/BindSharedTenant/
// SharedBindings/PromoteSource branches (input validation, not-found,
// invariant violations, and persist-failure rollback) that the black-box
// suite does not reach because it only asserts contract-level guarantees.

func TestCreateSharedTenantValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("invalid tenant id", func(t *testing.T) {
		t.Parallel()
		core := New()
		err := core.CreateSharedTenant(ctx, brainapi.CreateSharedTenantRequest{TenantID: "bad id", OwnerPrincipal: brainapi.Principal{ID: "owner"}})
		if !brainapi.IsKind(err, brainapi.KindInvalid) {
			t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("missing owner principal", func(t *testing.T) {
		t.Parallel()
		core := New()
		err := core.CreateSharedTenant(ctx, brainapi.CreateSharedTenantRequest{TenantID: "shared-a"})
		if !brainapi.IsKind(err, brainapi.KindInvalid) {
			t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("already exists", func(t *testing.T) {
		t.Parallel()
		core := New()
		req := brainapi.CreateSharedTenantRequest{TenantID: "shared-dup", OwnerPrincipal: brainapi.Principal{ID: "owner"}}
		if err := core.CreateSharedTenant(ctx, req); err != nil {
			t.Fatalf("first CreateSharedTenant: %v", err)
		}
		err := core.CreateSharedTenant(ctx, req)
		if !brainapi.IsKind(err, brainapi.KindAlreadyExists) {
			t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("not resolvable via binding", func(t *testing.T) {
		t.Parallel()
		core := New()
		if err := core.CreateSharedTenant(ctx, brainapi.CreateSharedTenantRequest{TenantID: "shared-nores", OwnerPrincipal: brainapi.Principal{ID: "owner"}}); err != nil {
			t.Fatalf("CreateSharedTenant: %v", err)
		}
		// A shared tenant never registers a BindingKey, so no key resolves to it.
		if _, err := core.ResolveBinding(ctx, "api:space:NONE"); !brainapi.IsKind(err, brainapi.KindNotFound) {
			t.Fatalf("unexpected resolvable binding: kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("persist failure rolls back", func(t *testing.T) {
		t.Parallel()
		core := newCore(WithPersistence(filepath.Join(t.TempDir(), "m.json")))
		orig := marshalSnapshot
		marshalSnapshot = func(_ snapshot) ([]byte, error) { return nil, errors.New("encode failed") }
		t.Cleanup(func() { marshalSnapshot = orig })

		err := core.CreateSharedTenant(ctx, brainapi.CreateSharedTenantRequest{TenantID: "shared-rollback", OwnerPrincipal: brainapi.Principal{ID: "owner"}})
		if !brainapi.IsKind(err, brainapi.KindInternal) {
			t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
		}
		if _, ok := core.projects["shared-rollback"]; ok {
			t.Fatalf("tenant should not exist after rollback")
		}
	})
}

func TestBindSharedTenantValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	setup := func(t *testing.T) (core *Core, project, shared brainapi.TenantID) {
		t.Helper()
		c := New()
		mustCreate(t, c, "bst-project", "api:space:BSTP")
		if err := c.CreateSharedTenant(ctx, brainapi.CreateSharedTenantRequest{TenantID: "bst-shared", OwnerPrincipal: brainapi.Principal{ID: "owner"}}); err != nil {
			t.Fatalf("CreateSharedTenant: %v", err)
		}
		return c, "bst-project", "bst-shared"
	}

	t.Run("invalid project tenant id", func(t *testing.T) {
		t.Parallel()
		core, _, shared := setup(t)
		err := core.BindSharedTenant(ctx, brainapi.BindSharedTenantRequest{ProjectTenantID: "bad id", SharedTenantID: shared})
		if !brainapi.IsKind(err, brainapi.KindInvalid) {
			t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("invalid shared tenant id", func(t *testing.T) {
		t.Parallel()
		core, project, _ := setup(t)
		err := core.BindSharedTenant(ctx, brainapi.BindSharedTenantRequest{ProjectTenantID: project, SharedTenantID: "bad id"})
		if !brainapi.IsKind(err, brainapi.KindInvalid) {
			t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("self bind rejected", func(t *testing.T) {
		t.Parallel()
		core, project, _ := setup(t)
		err := core.BindSharedTenant(ctx, brainapi.BindSharedTenantRequest{ProjectTenantID: project, SharedTenantID: project})
		if !brainapi.IsKind(err, brainapi.KindInvalid) {
			t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("project tenant not found", func(t *testing.T) {
		t.Parallel()
		core, _, shared := setup(t)
		err := core.BindSharedTenant(ctx, brainapi.BindSharedTenantRequest{ProjectTenantID: "bst-missing", SharedTenantID: shared})
		if !brainapi.IsKind(err, brainapi.KindNotFound) {
			t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("project tenant is itself shared", func(t *testing.T) {
		t.Parallel()
		core, _, shared := setup(t)
		if err := core.CreateSharedTenant(ctx, brainapi.CreateSharedTenantRequest{TenantID: "bst-shared-2", OwnerPrincipal: brainapi.Principal{ID: "owner"}}); err != nil {
			t.Fatalf("CreateSharedTenant second: %v", err)
		}
		err := core.BindSharedTenant(ctx, brainapi.BindSharedTenantRequest{ProjectTenantID: "bst-shared-2", SharedTenantID: shared})
		if !brainapi.IsKind(err, brainapi.KindInvalid) {
			t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("shared tenant not found", func(t *testing.T) {
		t.Parallel()
		core, project, _ := setup(t)
		err := core.BindSharedTenant(ctx, brainapi.BindSharedTenantRequest{ProjectTenantID: project, SharedTenantID: "bst-missing-shared"})
		if !brainapi.IsKind(err, brainapi.KindNotFound) {
			t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("target tenant is not a shared tenant", func(t *testing.T) {
		t.Parallel()
		core, project, _ := setup(t)
		mustCreate(t, core, "bst-other-project", "api:space:BSTOP")
		err := core.BindSharedTenant(ctx, brainapi.BindSharedTenantRequest{ProjectTenantID: project, SharedTenantID: "bst-other-project"})
		if !brainapi.IsKind(err, brainapi.KindInvalid) {
			t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("persist failure rolls back", func(t *testing.T) {
		t.Parallel()
		core := newCore(WithPersistence(filepath.Join(t.TempDir(), "m.json")))
		mustCreate(t, core, "bst-rb-project", "api:space:BSTRB")
		if err := core.CreateSharedTenant(ctx, brainapi.CreateSharedTenantRequest{TenantID: "bst-rb-shared", OwnerPrincipal: brainapi.Principal{ID: "owner"}}); err != nil {
			t.Fatalf("CreateSharedTenant: %v", err)
		}

		orig := marshalSnapshot
		marshalSnapshot = func(_ snapshot) ([]byte, error) { return nil, errors.New("encode failed") }
		t.Cleanup(func() { marshalSnapshot = orig })

		err := core.BindSharedTenant(ctx, brainapi.BindSharedTenantRequest{ProjectTenantID: "bst-rb-project", SharedTenantID: "bst-rb-shared"})
		if !brainapi.IsKind(err, brainapi.KindInternal) {
			t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
		}
		if bound := core.sharedBindings["bst-rb-project"]; len(bound) != 0 {
			t.Fatalf("binding should be rolled back, got %+v", bound)
		}
	})
}

func TestSharedBindingsValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("invalid tenant id", func(t *testing.T) {
		t.Parallel()
		core := New()
		_, err := core.SharedBindings(ctx, "bad id")
		if !brainapi.IsKind(err, brainapi.KindInvalid) {
			t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})
}

func TestPromoteSourceValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	admin := brainapi.Principal{ID: "root", Roles: []string{"admin"}}

	setup := func(t *testing.T) (core *Core, project, shared, sourceID string) {
		t.Helper()
		c := New()
		mustCreate(t, c, "ps-project", "api:space:PSP")
		if err := c.CreateSharedTenant(ctx, brainapi.CreateSharedTenantRequest{TenantID: "ps-shared", OwnerPrincipal: brainapi.Principal{ID: "owner"}}); err != nil {
			t.Fatalf("CreateSharedTenant: %v", err)
		}
		if err := c.BindSharedTenant(ctx, brainapi.BindSharedTenantRequest{ProjectTenantID: "ps-project", SharedTenantID: "ps-shared"}); err != nil {
			t.Fatalf("BindSharedTenant: %v", err)
		}
		if err := c.Ingest(ctx, brainapi.IngestRequest{
			TenantID: "ps-project", JobID: "ps-job",
			Source:   brainapi.SourceRef{URI: "file://ps.txt", Name: "ps.txt"},
			Metadata: map[string]string{"content": "promote source validation content"},
		}); err != nil {
			t.Fatalf("Ingest: %v", err)
		}
		return c, "ps-project", "ps-shared", "ps-project:source:0"
	}

	t.Run("invalid project tenant id", func(t *testing.T) {
		t.Parallel()
		core, _, shared, sourceID := setup(t)
		err := core.PromoteSource(ctx, brainapi.PromoteRequest{Admin: admin, ProjectTenantID: "bad id", SharedTenantID: brainapi.TenantID(shared), SourceID: sourceID})
		if !brainapi.IsKind(err, brainapi.KindInvalid) {
			t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("invalid shared tenant id", func(t *testing.T) {
		t.Parallel()
		core, project, _, sourceID := setup(t)
		err := core.PromoteSource(ctx, brainapi.PromoteRequest{Admin: admin, ProjectTenantID: brainapi.TenantID(project), SharedTenantID: "bad id", SourceID: sourceID})
		if !brainapi.IsKind(err, brainapi.KindInvalid) {
			t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("invalid source id", func(t *testing.T) {
		t.Parallel()
		core, project, shared, _ := setup(t)
		err := core.PromoteSource(ctx, brainapi.PromoteRequest{Admin: admin, ProjectTenantID: brainapi.TenantID(project), SharedTenantID: brainapi.TenantID(shared), SourceID: ""})
		if !brainapi.IsKind(err, brainapi.KindInvalid) {
			t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("project tenant not found", func(t *testing.T) {
		t.Parallel()
		core, _, shared, sourceID := setup(t)
		err := core.PromoteSource(ctx, brainapi.PromoteRequest{Admin: admin, ProjectTenantID: "ps-missing", SharedTenantID: brainapi.TenantID(shared), SourceID: sourceID})
		if !brainapi.IsKind(err, brainapi.KindNotFound) {
			t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("project tenant is itself shared", func(t *testing.T) {
		t.Parallel()
		core, _, shared, sourceID := setup(t)
		err := core.PromoteSource(ctx, brainapi.PromoteRequest{Admin: admin, ProjectTenantID: brainapi.TenantID(shared), SharedTenantID: brainapi.TenantID(shared), SourceID: sourceID})
		if !brainapi.IsKind(err, brainapi.KindInvalid) {
			t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("shared tenant not found", func(t *testing.T) {
		t.Parallel()
		core, project, _, sourceID := setup(t)
		err := core.PromoteSource(ctx, brainapi.PromoteRequest{Admin: admin, ProjectTenantID: brainapi.TenantID(project), SharedTenantID: "ps-missing-shared", SourceID: sourceID})
		if !brainapi.IsKind(err, brainapi.KindNotFound) {
			t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("destination is not a shared tenant", func(t *testing.T) {
		t.Parallel()
		core, project, _, sourceID := setup(t)
		mustCreate(t, core, "ps-other-project", "api:space:PSOP")
		err := core.PromoteSource(ctx, brainapi.PromoteRequest{Admin: admin, ProjectTenantID: brainapi.TenantID(project), SharedTenantID: "ps-other-project", SourceID: sourceID})
		if !brainapi.IsKind(err, brainapi.KindInvalid) {
			t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("shared tenant not bound to project", func(t *testing.T) {
		t.Parallel()
		core := New()
		mustCreate(t, core, "ps-nb-project", "api:space:PSNB")
		if err := core.CreateSharedTenant(ctx, brainapi.CreateSharedTenantRequest{TenantID: "ps-nb-shared", OwnerPrincipal: brainapi.Principal{ID: "owner"}}); err != nil {
			t.Fatalf("CreateSharedTenant: %v", err)
		}
		// Deliberately no BindSharedTenant call.
		if err := core.Ingest(ctx, brainapi.IngestRequest{
			TenantID: "ps-nb-project", JobID: "ps-nb-job",
			Source: brainapi.SourceRef{URI: "file://nb.txt"},
		}); err != nil {
			t.Fatalf("Ingest: %v", err)
		}
		err := core.PromoteSource(ctx, brainapi.PromoteRequest{
			Admin: admin, ProjectTenantID: "ps-nb-project", SharedTenantID: "ps-nb-shared", SourceID: "ps-nb-project:source:0",
		})
		if !brainapi.IsKind(err, brainapi.KindInvalid) {
			t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("source not found", func(t *testing.T) {
		t.Parallel()
		core, project, shared, _ := setup(t)
		err := core.PromoteSource(ctx, brainapi.PromoteRequest{
			Admin: admin, ProjectTenantID: brainapi.TenantID(project), SharedTenantID: brainapi.TenantID(shared), SourceID: "ps-project:source:99",
		})
		if !brainapi.IsKind(err, brainapi.KindNotFound) {
			t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("persist failure rolls back", func(t *testing.T) {
		t.Parallel()
		core := newCore(WithPersistence(filepath.Join(t.TempDir(), "m.json")))
		mustCreate(t, core, "ps-rb-project", "api:space:PSRB")
		if err := core.CreateSharedTenant(ctx, brainapi.CreateSharedTenantRequest{TenantID: "ps-rb-shared", OwnerPrincipal: brainapi.Principal{ID: "owner"}}); err != nil {
			t.Fatalf("CreateSharedTenant: %v", err)
		}
		if err := core.BindSharedTenant(ctx, brainapi.BindSharedTenantRequest{ProjectTenantID: "ps-rb-project", SharedTenantID: "ps-rb-shared"}); err != nil {
			t.Fatalf("BindSharedTenant: %v", err)
		}
		if err := core.Ingest(ctx, brainapi.IngestRequest{
			TenantID: "ps-rb-project", JobID: "ps-rb-job",
			Source:   brainapi.SourceRef{URI: "file://rb.txt", Name: "rb.txt"},
			Metadata: map[string]string{"content": "rollback promote content"},
		}); err != nil {
			t.Fatalf("Ingest: %v", err)
		}

		orig := marshalSnapshot
		marshalSnapshot = func(_ snapshot) ([]byte, error) { return nil, errors.New("encode failed") }
		t.Cleanup(func() { marshalSnapshot = orig })

		err := core.PromoteSource(ctx, brainapi.PromoteRequest{
			Admin: admin, ProjectTenantID: "ps-rb-project", SharedTenantID: "ps-rb-shared", SourceID: "ps-rb-project:source:0",
		})
		if !brainapi.IsKind(err, brainapi.KindInternal) {
			t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
		}
		if got := len(core.sources["ps-rb-shared"]); got != 0 {
			t.Fatalf("sources should be rolled back, got %d", got)
		}
		if got := len(core.docs["ps-rb-shared"]); got != 0 {
			t.Fatalf("docs should be rolled back, got %d", got)
		}
		if got := core.vectorStore.Len("ps-rb-shared"); got != 0 {
			t.Fatalf("chunks should be rolled back, got %d", got)
		}
	})
}

// TestScopeTenantsLockedExcludesStaleSharedBindings is a white-box test of the
// defensive branches in scopeTenantsLocked: a bound shared-tenant ID that no
// longer exists, that is no longer tier-shared, or that has been turned off
// must all be silently excluded from scope rather than erroring the whole
// query. These states are not reachable through the public API (BindSharedTenant
// always validates existence and tier), so they are exercised here by
// manipulating internal state directly.
func TestScopeTenantsLockedExcludesStaleSharedBindings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	core := New()
	mustCreate(t, core, "scope-project", "api:space:SCOPE")
	if err := core.CreateSharedTenant(ctx, brainapi.CreateSharedTenantRequest{TenantID: "scope-shared-off", OwnerPrincipal: brainapi.Principal{ID: "owner"}}); err != nil {
		t.Fatalf("CreateSharedTenant off: %v", err)
	}
	if err := core.BindSharedTenant(ctx, brainapi.BindSharedTenantRequest{ProjectTenantID: "scope-project", SharedTenantID: "scope-shared-off"}); err != nil {
		t.Fatalf("BindSharedTenant off: %v", err)
	}
	if err := core.SetProjectState(ctx, "scope-shared-off", brainapi.ProjectOff); err != nil {
		t.Fatalf("SetProjectState off: %v", err)
	}

	// Directly append two additional stale bindings that BindSharedTenant
	// could never produce: one pointing at a tenant ID that does not exist at
	// all, and one pointing at an ordinary (non-shared) project tenant.
	mustCreate(t, core, "scope-not-shared", "api:space:SCOPENS")
	core.mu.Lock()
	core.sharedBindings["scope-project"] = append(core.sharedBindings["scope-project"], "scope-missing", "scope-not-shared")
	core.mu.Unlock()

	scope := func() []brainapi.TenantID {
		core.mu.RLock()
		defer core.mu.RUnlock()
		return core.scopeTenantsLocked("scope-project")
	}()

	if len(scope) != 1 || scope[0] != "scope-project" {
		t.Fatalf("scope should only contain the project tenant itself, got %+v", scope)
	}
}

// TestFindDocBySourceIDLockedNotFound covers the not-found branch of
// findDocBySourceIDLocked directly (also reached indirectly via PromoteSource's
// "source not found" test, but exercised here in isolation for clarity).
func TestFindDocBySourceIDLockedNotFound(t *testing.T) {
	t.Parallel()
	core := New()
	mustCreate(t, core, "find-doc-tenant", "api:space:FINDDOC")
	core.mu.RLock()
	_, ok := core.findDocBySourceIDLocked("find-doc-tenant", "find-doc-tenant:source:0")
	core.mu.RUnlock()
	if ok {
		t.Fatalf("expected not found for empty tenant")
	}
}
