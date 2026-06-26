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
	res, err := NewDispatcherWithAdmin(userOnly, admin).Handle(context.Background(), Request{Principal: brainapi.Principal{ID: "admin", Roles: []string{"admin"}}, Text: "admin on tenant-1"})
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

func withText(req Request, text string) Request {
	req.Text = text
	return req
}

type fakeGateway struct {
	created      gateway.CreateProjectCommand
	asked        gateway.AskCommand
	discovered   gateway.DiscoverCommand
	ingested     gateway.IngestCommand
	status       gateway.StatusCommand
	projectState gateway.SetProjectStateCommand
	answer       string
	jobStatus    brainapi.JobStatus
	err          error
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
	return brainapi.QueryResponse{Answer: answer}, nil
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
