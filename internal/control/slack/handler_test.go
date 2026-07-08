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

	"github.com/sangyi/workspace-brain/internal/control/frontend"
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

func TestHandlerAskRendersCitationsWhenGrounded(t *testing.T) {
	t.Parallel()
	gw := &fakeGateway{
		answer:             "근거 기반 답변",
		sources:            []brainapi.Source{{Title: "Doc One", URI: "https://example.com/doc1"}},
		groundingAvailable: true,
	}
	h := NewHandler(gw, "secret", nil)
	h.Now = fixedNow
	rec := perform(t, h, "ask 질문")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	assertTextContains(t, rec.Body.String(), "근거 기반 답변")
	assertTextContains(t, rec.Body.String(), "출처:")
	assertTextContains(t, rec.Body.String(), "Doc One (https://example.com/doc1)")
}

func TestHandlerAskOmitsCitationsWhenUngrounded(t *testing.T) {
	t.Parallel()
	gw := &fakeGateway{answer: "수집된 근거가 없습니다: 질문", groundingAvailable: false}
	h := NewHandler(gw, "secret", nil)
	h.Now = fixedNow
	rec := perform(t, h, "ask 질문")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "출처:") {
		t.Fatalf("expected no citation list for ungrounded answer, body=%s", rec.Body.String())
	}
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
	created            gateway.CreateProjectCommand
	createCalls        int
	asked              gateway.AskCommand
	discovered         gateway.DiscoverCommand
	ingested           gateway.IngestCommand
	status             gateway.StatusCommand
	answer             string
	sources            []brainapi.Source
	groundingAvailable bool
	jobStatus          brainapi.JobStatus
	err                error
}

func (f *fakeGateway) CreateProject(_ context.Context, cmd gateway.CreateProjectCommand) (gateway.CreateProjectResult, error) {
	f.created = cmd
	f.createCalls++
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
	if got := safeMessage(errors.New("plain")); got != "plain" {
		t.Fatalf("safe plain=%q", got)
	}
}

func TestRenderMapsVisibilityToSlackResponseType(t *testing.T) {
	t.Parallel()

	got := render(frontend.Response{Visibility: frontend.VisibilityPrivate, Text: "hello"})
	if got.ResponseType != "ephemeral" || got.Text != "hello" {
		t.Fatalf("private: %+v", got)
	}

	got = render(frontend.Response{Text: "broadcast"})
	if got.ResponseType != "in_channel" || got.Text != "broadcast" {
		t.Fatalf("default (empty visibility): %+v", got)
	}

	got = render(frontend.Response{
		Visibility: frontend.VisibilityPrivate,
		Text:       "grounded",
		Sources:    []brainapi.Source{{Title: "Doc", URI: "https://example.com/d"}},
	})
	if got.Text != "grounded"+frontend.FormatCitations([]brainapi.Source{{Title: "Doc", URI: "https://example.com/d"}}) {
		t.Fatalf("with sources: %+v", got)
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

// interactionBody form-encodes a Slack interactivity JSON payload under the
// "payload" field, as Slack itself does when delivering block_actions.
func interactionBody(t *testing.T, payload interactionPayload) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return url.Values{"payload": {string(raw)}}.Encode()
}

func signedInteractionRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/slack/interactions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", timestamp())
	req.Header.Set("X-Slack-Signature", sign(body))
	return req
}

func TestHandlerServeInteractionDispatchesShareResponseAction(t *testing.T) {
	t.Parallel()
	h := NewHandler(&fakeGateway{}, "secret", nil)
	h.Now = fixedNow
	payload := interactionPayload{
		Type:      "block_actions",
		TriggerID: "trigger-share-1",
		Actions:   []interactionAction{{ActionID: actionShareResponse, Value: "공유된 답변"}},
	}
	rec := httptest.NewRecorder()
	h.ServeInteraction(rec, signedInteractionRequest(interactionBody(t, payload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp slackResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ResponseType != "in_channel" || resp.Text != "공유된 답변" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestHandlerServeInteractionDispatchesConfirmCommandAction(t *testing.T) {
	t.Parallel()
	gw := &fakeGateway{}
	h := NewHandler(gw, "secret", map[string]bool{"U-admin": true})
	h.Now = fixedNow
	payload := interactionPayload{
		Type:      "block_actions",
		TriggerID: "trigger-confirm-1",
		User: struct {
			ID string `json:"id"`
		}{ID: "U-admin"},
		Channel: struct {
			ID string `json:"id"`
		}{ID: "C1"},
		Actions: []interactionAction{{ActionID: actionConfirmCommand, Value: "create Demo"}},
	}
	rec := httptest.NewRecorder()
	h.ServeInteraction(rec, signedInteractionRequest(interactionBody(t, payload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gw.created.BindingKey != "slack:channel:C1" || !gw.created.Principal.HasRole("admin") || gw.created.Metadata["name"] != "Demo" {
		t.Fatalf("created command = %+v", gw.created)
	}
}

func TestHandlerServeInteractionActionDispatchErrorRendersSafeMessage(t *testing.T) {
	t.Parallel()
	gw := &fakeGateway{err: errors.New("gateway exploded")}
	h := NewHandler(gw, "secret", nil)
	h.Now = fixedNow
	payload := interactionPayload{
		Type:      "block_actions",
		TriggerID: "trigger-confirm-err",
		Actions:   []interactionAction{{ActionID: actionConfirmCommand, Value: "create Demo"}},
	}
	rec := httptest.NewRecorder()
	h.ServeInteraction(rec, signedInteractionRequest(interactionBody(t, payload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertTextContains(t, rec.Body.String(), "gateway exploded")
}

func TestHandlerServeInteractionUnsupportedTypeIsAcknowledgedGenerically(t *testing.T) {
	t.Parallel()
	h := NewHandler(&fakeGateway{}, "secret", nil)
	h.Now = fixedNow
	payload := interactionPayload{
		Type:      "view_submission",
		TriggerID: "trigger-view-1",
		Actions:   []interactionAction{{ActionID: actionShareResponse, Value: "무시됨"}},
	}
	rec := httptest.NewRecorder()
	h.ServeInteraction(rec, signedInteractionRequest(interactionBody(t, payload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertTextContains(t, rec.Body.String(), "상호작용 요청이 접수되었습니다")
}

func TestHandlerServeInteractionInvalidPayloadJSON(t *testing.T) {
	t.Parallel()
	h := NewHandler(&fakeGateway{}, "secret", nil)
	h.Now = fixedNow
	body := url.Values{"payload": {"not-json"}}.Encode()
	rec := httptest.NewRecorder()
	h.ServeInteraction(rec, signedInteractionRequest(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlerServeInteractionMalformedFormBody(t *testing.T) {
	t.Parallel()
	h := NewHandler(&fakeGateway{}, "secret", nil)
	h.Now = fixedNow
	body := "payload=%zz"
	rec := httptest.NewRecorder()
	h.ServeInteraction(rec, signedInteractionRequest(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlerServeInteractionDedupSkipsDuplicateDelivery(t *testing.T) {
	t.Parallel()
	gw := &fakeGateway{}
	h := NewHandler(gw, "secret", map[string]bool{"U-admin": true})
	h.Now = fixedNow
	payload := interactionPayload{
		Type:      "block_actions",
		TriggerID: "trigger-dedup-1",
		Channel: struct {
			ID string `json:"id"`
		}{ID: "C1"},
		User: struct {
			ID string `json:"id"`
		}{ID: "U-admin"},
		Actions: []interactionAction{{ActionID: actionConfirmCommand, Value: "create Demo"}},
	}
	body := interactionBody(t, payload)

	first := httptest.NewRecorder()
	h.ServeInteraction(first, signedInteractionRequest(body))
	if first.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	if gw.createCalls != 1 {
		t.Fatalf("createCalls after first delivery = %d, want 1", gw.createCalls)
	}

	second := httptest.NewRecorder()
	h.ServeInteraction(second, signedInteractionRequest(body))
	if second.Code != http.StatusOK {
		t.Fatalf("second status=%d body=%s", second.Code, second.Body.String())
	}
	if gw.createCalls != 1 {
		t.Fatalf("createCalls after duplicate delivery = %d, want still 1 (no double side effect)", gw.createCalls)
	}
	assertTextContains(t, second.Body.String(), "이미 처리된 요청입니다")

	retryReq := signedInteractionRequest(body)
	retryReq.Header.Set("X-Slack-Retry-Num", "1")
	third := httptest.NewRecorder()
	h.ServeInteraction(third, retryReq)
	if gw.createCalls != 1 {
		t.Fatalf("createCalls after retried duplicate delivery = %d, want still 1", gw.createCalls)
	}
	assertTextContains(t, third.Body.String(), "Slack 재시도 요청이 감지되어 중복 처리를 건너뛰었습니다")
}

func TestHandlerServeHTTPDedupSkipsDuplicateSlashCommand(t *testing.T) {
	t.Parallel()
	gw := &fakeGateway{}
	h := NewHandler(gw, "secret", map[string]bool{"U-admin": true})
	h.Now = fixedNow
	body := url.Values{"command": {"/brain"}, "channel_id": {"C1"}, "user_id": {"U-admin"}, "trigger_id": {"trigger-slash-1"}, "text": {"create Demo"}}.Encode()

	first := httptest.NewRecorder()
	h.ServeHTTP(first, signedRequest(body))
	if first.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	if gw.createCalls != 1 {
		t.Fatalf("createCalls after first request = %d, want 1", gw.createCalls)
	}

	second := httptest.NewRecorder()
	h.ServeHTTP(second, signedRequest(body))
	if second.Code != http.StatusOK {
		t.Fatalf("second status=%d body=%s", second.Code, second.Body.String())
	}
	if gw.createCalls != 1 {
		t.Fatalf("createCalls after duplicate request = %d, want still 1", gw.createCalls)
	}
	assertTextContains(t, second.Body.String(), "이미 처리된 요청입니다")
}

func TestIsSlackRetry(t *testing.T) {
	t.Parallel()
	withRetry := httptest.NewRequest(http.MethodPost, "/slack", nil)
	withRetry.Header.Set("X-Slack-Retry-Num", "1")
	if !isSlackRetry(withRetry) {
		t.Fatal("expected retry request to be detected")
	}

	withoutRetry := httptest.NewRequest(http.MethodPost, "/slack", nil)
	if isSlackRetry(withoutRetry) {
		t.Fatal("expected non-retry request to not be detected as retry")
	}
}

func TestDuplicateMessage(t *testing.T) {
	t.Parallel()
	retryReq := httptest.NewRequest(http.MethodPost, "/slack", nil)
	retryReq.Header.Set("X-Slack-Retry-Num", "1")
	if got := duplicateMessage(retryReq); got != "Slack 재시도 요청이 감지되어 중복 처리를 건너뛰었습니다." {
		t.Fatalf("duplicateMessage(retry) = %q", got)
	}

	plainReq := httptest.NewRequest(http.MethodPost, "/slack", nil)
	if got := duplicateMessage(plainReq); got != "이미 처리된 요청입니다." {
		t.Fatalf("duplicateMessage(plain) = %q", got)
	}
}

func TestDedupKeyFromBody(t *testing.T) {
	t.Parallel()
	if got := dedupKeyFromBody("trigger-1", []byte("ignored when trigger present")); got != "trigger:trigger-1" {
		t.Fatalf("dedupKeyFromBody with trigger = %q", got)
	}
	got1 := dedupKeyFromBody("", []byte("same body"))
	got2 := dedupKeyFromBody("", []byte("same body"))
	if got1 != got2 {
		t.Fatalf("dedupKeyFromBody fallback must be deterministic: %q != %q", got1, got2)
	}
	if !strings.HasPrefix(got1, "body:") {
		t.Fatalf("dedupKeyFromBody fallback = %q, want body: prefix", got1)
	}
	if got3 := dedupKeyFromBody("", []byte("different body")); got3 == got1 {
		t.Fatalf("dedupKeyFromBody fallback must differ for different bodies")
	}
}
