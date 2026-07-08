package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sangyi/workspace-brain/internal/control/gateway"
	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

func TestHandlerDispatchesTrustedJSONCommand(t *testing.T) {
	t.Parallel()
	gw := &fakeGateway{answer: "api answer"}
	h := NewHandler(gw, "token")
	rec := perform(t, h, http.MethodPost, `{"binding_key":"web:space:S1","principal":{"source":"web","id":"U1","roles":["member"]},"text":"ask hello"}`, "token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response commandResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Visibility != "private" || response.Text != "api answer" {
		t.Fatalf("response=%+v", response)
	}
	if gw.asked.BindingKey != "web:space:S1" || gw.asked.Principal.Source != "web" || gw.asked.Question != "hello" {
		t.Fatalf("asked=%+v", gw.asked)
	}
}

func TestHandlerAskExposesSourcesAndGroundingAvailable(t *testing.T) {
	t.Parallel()
	gw := &fakeGateway{
		answer:             "api answer",
		sources:            []brainapi.Source{{ID: "s1", Title: "Doc One", URI: "https://example.com/doc1", TenantID: "tenant-1"}},
		groundingAvailable: true,
	}
	h := NewHandler(gw, "token")
	rec := perform(t, h, http.MethodPost, `{"binding_key":"web:space:S1","principal":{"source":"web","id":"U1"},"text":"ask hello"}`, "token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response commandResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !response.GroundingAvailable || len(response.Sources) != 1 || response.Sources[0].Title != "Doc One" {
		t.Fatalf("response=%+v", response)
	}
}

func TestHandlerAskOmitsSourcesWhenUngrounded(t *testing.T) {
	t.Parallel()
	gw := &fakeGateway{answer: "수집된 근거가 없습니다: hello", groundingAvailable: false}
	h := NewHandler(gw, "token")
	rec := perform(t, h, http.MethodPost, `{"binding_key":"web:space:S1","principal":{"source":"web","id":"U1"},"text":"ask hello"}`, "token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response commandResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.GroundingAvailable || len(response.Sources) != 0 {
		t.Fatalf("response=%+v", response)
	}
}

func TestHandlerDispatchesAdminCommand(t *testing.T) {
	t.Parallel()
	gw := &fakeGateway{}
	h := NewHandler(gw, "token")
	rec := perform(t, h, http.MethodPost, `{"binding_key":"web:space:S1","principal":{"source":"web","id":"admin","roles":["admin"]},"text":"admin on tenant-1"}`, "token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gw.projectState.TenantID != "tenant-1" || gw.projectState.State != brainapi.ProjectOn {
		t.Fatalf("projectState=%+v", gw.projectState)
	}
}

func TestHandlerRequiresBearerToken(t *testing.T) {
	t.Parallel()
	h := NewHandler(&fakeGateway{}, "token")
	for _, token := range []string{"", "wrong"} {
		token := token
		t.Run(token, func(t *testing.T) {
			t.Parallel()
			rec := perform(t, h, http.MethodPost, `{}`, token)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandlerMapsGatewayErrorsSafely(t *testing.T) {
	t.Parallel()
	h := NewHandler(&fakeGateway{err: brainapi.SafeAccessError("hidden tenant")}, "token")
	rec := perform(t, h, http.MethodPost, `{"binding_key":"web:space:S1","principal":{"source":"web","id":"U1"},"text":"ask hidden"}`, "token")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "프로젝트에 접근할 수 없습니다") || strings.Contains(rec.Body.String(), "hidden tenant") {
		t.Fatalf("unsafe body=%s", rec.Body.String())
	}
}

func TestHandlerServeHTTPErrors(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		h      *Handler
		method string
		body   string
		token  string
		status int
	}{
		{"method", NewHandler(&fakeGateway{}, "token"), http.MethodGet, `{}`, "token", http.StatusMethodNotAllowed},
		{"missing config", &Handler{}, http.MethodPost, `{}`, "token", http.StatusInternalServerError},
		{"bad json", NewHandler(&fakeGateway{}, "token"), http.MethodPost, `{`, "token", http.StatusBadRequest},
		{"unknown field", NewHandler(&fakeGateway{}, "token"), http.MethodPost, `{"extra":true}`, "token", http.StatusBadRequest},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := perform(t, tt.h, tt.method, tt.body, tt.token)
			if rec.Code != tt.status {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tt.status, rec.Body.String())
			}
		})
	}
}

func TestStatusFor(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		kind brainapi.ErrorKind
		want int
	}{
		{brainapi.KindInvalid, http.StatusBadRequest},
		{brainapi.KindUnauthorized, http.StatusForbidden},
		{brainapi.KindNotFound, http.StatusNotFound},
		{brainapi.KindConflict, http.StatusConflict},
		{brainapi.KindAlreadyExists, http.StatusConflict},
		{brainapi.KindInternal, http.StatusInternalServerError},
	} {
		err := brainapi.E(tt.kind, "op", "msg", nil)
		if got := statusFor(err); got != tt.want {
			t.Fatalf("statusFor(%s)=%d want=%d", tt.kind, got, tt.want)
		}
	}
	if got := statusFor(errors.New("plain")); got != http.StatusInternalServerError {
		t.Fatalf("plain=%d", got)
	}
}

func perform(t *testing.T, h *Handler, method, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/commands", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type fakeGateway struct {
	asked              gateway.AskCommand
	projectState       gateway.SetProjectStateCommand
	answer             string
	sources            []brainapi.Source
	groundingAvailable bool
	err                error
}

func (f *fakeGateway) CreateProject(context.Context, gateway.CreateProjectCommand) (gateway.CreateProjectResult, error) {
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
	return brainapi.QueryResponse{Answer: f.answer, Sources: f.sources, GroundingAvailable: f.groundingAvailable}, nil
}

func (f *fakeGateway) Discover(context.Context, gateway.DiscoverCommand) (brainapi.DiscoverResponse, error) {
	if f.err != nil {
		return brainapi.DiscoverResponse{}, f.err
	}
	return brainapi.DiscoverResponse{}, nil
}

func (f *fakeGateway) Ingest(context.Context, gateway.IngestCommand) (gateway.IngestResult, error) {
	if f.err != nil {
		return gateway.IngestResult{}, f.err
	}
	return gateway.IngestResult{JobID: "job-1"}, nil
}

func (f *fakeGateway) Status(_ context.Context, cmd gateway.StatusCommand) (brainapi.JobSnapshot, error) {
	if f.err != nil {
		return brainapi.JobSnapshot{}, f.err
	}
	return brainapi.JobSnapshot{JobID: cmd.JobID, Status: brainapi.JobRunning}, nil
}

func (f *fakeGateway) SetProjectState(_ context.Context, cmd gateway.SetProjectStateCommand) (gateway.SetProjectStateResult, error) {
	f.projectState = cmd
	if f.err != nil {
		return gateway.SetProjectStateResult{}, f.err
	}
	return gateway.SetProjectStateResult{TenantID: cmd.TenantID, State: cmd.State}, nil
}

func (f *fakeGateway) AdminListTenants(context.Context, gateway.AdminListTenantsCommand) (gateway.AdminListTenantsResult, error) {
	return gateway.AdminListTenantsResult{}, f.err
}
func (f *fakeGateway) AdminListBindings(context.Context, gateway.AdminListBindingsCommand) (gateway.AdminListBindingsResult, error) {
	return gateway.AdminListBindingsResult{}, f.err
}
func (f *fakeGateway) AdminListSources(context.Context, gateway.AdminListSourcesCommand) (gateway.AdminListSourcesResult, error) {
	return gateway.AdminListSourcesResult{}, f.err
}
func (f *fakeGateway) AdminListJobs(context.Context, gateway.AdminListJobsCommand) (gateway.AdminListJobsResult, error) {
	return gateway.AdminListJobsResult{}, f.err
}
func (f *fakeGateway) AdminGetJob(context.Context, gateway.AdminGetJobCommand) (brainapi.JobSnapshot, error) {
	return brainapi.JobSnapshot{}, f.err
}
