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
		return private(answer.Answer), nil
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
	if len(fields) != 2 {
		return Response{}, brainapi.E(brainapi.KindInvalid, "frontend_admin", "admin 명령은 `admin on|off <tenant_id>` 형식이어야 합니다.", nil)
	}
	state := brainapi.ProjectState(strings.ToLower(fields[0]))
	if state != brainapi.ProjectOn && state != brainapi.ProjectOff {
		return Response{}, brainapi.E(brainapi.KindInvalid, "frontend_admin", "project state는 on 또는 off여야 합니다.", nil)
	}
	if d.admin == nil {
		return Response{}, brainapi.E(brainapi.KindInvalid, "frontend_admin", "admin capability is not configured", nil)
	}
	result, err := d.admin.SetProjectState(ctx, gateway.SetProjectStateCommand{Principal: principal, TenantID: brainapi.TenantID(fields[1]), State: state})
	if err != nil {
		return Response{}, err
	}
	return private("프로젝트 상태가 변경되었습니다: " + string(result.TenantID) + "=" + string(result.State)), nil
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
