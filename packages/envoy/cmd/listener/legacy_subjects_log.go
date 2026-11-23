package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
)

// The two metrics packages/envoy/scripts/legacy-subjects-gate.sh reads of every listener: the count
// of names callers gave in their old form, and when this process started, so that two readings of
// the count are known to be one process's.
const (
	legacySubjectsMetric = "envoy_listener_legacy_subjects_total"
	startTimeMetric      = "envoy_listener_start_time_seconds"
)

// legacySubjectsLogLine is the message of the log line that carries those two metrics, the line
// the gate finds in a listener pod's log.
const legacySubjectsLogLine = "listener legacy subjects"

// legacySubjectsLogInterval is how often the listener logs that line.
const legacySubjectsLogInterval = time.Minute

// startLegacySubjectsLog logs legacySubjectsLogLine at once and every interval until ctx ends: the
// text /metrics serves for the old-form counter and the start-time gauge (metrics.Registry.Text,
// attribute metrics), and the Unix second it was read at (attribute at). The gate reads a listener
// in a cluster from the newest such line of its pod's log rather than from its /metrics, because
// the operator who runs the gate may read a pod's log in each listener's namespace (Dispatch's
// listener included) and may not reach its /metrics: the pod proxy is refused there, and the
// listener's NetworkPolicy admits its port from Dispatch's own pods alone.
func startLegacySubjectsLog(ctx context.Context, logger *logging.Logger, met *metrics.Registry, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			logger.Info(legacySubjectsLogLine,
				slog.Int64("at", time.Now().Unix()),
				slog.String("metrics", string(met.Text(legacySubjectsMetric, startTimeMetric))))
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
