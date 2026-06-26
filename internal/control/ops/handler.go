// Package ops exposes operational health and readiness HTTP handlers.
package ops

import (
	"context"
	"encoding/json"
	"net/http"
)

// ReadinessChecker reports whether an external dependency is ready to serve traffic.
type ReadinessChecker interface {
	Ready(ctx context.Context) error
}

// ReadinessCheck adapts a function into a ReadinessChecker.
type ReadinessCheck func(context.Context) error

// Ready calls f(ctx).
func (f ReadinessCheck) Ready(ctx context.Context) error {
	return f(ctx)
}

// Default operational endpoint paths.
const (
	HealthPath    = "/healthz"
	ReadinessPath = "/readyz"
)

// Handler serves JSON health and readiness endpoints.
type Handler struct {
	checker ReadinessChecker
}

// NewHandler creates an operational handler with an optional readiness checker.
func NewHandler(checker ReadinessChecker) *Handler {
	return &Handler{checker: checker}
}

// ServeHTTP routes the default operational endpoints.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case HealthPath:
		h.Health(w, r)
	case ReadinessPath:
		h.Readiness(w, r)
	default:
		writeJSON(w, http.StatusNotFound, statusResponse{Status: "error", Error: "endpoint not found"})
	}
}

// Health returns a liveness response. It does not check dependencies.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

// Readiness returns a readiness response after checking configured dependencies.
func (h *Handler) Readiness(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	if h == nil || h.checker == nil {
		writeJSON(w, http.StatusOK, statusResponse{Status: "ready"})
		return
	}
	if err := h.checker.Ready(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, statusResponse{Status: "not_ready", Error: "dependency readiness check failed"})
		return
	}
	writeJSON(w, http.StatusOK, statusResponse{Status: "ready"})
}

type statusResponse struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

func allowMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	writeJSON(w, http.StatusMethodNotAllowed, statusResponse{Status: "error", Error: "method not allowed"})
	return false
}

func writeJSON(w http.ResponseWriter, status int, payload statusResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
