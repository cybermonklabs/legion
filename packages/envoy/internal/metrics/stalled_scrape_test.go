package metrics

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testwait"
)

// stalledResponse is a scrape's response whose writes stop at the first one that carries at, as a
// scrape client that stops reading stops the metrics server's writes once the socket's buffers are
// full: the server's write then waits on the client, until the server's write timeout where it
// sets one. entered is closed when a write has stopped, and closing released lets every write
// through.
type stalledResponse struct {
	at       []byte
	header   http.Header
	entered  chan struct{}
	once     sync.Once
	released chan struct{}
}

func (s *stalledResponse) Header() http.Header { return s.header }

func (s *stalledResponse) WriteHeader(int) {}

func (s *stalledResponse) Write(p []byte) (int, error) {
	if bytes.Contains(p, s.at) {
		s.once.Do(func() { close(s.entered) })
		<-s.released
	}
	return len(p), nil
}

// A scrape whose client stops reading blocks nothing that records. In each row the scrape's write
// stops on the line of one metric's series, and that metric is then recorded to, or a metric
// registered, which must return at once. A handler that held the metric's lock, or the registry's,
// across its write to the scrape would hold the recording while that write waited on the client:
// on Dispatch, which sets no write timeout, as long as the client did not read, and on the
// listener and the broker up to their write timeouts (10 s and 60 s). All three record as they
// answer requests and serve this handler.
func TestAStalledScrapeBlocksNoRecording(t *testing.T) {
	r := New()
	counter := r.NewCounter("envoy_test_answers_total", "Answers the test counts")
	counter.Inc([2]string{"outcome", "refused"})
	histogram := r.NewHistogram("envoy_test_seconds", "Seconds the test observes", []float64{0.1, 1})
	histogram.Observe(0.05, [2]string{"kind", "session"})

	for _, row := range []struct {
		name   string
		at     string
		record func()
	}{
		{"a counter's existing series", `envoy_test_answers_total{outcome="refused"}`, func() { counter.Inc([2]string{"outcome", "refused"}) }},
		{"a counter's new series", `envoy_test_answers_total{outcome="refused"}`, func() { counter.Inc([2]string{"outcome", "admitted"}) }},
		{"a histogram's existing series", `envoy_test_seconds_bucket{kind="session",le="0.1"}`, func() { histogram.Observe(0.5, [2]string{"kind", "session"}) }},
		{"a histogram's new series", `envoy_test_seconds_bucket{kind="session",le="0.1"}`, func() { histogram.Observe(0.5, [2]string{"kind", "service"}) }},
		{"a new metric", `envoy_test_answers_total{outcome="refused"}`, func() { r.NewCounter("envoy_test_late_total", "A metric registered while a scrape stalls") }},
	} {
		t.Run(row.name, func(t *testing.T) {
			stalled := &stalledResponse{at: []byte(row.at), header: http.Header{}, entered: make(chan struct{}), released: make(chan struct{})}
			scraped := make(chan struct{})
			go func() {
				defer close(scraped)
				r.Handler().ServeHTTP(stalled, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			}()
			t.Cleanup(func() {
				close(stalled.released)
				<-scraped
			})
			select {
			case <-stalled.entered:
			case <-time.After(testwait.Bounded(t, time.Minute, time.Second)):
				t.Fatalf("the scrape never wrote %s", row.at)
			}

			// The scrape stays stalled until the row's cleanup, so a recording that waited on it
			// never returns: the bound only ends a run that would otherwise hang, and a slow
			// machine is no failure.
			done := make(chan struct{})
			go func() {
				defer close(done)
				row.record()
			}()
			select {
			case <-done:
			case <-time.After(testwait.Bounded(t, time.Minute, time.Second)):
				t.Fatalf("recording %s waited on the stalled scrape", row.name)
			}
		})
	}
}
