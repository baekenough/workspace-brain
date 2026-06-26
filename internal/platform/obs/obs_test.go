package obs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sangyi/workspace-brain/internal/platform/obs"
)

// ── Logger ────────────────────────────────────────────────────────────────────

func TestNewLogger_JSONProducesJSONRecord(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := obs.NewLogger(&buf, "json")
	l.Info("hello", "key", "val")
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("output is not valid JSON: %v — got: %s", err, buf.String())
	}
	if m["msg"] != "hello" {
		t.Fatalf("msg=%v want hello", m["msg"])
	}
}

func TestNewLogger_TextProducesTextRecord(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := obs.NewLogger(&buf, "text")
	l.Info("hello", "key", "val")
	out := buf.String()
	if !strings.Contains(out, "hello") {
		t.Fatalf("text output does not contain 'hello': %s", out)
	}
	// text handler must NOT produce a top-level JSON object.
	if len(out) > 0 && out[0] == '{' {
		t.Fatalf("text handler produced JSON output: %s", out)
	}
}

// ── Context helpers ───────────────────────────────────────────────────────────

func TestContextRequestID_StoreAndRetrieve(t *testing.T) {
	t.Parallel()
	ctx := obs.ContextWithRequestID(context.Background(), "req-abc")
	if got := obs.RequestIDFromContext(ctx); got != "req-abc" {
		t.Fatalf("got %q, want req-abc", got)
	}
}

func TestContextRequestID_AbsentReturnsEmpty(t *testing.T) {
	t.Parallel()
	if got := obs.RequestIDFromContext(context.Background()); got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

// ── DefaultIDGenerator ────────────────────────────────────────────────────────

func TestDefaultIDGenerator_NonEmptyAndUnique(t *testing.T) {
	t.Parallel()
	id1 := obs.DefaultIDGenerator()
	id2 := obs.DefaultIDGenerator()
	if id1 == "" {
		t.Fatal("expected non-empty ID")
	}
	if id1 == id2 {
		t.Fatalf("expected unique IDs; both = %q", id1)
	}
}

// ── CorrelationMiddleware ─────────────────────────────────────────────────────

// runCorrMiddleware sends a GET request through the middleware and returns the
// recorder and the context captured by the inner handler.
func runCorrMiddleware(t *testing.T, mw func(http.Handler) http.Handler, path, headerID string) (*httptest.ResponseRecorder, context.Context) {
	t.Helper()
	var capturedCtx context.Context
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedCtx = r.Context()
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if headerID != "" {
		req.Header.Set("X-Request-ID", headerID)
	}
	rec := httptest.NewRecorder()
	mw(inner).ServeHTTP(rec, req)
	return rec, capturedCtx
}

func TestCorrelationMiddleware_PropagatesHeaderID(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	gen := obs.IDGenerator(func() string { return "should-not-be-called" })

	rec, ctx := runCorrMiddleware(t,
		obs.CorrelationMiddleware(logger, gen),
		"/ping", "client-id-42",
	)

	if got := rec.Header().Get("X-Request-ID"); got != "client-id-42" {
		t.Fatalf("X-Request-ID=%q, want client-id-42", got)
	}
	if got := obs.RequestIDFromContext(ctx); got != "client-id-42" {
		t.Fatalf("context request_id=%q, want client-id-42", got)
	}
	if !strings.Contains(buf.String(), "client-id-42") {
		t.Fatalf("log output missing request_id: %s", buf.String())
	}
}

func TestCorrelationMiddleware_GeneratesIDWhenHeaderAbsent(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	gen := obs.IDGenerator(func() string { return "generated-99" })

	rec, ctx := runCorrMiddleware(t,
		obs.CorrelationMiddleware(logger, gen),
		"/ping", "",
	)

	if got := rec.Header().Get("X-Request-ID"); got != "generated-99" {
		t.Fatalf("X-Request-ID=%q, want generated-99", got)
	}
	if got := obs.RequestIDFromContext(ctx); got != "generated-99" {
		t.Fatalf("context request_id=%q, want generated-99", got)
	}
}

func TestCorrelationMiddleware_NilGeneratorUsesDefault(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	// Passing nil gen triggers the DefaultIDGenerator fallback.
	rec, ctx := runCorrMiddleware(t,
		obs.CorrelationMiddleware(logger, nil),
		"/ping", "",
	)

	id := rec.Header().Get("X-Request-ID")
	if id == "" {
		t.Fatal("expected non-empty generated request ID when gen is nil")
	}
	if got := obs.RequestIDFromContext(ctx); got != id {
		t.Fatalf("context request_id=%q, want %q", got, id)
	}
}

// ── Counters & CountingMiddleware ─────────────────────────────────────────────

func TestNewCounters_SingletonWithNonNilFields(t *testing.T) {
	t.Parallel()
	c1 := obs.NewCounters()
	c2 := obs.NewCounters()
	if c1 == nil {
		t.Fatal("expected non-nil Counters")
	}
	if c1.HTTPRequests == nil {
		t.Fatal("HTTPRequests must not be nil")
	}
	if c1.IngestJobs == nil {
		t.Fatal("IngestJobs must not be nil")
	}
	if c1 != c2 {
		t.Fatal("NewCounters must return the same singleton on repeated calls")
	}
}

func TestCountingMiddleware_IncrementsHTTPRequests(t *testing.T) {
	t.Parallel()
	c := obs.NewCounters()
	before := c.HTTPRequests.Value()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mw := obs.CountingMiddleware(c)(inner)

	const n = 3
	for i := 0; i < n; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)
	}

	if got := c.HTTPRequests.Value(); got != before+n {
		t.Fatalf("HTTPRequests=%d, want %d", got, before+n)
	}
}

// ── expvar Handler ────────────────────────────────────────────────────────────

func TestHandler_Returns200WithJSONBody(t *testing.T) {
	t.Parallel()
	h := obs.Handler()
	if h == nil {
		t.Fatal("expected non-nil handler")
	}
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type=%q, want application/json; charset=utf-8", ct)
	}
	body := rec.Body.Bytes()
	if len(body) == 0 || body[0] != '{' {
		t.Fatalf("expected JSON object body, got: %s", body)
	}
}
