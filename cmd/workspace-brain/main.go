package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/sangyi/workspace-brain/internal/control/gateway"
	"github.com/sangyi/workspace-brain/internal/control/httpapi"
	"github.com/sangyi/workspace-brain/internal/control/jobs"
	"github.com/sangyi/workspace-brain/internal/control/ops"
	slackadapter "github.com/sangyi/workspace-brain/internal/control/slack"
	"github.com/sangyi/workspace-brain/internal/control/sources"
	"github.com/sangyi/workspace-brain/internal/core/memory"
	"github.com/sangyi/workspace-brain/internal/platform/config"
	platformserver "github.com/sangyi/workspace-brain/internal/platform/server"
	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

type envFunc func(string) string
type serveFunc func(context.Context, *http.Server, config.Config) error

type appGateway interface {
	slackadapter.Gateway
}

type demoCore interface {
	brainapi.Core
	CompleteJob(tenantID brainapi.TenantID, jobID brainapi.JobID, status brainapi.JobStatus, resultRef, message string) error
}

var (
	fatal         = log.Fatal
	newAppCore    = defaultNewAppCore
	newAppGateway = defaultNewAppGateway
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args, os.Getenv, serveHTTPServer, os.Stdout); err != nil {
		fatal(err)
	}
}

func run(ctx context.Context, args []string, getenv envFunc, serve serveFunc, out io.Writer) error {
	if len(args) > 1 && args[1] == "demo" {
		return runDemo(ctx, out)
	}
	return runServer(ctx, getenv, serve)
}

func runServer(ctx context.Context, getenv envFunc, serve serveFunc) error {
	cfg, err := config.FromEnv(config.LookupFunc(getenv))
	if err != nil {
		return err
	}
	core, err := newConfiguredCore(cfg)
	if err != nil {
		return err
	}
	gw, err := newAppGateway(core, gateway.RoleAuthorizer{}, jobs.NewStore())
	if err != nil {
		return err
	}
	handler, err := buildHandler(cfg, gw)
	if err != nil {
		return err
	}
	srv := platformserver.New(platformserver.Config{
		Addr:              cfg.Addr,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		ShutdownTimeout:   cfg.ShutdownTimeout,
	}, handler)
	log.Printf("workspace-brain listening on %s", cfg.Addr)
	return serve(ctx, srv, cfg)
}

func defaultNewAppGateway(core brainapi.Core, authorizer gateway.Authorizer, store *jobs.Store) (appGateway, error) {
	return gateway.New(core, authorizer, store, gateway.WithSourceLoader(sources.NewLoader()))
}

func defaultNewAppCore(cfg config.Config) (demoCore, error) {
	if cfg.DataPath == "" {
		return memory.New(), nil
	}
	return memory.NewPersistent(filepath.Join(cfg.DataPath, "memory.json"))
}

func newConfiguredCore(cfg config.Config) (demoCore, error) {
	if cfg.DataPath != "" {
		if err := os.MkdirAll(cfg.DataPath, 0o700); err != nil {
			return nil, fmt.Errorf("prepare DATA_PATH: %w", err)
		}
	}
	return newAppCore(cfg)
}

func buildHandler(cfg config.Config, gw appGateway) (http.Handler, error) {
	mux := http.NewServeMux()
	opsHandler := ops.NewHandler(readinessChecker(cfg))
	mux.Handle(ops.HealthPath, opsHandler)
	mux.Handle(ops.ReadinessPath, opsHandler)
	if cfg.SlackSigningSecret != "" {
		slackHandler := slackadapter.NewHandler(gw, cfg.SlackSigningSecret, cfg.AdminUserSet())
		slackHandler.CommandName = cfg.CommandName
		mux.Handle("/slack/commands", slackHandler)
		mux.Handle("/slack/interactions", http.HandlerFunc(slackHandler.ServeInteraction))
		if cfg.PublicBaseURL != "" {
			manifestHandler, err := slackManifestHandler(slackadapter.ManifestConfig{
				AppName:          cfg.SlackAppName,
				CommandName:      cfg.CommandName,
				CommandURL:       cfg.PublicBaseURL + "/slack/commands",
				InteractivityURL: cfg.PublicBaseURL + "/slack/interactions",
			})
			if err != nil {
				return nil, err
			}
			mux.Handle("/slack/manifest.json", manifestHandler)
		}
	}
	if cfg.APIToken != "" {
		mux.Handle("/api/commands", httpapi.NewHandler(gw, cfg.APIToken))
	}
	return mux, nil
}

func readinessChecker(cfg config.Config) ops.ReadinessChecker {
	if !cfg.ReadinessRequired {
		return nil
	}
	return ops.ReadinessCheck(func(context.Context) error {
		if cfg.DataPath == "" {
			return nil
		}
		info, err := os.Stat(cfg.DataPath)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("DATA_PATH is not a directory")
		}
		return nil
	})
}

func serveHTTPServer(ctx context.Context, srv *http.Server, cfg config.Config) error {
	return platformserver.Serve(ctx, srv, cfg.ShutdownTimeout)
}

func runDemo(ctx context.Context, out io.Writer) error {
	core, err := newAppCore(config.Config{})
	if err != nil {
		return err
	}
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
	ingest, err := gw.Ingest(ctx, gateway.IngestCommand{BindingKey: "demo:project:default", Principal: member, Source: brainapi.SourceRef{URI: "file://demo.md", Name: "demo.md", MimeType: "text/markdown"}, Metadata: map[string]string{"content": "workspace-brain은 테넌트별 지식을 수집하고 근거 기반 응답을 제공하는 로컬 RAG 시스템입니다."}})
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

func slackManifestHandler(cfg slackadapter.ManifestConfig) (http.Handler, error) {
	body, err := cfg.ManifestJSON()
	if err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "GET만 지원합니다.", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}), nil
}
