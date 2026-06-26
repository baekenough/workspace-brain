package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/sangyi/workspace-brain/internal/control/gateway"
	"github.com/sangyi/workspace-brain/internal/control/httpapi"
	"github.com/sangyi/workspace-brain/internal/control/ingest"
	"github.com/sangyi/workspace-brain/internal/control/jobs"
	"github.com/sangyi/workspace-brain/internal/control/ops"
	slackadapter "github.com/sangyi/workspace-brain/internal/control/slack"
	"github.com/sangyi/workspace-brain/internal/control/sources"
	"github.com/sangyi/workspace-brain/internal/core/ai"
	"github.com/sangyi/workspace-brain/internal/core/memory"
	"github.com/sangyi/workspace-brain/internal/platform/config"
	"github.com/sangyi/workspace-brain/internal/platform/obs"
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
	// Configure structured logging for the lifetime of this server run.
	// LOG_FORMAT=json selects JSON output; anything else uses text (default).
	slog.SetDefault(obs.NewLogger(os.Stderr, getenv("LOG_FORMAT")))

	core, err := newConfiguredCore(cfg)
	if err != nil {
		return err
	}
	jobStore := jobs.NewStore()
	gw, err := newAppGateway(core, gateway.RoleAuthorizer{}, jobStore)
	if err != nil {
		return err
	}

	// Wire the async ingest-completion worker when using the real gateway
	// implementation.  The type assertion succeeds for defaultNewAppGateway;
	// tests that override newAppGateway with a stub are unaffected.
	concreteGW := gw.(*gateway.Gateway)
	worker := ingest.NewWorker(core, concreteGW, 64)
	worker.Start()
	defer worker.Stop()
	concreteGW.SetQueue(worker)

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
	slog.Info("workspace-brain listening", "addr", cfg.Addr)
	return serve(ctx, srv, cfg)
}

func defaultNewAppGateway(core brainapi.Core, authorizer gateway.Authorizer, store *jobs.Store) (appGateway, error) {
	return gateway.New(core, authorizer, store, gateway.WithSourceLoader(sources.NewLoader()))
}

// openAIEmbeddingDimensions is the MRL target dimension for text-embedding-3-*
// models. Vectors longer than this are truncated; shorter vectors are an error.
const openAIEmbeddingDimensions = 1024

// openAISynthesizerBridge adapts an ai.Responder to the memory.Synthesizer
// interface. The bridge lives in the composition root (cmd/main.go) so that
// internal/core/memory never imports internal/core/ai.
//
// Grounded spans are the raw retrieved passages. The model's synthesised output
// is reported as a supplemented span, representing content the model generated
// beyond direct quotation of the sources.
type openAISynthesizerBridge struct {
	responder ai.Responder
}

// Synthesize implements memory.Synthesizer. It builds a Korean prompt from
// the retrieved passages and the question, calls the Responder, and
// classifies the model output as a supplemented span while listing all
// input passages as grounded spans. An empty inputs slice returns an
// abstention without calling the Responder.
func (b *openAISynthesizerBridge) Synthesize(ctx context.Context, question string, inputs []memory.SynthesisInput) (memory.SynthesisResult, error) {
	if len(inputs) == 0 {
		return memory.SynthesisResult{Answer: "수집된 근거가 없습니다: " + question}, nil
	}
	var sb strings.Builder
	sb.WriteString("다음 내용을 기반으로 질문에 답변하시오.\n\n")
	for i, inp := range inputs {
		fmt.Fprintf(&sb, "[%d] %s\n", i+1, inp.Text)
	}
	fmt.Fprintf(&sb, "\n질문: %s\n답변:", question)

	answer, err := b.responder.Respond(ctx, sb.String())
	if err != nil {
		return memory.SynthesisResult{}, err
	}

	groundedSpans := make([]string, len(inputs))
	for i, inp := range inputs {
		groundedSpans[i] = inp.Text
	}
	return memory.SynthesisResult{
		Answer:            answer,
		GroundedSpans:     groundedSpans,
		SupplementedSpans: []string{answer},
	}, nil
}

// compile-time check: openAISynthesizerBridge satisfies memory.Synthesizer.
var _ memory.Synthesizer = (*openAISynthesizerBridge)(nil)

// openAIEmbedderBridge adapts an *ai.OpenAIClient to the memory.Embedder
// interface. The bridge is placed in the composition root so that
// internal/core/memory never imports internal/core/ai.
//
// On any error from the OpenAI API, Embed returns a zero vector of length dims
// so the embedding call always produces a valid (if uninformative) vector.
type openAIEmbedderBridge struct {
	client *ai.OpenAIClient
	dims   int
}

// Embed implements memory.Embedder. It calls the OpenAI /embeddings endpoint
// with a background context and returns the first embedding vector. On error
// or an empty response it returns a zero vector.
func (b *openAIEmbedderBridge) Embed(text string) []float64 {
	vecs, err := b.client.Embed(context.Background(), []string{text})
	if err != nil || len(vecs) == 0 {
		return make([]float64, b.dims)
	}
	return vecs[0]
}

// openAIEmbedderOption returns a memory.WithEmbedder option wired to the real
// OpenAI embedding endpoint when both OPENAI_API_KEY and OPENAI_EMBEDDING_MODEL
// are configured. It returns nil (no option, local FNV embedder used) otherwise.
func openAIEmbedderOption(cfg config.Config) []memory.Option {
	if cfg.OpenAIAPIKey == "" || cfg.OpenAIEmbeddingModel == "" {
		return nil
	}
	client, _ := ai.NewOpenAIClient(ai.OpenAIConfig{
		BaseURL:             cfg.OpenAIBaseURL,
		APIKey:              cfg.OpenAIAPIKey,
		EmbeddingModel:      cfg.OpenAIEmbeddingModel,
		OrganizationID:      cfg.OpenAIOrganizationID,
		ProjectID:           cfg.OpenAIProjectID,
		EmbeddingDimensions: openAIEmbeddingDimensions,
	})
	return []memory.Option{memory.WithEmbedder(&openAIEmbedderBridge{client: client, dims: openAIEmbeddingDimensions})}
}

func defaultNewAppCore(cfg config.Config) (demoCore, error) {
	opts := openAIEmbedderOption(cfg)
	if cfg.DataPath == "" {
		return memory.New(opts...), nil
	}
	return memory.NewPersistent(filepath.Join(cfg.DataPath, "memory.json"), opts...)
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
	mux.Handle(ops.MetricsPath, opsHandler)
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

	// Wrap the mux with operational middleware.
	// CountingMiddleware increments http_requests_total on every request.
	// CorrelationMiddleware propagates or generates X-Request-ID and logs
	// each request with the active slog logger, threading the correlation ID
	// into the request context so downstream handlers can attach it to records.
	counters := obs.NewCounters()
	var h http.Handler = mux
	h = obs.CountingMiddleware(counters)(h)
	h = obs.CorrelationMiddleware(slog.Default(), nil)(h)
	return h, nil
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
