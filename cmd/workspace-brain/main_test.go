package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/sangyi/workspace-brain/internal/control/gateway"
	"github.com/sangyi/workspace-brain/internal/control/jobs"
	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

func TestRunDemo(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := runDemo(context.Background(), &out); err != nil {
		t.Fatalf("runDemo: %v", err)
	}
	if !strings.Contains(out.String(), "tenant=tenant-000001") || !strings.Contains(out.String(), "job=job-000001") {
		t.Fatalf("demo output = %q", out.String())
	}
}

func TestRunRoutesDemo(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := run([]string{"workspace-brain", "demo"}, func(string) string { return "" }, func(string, http.Handler) error { t.Fatal("serve should not run"); return nil }, &out)
	if err != nil {
		t.Fatalf("run demo: %v", err)
	}
	if !strings.Contains(out.String(), "walking skeleton") {
		t.Fatalf("demo output = %q", out.String())
	}
}

func TestRunRoutesServer(t *testing.T) {
	t.Parallel()
	called := false
	err := run([]string{"workspace-brain"}, func(key string) string {
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
	}, func(addr string, handler http.Handler) error {
		called = true
		if addr != ":9999" || handler == nil {
			t.Fatalf("serve addr=%q handler=%v", addr, handler)
		}
		return errors.New("stop")
	}, &bytes.Buffer{})
	if err == nil || err.Error() != "stop" || !called {
		t.Fatalf("run server err=%v called=%v", err, called)
	}
}

func TestRunServerRequiresSlackSecret(t *testing.T) {
	t.Parallel()
	if err := runServer(func(string) string { return "" }, func(string, http.Handler) error { return nil }); err == nil {
		t.Fatalf("expected missing secret error")
	}
}

func TestParseAdminUsers(t *testing.T) {
	t.Parallel()
	admins := parseAdminUsers(" U1, U2,, ")
	if !admins["U1"] || !admins["U2"] || admins[""] {
		t.Fatalf("admins = %+v", admins)
	}
}

func TestEnvOr(t *testing.T) {
	t.Parallel()
	if got := envOr(func(string) string { return "" }, "KEY", "fallback"); got != "fallback" {
		t.Fatalf("fallback = %q", got)
	}
	if got := envOr(func(string) string { return "value" }, "KEY", "fallback"); got != "value" {
		t.Fatalf("value = %q", got)
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
	err := runServer(func(key string) string {
		if key == "SLACK_SIGNING_SECRET" {
			return "secret"
		}
		return ""
	}, func(string, http.Handler) error { return nil })
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
