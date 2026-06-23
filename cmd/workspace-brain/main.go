package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/sangyi/workspace-brain/internal/control/gateway"
	"github.com/sangyi/workspace-brain/internal/control/jobs"
	slackadapter "github.com/sangyi/workspace-brain/internal/control/slack"
	"github.com/sangyi/workspace-brain/internal/core/memory"
	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

type envFunc func(string) string
type serveFunc func(string, http.Handler) error

type appGateway interface {
	slackadapter.Gateway
	Ask(ctx context.Context, cmd gateway.AskCommand) (brainapi.QueryResponse, error)
	CreateProject(ctx context.Context, cmd gateway.CreateProjectCommand) (gateway.CreateProjectResult, error)
	Ingest(ctx context.Context, cmd gateway.IngestCommand) (gateway.IngestResult, error)
}

type demoCore interface {
	brainapi.Core
	CompleteJob(tenantID brainapi.TenantID, jobID brainapi.JobID, status brainapi.JobStatus, resultRef, message string) error
}

var (
	fatal         = log.Fatal
	newAppCore    = func() demoCore { return memory.New() }
	newAppGateway = func(core brainapi.Core, authorizer gateway.Authorizer, store *jobs.Store) (appGateway, error) {
		return gateway.New(core, authorizer, store)
	}
)

func main() {
	if err := run(os.Args, os.Getenv, http.ListenAndServe, os.Stdout); err != nil {
		fatal(err)
	}
}

func run(args []string, getenv envFunc, serve serveFunc, out io.Writer) error {
	if len(args) > 1 && args[1] == "demo" {
		return runDemo(context.Background(), out)
	}
	return runServer(getenv, serve)
}

func runServer(getenv envFunc, serve serveFunc) error {
	secret := getenv("SLACK_SIGNING_SECRET")
	if secret == "" {
		return fmt.Errorf("SLACK_SIGNING_SECRET is required; use `go run ./cmd/workspace-brain demo` for a local contract demo")
	}
	core := newAppCore()
	gw, err := newAppGateway(core, gateway.RoleAuthorizer{}, jobs.NewStore())
	if err != nil {
		return err
	}
	handler := slackadapter.NewHandler(gw, secret, parseAdminUsers(getenv("ADMIN_USERS")))
	addr := envOr(getenv, "ADDR", ":8080")
	mux := http.NewServeMux()
	mux.Handle("/slack/commands", handler)
	log.Printf("workspace-brain listening on %s", addr)
	return serve(addr, mux)
}

func runDemo(ctx context.Context, out io.Writer) error {
	core := newAppCore()
	gw, err := newAppGateway(core, gateway.RoleAuthorizer{}, jobs.NewStore())
	if err != nil {
		return err
	}
	return runDemoFlow(ctx, out, core, gw)
}

func runDemoFlow(ctx context.Context, out io.Writer, core demoCore, gw appGateway) error {
	admin := brainapi.Principal{Source: "demo", ID: "admin", Roles: []string{"admin"}}
	member := brainapi.Principal{Source: "demo", ID: "member", Roles: []string{"member"}}
	created, err := gw.CreateProject(ctx, gateway.CreateProjectCommand{BindingKey: "demo:project:default", Principal: admin, Metadata: map[string]string{"name": "demo"}})
	if err != nil {
		return err
	}
	ingest, err := gw.Ingest(ctx, gateway.IngestCommand{BindingKey: "demo:project:default", Principal: member, Source: brainapi.SourceRef{URI: "file://demo.md", Name: "demo.md", MimeType: "text/markdown"}})
	if err != nil {
		return err
	}
	if err := core.CompleteJob(created.TenantID, ingest.JobID, brainapi.JobCompleted, "memory://demo", ""); err != nil {
		return err
	}
	answer, err := gw.Ask(ctx, gateway.AskCommand{BindingKey: "demo:project:default", Principal: member, Question: "workspace-brain은 무엇인가?"})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "tenant=%s job=%s answer=%s\n", created.TenantID, ingest.JobID, answer.Answer)
	return err
}

func parseAdminUsers(raw string) map[string]bool {
	admins := make(map[string]bool)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			admins[part] = true
		}
	}
	return admins
}

func envOr(getenv envFunc, key, fallback string) string {
	if value := getenv(key); value != "" {
		return value
	}
	return fallback
}
