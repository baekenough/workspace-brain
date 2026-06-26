package ops

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestServeHTTPRoutesDefaultEndpoints(t *testing.T) {
	t.Parallel()
	h := NewHandler(ReadinessCheck(func(context.Context) error { return nil }))

	for _, tt := range []struct {
		name string
		path string
		want statusResponse
	}{
		{name: "health", path: HealthPath, want: statusResponse{Status: "ok"}},
		{name: "readiness", path: ReadinessPath, want: statusResponse{Status: "ready"}},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := performPath(h, http.MethodGet, tt.path)
			assertResponse(t, rec, http.StatusOK, tt.want)
		})
	}
}

func TestServeHTTPRejectsUnknownEndpoint(t *testing.T) {
	t.Parallel()
	h := NewHandler(nil)
	rec := performPath(h, http.MethodGet, "/metrics")
	assertResponse(t, rec, http.StatusNotFound, statusResponse{Status: "error", Error: "endpoint not found"})
}

func TestHealth(t *testing.T) {
	t.Parallel()
	h := NewHandler(failingChecker{})
	rec := perform(http.HandlerFunc(h.Health), http.MethodGet)
	assertResponse(t, rec, http.StatusOK, statusResponse{Status: "ok"})
}

func TestHealthRejectsUnsupportedMethod(t *testing.T) {
	t.Parallel()
	h := NewHandler(nil)
	rec := perform(http.HandlerFunc(h.Health), http.MethodPost)
	assertResponse(t, rec, http.StatusMethodNotAllowed, statusResponse{Status: "error", Error: "method not allowed"})
	if got := rec.Header().Get("Allow"); got != http.MethodGet {
		t.Fatalf("Allow=%q want=%q", got, http.MethodGet)
	}
}

func TestReadinessWithoutCheckerIsReady(t *testing.T) {
	t.Parallel()
	h := NewHandler(nil)
	rec := perform(http.HandlerFunc(h.Readiness), http.MethodGet)
	assertResponse(t, rec, http.StatusOK, statusResponse{Status: "ready"})
}

func TestNilHandlerReadinessIsReady(t *testing.T) {
	t.Parallel()
	var h *Handler
	rec := perform(http.HandlerFunc(h.Readiness), http.MethodGet)
	assertResponse(t, rec, http.StatusOK, statusResponse{Status: "ready"})
}

func TestReadinessWithReadyChecker(t *testing.T) {
	t.Parallel()
	called := false
	h := NewHandler(ReadinessCheck(func(ctx context.Context) error {
		called = true
		if ctx == nil {
			t.Fatal("context is nil")
		}
		return nil
	}))
	rec := perform(http.HandlerFunc(h.Readiness), http.MethodGet)
	assertResponse(t, rec, http.StatusOK, statusResponse{Status: "ready"})
	if !called {
		t.Fatal("checker was not called")
	}
}

func TestReadinessWithFailedChecker(t *testing.T) {
	t.Parallel()
	h := NewHandler(failingChecker{})
	rec := perform(http.HandlerFunc(h.Readiness), http.MethodGet)
	assertResponse(t, rec, http.StatusServiceUnavailable, statusResponse{Status: "not_ready", Error: "dependency readiness check failed"})
}

func TestReadinessRejectsUnsupportedMethodWithoutCallingChecker(t *testing.T) {
	t.Parallel()
	checker := &countingChecker{}
	h := NewHandler(checker)
	rec := perform(http.HandlerFunc(h.Readiness), http.MethodPut)
	assertResponse(t, rec, http.StatusMethodNotAllowed, statusResponse{Status: "error", Error: "method not allowed"})
	if checker.calls != 0 {
		t.Fatalf("checker calls=%d want=0", checker.calls)
	}
}

func perform(h http.Handler, method string) *httptest.ResponseRecorder {
	return performPath(h, method, "/")
}

func performPath(h http.Handler, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func assertResponse(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, want statusResponse) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, wantStatus, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type=%q", got)
	}
	var got statusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got != want {
		t.Fatalf("response=%+v want=%+v", got, want)
	}
}

type failingChecker struct{}

func (failingChecker) Ready(context.Context) error {
	return errors.New("database unavailable")
}

type countingChecker struct {
	calls int
}

func (c *countingChecker) Ready(context.Context) error {
	c.calls++
	return nil
}
