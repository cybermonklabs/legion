package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/metrics"
)

// getMetrics serves the process's metrics registry (Deps.Metrics) in the Prometheus text format.
// It is a row of the table, so it needs a credential as every row does, and its read class is
// deployment (read_classes.go): the counters name no project, issue, person or session
// (readclass's TestMetricsNameNoProject), and they count the whole deployment's work, which only an
// open viewer reads. A server registered with no registry, a test's, serves an empty one.
func (s *server) getMetrics(w http.ResponseWriter, r *http.Request) {
	registry := s.deps.Metrics
	if registry == nil {
		registry = metrics.New().Handler()
	}
	registry.ServeHTTP(w, r)
}
