package natscallout

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// stalledScrape is a scrape's response whose gate holds the write that carries the line of the
// metric name's series: a scrape client that stops reading holds the metrics server's writes once
// the socket's buffers are full, for up to the server's write timeout where it sets one.
type stalledScrape struct {
	*writeGate
	name   string
	header http.Header
}

func (s *stalledScrape) Header() http.Header { return s.header }

func (s *stalledScrape) WriteHeader(int) {}

func (s *stalledScrape) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(s.name+"{")) {
		s.hold()
	}
	return len(p), nil
}

// stallScrape starts one scrape of registry and holds it at the decision counter's first series
// line, once a first decision has made that series. It releases the scrape before the service's
// own cleanup closes the service, which waits for every request. server is the hub the service
// answers, started with its debug log on (startDebugCalloutServer), as requireRefusedByTheService
// reads.
func stallScrape(t *testing.T, server *testnats.Server, registry *metrics.Registry) {
	t.Helper()
	requireRefusedByTheService(t, server, "first")
	testwait.WithinPolling(t, 5*time.Second, 20*time.Millisecond, "the first refusal recorded", func() bool {
		return strings.Contains(scrape(t, registry), "broker_nats_callout_decisions_total{")
	})
	stalled := &stalledScrape{writeGate: newWriteGate(), name: "broker_nats_callout_decisions_total", header: http.Header{}}
	t.Cleanup(stalled.release)
	go registry.Handler().ServeHTTP(stalled, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	stalled.awaitHeld(t, "the decision counter's series in the scrape")
}

// A scrape client that stops reading holds no decision slot. A request decided in a slot keeps it
// until answer returns, and answer records the decision (Metrics.record) after it sends it; were
// the registry to hold the decision counter's lock across its write to a scrape, as its handler
// once did, a stopped scrape would hold every request recording behind it in its slot.
// metrics.Registry.Handler renders the text under its locks and writes it holding none. With one
// slot and a scrape stopped on the decision counter's line, a connection holding no credential is
// refused in the slot, and a live session connecting after it must be admitted, with the replica
// then holding nothing.
func TestAStalledScrapeHoldsNoDecisionSlot(t *testing.T) {
	withDecisionSlots(t, 1)
	keys := testnats.NewCalloutKeys(t)
	server, url := startDebugCalloutServer(t, keys)
	live := newSession(t, "ses_live", 15*time.Minute)
	registry := metrics.New()
	service := startService(t, url, keys, Deps{Introspect: introspector(live), Metrics: NewMetrics(registry)}, Decide)
	stallScrape(t, server, registry)

	requireRefusedByTheService(t, server, "second")
	requireLiveAdmitted(t, url, live, "a refusal recording behind the stopped scrape")
	requireHoldsNothing(t, service)
}

// The same at the deployed number of slots: once concurrentDecisions requests were held recording
// behind a stopped scrape, no caller, however live its credential, would be decided. More than
// concurrentDecisions connections holding no credential are each refused by the service in a slot
// while a scrape is stopped on the decision counter's line, and then a live session is admitted,
// with the replica holding nothing.
func TestAStalledScrapeHoldsNoneOfTheDeployedSlots(t *testing.T) {
	keys := testnats.NewCalloutKeys(t)
	server, url := startDebugCalloutServer(t, keys)
	live := newSession(t, "ses_live", 15*time.Minute)
	registry := metrics.New()
	service := startService(t, url, keys, Deps{Introspect: introspector(live), Metrics: NewMetrics(registry)}, Decide)
	stallScrape(t, server, registry)

	for i := range concurrentDecisions + 6 {
		requireRefusedByTheService(t, server, fmt.Sprintf("#%d", i+1))
	}
	requireLiveAdmitted(t, url, live, "refusals recording behind the stopped scrape")
	requireHoldsNothing(t, service)
}
