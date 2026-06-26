package main

import (
	"bytes"
	"context"
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
	"github.com/sangyi/workspace-brain/internal/control/jobs"
	slackadapter "github.com/sangyi/workspace-brain/internal/control/slack"
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
