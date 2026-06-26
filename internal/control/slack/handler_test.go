package slack

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sangyi/workspace-brain/internal/control/gateway"
	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

func TestHandlerRejectsBadSignature(t *testing.T) {
	t.Parallel()
	h := NewHandler(&fakeGateway{}, "secret", map[string]bool{"U-admin": true})
	h.Now = fixedNow
	req := httptest.NewRequest(http.MethodPost, "/slack", strings.NewReader(formBody("ask hi")))
	req.Header.Set("X-Slack-Request-Timestamp", timestamp())
	req.Header.Set("X-Slack-Signature", "v0=bad")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlerCreateDispatchesWithAdminPrincipal(t *testing.T) {
	t.Parallel()
	gw := &fakeGateway{}
	h := NewHandler(gw, "secret", map[string]bool{"U-admin": true})
	h.Now = fixedNow
	rec := perform(t, h, "create Demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if gw.created.BindingKey != "slack:channel:C1" || !gw.created.Principal.HasRole("admin") || gw.created.Metadata["name"] != "Demo" {
		t.Fatalf("created command = %+v", gw.created)
	}
	assertTextContains(t, rec.Body.String(), "tenant-1")
}

func TestHandlerAskReturnsEphemeralAnswer(t *testing.T) {
	t.Parallel()
	gw := &fakeGateway{answer: "근거 기반 답변"}
	h := NewHandler(gw, "secret", nil)
	h.Now = fixedNow
	rec := perform(t, h, "ask 질문")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if gw.asked.Question != "질문" || gw.asked.BindingKey != "slack:channel:C1" {
		t.Fatalf("asked = %+v", gw.asked)
	}
	assertTextContains(t, rec.Body.String(), "근거 기반 답변")
}

func TestHandlerIngestAndStatus(t *testing.T) {
	t.Parallel()
	gw := &fakeGateway{jobStatus: brainapi.JobCompleted}
	h := NewHandler(gw, "secret", nil)
	h.Now = fixedNow
	rec := perform(t, h, "ingest file://a.pdf")
	if rec.Code != http.StatusOK {
		t.Fatalf("ingest status = %d body=%s", rec.Code, rec.Body.String())
	}
	if gw.ingested.Source.URI != "file://a.pdf" {
		t.Fatalf("ingested = %+v", gw.ingested)
	}
	assertTextContains(t, rec.Body.String(), "job-1")

	rec = perform(t, h, "status job-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status status = %d body=%s", rec.Code, rec.Body.String())
	}
	if gw.status.JobID != "job-1" || !gw.status.Reconcile {
		t.Fatalf("status cmd = %+v", gw.status)
	}
	assertTextContains(t, rec.Body.String(), "completed")
}

func TestHandlerMapsUnauthorizedToSafeUserMessage(t *testing.T) {
	t.Parallel()
	gw := &fakeGateway{err: brainapi.SafeAccessError("ask")}
	h := NewHandler(gw, "secret", nil)
	h.Now = fixedNow
	rec := perform(t, h, "ask hidden")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	assertTextContains(t, rec.Body.String(), "프로젝트에 접근할 수 없습니다")
	if strings.Contains(rec.Body.String(), "hidden") {
		t.Fatalf("response leaked request/error detail: %s", rec.Body.String())
	}
}

func TestHandlerRejectsWrongCommandName(t *testing.T) {
	t.Parallel()
	h := NewHandler(&fakeGateway{}, "secret", nil)
	h.Now = fixedNow
	body := url.Values{"command": {"/other"}, "channel_id": {"C1"}, "user_id": {"U1"}, "text": {"ask hi"}}.Encode()
	req := signedRequest(body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func perform(t *testing.T, h *Handler, text string) *httptest.ResponseRecorder {
	t.Helper()
	req := signedRequest(formBody(text))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func signedRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/slack", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", timestamp())
	req.Header.Set("X-Slack-Signature", sign(body))
	return req
}

func formBody(text string) string {
	return url.Values{"command": {"/brain"}, "channel_id": {"C1"}, "user_id": {"U-admin"}, "text": {text}}.Encode()
}

func timestamp() string { return "1782194400" }

func fixedNow() time.Time { return time.Unix(1782194400, 0) }

func sign(body string) string {
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write([]byte("v0:" + timestamp() + ":" + body))
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func assertTextContains(t *testing.T, body, want string) {
	t.Helper()
	var response slackResponse
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatalf("json decode: %v body=%s", err, body)
	}
	if response.ResponseType != "ephemeral" || !strings.Contains(response.Text, want) {
		t.Fatalf("response = %+v, want text containing %q", response, want)
	}
}

type fakeGateway struct {
	created    gateway.CreateProjectCommand
	asked      gateway.AskCommand
	discovered gateway.DiscoverCommand
	ingested   gateway.IngestCommand
	status     gateway.StatusCommand
	answer     string
	jobStatus  brainapi.JobStatus
	err        error
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
	return brainapi.DiscoverResponse{Results: []brainapi.MetadataResult{{ID: "m1", Title: "demo"}}}, nil
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

func TestHandlerServeHTTPErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		h      *Handler
		req    *http.Request
		status int
	}{
		{"method", NewHandler(&fakeGateway{}, "secret", nil), httptest.NewRequest(http.MethodGet, "/slack", nil), http.StatusMethodNotAllowed},
		{"missing config", &Handler{}, httptest.NewRequest(http.MethodPost, "/slack", strings.NewReader(formBody("ask hi"))), http.StatusInternalServerError},
		{"missing headers", NewHandler(&fakeGateway{}, "secret", nil), httptest.NewRequest(http.MethodPost, "/slack", strings.NewReader(formBody("ask hi"))), http.StatusUnauthorized},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.h.Now == nil {
				tt.h.Now = fixedNow
			}
			rec := httptest.NewRecorder()
			tt.h.ServeHTTP(rec, tt.req)
			if rec.Code != tt.status {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tt.status, rec.Body.String())
			}
		})
	}
}

func TestHandlerVerifyTimestampErrors(t *testing.T) {
	t.Parallel()
	h := NewHandler(&fakeGateway{}, "secret", nil)
	h.Now = fixedNow
	body := formBody("ask hi")
	for _, tt := range []struct {
		name      string
		timestamp string
		signature string
	}{
		{"invalid timestamp", "not-int", "v0=whatever"},
		{"stale timestamp", "1", signWithTimestamp(body, "1")},
		{"future timestamp", "9999999999", signWithTimestamp(body, "9999999999")},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPost, "/slack", strings.NewReader(body))
			req.Header.Set("X-Slack-Request-Timestamp", tt.timestamp)
			req.Header.Set("X-Slack-Signature", tt.signature)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandlerDispatchBranches(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		text string
		want string
	}{
		{"empty", "", "서브커맨드"},
		{"discover", "discover anything", "메타데이터 결과: 1"},
		{"ingest missing", "ingest", "source URI"},
		{"status missing", "status", "job_id"},
		{"unknown", "nope", "알 수 없는"},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := NewHandler(&fakeGateway{jobStatus: brainapi.JobRunning}, "secret", nil)
			h.Now = fixedNow
			rec := perform(t, h, tt.text)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			assertTextContains(t, rec.Body.String(), tt.want)
		})
	}
}

func TestHandlerServeInteraction(t *testing.T) {
	t.Parallel()
	h := NewHandler(&fakeGateway{}, "secret", nil)
	h.Now = fixedNow
	rec := httptest.NewRecorder()
	h.ServeInteraction(rec, signedRequest(formBody("payload")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertTextContains(t, rec.Body.String(), "상호작용 요청이 접수되었습니다")
}

func TestHandlerServeInteractionErrors(t *testing.T) {
	t.Parallel()
	h := NewHandler(&fakeGateway{}, "secret", nil)
	h.Now = fixedNow
	for _, tt := range []struct {
		name   string
		h      *Handler
		req    *http.Request
		status int
	}{
		{"method", h, httptest.NewRequest(http.MethodGet, "/slack/interactions", nil), http.StatusMethodNotAllowed},
		{"missing config", &Handler{}, httptest.NewRequest(http.MethodPost, "/slack/interactions", strings.NewReader(formBody("payload"))), http.StatusInternalServerError},
		{"read", h, httptest.NewRequest(http.MethodPost, "/slack/interactions", errReader{}), http.StatusBadRequest},
		{"signature", h, httptest.NewRequest(http.MethodPost, "/slack/interactions", strings.NewReader(formBody("payload"))), http.StatusUnauthorized},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.h.Now == nil {
				tt.h.Now = fixedNow
			}
			rec := httptest.NewRecorder()
			tt.h.ServeInteraction(rec, tt.req)
			if rec.Code != tt.status {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tt.status, rec.Body.String())
			}
		})
	}
}

func TestHandlerValidateCommandDefaultAndHelpers(t *testing.T) {
	t.Parallel()
	h := &Handler{}
	if err := h.validateCommand("/brain"); err != nil {
		t.Fatalf("default command: %v", err)
	}
	if got := intString(0); got != "0" {
		t.Fatalf("intString(0)=%q", got)
	}
	if got := intString(42); got != "42" {
		t.Fatalf("intString(42)=%q", got)
	}
	if got := safeMessage(errors.New("plain")); got != "plain" {
		t.Fatalf("safe plain=%q", got)
	}
}

func signWithTimestamp(body, ts string) string {
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write([]byte("v0:" + ts + ":" + body))
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func TestHandlerGatewayErrorsForAllSubcommands(t *testing.T) {
	t.Parallel()
	for _, text := range []string{"create demo", "discover q", "ingest file://a", "status job-1"} {
		text := text
		t.Run(text, func(t *testing.T) {
			t.Parallel()
			h := NewHandler(&fakeGateway{err: errors.New("gateway error")}, "secret", map[string]bool{"U-admin": true})
			h.Now = fixedNow
			rec := perform(t, h, text)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			assertTextContains(t, rec.Body.String(), "gateway error")
		})
	}
}

func TestHandlerBodyReadAndParseErrors(t *testing.T) {
	t.Parallel()
	h := NewHandler(&fakeGateway{}, "secret", nil)
	h.Now = fixedNow
	req := httptest.NewRequest(http.MethodPost, "/slack", errReader{})
	req.Header.Set("X-Slack-Request-Timestamp", timestamp())
	req.Header.Set("X-Slack-Signature", "v0=whatever")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("read status=%d body=%s", rec.Code, rec.Body.String())
	}

	body := "command=%/bad"
	req = signedRequest(body)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("parse status=%d body=%s", rec.Code, rec.Body.String())
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read") }
