// Package frontend dispatches surface-normalized user commands to the control gateway.
package frontend

import (
	"context"
	"strconv"
	"strings"

	"github.com/sangyi/workspace-brain/internal/control/gateway"
	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

// Gateway is the subset of gateway.Gateway used by frontend adapters.
type Gateway interface {
	CreateProject(ctx context.Context, cmd gateway.CreateProjectCommand) (gateway.CreateProjectResult, error)
	Ask(ctx context.Context, cmd gateway.AskCommand) (brainapi.QueryResponse, error)
	Discover(ctx context.Context, cmd gateway.DiscoverCommand) (brainapi.DiscoverResponse, error)
	Ingest(ctx context.Context, cmd gateway.IngestCommand) (gateway.IngestResult, error)
	Status(ctx context.Context, cmd gateway.StatusCommand) (brainapi.JobSnapshot, error)
}

// AdminGateway is the optional admin capability used by admin commands.
type AdminGateway interface {
	SetProjectState(ctx context.Context, cmd gateway.SetProjectStateCommand) (gateway.SetProjectStateResult, error)
	AdminListTenants(ctx context.Context, cmd gateway.AdminListTenantsCommand) (gateway.AdminListTenantsResult, error)
	AdminListBindings(ctx context.Context, cmd gateway.AdminListBindingsCommand) (gateway.AdminListBindingsResult, error)
	AdminListSources(ctx context.Context, cmd gateway.AdminListSourcesCommand) (gateway.AdminListSourcesResult, error)
	AdminListJobs(ctx context.Context, cmd gateway.AdminListJobsCommand) (gateway.AdminListJobsResult, error)
	AdminGetJob(ctx context.Context, cmd gateway.AdminGetJobCommand) (brainapi.JobSnapshot, error)
}

// Visibility tells a frontend adapter how broadly a response should be shown.
type Visibility string

const (
	// VisibilityPrivate means the response should be visible only to the requester.
	VisibilityPrivate Visibility = "private"
)

// Request is the surface-neutral command envelope produced by a frontend adapter.
type Request struct {
	BindingKey brainapi.BindingKey
	Principal  brainapi.Principal
	Text       string
}

// Response is the surface-neutral message returned to a frontend adapter.
type Response struct {
	Visibility Visibility
	Text       string
	// Sources lists the grounded citations backing Text. It is populated only
	// for "ask" responses that found grounded evidence; it is empty for every
	// other command and for ungrounded ("모른다") answers.
	Sources []brainapi.Source
	// GroundingAvailable reports whether the underlying query found grounded
	// evidence. Adapters must not render a citation list when this is false.
	GroundingAvailable bool
}

// Dispatcher routes normalized frontend commands to gateway methods.
type Dispatcher struct {
	gateway Gateway
	admin   AdminGateway
}

// NewDispatcher creates a command dispatcher and enables admin commands when gw has admin capability.
func NewDispatcher(gw Gateway) *Dispatcher {
	d := &Dispatcher{gateway: gw}
	if admin, ok := gw.(AdminGateway); ok {
		d.admin = admin
	}
	return d
}

// newDispatcherWithAdmin creates a command dispatcher with an explicit admin capability.
func newDispatcherWithAdmin(gw Gateway, admin AdminGateway) *Dispatcher {
	return &Dispatcher{gateway: gw, admin: admin}
}

// Handle parses req.Text and executes the matching gateway command.
func (d *Dispatcher) Handle(ctx context.Context, req Request) (Response, error) {
	const op = "frontend_dispatch"
	if d == nil || d.gateway == nil {
		return Response{}, brainapi.E(brainapi.KindInvalid, op, "gateway is required", nil)
	}
	text := strings.TrimSpace(req.Text)
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return Response{}, brainapi.E(brainapi.KindInvalid, op, "서브커맨드를 입력하세요: create|ingest|ask|discover|status|admin", nil)
	}
	subcommand := strings.ToLower(fields[0])
	args := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
	switch subcommand {
	case "create":
		result, err := d.gateway.CreateProject(ctx, gateway.CreateProjectCommand{BindingKey: req.BindingKey, Principal: req.Principal, Metadata: map[string]string{"name": args}})
		if err != nil {
			return Response{}, err
		}
		return private("프로젝트가 생성되었습니다: " + string(result.TenantID)), nil
	case "ask":
		answer, err := d.gateway.Ask(ctx, gateway.AskCommand{BindingKey: req.BindingKey, Principal: req.Principal, Question: args})
		if err != nil {
			return Response{}, err
		}
		res := private(answer.Answer)
		res.Sources = answer.Sources
		res.GroundingAvailable = answer.GroundingAvailable
		return res, nil
	case "discover":
		result, err := d.gateway.Discover(ctx, gateway.DiscoverCommand{BindingKey: req.BindingKey, Principal: req.Principal, Query: args})
		if err != nil {
			return Response{}, err
		}
		return private("메타데이터 결과: " + strconv.Itoa(len(result.Results))), nil
	case "ingest":
		if args == "" {
			return Response{}, brainapi.E(brainapi.KindInvalid, "frontend_ingest", "source URI가 필요합니다.", nil)
		}
		result, err := d.gateway.Ingest(ctx, gateway.IngestCommand{BindingKey: req.BindingKey, Principal: req.Principal, Source: brainapi.SourceRef{URI: args, Name: args}})
		if err != nil {
			return Response{}, err
		}
		return private("수집 작업이 접수되었습니다: " + string(result.JobID)), nil
	case "status":
		if args == "" {
			return Response{}, brainapi.E(brainapi.KindInvalid, "frontend_status", "job_id가 필요합니다.", nil)
		}
		status, err := d.gateway.Status(ctx, gateway.StatusCommand{BindingKey: req.BindingKey, Principal: req.Principal, JobID: brainapi.JobID(args), Reconcile: true})
		if err != nil {
			return Response{}, err
		}
		return private("작업 상태: " + string(status.Status)), nil
	case "admin":
		return d.handleAdmin(ctx, req.Principal, args)
	default:
		return Response{}, brainapi.E(brainapi.KindInvalid, op, "알 수 없는 서브커맨드입니다.", nil)
	}
}

func (d *Dispatcher) handleAdmin(ctx context.Context, principal brainapi.Principal, args string) (Response, error) {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return Response{}, brainapi.E(brainapi.KindInvalid, "frontend_admin", "admin 명령은 `admin on|off <tenant>`, `admin list tenants|bindings|sources <tenant>|jobs <tenant>`, `admin get job <tenant> <jobID>` 형식이어야 합니다.", nil)
	}
	if d.admin == nil {
		return Response{}, brainapi.E(brainapi.KindInvalid, "frontend_admin", "admin capability is not configured", nil)
	}
	switch strings.ToLower(fields[0]) {
	case "on", "off":
		if len(fields) != 2 {
			return Response{}, brainapi.E(brainapi.KindInvalid, "frontend_admin", "admin on|off 명령은 `admin on|off <tenant_id>` 형식이어야 합니다.", nil)
		}
		state := brainapi.ProjectState(strings.ToLower(fields[0]))
		result, err := d.admin.SetProjectState(ctx, gateway.SetProjectStateCommand{Principal: principal, TenantID: brainapi.TenantID(fields[1]), State: state})
		if err != nil {
			return Response{}, err
		}
		return private("프로젝트 상태가 변경되었습니다: " + string(result.TenantID) + "=" + string(result.State)), nil
	case "list":
		return d.handleAdminList(ctx, principal, fields[1:])
	case "get":
		return d.handleAdminGet(ctx, principal, fields[1:])
	default:
		return Response{}, brainapi.E(brainapi.KindInvalid, "frontend_admin", "알 수 없는 admin 서브커맨드입니다.", nil)
	}
}

func (d *Dispatcher) handleAdminList(ctx context.Context, principal brainapi.Principal, args []string) (Response, error) {
	if len(args) == 0 {
		return Response{}, brainapi.E(brainapi.KindInvalid, "frontend_admin_list", "admin list 명령은 `admin list tenants|bindings|sources <tenant>|jobs <tenant>` 형식이어야 합니다.", nil)
	}
	switch strings.ToLower(args[0]) {
	case "tenants":
		result, err := d.admin.AdminListTenants(ctx, gateway.AdminListTenantsCommand{Principal: principal})
		if err != nil {
			return Response{}, err
		}
		return private("테넌트 목록: " + strconv.Itoa(len(result.Tenants))), nil
	case "bindings":
		var tenantID brainapi.TenantID
		if len(args) > 1 {
			tenantID = brainapi.TenantID(args[1])
		}
		result, err := d.admin.AdminListBindings(ctx, gateway.AdminListBindingsCommand{Principal: principal, TenantID: tenantID})
		if err != nil {
			return Response{}, err
		}
		return private("바인딩 목록: " + strconv.Itoa(len(result.Bindings))), nil
	case "sources":
		if len(args) < 2 {
			return Response{}, brainapi.E(brainapi.KindInvalid, "frontend_admin_list", "admin list sources 명령은 `admin list sources <tenant>` 형식이어야 합니다.", nil)
		}
		result, err := d.admin.AdminListSources(ctx, gateway.AdminListSourcesCommand{Principal: principal, TenantID: brainapi.TenantID(args[1])})
		if err != nil {
			return Response{}, err
		}
		return private("소스 목록: " + strconv.Itoa(len(result.Sources))), nil
	case "jobs":
		if len(args) < 2 {
			return Response{}, brainapi.E(brainapi.KindInvalid, "frontend_admin_list", "admin list jobs 명령은 `admin list jobs <tenant>` 형식이어야 합니다.", nil)
		}
		result, err := d.admin.AdminListJobs(ctx, gateway.AdminListJobsCommand{Principal: principal, TenantID: brainapi.TenantID(args[1])})
		if err != nil {
			return Response{}, err
		}
		return private("작업 목록: " + strconv.Itoa(len(result.Jobs))), nil
	default:
		return Response{}, brainapi.E(brainapi.KindInvalid, "frontend_admin_list", "알 수 없는 admin list 대상입니다.", nil)
	}
}

func (d *Dispatcher) handleAdminGet(ctx context.Context, principal brainapi.Principal, args []string) (Response, error) {
	if len(args) == 0 {
		return Response{}, brainapi.E(brainapi.KindInvalid, "frontend_admin_get", "admin get 명령은 `admin get job <tenant> <jobID>` 형식이어야 합니다.", nil)
	}
	switch strings.ToLower(args[0]) {
	case "job":
		if len(args) < 3 {
			return Response{}, brainapi.E(brainapi.KindInvalid, "frontend_admin_get", "admin get job 명령은 `admin get job <tenant> <jobID>` 형식이어야 합니다.", nil)
		}
		snap, err := d.admin.AdminGetJob(ctx, gateway.AdminGetJobCommand{Principal: principal, TenantID: brainapi.TenantID(args[1]), JobID: brainapi.JobID(args[2])})
		if err != nil {
			return Response{}, err
		}
		return private("작업 상태: " + string(snap.Status)), nil
	default:
		return Response{}, brainapi.E(brainapi.KindInvalid, "frontend_admin_get", "알 수 없는 admin get 대상입니다.", nil)
	}
}

// SafeMessage strips authorization details before an adapter renders an error.
func SafeMessage(err error) string {
	if brainapi.IsKind(err, brainapi.KindUnauthorized) {
		return "프로젝트에 접근할 수 없습니다."
	}
	return err.Error()
}

func private(text string) Response {
	return Response{Visibility: VisibilityPrivate, Text: text}
}

// FormatCitations renders a numbered citation list suitable for appending
// after an answer's text. It returns an empty string when there are no
// sources to cite, so callers can safely append the result unconditionally
// (e.g. ungrounded "모른다" answers are left untouched).
func FormatCitations(sources []brainapi.Source) string {
	if len(sources) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n출처:")
	for i, s := range sources {
		label := s.Title
		if label == "" {
			label = s.URI
		}
		b.WriteString("\n")
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(". ")
		b.WriteString(label)
		if s.URI != "" && s.URI != label {
			b.WriteString(" (")
			b.WriteString(s.URI)
			b.WriteString(")")
		}
	}
	return b.String()
}
