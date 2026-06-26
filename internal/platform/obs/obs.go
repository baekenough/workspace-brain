// Package obs provides structured logging and HTTP request-correlation
// infrastructure for workspace-brain services.
//
// Logging: NewLogger creates a [*slog.Logger] with a JSON or text handler
// selected by the LOG_FORMAT environment variable ("json" → JSON, anything
// else → text).
//
// Correlation IDs: CorrelationMiddleware reads X-Request-ID from incoming
// requests (or generates one via an injectable IDGenerator) and propagates
// the ID through the request context and the X-Request-ID response header.
// Use RequestIDFromContext to retrieve the ID downstream.
package obs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
)

// contextKey is an unexported type used to namespace context values.
type contextKey int

const reqIDKey contextKey = 0

// IDGenerator generates a unique request-ID string.
// Implementations must be safe for concurrent use.
type IDGenerator func() string

// DefaultIDGenerator produces a cryptographically random 16-hex-char ID.
func DefaultIDGenerator() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NewLogger returns a [*slog.Logger] that writes structured log records to w.
// format "json" selects [slog.JSONHandler]; any other value selects
// [slog.TextHandler].
func NewLogger(w io.Writer, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// ContextWithRequestID returns a copy of ctx carrying id.
func ContextWithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, reqIDKey, id)
}

// RequestIDFromContext returns the request ID stored in ctx, or "" if absent.
func RequestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(reqIDKey).(string)
	return v
}

// CorrelationMiddleware returns an HTTP middleware that:
//   - reads X-Request-ID from the incoming request header, or calls gen to
//     produce a new ID when the header is absent;
//   - writes the ID to the X-Request-ID response header;
//   - stores the ID in the request context via [ContextWithRequestID]; and
//   - emits a structured Info log record via logger.
//
// gen is the ID generator; pass nil to use [DefaultIDGenerator].
func CorrelationMiddleware(logger *slog.Logger, gen IDGenerator) func(http.Handler) http.Handler {
	if gen == nil {
		gen = DefaultIDGenerator
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-ID")
			if id == "" {
				id = gen()
			}
			w.Header().Set("X-Request-ID", id)
			ctx := ContextWithRequestID(r.Context(), id)
			logger.InfoContext(ctx, "http request",
				"request_id", id,
				"method", r.Method,
				"path", r.URL.Path,
			)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
