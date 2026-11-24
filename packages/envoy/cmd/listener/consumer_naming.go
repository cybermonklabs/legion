package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
)

// fixedNamingRetry is how long a fixed-mode start waits before it reads the selection bucket's
// marker again after a read that answered nothing (awaitFixedNaming).
const fixedNamingRetry = 2 * time.Second

// startConsumerNaming runs the start's steps of the consumer naming mode, once the session
// consumers have connected and before the relay and the durable's bind. In the fixed mode it gates
// the session consumers' connection and the main client, on which the durable is created and bound,
// on the selection bucket's cutover marker (guardFixedNaming), and converted ends the process when a
// reconnect finds the store converted. In the fresh mode it counts the selection steps in
// envoy_session_consumer_names_total and checks the selection bucket once: a refusal there is
// logged, not the start's end, since every session-consumer call refuses until the bucket, its
// sentinel and the cutover are there, and /healthz names what is missing meanwhile. An error ends
// the start.
func startConsumerNaming(host bool, consumers *bus.SessionConsumers, client *bus.Client, met *metrics.Registry, logger *logging.Logger, converted func(error)) error {
	if !consumers.Fresh() {
		gated := []gatedConnection{{name: "session consumers", connection: consumers}, {name: "durable", connection: client}}
		return guardFixedNaming(context.Background(), host, gated, logger, fixedNamingRetry, converted)
	}
	names := met.NewCounter("envoy_session_consumer_names_total", "Session consumer selection steps in the fresh naming mode, by op (select, create, replace, delete, recover, missing_current, last_floor, cleanup) and outcome (ok, adopted, retired, refused, conflict, limit, error)")
	consumers.ObserveNames(func(op consumernames.Operation, outcome consumernames.Outcome) {
		names.Inc([2]string{"op", string(op)}, [2]string{"outcome", string(outcome)})
	})
	checkCtx, cancel := context.WithTimeout(context.Background(), sessionConsumerCallTimeout)
	defer cancel()
	if err := consumers.CheckSelection(checkCtx); err != nil {
		logger.Error("session consumer selection refuses every registration: "+err.Error(),
			slog.String("bucket", consumernames.SessionBucket), slog.String("error", err.Error()))
	}
	return nil
}

// markerGated is a connection with the fixed mode's gate on the selection bucket's cutover marker:
// the session consumers' (bus.SessionConsumers) and the listener's main client, on which its durable
// is created and bound (bus.Client).
type markerGated interface {
	GateOnMarker(converted func(error))
	ReadMarker(ctx context.Context) error
}

// gatedConnection is one connection guardFixedNaming gates, and the name its lines give it.
type gatedConnection struct {
	name       string
	connection markerGated
}

// guardFixedNaming is the fixed mode's guard against running today's names over a selection
// bucket the cutover converted. It shuts each connection's marker gate (bus.Client.GateOnMarker),
// so no consumer call, durable create or bind runs on it until a read of the marker on that very
// connection answers that the cutover did not convert the store, then reads the marker on each
// before the start goes on (awaitFixedNaming). From then on every reconnect shuts the gate and reads
// again, calling converted when the marker says done. A host's listener (ENVOY_LISTENER_ROLE=host)
// skips both and reads nothing: it runs fixed names for good. Its NATS user, the one its host's
// sessions share (natsgrant.Host), holds no read of the bucket, so each read would only have the
// server log a permissions violation at every start and connect; natsgrant.Host is retiring (plan
// E's t-e-59 deletes it), no production connection holds it, and the cutover's window has the
// precondition that no host listener runs at or after it, so a host listener never meets a
// converted bucket and its durable is never converted.
func guardFixedNaming(ctx context.Context, host bool, connections []gatedConnection, logger *logging.Logger, interval time.Duration, converted func(error)) error {
	if host {
		logger.Info("a host's listener runs fixed consumer names for good and reads no cutover marker",
			slog.String("role", bus.ListenerRoleHost), slog.String("bucket", consumernames.SessionBucket))
		return nil
	}
	for _, gated := range connections {
		gated.connection.GateOnMarker(converted)
		if err := awaitFixedNaming(ctx, gated, logger, interval); err != nil {
			return err
		}
	}
	return nil
}

// awaitFixedNaming reads, in the fixed mode, the selection bucket's cutover marker on gated's
// connection before the listener runs today's names there (bus.Client.ReadMarker), which opens
// that connection's marker gate when the store is not converted, and answers:
//   - nil when there is no marker: no bucket, a sentinel whose marker is not done, or a read the
//     connection's grant refuses, which is logged as a warning (the listener beside Dispatch and a
//     listener whose hub never gave it the window's grants read no bucket at all, and neither can
//     hold a converted one);
//   - bus.ErrConvertedStore when the marker says the cutover converted every session consumer, on
//     which the start ends, naming ENVOY_CONSUMER_NAMES;
//   - ctx's error when ctx ends first.
//
// Any other failed read tells nothing, so it is logged and read again every interval: a start that
// cannot read the marker waits rather than run today's names unread. That includes no connection
// and a timeout, and a bucket that exists with no sentinel or with a value under the sentinel's key
// that is not the sentinel, which is never no marker (bus.Client.ReadMarker).
func awaitFixedNaming(ctx context.Context, gated gatedConnection, logger *logging.Logger, interval time.Duration) error {
	for logged := false; ; {
		readCtx, cancel := context.WithTimeout(ctx, sessionConsumerCallTimeout)
		err := gated.connection.ReadMarker(readCtx)
		cancel()
		switch {
		case err == nil:
			return nil
		case consumerGrantRefused(err):
			logger.Warn("the NATS user may not read the session consumers' selection bucket; the fixed naming mode runs, as on a hub that holds no converted bucket",
				slog.String("connection", gated.name), slog.String("bucket", consumernames.SessionBucket), slog.String("error", err.Error()))
			return nil
		case errors.Is(err, bus.ErrConvertedStore):
			return err
		case ctx.Err() != nil:
			return ctx.Err()
		}
		if !logged {
			logged = true
			logger.Warn("the fixed naming mode waits to read the session consumers' selection bucket before it runs today's names",
				slog.String("connection", gated.name), slog.String("bucket", consumernames.SessionBucket), slog.String("error", err.Error()), slog.String("retry", interval.String()))
		}
		if err := waitUnlessDone(ctx, interval); err != nil {
			return err
		}
	}
}

// sentinelRefusal is why err, a read of the selection bucket in the fresh mode, refuses every
// call over a bucket that exists: its sentinel key (consumernames.SentinelKey) holds nothing
// ("absent", consumernames.ErrNoSentinel), a sentinel of another store's family ("another family")
// or a value that is no sentinel of its own ("malformed", both consumernames.ErrMalformedSentinel).
// ok is false for any other answer, a missing bucket and an admitted sentinel among them.
func sentinelRefusal(err error) (reason string, ok bool) {
	switch {
	case errors.Is(err, consumernames.ErrNoSentinel):
		return "absent", true
	case !errors.Is(err, consumernames.ErrMalformedSentinel):
		return "", false
	case errors.Is(err, consumernames.ErrSentinelFamily):
		return "another family", true
	}
	return "malformed", true
}

// startingSelectionHealth is /healthz's answer while the listener has not bound its durable
// (healthzHandler, before main publishes its deps): in the fresh mode, a start whose selection
// state refuses every call binds nothing however long it retries, so the answer is 503 not_ready
// naming the bucket and why, read within ctx: the bucket missing ("missing"), its sentinel absent,
// of another family or malformed (sentinelRefusal), or the cutover not run or rolled back ("cutover
// not done", consumernames.Admit), the last two naming the sentinel's key too. nil with no client
// yet, in the fixed mode, for a read that tells nothing (no answer, a timeout: the bind retries
// it), and over an admitted sentinel.
func startingSelectionHealth(ctx context.Context, client *bus.Client) map[string]interface{} {
	if client == nil || client.ConsumerNames() != consumernames.ModeFresh {
		return nil
	}
	state, err := client.SelectionStore().State(ctx)
	body := map[string]interface{}{
		"status":         "not_ready",
		"bucket":         consumernames.SessionBucket,
		"consumer_names": string(consumernames.ModeFresh),
	}
	switch reason, refused := sentinelRefusal(err); {
	case errors.Is(err, consumernames.ErrNoStore):
		body["reason"] = "missing"
	case refused:
		body["reason"], body["key"] = reason, consumernames.SentinelKey
	case err != nil:
		return nil
	default:
		if err = consumernames.Admit(state); err == nil {
			return nil
		}
		body["reason"], body["key"], body["cutover"] = "cutover not done", consumernames.SentinelKey, state.Cutover
	}
	body["error"] = dependencyUnavailable("session consumer selection bucket "+consumernames.SessionBucket, err)
	return body
}

// hookStallHealth is /healthz's answer for the bus's reconnect hooks stalled for stall, past
// bus.RepairGrace (healthzHandler, below every 503 check), whose last failure was hookErr: the
// dependency it names, its error and the fields beside it. The stall is the KV watch rebuild's,
// unless its last failure is the fixed mode's read of the cutover marker (bus.ErrMarkerRead),
// which runs after the rebuild: then the caches are current, and the answer names the "cutover
// marker read" with the selection bucket and the sentinel's key, so neither the answer nor the
// paging alarm sends an operator to watchers that are fine.
func hookStallHealth(stall time.Duration, hookErr error) (dependency string, err error, extra map[string]interface{}) {
	dependency, step := "KV watch rebuild", "rebuild"
	if errors.Is(hookErr, bus.ErrMarkerRead) {
		dependency, step = "cutover marker read", "read"
		extra = map[string]interface{}{"bucket": consumernames.SessionBucket, "key": consumernames.SentinelKey}
	}
	err = fmt.Errorf("no %s has succeeded for %s", step, stall.Round(time.Second))
	if hookErr != nil {
		// The last failure says what is broken; the stall, for how long. A run still in flight
		// past the grace has no error of its own, and writeDependencyHealth needs one, so then the
		// stall is the whole report.
		err = fmt.Errorf("%w (%w)", hookErr, err)
	}
	return dependency, err, extra
}
