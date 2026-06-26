package obs

import (
	"expvar"
	"net/http"
	"sync"
)

// Counters holds named expvar integers for operational metrics.
// Use [NewCounters] to obtain the process-wide singleton.
type Counters struct {
	// HTTPRequests is incremented for each HTTP request received.
	HTTPRequests *expvar.Int
	// IngestJobs is incremented each time an ingest job is accepted.
	IngestJobs *expvar.Int
}

var (
	defaultCounters *Counters
	countersOnce    sync.Once
)

// NewCounters returns the process-wide [Counters] singleton, registering
// expvar variables on the first call. Subsequent calls return the same
// pointer without re-registering.
func NewCounters() *Counters {
	countersOnce.Do(func() {
		defaultCounters = &Counters{
			HTTPRequests: expvar.NewInt("http_requests_total"),
			IngestJobs:   expvar.NewInt("ingest_jobs_total"),
		}
	})
	return defaultCounters
}

// Handler returns the standard expvar HTTP handler, suitable for mounting
// at a /metrics endpoint.
func Handler() http.Handler {
	return expvar.Handler()
}

// CountingMiddleware returns an HTTP middleware that increments
// counters.HTTPRequests for every request it receives.
// counters must not be nil.
func CountingMiddleware(counters *Counters) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counters.HTTPRequests.Add(1)
			next.ServeHTTP(w, r)
		})
	}
}
