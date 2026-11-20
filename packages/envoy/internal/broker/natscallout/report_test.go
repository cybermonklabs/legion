package natscallout

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// countingHandler counts the records being handled at once, and the most there have been. It
// counts at the handler, not at the writer: slog's text handler holds its own mutex across a
// write, so a second goroutine logging behind a stalled write waits on that mutex and never
// reaches the writer, and a count taken there could never pass one.
type countingHandler struct {
	slog.Handler
	inFlight, most *atomic.Int32
}

func newCountingHandler(inner slog.Handler) countingHandler {
	return countingHandler{Handler: inner, inFlight: &atomic.Int32{}, most: &atomic.Int32{}}
}

func (h countingHandler) Handle(ctx context.Context, record slog.Record) error {
	n := h.inFlight.Add(1)
	defer h.inFlight.Add(-1)
	for most := h.most.Load(); n > most && !h.most.CompareAndSwap(most, n); most = h.most.Load() {
	}
	return h.Handler.Handle(ctx, record)
}

func (h countingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return countingHandler{Handler: h.Handler.WithAttrs(attrs), inFlight: h.inFlight, most: h.most}
}

func (h countingHandler) WithGroup(name string) slog.Handler {
	return countingHandler{Handler: h.Handler.WithGroup(name), inFlight: h.inFlight, most: h.most}
}

// A stalled log grows nothing past its bound: while it takes no write, and once the reporter's
// write is stuck on it, fifty more refusals are each answered at once, no more than one record is
// ever being logged, the reporter's, and what the service keeps for the log meanwhile is a count
// per label and one detail, not a line, a goroutine or a buffer per refusal. Written by the
// goroutine that made it, before its answer or after, each refusal's line would be a record of its
// own waiting on the log, holding its goroutine.
func TestAStalledLogHoldsOneWriteAndCountsTheRest(t *testing.T) {
	withLiveness(t, 10*time.Millisecond, 10*time.Second)
	keys := testnats.NewCalloutKeys(t)
	server, url := startDebugCalloutServer(t, keys)
	log := newStalledLog()
	handler := newCountingHandler(slog.NewTextHandler(log, nil))
	service := startService(t, url, keys, Deps{Log: slog.New(handler)}, Decide)
	t.Cleanup(log.release)
	stallWithALine(t, server, log)

	for range 50 {
		requireRefusedByTheService(t, server, "next")
	}
	time.Sleep(100 * time.Millisecond)
	require.LessOrEqual(t, handler.most.Load(), int32(1), "more than the reporter's one record was logged at once on the stalled log")
	refusals := service.answer.refusals
	refusals.mu.Lock()
	defer refusals.mu.Unlock()
	require.LessOrEqual(t, len(refusals.counts), 1, "the refusals since the last line are kept by more than their one label: %v", refusals.counts)
}

// The drop metric moves while the log is stalled: the counter moves each drop into
// broker_nats_callout_dropped_total on its own goroutine (countWhileRunning), and the reporter,
// blocked on the log by the first drop's line, holds nothing the metric waits for. Both bounds
// are zero, so every request is dropped; the second drop is made once the reporter's write of the
// first is stuck on the log, and reaches the metric all the same.
func TestTheDropMetricMovesWhileTheLogIsStalled(t *testing.T) {
	withWaitingBound(t, 0)
	withRefusingBound(t, 0)
	keys := testnats.NewCalloutKeys(t)
	url := testnats.StartWithCallout(t, keys)
	registry := metrics.New()
	log, _ := startWithStalledLog(t, url, keys, Deps{Metrics: NewMetrics(registry)})
	log.stalled.Store(true)

	for attempt := 1; attempt <= 2; attempt++ {
		go func() {
			if nc, err := nats.Connect(url, nats.NoReconnect(), nats.Timeout(5*time.Second)); err == nil {
				nc.Close()
			}
		}()
		testwait.WithinPolling(t, 5*time.Second, 20*time.Millisecond, fmt.Sprintf("drop %d in the metric with the log stalled", attempt), func() bool {
			return strings.Contains(scrape(t, registry), fmt.Sprintf(`broker_nats_callout_dropped_total{reason="past_bounds"} %d`, attempt))
		})
		if attempt == 1 {
			log.awaitStuck(t)
		}
	}
}

// A closing replica counts what nats.go dropped past the pending limits since the counter's last
// tick: Close reads the subscription's count before it unsubscribes, since a closed subscription
// answers Dropped with an error. The tick is an hour here, so no tick counts them; delivery of the
// first request is held, and the two sent behind it find the one-request limit full.
func TestAClosingReplicaCountsWhatNatsDroppedSinceItsLastTick(t *testing.T) {
	withWaitingBound(t, 0)
	withRefusingBound(t, 0)
	withLiveness(t, time.Hour, 2*time.Hour)
	target, entered, release := holdDeliveries(t)
	keys := testnats.NewCalloutKeys(t)
	url := testnats.StartWithCallout(t, keys)
	registry := metrics.New()
	service := startService(t, url, keys, Deps{Metrics: NewMetrics(registry)}, Decide)
	target.Store(service)

	connect := func() {
		if nc, err := nats.Connect(url, nats.NoReconnect(), nats.Timeout(5*time.Second)); err == nil {
			nc.Close()
		}
	}
	go connect()
	<-entered
	go connect()
	go connect()
	testwait.WithinPolling(t, 5*time.Second, 20*time.Millisecond, "nats.go dropping the two past the limit", func() bool {
		dropped, err := service.sub.Dropped()
		return err == nil && dropped == 2
	})
	release()
	service.Close()
	require.Contains(t, scrape(t, registry), `broker_nats_callout_dropped_total{reason="pending_limit"} 2`, "the drops since the last tick never reached the metric")
}

// Each label keeps its own first detail until the next line: a broker-side cause counted after
// another label's refusal in the same minute is in that line, not dropped behind the other's.
func TestATallyKeepsEachLabelsFirstDetail(t *testing.T) {
	var refusals tally
	refusals.add(labelNoCredential, func() string { return "no credential, from 10.0.0.1" })
	refusals.add(labelServiceFailed, func() string { return "the service failed: introspect: connection refused" })
	refusals.add(labelServiceFailed, func() string { return "a second cause, which is not kept" })
	counts, first, ok := refusals.take(true)
	require.True(t, ok)
	require.Equal(t, map[string]int64{labelNoCredential: 1, labelServiceFailed: 2}, counts)
	require.Equal(t, map[string]string{
		labelNoCredential:  "no credential, from 10.0.0.1",
		labelServiceFailed: "the service failed: introspect: connection refused",
	}, first, "a label's first detail was lost behind another label's")
}
