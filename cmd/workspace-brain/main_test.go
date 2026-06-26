package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sangyi/workspace-brain/internal/control/gateway"
	"github.com/sangyi/workspace-brain/internal/control/ingest"
	"github.com/sangyi/workspace-brain/internal/control/jobs"
	"github.com/sangyi/workspace-brain/internal/control/ops"
	slackadapter "github.com/sangyi/workspace-brain/internal/control/slack"
	"github.com/sangyi/workspace-brain/internal/core/ai"
	"github.com/sangyi/workspace-brain/internal/core/memory"
	"github.com/sangyi/workspace-brain/internal/platform/config"
	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

func TestRunDemo(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := runDemo(context.Background(), &out); err != nil {
		t.Fatalf("runDemo: %v", err)
	}
	if !strings.Contains(out.String(), "tenant=tenant-") || !strings.Contains(out.String(), "job=job-") {
		t.Fatalf("demo output = %q", out.String())
	}
}

func TestRunRoutesDemo(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := run(context.Background(), []string{"workspace-brain", "demo"}, func(string) string { return "" }, func(context.Context, *http.Server, config.Config) error { t.Fatal("serve should not run"); return nil }, &out)
	if err != nil {
		t.Fatalf("run demo: %v", err)
	}
	if !strings.Contains(out.String(), "근거 기반 응답") {
		t.Fatalf("demo output = %q", out.String())
	}
}

func TestRunRoutesServer(t *testing.T) {
	t.Parallel()
	called := false
	err := run(context.Background(), []string{"workspace-brain"}, func(key string) string {
		switch key {
		case "SLACK_SIGNING_SECRET":
			return "secret"
		case "ADMIN_USERS":
			return "U1"
		case "ADDR":
			return ":9999"
		default:
			return ""
		}
	}, func(_ context.Context, srv *http.Server, _ config.Config) error {
		called = true
		if srv.Addr != ":9999" || srv.Handler == nil {
			t.Fatalf("serve addr=%q handler=%v", srv.Addr, srv.Handler)
		}
		return errors.New("stop")
	}, &bytes.Buffer{})
	if err == nil || err.Error() != "stop" || !called {
		t.Fatalf("run server err=%v called=%v", err, called)
	}
}

func TestRunServerRequiresAFrontendAdapter(t *testing.T) {
	t.Parallel()
	if err := runServer(context.Background(), func(string) string { return "" }, func(context.Context, *http.Server, config.Config) error { return nil }); err == nil {
		t.Fatalf("expected missing frontend adapter error")
	}
}

func TestDefaultNewAppGatewayUsesSourceLoader(t *testing.T) {
	t.Parallel()
	core, err := newAppCore(config.Config{})
	if err != nil {
		t.Fatalf("newAppCore: %v", err)
	}
	gw, err := defaultNewAppGateway(core, gateway.RoleAuthorizer{}, jobs.NewStore())
	if err != nil {
		t.Fatalf("defaultNewAppGateway: %v", err)
	}
	admin := brainapi.Principal{Source: "test", ID: "admin", Roles: []string{"admin"}}
	member := brainapi.Principal{Source: "test", ID: "member", Roles: []string{"member"}}
	if _, err := gw.CreateProject(context.Background(), gateway.CreateProjectCommand{BindingKey: "test:space:loader", Principal: admin}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := gw.Ingest(context.Background(), gateway.IngestCommand{BindingKey: "test:space:loader", Principal: member, Source: brainapi.SourceRef{URI: "text:alpha%20beta%20gamma", Name: "encoded"}}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	answer, err := gw.Ask(context.Background(), gateway.AskCommand{BindingKey: "test:space:loader", Principal: member, Question: "alpha beta gamma"})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if !strings.Contains(answer.Answer, "근거 기반 응답: alpha beta gamma") {
		t.Fatalf("answer=%q", answer.Answer)
	}
}

func TestRunServerJSONAPIIngestUsesDefaultSourceLoader(t *testing.T) {
	t.Parallel()
	called := false
	err := runServer(context.Background(), func(key string) string {
		switch key {
		case "API_TOKEN":
			return "token"
		default:
			return ""
		}
	}, func(_ context.Context, srv *http.Server, _ config.Config) error {
		called = true
		post := func(body string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodPost, "/api/commands", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer token")
			rec := httptest.NewRecorder()
			srv.Handler.ServeHTTP(rec, req)
			return rec
		}

		create := post(`{"binding_key":"web:space:S1","principal":{"source":"web","id":"admin","roles":["admin"]},"text":"create Demo"}`)
		if create.Code != http.StatusOK {
			t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
		}
		ingest := post(`{"binding_key":"web:space:S1","principal":{"source":"web","id":"U1","roles":["member"]},"text":"ingest text:alpha%20beta%20gamma"}`)
		if ingest.Code != http.StatusOK {
			t.Fatalf("ingest status=%d body=%s", ingest.Code, ingest.Body.String())
		}
		ask := post(`{"binding_key":"web:space:S1","principal":{"source":"web","id":"U1","roles":["member"]},"text":"ask alpha beta gamma"}`)
		if ask.Code != http.StatusOK || !strings.Contains(ask.Body.String(), "근거 기반 응답: alpha beta gamma") {
			t.Fatalf("ask status=%d body=%s", ask.Code, ask.Body.String())
		}
		return errors.New("stop")
	})
	if err == nil || err.Error() != "stop" || !called {
		t.Fatalf("run server err=%v called=%v", err, called)
	}
}

func TestRunServerAllowsAPITokenOnly(t *testing.T) {
	t.Parallel()
	called := false
	err := runServer(context.Background(), func(key string) string {
		switch key {
		case "API_TOKEN":
			return "token"
		case "ADDR":
			return ":9998"
		default:
			return ""
		}
	}, func(_ context.Context, srv *http.Server, _ config.Config) error {
		called = true
		if srv.Addr != ":9998" || srv.Handler == nil {
			t.Fatalf("serve addr=%q handler=%v", srv.Addr, srv.Handler)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/commands", strings.NewReader(`{"binding_key":"web:space:S1","principal":{"source":"web","id":"U1"},"text":""}`))
		req.Header.Set("Authorization", "Bearer token")
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("api route status=%d body=%s", rec.Code, rec.Body.String())
		}
		return errors.New("stop")
	})
	if err == nil || err.Error() != "stop" || !called {
		t.Fatalf("run server err=%v called=%v", err, called)
	}
}

func TestRunServerMountsOpsAndSlackManifest(t *testing.T) {
	t.Parallel()
	err := runServer(context.Background(), func(key string) string {
		switch key {
		case "SLACK_SIGNING_SECRET":
			return "secret"
		case "PUBLIC_BASE_URL":
			return "https://example.com"
		case "SLACK_APP_NAME":
			return "Workspace Brain"
		case "COMMAND_NAME":
			return "brain"
		default:
			return ""
		}
	}, func(_ context.Context, srv *http.Server, _ config.Config) error {
		for _, path := range []string{"/healthz", "/readyz"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			srv.Handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body.String())
			}
		}
		req := httptest.NewRequest(http.MethodGet, "/slack/manifest.json", nil)
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "https://example.com/slack/commands") {
			t.Fatalf("manifest status=%d body=%s", rec.Code, rec.Body.String())
		}
		req = httptest.NewRequest(http.MethodPost, "/slack/manifest.json", nil)
		rec = httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("manifest method status=%d", rec.Code)
		}
		return errors.New("stop")
	})
	if err == nil || err.Error() != "stop" {
		t.Fatalf("err=%v", err)
	}
}

func TestRunServerAppliesTimeoutsAndPreparesDataPath(t *testing.T) {
	t.Parallel()
	dataPath := t.TempDir() + "/state"
	called := false
	err := runServer(context.Background(), func(key string) string {
		switch key {
		case "API_TOKEN":
			return "token"
		case "DATA_PATH":
			return dataPath
		case "READINESS_REQUIRED":
			return "true"
		case "HTTP_READ_HEADER_TIMEOUT":
			return "1s"
		case "HTTP_READ_TIMEOUT":
			return "2s"
		case "HTTP_WRITE_TIMEOUT":
			return "3s"
		case "HTTP_IDLE_TIMEOUT":
			return "4s"
		case "HTTP_SHUTDOWN_TIMEOUT":
			return "5s"
		default:
			return ""
		}
	}, func(_ context.Context, srv *http.Server, cfg config.Config) error {
		called = true
		if srv.ReadHeaderTimeout != time.Second || srv.ReadTimeout != 2*time.Second || srv.WriteTimeout != 3*time.Second || srv.IdleTimeout != 4*time.Second || cfg.ShutdownTimeout != 5*time.Second {
			t.Fatalf("timeouts srv=%+v cfg=%+v", srv, cfg)
		}
		if _, err := os.Stat(dataPath); err != nil {
			t.Fatalf("DATA_PATH not prepared: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("readyz status=%d body=%s", rec.Code, rec.Body.String())
		}
		return errors.New("stop")
	})
	if err == nil || err.Error() != "stop" || !called {
		t.Fatalf("run server err=%v called=%v", err, called)
	}
}

func TestReadinessRequiredFailsWhenDataPathDisappears(t *testing.T) {
	t.Parallel()
	dataPath := t.TempDir() + "/missing"
	cfg := config.Config{APIToken: "token", DataPath: dataPath, ReadinessRequired: true, CommandName: "brain", SlackAppName: "workspace-brain", Addr: ":0", ReadHeaderTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, IdleTimeout: time.Second, ShutdownTimeout: time.Second}
	handler, err := buildHandler(cfg, &demoGatewayFake{})
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRunServerRejectsInvalidSlackManifestConfig(t *testing.T) {
	t.Parallel()
	err := runServer(context.Background(), func(key string) string {
		switch key {
		case "SLACK_SIGNING_SECRET":
			return "secret"
		case "PUBLIC_BASE_URL":
			return "http://example.com"
		default:
			return ""
		}
	}, func(context.Context, *http.Server, config.Config) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "command URL must be an absolute HTTPS URL") {
		t.Fatalf("err=%v", err)
	}
}

func TestSlackManifestHandlerValidation(t *testing.T) {
	t.Parallel()
	if _, err := slackManifestHandler(slackadapter.ManifestConfig{}); err == nil {
		t.Fatalf("expected validation error")
	}
}

func TestMainSuccessAndFatalPath(t *testing.T) {
	oldArgs := osArgs()
	oldFatal := fatal
	defer func() {
		setArgs(oldArgs)
		fatal = oldFatal
	}()
	setArgs([]string{"workspace-brain", "demo"})
	main()

	setArgs([]string{"workspace-brain"})
	fatal = func(v ...any) { panic("fatal called") }
	defer func() {
		if recover() == nil {
			t.Fatalf("expected fatal panic")
		}
	}()
	main()
}

func TestRunServerGatewayError(t *testing.T) {
	old := newAppGateway
	defer func() { newAppGateway = old }()
	newAppGateway = func(brainapi.Core, gateway.Authorizer, *jobs.Store) (appGateway, error) {
		return nil, errors.New("gateway boom")
	}
	err := runServer(context.Background(), func(key string) string {
		if key == "SLACK_SIGNING_SECRET" {
			return "secret"
		}
		return ""
	}, func(context.Context, *http.Server, config.Config) error { return nil })
	if err == nil || err.Error() != "gateway boom" {
		t.Fatalf("err=%v", err)
	}
}

func TestRunDemoGatewayError(t *testing.T) {
	old := newAppGateway
	defer func() { newAppGateway = old }()
	newAppGateway = func(brainapi.Core, gateway.Authorizer, *jobs.Store) (appGateway, error) {
		return nil, errors.New("gateway boom")
	}
	if err := runDemo(context.Background(), &bytes.Buffer{}); err == nil || err.Error() != "gateway boom" {
		t.Fatalf("err=%v", err)
	}
}

func TestRunDemoFlowErrors(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		gw   *demoGatewayFake
		core *demoCoreFake
		out  io.Writer
	}{
		{"create", &demoGatewayFake{createErr: errors.New("create")}, &demoCoreFake{}, &bytes.Buffer{}},
		{"ingest", &demoGatewayFake{ingestErr: errors.New("ingest")}, &demoCoreFake{}, &bytes.Buffer{}},
		{"complete", &demoGatewayFake{}, &demoCoreFake{completeErr: errors.New("complete")}, &bytes.Buffer{}},
		{"ask", &demoGatewayFake{askErr: errors.New("ask")}, &demoCoreFake{}, &bytes.Buffer{}},
		{"writer", &demoGatewayFake{}, &demoCoreFake{}, errWriter{}},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := runDemoFlow(context.Background(), tt.out, tt.core, tt.gw); err == nil {
				t.Fatalf("expected error")
			}
		})
	}
}

type demoGatewayFake struct {
	createErr error
	ingestErr error
	askErr    error
}

func (f *demoGatewayFake) CreateProject(context.Context, gateway.CreateProjectCommand) (gateway.CreateProjectResult, error) {
	return gateway.CreateProjectResult{TenantID: "tenant-fake"}, f.createErr
}
func (f *demoGatewayFake) Ingest(context.Context, gateway.IngestCommand) (gateway.IngestResult, error) {
	return gateway.IngestResult{JobID: "job-fake"}, f.ingestErr
}
func (f *demoGatewayFake) Ask(context.Context, gateway.AskCommand) (brainapi.QueryResponse, error) {
	return brainapi.QueryResponse{Answer: "answer"}, f.askErr
}
func (f *demoGatewayFake) Discover(context.Context, gateway.DiscoverCommand) (brainapi.DiscoverResponse, error) {
	return brainapi.DiscoverResponse{}, nil
}
func (f *demoGatewayFake) Status(context.Context, gateway.StatusCommand) (brainapi.JobSnapshot, error) {
	return brainapi.JobSnapshot{}, nil
}
func (f *demoGatewayFake) SetProjectState(context.Context, gateway.SetProjectStateCommand) (gateway.SetProjectStateResult, error) {
	return gateway.SetProjectStateResult{}, nil
}

type demoCoreFake struct{ completeErr error }

func (f *demoCoreFake) CompleteJob(brainapi.TenantID, brainapi.JobID, brainapi.JobStatus, string, string) error {
	return f.completeErr
}
func (f *demoCoreFake) ResolveBinding(context.Context, brainapi.BindingKey) (brainapi.TenantID, error) {
	return "", nil
}
func (f *demoCoreFake) CreateProject(context.Context, brainapi.CreateProjectRequest) error {
	return nil
}
func (f *demoCoreFake) Ingest(context.Context, brainapi.IngestRequest) error { return nil }
func (f *demoCoreFake) JobStatus(context.Context, brainapi.TenantID, brainapi.JobID) (brainapi.JobSnapshot, error) {
	return brainapi.JobSnapshot{}, nil
}
func (f *demoCoreFake) Query(context.Context, brainapi.QueryRequest) (brainapi.QueryResponse, error) {
	return brainapi.QueryResponse{}, nil
}
func (f *demoCoreFake) Discover(context.Context, brainapi.DiscoverRequest) (brainapi.DiscoverResponse, error) {
	return brainapi.DiscoverResponse{}, nil
}
func (f *demoCoreFake) SetProjectState(context.Context, brainapi.TenantID, brainapi.ProjectState) error {
	return nil
}
func (f *demoCoreFake) AdminListTenants(context.Context, brainapi.AdminListTenantsRequest) (brainapi.AdminListTenantsResponse, error) {
	return brainapi.AdminListTenantsResponse{}, nil
}
func (f *demoCoreFake) AdminListBindings(context.Context, brainapi.AdminListBindingsRequest) (brainapi.AdminListBindingsResponse, error) {
	return brainapi.AdminListBindingsResponse{}, nil
}
func (f *demoCoreFake) AdminListSources(context.Context, brainapi.TenantID) (brainapi.AdminListSourcesResponse, error) {
	return brainapi.AdminListSourcesResponse{}, nil
}
func (f *demoCoreFake) AdminListJobs(context.Context, brainapi.TenantID) (brainapi.AdminListJobsResponse, error) {
	return brainapi.AdminListJobsResponse{}, nil
}
func (f *demoCoreFake) AdminGetJob(context.Context, brainapi.TenantID, brainapi.JobID) (brainapi.JobSnapshot, error) {
	return brainapi.JobSnapshot{}, nil
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("write") }

func osArgs() []string { return append([]string(nil), os.Args...) }

func setArgs(args []string) { os.Args = append([]string(nil), args...) }

func TestRunServerReturnsConfigError(t *testing.T) {
	t.Parallel()
	err := runServer(context.Background(), func(key string) string {
		if key == "READINESS_REQUIRED" {
			return "not-a-bool"
		}
		return ""
	}, func(context.Context, *http.Server, config.Config) error {
		t.Fatal("serve should not run when config parsing fails")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "READINESS_REQUIRED must be a boolean") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunServerReturnsDataPathPrepareError(t *testing.T) {
	t.Parallel()
	file, err := os.CreateTemp(t.TempDir(), "data-path-file")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err = runServer(context.Background(), func(key string) string {
		switch key {
		case "API_TOKEN":
			return "token"
		case "DATA_PATH":
			return file.Name()
		default:
			return ""
		}
	}, func(context.Context, *http.Server, config.Config) error {
		t.Fatal("serve should not run when DATA_PATH cannot be prepared")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "prepare DATA_PATH") {
		t.Fatalf("err=%v", err)
	}
}

func TestReadinessRequiredAllowsEmptyDataPath(t *testing.T) {
	t.Parallel()
	cfg := config.Config{APIToken: "token", ReadinessRequired: true, CommandName: "brain", SlackAppName: "workspace-brain", Addr: ":0", ReadHeaderTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, IdleTimeout: time.Second, ShutdownTimeout: time.Second}
	handler, err := buildHandler(cfg, &demoGatewayFake{})
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestReadinessRequiredFailsWhenDataPathIsFile(t *testing.T) {
	t.Parallel()
	file, err := os.CreateTemp(t.TempDir(), "data-path-file")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	cfg := config.Config{APIToken: "token", DataPath: file.Name(), ReadinessRequired: true, CommandName: "brain", SlackAppName: "workspace-brain", Addr: ":0", ReadHeaderTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, IdleTimeout: time.Second, ShutdownTimeout: time.Second}
	handler, err := buildHandler(cfg, &demoGatewayFake{})
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestServeHTTPServerDelegatesToPlatformServer(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv := &http.Server{Addr: addr, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })}
	errCh := make(chan error, 1)
	go func() { errCh <- serveHTTPServer(ctx, srv, config.Config{ShutdownTimeout: time.Second}) }()
	deadline := time.After(2 * time.Second)
	for {
		resp, err := http.Get("http://" + addr)
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatalf("server did not start: %v", err)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("serveHTTPServer: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("serveHTTPServer did not return after cancel")
	}
}

// ── OpenAI synthesizer bridge tests ──────────────────────────────────────────

// TestOpenAISynthesizerBridgeHappyPath wires the bridge with ai.LocalClient
// and verifies the answer, grounded spans, and supplemented spans are set.
func TestOpenAISynthesizerBridgeHappyPath(t *testing.T) {
	t.Parallel()
	bridge := &openAISynthesizerBridge{responder: ai.LocalClient{}}
	inputs := []memory.SynthesisInput{{Text: "first chunk"}, {Text: "second chunk"}}
	result, err := bridge.Synthesize(context.Background(), "what is this?", inputs)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if result.Answer == "" {
		t.Fatal("expected non-empty answer")
	}
	if len(result.GroundedSpans) != 2 {
		t.Fatalf("grounded spans = %d, want 2", len(result.GroundedSpans))
	}
	if result.GroundedSpans[0] != "first chunk" || result.GroundedSpans[1] != "second chunk" {
		t.Fatalf("grounded spans = %v", result.GroundedSpans)
	}
	if len(result.SupplementedSpans) != 1 || result.SupplementedSpans[0] != result.Answer {
		t.Fatalf("supplemented spans = %v, answer = %q", result.SupplementedSpans, result.Answer)
	}
}

// TestOpenAISynthesizerBridgeEmptyInputs checks the no-grounding abstention path.
func TestOpenAISynthesizerBridgeEmptyInputs(t *testing.T) {
	t.Parallel()
	bridge := &openAISynthesizerBridge{responder: ai.LocalClient{}}
	result, err := bridge.Synthesize(context.Background(), "q", nil)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if !strings.Contains(result.Answer, "수집된 근거가 없습니다") {
		t.Fatalf("abstention answer = %q", result.Answer)
	}
	if result.GroundedSpans != nil || result.SupplementedSpans != nil {
		t.Fatalf("expected nil spans for abstention; grounded=%v supplemented=%v", result.GroundedSpans, result.SupplementedSpans)
	}
}

// TestOpenAISynthesizerBridgeResponderError confirms that a Responder error
// surfaces as a Synthesize error without a partial result.
func TestOpenAISynthesizerBridgeResponderError(t *testing.T) {
	t.Parallel()
	boom := errors.New("responder down")
	bridge := &openAISynthesizerBridge{responder: &alwaysErrResponder{err: boom}}
	inputs := []memory.SynthesisInput{{Text: "some chunk"}}
	_, err := bridge.Synthesize(context.Background(), "question", inputs)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

// alwaysErrResponder is a test double that always returns an error.
type alwaysErrResponder struct{ err error }

func (r *alwaysErrResponder) Respond(context.Context, string) (string, error) {
	return "", r.err
}

// TestOpenAISynthesizerBridgeWiredViaWithSynthesizer confirms that the bridge
// can be injected into a memory.Core via WithSynthesizer and that Query
// returns a non-empty answer produced by the bridge (ai.LocalClient).
func TestOpenAISynthesizerBridgeWiredViaWithSynthesizer(t *testing.T) {
	t.Parallel()
	core, err := newAppCore(config.Config{})
	if err != nil {
		t.Fatalf("newAppCore: %v", err)
	}
	// Replace synthesizer with the bridge backed by LocalClient.
	coreSynthesized := memory.New(
		memory.WithSynthesizer(&openAISynthesizerBridge{responder: ai.LocalClient{}}),
	)
	ctx := context.Background()
	if err := coreSynthesized.CreateProject(ctx, brainapi.CreateProjectRequest{
		TenantID: "tenant-bridge", BindingKey: "test:space:bridge",
		OwnerPrincipal: brainapi.Principal{ID: "owner"},
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := coreSynthesized.Ingest(ctx, brainapi.IngestRequest{
		TenantID: "tenant-bridge", JobID: "job-1",
		Source:   brainapi.SourceRef{URI: "file://test.txt"},
		Metadata: map[string]string{"content": "workspace brain knowledge"},
	}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	resp, err := coreSynthesized.Query(ctx, brainapi.QueryRequest{
		TenantID: "tenant-bridge", Question: "knowledge",
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if resp.Answer == "" {
		t.Fatal("expected non-empty answer from bridge")
	}
	if !resp.GroundingAvailable {
		t.Fatal("expected grounding available")
	}
	if len(resp.SupplementedSpans) == 0 {
		t.Fatal("expected supplemented spans from bridge synthesizer")
	}
	_ = core // core was allocated; keep it to satisfy the compiler
}

// ── OpenAI embedder bridge tests ─────────────────────────────────────────────

func make1024DimJSON() []byte {
	vec := make([]float64, openAIEmbeddingDimensions)
	for i := range vec {
		vec[i] = float64(i + 1)
	}
	b, _ := json.Marshal(map[string]any{
		"data": []any{map[string]any{"embedding": vec}},
	})
	return b
}

func TestOpenAIEmbedderBridgeHappyPath(t *testing.T) {
	t.Parallel()
	body := make1024DimJSON()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer server.Close()

	client, err := ai.NewOpenAIClient(ai.OpenAIConfig{
		BaseURL:             server.URL,
		APIKey:              "key",
		EmbeddingModel:      "embed",
		HTTPClient:          server.Client(),
		EmbeddingDimensions: openAIEmbeddingDimensions,
	})
	if err != nil {
		t.Fatalf("NewOpenAIClient: %v", err)
	}
	bridge := &openAIEmbedderBridge{client: client, dims: openAIEmbeddingDimensions}
	result := bridge.Embed("hello world")
	if len(result) != openAIEmbeddingDimensions {
		t.Fatalf("len(result)=%d, want %d", len(result), openAIEmbeddingDimensions)
	}
	if result[0] != 1 || result[openAIEmbeddingDimensions-1] != float64(openAIEmbeddingDimensions) {
		t.Fatalf("unexpected vector values: [0]=%v [last]=%v", result[0], result[openAIEmbeddingDimensions-1])
	}
}

func TestOpenAIEmbedderBridgeReturnsZeroVecOnClientError(t *testing.T) {
	t.Parallel()
	// 418 is not retried — client returns an error immediately.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client, _ := ai.NewOpenAIClient(ai.OpenAIConfig{
		BaseURL: server.URL, APIKey: "key", EmbeddingModel: "embed",
		HTTPClient: server.Client(),
	})
	bridge := &openAIEmbedderBridge{client: client, dims: openAIEmbeddingDimensions}
	result := bridge.Embed("hello")
	if len(result) != openAIEmbeddingDimensions {
		t.Fatalf("len=%d, want %d", len(result), openAIEmbeddingDimensions)
	}
	for _, v := range result {
		if v != 0 {
			t.Fatalf("expected zero vector on error, got non-zero element")
		}
	}
}

func TestOpenAIEmbedderBridgeReturnsZeroVecOnEmptyResponse(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()
	client, _ := ai.NewOpenAIClient(ai.OpenAIConfig{
		BaseURL: server.URL, APIKey: "key", EmbeddingModel: "embed",
		HTTPClient: server.Client(),
	})
	bridge := &openAIEmbedderBridge{client: client, dims: openAIEmbeddingDimensions}
	result := bridge.Embed("hello")
	if len(result) != openAIEmbeddingDimensions {
		t.Fatalf("len=%d, want %d", len(result), openAIEmbeddingDimensions)
	}
}

func TestOpenAIEmbedderOptionReturnsNilWhenAPIKeyMissing(t *testing.T) {
	t.Parallel()
	opts := openAIEmbedderOption(config.Config{OpenAIEmbeddingModel: "text-embedding-3-small"})
	if opts != nil {
		t.Fatalf("expected nil options when API key is missing, got %v", opts)
	}
}

func TestOpenAIEmbedderOptionReturnsNilWhenModelMissing(t *testing.T) {
	t.Parallel()
	opts := openAIEmbedderOption(config.Config{OpenAIAPIKey: "sk-test"})
	if opts != nil {
		t.Fatalf("expected nil options when embedding model is missing, got %v", opts)
	}
}

func TestOpenAIEmbedderOptionReturnsBridgeWhenFullyConfigured(t *testing.T) {
	t.Parallel()
	opts := openAIEmbedderOption(config.Config{
		OpenAIAPIKey:         "sk-test",
		OpenAIEmbeddingModel: "text-embedding-3-small",
		OpenAIBaseURL:        "https://api.openai.com/v1",
	})
	if len(opts) != 1 {
		t.Fatalf("expected 1 option, got %d", len(opts))
	}
}

func TestDefaultNewAppCoreUsesLocalEmbedderWhenOpenAINotConfigured(t *testing.T) {
	t.Parallel()
	core, err := defaultNewAppCore(config.Config{})
	if err != nil {
		t.Fatalf("defaultNewAppCore: %v", err)
	}
	if core == nil {
		t.Fatal("expected non-nil core")
	}
}

func TestDefaultNewAppCoreUsesOpenAIEmbedderWhenConfigured(t *testing.T) {
	t.Parallel()
	// The bridge is created; we verify the core is non-nil without making
	// real network calls (Embed is only called on Ingest/Query).
	cfg := config.Config{
		OpenAIAPIKey:         "sk-test",
		OpenAIEmbeddingModel: "text-embedding-3-small",
	}
	core, err := defaultNewAppCore(cfg)
	if err != nil {
		t.Fatalf("defaultNewAppCore: %v", err)
	}
	if core == nil {
		t.Fatal("expected non-nil core")
	}
}

func TestRunServerWiresOpenAIEmbedderFromEnv(t *testing.T) {
	t.Parallel()
	// Verify that OpenAI env vars flow through FromEnv → defaultNewAppCore
	// without error. The bridge only makes network calls when Embed is invoked.
	called := false
	err := runServer(context.Background(), func(key string) string {
		switch key {
		case "API_TOKEN":
			return "token"
		case "OPENAI_API_KEY":
			return "sk-test"
		case "OPENAI_EMBEDDING_MODEL":
			return "text-embedding-3-small"
		default:
			return ""
		}
	}, func(_ context.Context, _ *http.Server, _ config.Config) error {
		called = true
		return errors.New("stop")
	})
	if err == nil || err.Error() != "stop" || !called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

func TestRunDemoReturnsCoreError(t *testing.T) {
	old := newAppCore
	defer func() { newAppCore = old }()
	newAppCore = func(config.Config) (demoCore, error) {
		return nil, errors.New("core boom")
	}
	if err := runDemo(context.Background(), &bytes.Buffer{}); err == nil || err.Error() != "core boom" {
		t.Fatalf("err=%v", err)
	}
}

// ── RabbitMQ queue-selection wiring tests ────────────────────────────────────

// stubQueueRunner is a test double that satisfies queueRunner without needing
// a real RabbitMQ broker.
type stubQueueRunner struct {
	startErr  error
	startedCh chan struct{}
	stoppedCh chan struct{}
}

func newStubQueueRunner(startErr error) *stubQueueRunner {
	return &stubQueueRunner{
		startErr:  startErr,
		startedCh: make(chan struct{}),
		stoppedCh: make(chan struct{}),
	}
}

func (s *stubQueueRunner) Start(_ context.Context) error {
	if s.startErr != nil {
		return s.startErr
	}
	close(s.startedCh)
	return nil
}
func (s *stubQueueRunner) Stop()                                          { close(s.stoppedCh) }
func (s *stubQueueRunner) Enqueue(_ context.Context, _ ingest.Task) error { return nil }

// TestRunServerUsesRabbitMQQueueWhenURLSet verifies that when RABBITMQ_URL is
// configured the RabbitMQ queue is started and stopped as part of the server
// lifecycle.
func TestRunServerUsesRabbitMQQueueWhenURLSet(t *testing.T) {
	oldNewRMQ := newRabbitMQQueue
	defer func() { newRabbitMQQueue = oldNewRMQ }()

	stub := newStubQueueRunner(nil)
	newRabbitMQQueue = func(_ string, _ ingest.CoreCompleter, _ ingest.GatewayCompleter) queueRunner {
		return stub
	}

	called := false
	err := runServer(context.Background(), func(key string) string {
		switch key {
		case "API_TOKEN":
			return "token"
		case "RABBITMQ_URL":
			return "amqp://localhost:5672/"
		default:
			return ""
		}
	}, func(_ context.Context, _ *http.Server, _ config.Config) error {
		called = true
		return errors.New("stop")
	})
	if err == nil || err.Error() != "stop" || !called {
		t.Fatalf("err=%v called=%v", err, called)
	}
	// Verify Start was called (channel closed).
	select {
	case <-stub.startedCh:
	default:
		t.Fatal("expected queue Start() to be called")
	}
	// Verify Stop was called (defer fires after serve returns).
	select {
	case <-stub.stoppedCh:
	default:
		t.Fatal("expected queue Stop() to be called")
	}
}

// TestRunServerRabbitMQStartErrorPropagates verifies that a Start() failure
// causes runServer to return the error without calling the serve function.
func TestRunServerRabbitMQStartErrorPropagates(t *testing.T) {
	oldNewRMQ := newRabbitMQQueue
	defer func() { newRabbitMQQueue = oldNewRMQ }()

	newRabbitMQQueue = func(_ string, _ ingest.CoreCompleter, _ ingest.GatewayCompleter) queueRunner {
		return newStubQueueRunner(errors.New("connection refused"))
	}

	err := runServer(context.Background(), func(key string) string {
		switch key {
		case "API_TOKEN":
			return "token"
		case "RABBITMQ_URL":
			return "amqp://localhost:5672/"
		default:
			return ""
		}
	}, func(context.Context, *http.Server, config.Config) error {
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err=%v, want 'connection refused'", err)
	}
}

// ── Healthcheck subcommand tests ──────────────────────────────────────────────

// failTransport is an http.RoundTripper that always returns an error.
type failTransport struct{ err error }

func (f failTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

// TestRunHealthcheckReturns0On2xx verifies that a 2xx response yields exit code 0.
func TestRunHealthcheckReturns0On2xx(t *testing.T) {
	t.Parallel()
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer svr.Close()
	if code := runHealthcheck(svr.Listener.Addr().String(), svr.Client()); code != 0 {
		t.Fatalf("code=%d, want 0", code)
	}
}

// TestRunHealthcheckReturns1OnNon2xx verifies that a non-2xx response yields
// exit code 1.
func TestRunHealthcheckReturns1OnNon2xx(t *testing.T) {
	t.Parallel()
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer svr.Close()
	if code := runHealthcheck(svr.Listener.Addr().String(), svr.Client()); code != 1 {
		t.Fatalf("code=%d, want 1", code)
	}
}

// TestRunHealthcheckReturns1OnClientError verifies that a transport error
// yields exit code 1.
func TestRunHealthcheckReturns1OnClientError(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: failTransport{err: errors.New("connect refused")}}
	if code := runHealthcheck("127.0.0.1:9999", client); code != 1 {
		t.Fatalf("code=%d, want 1", code)
	}
}

// TestRunHealthcheckReturns1OnInvalidAddr verifies that an unparseable addr
// yields exit code 1 without making any network call.
func TestRunHealthcheckReturns1OnInvalidAddr(t *testing.T) {
	t.Parallel()
	// "no-port" has no port separator — net.SplitHostPort returns an error.
	if code := runHealthcheck("no-port", nil); code != 1 {
		t.Fatalf("code=%d, want 1", code)
	}
}

// TestRunHealthcheckBindAllHostReplacedWithLoopback verifies that an empty
// host (e.g., ":8080" bind-all) is replaced with 127.0.0.1 so the probe
// reaches the local server.
func TestRunHealthcheckBindAllHostReplacedWithLoopback(t *testing.T) {
	t.Parallel()
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == ops.ReadinessPath {
			w.WriteHeader(http.StatusOK)
		} else {
			http.NotFound(w, r)
		}
	}))
	defer svr.Close()
	// Build a bind-all address (empty host) using the server's port.
	_, portStr, err := net.SplitHostPort(svr.Listener.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	addr := ":" + portStr // host="" triggers the 127.0.0.1 substitution
	if code := runHealthcheck(addr, svr.Client()); code != 0 {
		t.Fatalf("code=%d, want 0 (bind-all host not replaced with 127.0.0.1)", code)
	}
}

// TestRunHealthcheckDefaultAddrWhenEnvEmpty verifies that when ADDR is unset
// runHealthcheck is called with the default ":8080" address.  Because no real
// server is listening, the injected client returns a transport error and the
// captured exit code is 1.
func TestRunHealthcheckDefaultAddrWhenEnvEmpty(t *testing.T) {
	// Modifies package-level vars — must not run in parallel.
	oldOsExit := osExit
	defer func() { osExit = oldOsExit }()
	oldHTTPClient := httpClient
	defer func() { httpClient = oldHTTPClient }()

	var capturedCode int
	osExit = func(code int) { capturedCode = code }
	httpClient = &http.Client{Transport: failTransport{err: errors.New("no server")}}

	err := run(context.Background(), []string{"wb", "healthcheck"}, func(string) string {
		return "" // ADDR is empty → default ":8080" should be used
	}, nil, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if capturedCode != 1 {
		t.Fatalf("exit code=%d, want 1 (no server listening at default addr)", capturedCode)
	}
}

// TestDefaultNewRabbitMQQueueIsCalledWhenNotOverridden exercises the real
// defaultNewRabbitMQQueue shim.  An invalid AMQP scheme causes amqp.Dial to
// fail immediately (no network I/O), so the test is deterministic.
func TestDefaultNewRabbitMQQueueIsCalledWhenNotOverridden(t *testing.T) {
	// Do NOT override newRabbitMQQueue so that defaultNewRabbitMQQueue is used.
	// Requires a non-parallel test to avoid races with tests that DO override it.
	err := runServer(context.Background(), func(key string) string {
		switch key {
		case "API_TOKEN":
			return "token"
		case "RABBITMQ_URL":
			// "invalid" scheme → amqp.Dial rejects it immediately without dialing.
			return "invalid://localhost"
		default:
			return ""
		}
	}, func(context.Context, *http.Server, config.Config) error { return nil })
	if err == nil {
		t.Fatalf("expected error from invalid RabbitMQ scheme, got nil")
	}
}

// TestRunRoutesHealthcheckSubcommand verifies that run() dispatches to
// runHealthcheck when the "healthcheck" subcommand is passed, captures the
// exit code via the osExit seam, and returns nil (the real os.Exit would have
// stopped execution).
func TestRunRoutesHealthcheckSubcommand(t *testing.T) {
	// Modifies package-level vars — must not run in parallel with other tests
	// that do the same.
	oldOsExit := osExit
	defer func() { osExit = oldOsExit }()
	oldHTTPClient := httpClient
	defer func() { httpClient = oldHTTPClient }()

	var capturedCode int
	osExit = func(code int) { capturedCode = code }

	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == ops.ReadinessPath {
			w.WriteHeader(http.StatusOK)
		} else {
			http.NotFound(w, r)
		}
	}))
	defer svr.Close()

	httpClient = svr.Client()
	addr := svr.Listener.Addr().String()

	err := run(context.Background(), []string{"wb", "healthcheck"}, func(key string) string {
		if key == "ADDR" {
			return addr
		}
		return ""
	}, nil, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if capturedCode != 0 {
		t.Fatalf("exit code=%d, want 0", capturedCode)
	}
}
