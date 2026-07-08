package frontend

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sangyi/workspace-brain/internal/control/gateway"
	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

func TestDispatcherUsesSurfaceNeutralEnvelope(t *testing.T) {
	t.Parallel()
	gw := &fakeGateway{answer: "grounded answer", jobStatus: brainapi.JobCompleted}
	d := NewDispatcher(gw)
	req := Request{BindingKey: "web:space:S1", Principal: brainapi.Principal{Source: "web", ID: "user-1", Roles: []string{"member"}}}

	res, err := d.Handle(context.Background(), withText(req, "ask what changed?"))
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if res.Visibility != VisibilityPrivate || res.Text != "grounded answer" {
		t.Fatalf("ask response = %+v", res)
	}
	if gw.asked.BindingKey != "web:space:S1" || gw.asked.Principal.Source != "web" || gw.asked.Question != "what changed?" {
		t.Fatalf("asked command = %+v", gw.asked)
	}

	res, err = d.Handle(context.Background(), withText(req, "ingest file://source.md"))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if !strings.Contains(res.Text, "job-1") || gw.ingested.Source.URI != "file://source.md" {
		t.Fatalf("ingest response=%+v command=%+v", res, gw.ingested)
	}

	res, err = d.Handle(context.Background(), withText(req, "status job-1"))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(res.Text, "completed") || gw.status.JobID != "job-1" || !gw.status.Reconcile {
		t.Fatalf("status response=%+v command=%+v", res, gw.status)
	}
}

// TestDispatcherAskExposesProvenance verifies that grounded citations and the
// GroundingAvailable flag survive the trip from the gateway answer to the
// surface-neutral envelope, and that ungrounded ("모른다") answers carry no
// provenance.
func TestDispatcherAskExposesProvenance(t *testing.T) {
	t.Parallel()
	sources := []brainapi.Source{
		{ID: "s1", Title: "Doc One", URI: "https://example.com/doc1", TenantID: "tenant-1"},
		{ID: "s2", Title: "Doc Two", URI: "https://example.com/doc2", TenantID: "tenant-1"},
	}
	gw := &fakeGateway{answer: "grounded answer", sources: sources, groundingAvailable: true}
	d := NewDispatcher(gw)
	req := Request{BindingKey: "web:space:S1", Principal: brainapi.Principal{Source: "web", ID: "user-1"}}

	res, err := d.Handle(context.Background(), withText(req, "ask what changed?"))
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if !res.GroundingAvailable {
		t.Fatalf("expected GroundingAvailable=true, got %+v", res)
	}
	if len(res.Sources) != 2 || res.Sources[0].Title != "Doc One" {
		t.Fatalf("sources = %+v", res.Sources)
	}

	ungrounded := &fakeGateway{answer: "수집된 근거가 없습니다: q", groundingAvailable: false}
	res, err = NewDispatcher(ungrounded).Handle(context.Background(), withText(req, "ask unknown?"))
	if err != nil {
		t.Fatalf("ask ungrounded: %v", err)
	}
	if res.GroundingAvailable || len(res.Sources) != 0 {
		t.Fatalf("expected no provenance for ungrounded answer, got %+v", res)
	}
}

// TestFormatCitations exercises every statement in FormatCitations: the
// empty-sources short circuit, the title-fallback-to-URI assignment, and the
// URI-suffix branch.
func TestFormatCitations(t *testing.T) {
	t.Parallel()
	if got := FormatCitations(nil); got != "" {
		t.Fatalf("empty sources = %q", got)
	}
	got := FormatCitations([]brainapi.Source{
		{Title: "Doc One", URI: "https://example.com/doc1"},
		{URI: "https://example.com/doc2"},
	})
	want := "\n\n출처:\n1. Doc One (https://example.com/doc1)\n2. https://example.com/doc2"
	if got != want {
		t.Fatalf("format = %q, want %q", got, want)
	}
}

func TestDispatcherCreateAndDiscover(t *testing.T) {
	t.Parallel()
	gw := &fakeGateway{}
	d := NewDispatcher(gw)
	req := Request{BindingKey: "cli:project:default", Principal: brainapi.Principal{Source: "cli", ID: "admin", Roles: []string{"admin"}}}

	res, err := d.Handle(context.Background(), withText(req, "create Demo Project"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.Contains(res.Text, "tenant-1") || gw.created.Metadata["name"] != "Demo Project" {
		t.Fatalf("create response=%+v command=%+v", res, gw.created)
	}

	res, err = d.Handle(context.Background(), withText(req, "discover docs"))
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if !strings.Contains(res.Text, "2") || gw.discovered.Query != "docs" {
		t.Fatalf("discover response=%+v command=%+v", res, gw.discovered)
	}
}

func TestDispatcherAdminCapabilitySeams(t *testing.T) {
	t.Parallel()
	userOnly := &userOnlyGateway{base: &fakeGateway{}}
	_, err := NewDispatcher(userOnly).Handle(context.Background(), Request{Principal: brainapi.Principal{ID: "admin", Roles: []string{"admin"}}, Text: "admin on tenant-1"})
	if err == nil || !strings.Contains(err.Error(), "admin capability") {
		t.Fatalf("admin without capability err=%v", err)
	}
	admin := &fakeGateway{}
	res, err := newDispatcherWithAdmin(userOnly, admin).Handle(context.Background(), Request{Principal: brainapi.Principal{ID: "admin", Roles: []string{"admin"}}, Text: "admin on tenant-1"})
	if err != nil {
		t.Fatalf("admin with explicit capability: %v", err)
	}
	if !strings.Contains(res.Text, "tenant-1=on") || admin.projectState.State != brainapi.ProjectOn {
		t.Fatalf("response=%+v admin state=%+v", res, admin.projectState)
	}
}

func TestDispatcherAdmin(t *testing.T) {
	t.Parallel()
	gw := &fakeGateway{}
	d := NewDispatcher(gw)
	req := Request{Principal: brainapi.Principal{Source: "api", ID: "admin", Roles: []string{"admin"}}}
	res, err := d.Handle(context.Background(), withText(req, "admin off tenant-1"))
	if err != nil {
		t.Fatalf("admin off: %v", err)
	}
	if !strings.Contains(res.Text, "tenant-1=off") || gw.projectState.TenantID != "tenant-1" || gw.projectState.State != brainapi.ProjectOff {
		t.Fatalf("response=%+v projectState=%+v", res, gw.projectState)
	}

	_, err = NewDispatcher(&fakeGateway{err: errors.New("gateway error")}).Handle(context.Background(), withText(req, "admin on tenant-1"))
	if err == nil || err.Error() != "gateway error" {
		t.Fatalf("admin gateway error = %v", err)
	}

	for _, text := range []string{"admin", "admin pause tenant-1"} {
		text := text
		t.Run(text, func(t *testing.T) {
			t.Parallel()
			_, err := d.Handle(context.Background(), withText(req, text))
			if err == nil || !brainapi.IsKind(err, brainapi.KindInvalid) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestDispatcherValidationAndSafeErrors(t *testing.T) {
	t.Parallel()
	d := NewDispatcher(&fakeGateway{})
	req := Request{BindingKey: "web:space:S1", Principal: brainapi.Principal{Source: "web", ID: "user-1"}}
	for _, tt := range []struct {
		name string
		text string
		want string
	}{
		{"empty", "", "서브커맨드"},
		{"ingest missing", "ingest", "source URI"},
		{"status missing", "status", "job_id"},
		{"unknown", "share", "알 수 없는"},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := d.Handle(context.Background(), withText(req, tt.text))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err=%v want %q", err, tt.want)
			}
		})
	}

	_, err := (*Dispatcher)(nil).Handle(context.Background(), req)
	if err == nil || !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("nil dispatcher err=%v", err)
	}
	if got := SafeMessage(brainapi.SafeAccessError("ask hidden")); got != "프로젝트에 접근할 수 없습니다." {
		t.Fatalf("safe unauthorized = %q", got)
	}
	if got := SafeMessage(errors.New("plain")); got != "plain" {
		t.Fatalf("safe plain = %q", got)
	}
}

func TestDispatcherPropagatesGatewayErrors(t *testing.T) {
	t.Parallel()
	d := NewDispatcher(&fakeGateway{err: errors.New("gateway error")})
	req := Request{BindingKey: "web:space:S1", Principal: brainapi.Principal{Source: "web", ID: "user-1"}}
	for _, text := range []string{"create x", "ask q", "discover q", "ingest file://a", "status job-1"} {
		text := text
		t.Run(text, func(t *testing.T) {
			t.Parallel()
			_, err := d.Handle(context.Background(), withText(req, text))
			if err == nil || err.Error() != "gateway error" {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

// TestDispatcherAdminListSubcommands covers all admin list and admin get subcommands.
func TestDispatcherAdminListSubcommands(t *testing.T) {
	t.Parallel()
	gw := &fakeGateway{}
	d := NewDispatcher(gw)
	admin := brainapi.Principal{Source: "api", ID: "admin", Roles: []string{"admin"}}

	for _, tt := range []struct {
		name    string
		text    string
		want    string
		wantErr bool
	}{
		{"list tenants", "admin list tenants", "테넌트 목록", false},
		{"list bindings", "admin list bindings", "바인딩 목록", false},
		{"list bindings tenant", "admin list bindings t1", "바인딩 목록", false},
		{"list sources", "admin list sources t1", "소스 목록", false},
		{"list jobs", "admin list jobs t1", "작업 목록", false},
		{"get job", "admin get job t1 job-1", "작업 상태", false},
		{"list sources missing tenant", "admin list sources", "", true},
		{"list jobs missing tenant", "admin list jobs", "", true},
		{"get job missing args", "admin get job t1", "", true},
		{"get no sub", "admin get", "", true},
		{"list no sub", "admin list", "", true},
		{"list unknown sub", "admin list blah", "", true},
		{"get unknown sub", "admin get blah", "", true},
		// on/off without a tenant ID must return KindInvalid.
		{"on missing tenant", "admin on", "", true},
		{"off too many args", "admin off t1 t2", "", true},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res, err := d.Handle(context.Background(), withText(Request{Principal: admin}, tt.text))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got response %+v", res)
				}
				if !brainapi.IsKind(err, brainapi.KindInvalid) {
					t.Fatalf("expected KindInvalid, got kind=%q err=%v", brainapi.KindOf(err), err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if !strings.Contains(res.Text, tt.want) {
				t.Fatalf("response %q does not contain %q", res.Text, tt.want)
			}
		})
	}
}

// TestDispatcherAdminListGatewayErrors verifies that gateway errors from admin
// list/get methods are propagated to the caller.
func TestDispatcherAdminListGatewayErrors(t *testing.T) {
	t.Parallel()
	boom := errors.New("gateway down")
	d := NewDispatcher(&fakeGateway{err: boom})
	admin := brainapi.Principal{Source: "api", ID: "admin", Roles: []string{"admin"}}
	for _, text := range []string{
		"admin list tenants",
		"admin list bindings",
		"admin list sources t1",
		"admin list jobs t1",
		"admin get job t1 job-1",
	} {
		text := text
		t.Run(text, func(t *testing.T) {
			t.Parallel()
			_, err := d.Handle(context.Background(), withText(Request{Principal: admin}, text))
			if err == nil || err.Error() != "gateway down" {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func withText(req Request, text string) Request {
	req.Text = text
	return req
}

type fakeGateway struct {
	created            gateway.CreateProjectCommand
	asked              gateway.AskCommand
	discovered         gateway.DiscoverCommand
	ingested           gateway.IngestCommand
	status             gateway.StatusCommand
	projectState       gateway.SetProjectStateCommand
	answer             string
	sources            []brainapi.Source
	groundingAvailable bool
	jobStatus          brainapi.JobStatus
	err                error
}

func (f *fakeGateway) CreateProject(_ context.Context, cmd gateway.CreateProjectCommand) (gateway.CreateProjectResult, error) {
	f.created = cmd
	if f.err != nil {
		return gateway.CreateProjectResult{}, f.err
	}
	return gateway.CreateProjectResult{TenantID: "tenant-1"}, nil
}

func (f *fakeGateway) Ask(_ context.Context, cmd gateway.AskCommand) (brainapi.QueryResponse, error) {
	f.asked = cmd
	if f.err != nil {
		return brainapi.QueryResponse{}, f.err
	}
	answer := f.answer
	if answer == "" {
		answer = "ok"
	}
	return brainapi.QueryResponse{Answer: answer, Sources: f.sources, GroundingAvailable: f.groundingAvailable}, nil
}

func (f *fakeGateway) Discover(_ context.Context, cmd gateway.DiscoverCommand) (brainapi.DiscoverResponse, error) {
	f.discovered = cmd
	if f.err != nil {
		return brainapi.DiscoverResponse{}, f.err
	}
	return brainapi.DiscoverResponse{Results: []brainapi.MetadataResult{{ID: "m1"}, {ID: "m2"}}}, nil
}

func (f *fakeGateway) Ingest(_ context.Context, cmd gateway.IngestCommand) (gateway.IngestResult, error) {
	f.ingested = cmd
	if f.err != nil {
		return gateway.IngestResult{}, f.err
	}
	return gateway.IngestResult{TenantID: "tenant-1", JobID: "job-1"}, nil
}

func (f *fakeGateway) Status(_ context.Context, cmd gateway.StatusCommand) (brainapi.JobSnapshot, error) {
	f.status = cmd
	if f.err != nil {
		return brainapi.JobSnapshot{}, f.err
	}
	status := f.jobStatus
	if status == "" {
		status = brainapi.JobRunning
	}
	return brainapi.JobSnapshot{TenantID: "tenant-1", JobID: cmd.JobID, Status: status}, nil
}

func (f *fakeGateway) SetProjectState(_ context.Context, cmd gateway.SetProjectStateCommand) (gateway.SetProjectStateResult, error) {
	f.projectState = cmd
	if f.err != nil {
		return gateway.SetProjectStateResult{}, f.err
	}
	return gateway.SetProjectStateResult{TenantID: cmd.TenantID, State: cmd.State}, nil
}

func (f *fakeGateway) AdminListTenants(_ context.Context, _ gateway.AdminListTenantsCommand) (gateway.AdminListTenantsResult, error) {
	if f.err != nil {
		return gateway.AdminListTenantsResult{}, f.err
	}
	return gateway.AdminListTenantsResult{Tenants: []brainapi.TenantInfo{{TenantID: "t1"}}}, nil
}

func (f *fakeGateway) AdminListBindings(_ context.Context, _ gateway.AdminListBindingsCommand) (gateway.AdminListBindingsResult, error) {
	if f.err != nil {
		return gateway.AdminListBindingsResult{}, f.err
	}
	return gateway.AdminListBindingsResult{Bindings: []brainapi.BindingInfo{{BindingKey: "api:space:S1", TenantID: "t1"}}}, nil
}

func (f *fakeGateway) AdminListSources(_ context.Context, _ gateway.AdminListSourcesCommand) (gateway.AdminListSourcesResult, error) {
	if f.err != nil {
		return gateway.AdminListSourcesResult{}, f.err
	}
	return gateway.AdminListSourcesResult{Sources: []brainapi.SourceInfo{{ID: "s1", TenantID: "t1", Name: "doc.pdf"}}}, nil
}

func (f *fakeGateway) AdminListJobs(_ context.Context, _ gateway.AdminListJobsCommand) (gateway.AdminListJobsResult, error) {
	if f.err != nil {
		return gateway.AdminListJobsResult{}, f.err
	}
	return gateway.AdminListJobsResult{Jobs: []brainapi.JobSnapshot{{TenantID: "t1", JobID: "job-1", Status: brainapi.JobRunning}}}, nil
}

func (f *fakeGateway) AdminGetJob(_ context.Context, _ gateway.AdminGetJobCommand) (brainapi.JobSnapshot, error) {
	if f.err != nil {
		return brainapi.JobSnapshot{}, f.err
	}
	return brainapi.JobSnapshot{TenantID: "t1", JobID: "job-1", Status: brainapi.JobCompleted}, nil
}

type userOnlyGateway struct{ base *fakeGateway }

func (g *userOnlyGateway) CreateProject(ctx context.Context, cmd gateway.CreateProjectCommand) (gateway.CreateProjectResult, error) {
	return g.base.CreateProject(ctx, cmd)
}
func (g *userOnlyGateway) Ask(ctx context.Context, cmd gateway.AskCommand) (brainapi.QueryResponse, error) {
	return g.base.Ask(ctx, cmd)
}
func (g *userOnlyGateway) Discover(ctx context.Context, cmd gateway.DiscoverCommand) (brainapi.DiscoverResponse, error) {
	return g.base.Discover(ctx, cmd)
}
func (g *userOnlyGateway) Ingest(ctx context.Context, cmd gateway.IngestCommand) (gateway.IngestResult, error) {
	return g.base.Ingest(ctx, cmd)
}
func (g *userOnlyGateway) Status(ctx context.Context, cmd gateway.StatusCommand) (brainapi.JobSnapshot, error) {
	return g.base.Status(ctx, cmd)
}
