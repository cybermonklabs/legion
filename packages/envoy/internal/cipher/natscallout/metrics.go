package natscallout

import (
	"time"

	"github.com/sjawhar/envoy/internal/metrics"
)

// Metrics counts the service's decisions: broker_nats_callout_decisions_total by credential kind,
// outcome and reason, which is the production census of the services the hub admits (its
// kind="service" series), and broker_nats_callout_seconds, how long each decision took from the
// request's arrival; broker_nats_callout_dropped_total, the requests it answered not at all, by
// reason (drops); and broker_nats_callout_hold_unknown, whether the last reading of the NATS writer
// hold's row was unknown (HoldView), which alerts while every client but the operator is refused.
type Metrics struct {
	decisions   *metrics.Counter
	seconds     *metrics.Histogram
	drops       *metrics.Counter
	holdUnknown *metrics.Gauge
}

// NewMetrics registers the service's metrics on registry.
func NewMetrics(registry *metrics.Registry) *Metrics {
	return &Metrics{
		decisions: registry.NewCounter("broker_nats_callout_decisions_total",
			"NATS connections the auth service decided, by credential kind (session, service, none), outcome (admitted, refused) and the refusal's reason."),
		seconds: registry.NewHistogram("broker_nats_callout_seconds",
			"How long the auth service took to answer one NATS connection from the request's arrival, in seconds. Its buckets "+
				"straddle the decision's 1.5 s budget and the two seconds a client's connect allows.",
			[]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 1.5, 2}),
		drops: registry.NewCounter("broker_nats_callout_dropped_total",
			"Authorization requests the auth service dropped unanswered, whose clients the hub refuses at its own timeout, by reason: "+
				"past_bounds (the replica already held all it bounds), hub_gave_up (the hub had stopped waiting, by its own clock), "+
				"unopened (not sealed to the service's curve key), not_a_request (no authorization request JWT) and pending_limit "+
				"(dropped by the NATS client on arrival, past the subscription's pending limits)."),
		holdUnknown: registry.NewGauge("broker_nats_callout_hold_unknown",
			"1 while the auth service's last reading of the NATS writer hold's row was unknown (no row, a row that does not verify, "+
				"a gen below the one already read, a phase outside the three, or a failed read), when every client but the operator "+
				"is refused hold_unknown; 0 otherwise, and 0 on a broker that does not read the hold (BROKER_NATS_HOLD_READ false)."),
	}
}

// dropped counts n drops for reason, one of the drop reason labels (dropLabels).
func (m *Metrics) dropped(reason string, n int64) {
	if m == nil {
		return
	}
	m.drops.Add(n, [2]string{"reason", reason})
}

// holdReading records whether the last reading of the writer hold's row was unknown.
func (m *Metrics) holdReading(unknown bool) {
	if m == nil {
		return
	}
	value := int64(0)
	if unknown {
		value = 1
	}
	m.holdUnknown.Set(value)
}

// record counts one answer to a request whose credential is kind, refused by refused when it is
// not nil, which took took from the request's arrival. The reason label is the refusal's own
// (Refusal.Label), one fixed word per kind of refusal, never read out of its text.
func (m *Metrics) record(kind string, refused *Refusal, took time.Duration) {
	if m == nil {
		return
	}
	outcome, reason := "admitted", ""
	if refused != nil {
		outcome, reason = "refused", refused.Label
	}
	m.decisions.Inc([2]string{"kind", kind}, [2]string{"outcome", outcome}, [2]string{"reason", reason})
	m.seconds.Observe(took.Seconds())
}
