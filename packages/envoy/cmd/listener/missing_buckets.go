package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
)

// A listener never creates a bucket its watchers copy (bus.CopiedBuckets). A bucket deleted while
// a listener writes it and then created again loses every write the deleted bucket acknowledged,
// and the listener that made the write answered its caller that it landed (INFRA-4094). So a
// listener that finds one missing fails closed: at its start it waits for the bucket and serves
// nothing but 503s (awaitCopiedBuckets, the starting gates), and once it serves, a write to the
// missing bucket answers the bus-unavailable 503 (writeStoreError) while /healthz names the bucket
// and the self-health monitor neither rebuilds nor restarts over it. Every check logs
// missingBucketLine at ERROR, which the AWS listener's bucket-missing alarm pages on
// (infra/aws/rendering/metricfilters.go). The operator recreates the bucket with `envoy-infra
// buckets`, which refuses while any listener of the deployment runs.

// missingBucketLine is the ERROR a listener logs for each check that finds a copied bucket missing.
const missingBucketLine = "listener KV bucket is missing"

// throwawayNATSEnv, set to 1, says the NATS server is a throwaway one a test or a dev rig started
// for this run: the listener then creates every copied bucket it finds missing at its start, as no
// deployment's listener does. Nothing envoy-infra renders sets it, and a deployment must not: a
// listener that restarts while a bucket is missing would create it again under the writers of the
// deleted one. A running listener never creates a bucket, with it or without it.
const throwawayNATSEnv = "ENVOY_THROWAWAY_NATS"

// bucketWaitInterval is how often a start that found a copied bucket missing asks again, and
// missingBucketLogInterval how often it logs missingBucketLine while one stays missing: as often
// as the self-health monitor checks a serving listener, so the alarm reads one cadence.
const (
	bucketWaitInterval       = 2 * time.Second
	missingBucketLogInterval = 30 * time.Second
	bucketCheckTimeout       = 10 * time.Second
)

// missingBuckets is the copied buckets this listener last found missing: the start's wait and the
// self-health monitor record what each check found, and /healthz and the
// envoy_kv_buckets_missing gauge read it. A nil one finds nothing missing, as a test's cache list
// that names no tracker does.
type missingBuckets struct {
	mu    sync.Mutex
	names []string
}

// record replaces the set with names, those the latest check found missing.
func (m *missingBuckets) record(names []string) {
	sorted := slices.Clone(names)
	slices.Sort(sorted)
	m.mu.Lock()
	m.names = sorted
	m.mu.Unlock()
}

// list is the buckets the latest check found missing, sorted; nil when none.
func (m *missingBuckets) list() []string {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.names)
}

// holds reports whether the latest check found bucket missing.
func (m *missingBuckets) holds(bucket string) bool {
	return slices.Contains(m.list(), bucket)
}

// err is the missing buckets as the error /healthz and the self-health monitor report, nil when none.
func (m *missingBuckets) err() error {
	names := m.list()
	if len(names) == 0 {
		return nil
	}
	return &bucketsMissingError{buckets: names}
}

// writeMissingBuckets answers /healthz for copied buckets found missing: 200 "degraded", naming
// them in "error" and listing them in "missing_buckets". Degraded and not 503: the listener is
// the one that answers each write to a missing bucket its 503, which a 503 here would take out of
// a load balancer's rotation, and a 503 would have ECS replace the task with one that waits at its
// start for the same bucket, or roll a deployment back to an older build that creates it.
func writeMissingBuckets(w http.ResponseWriter, names []string) {
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":          "degraded",
		"error":           (&bucketsMissingError{buckets: names}).Error(),
		"missing_buckets": names,
	})
}

// bucketsMissingError is copied buckets found missing. The self-health monitor never takes it for
// a failure a rebuild or a restart repairs (isUnrecoverableSelfHealthFailure): no listener creates
// a bucket, so only the operator's `envoy-infra buckets` brings one back.
type bucketsMissingError struct {
	buckets []string
}

func (e *bucketsMissingError) Error() string {
	return fmt.Sprintf("%s %s: no listener creates a bucket it copies; stop every listener of the deployment, then run `envoy-infra buckets create`",
		map[bool]string{true: "bucket", false: "buckets"}[len(e.buckets) == 1], strings.Join(e.buckets, ", "))
}

// listenerCopiedBuckets are the copied buckets this listener opens: all of bus.CopiedBuckets but
// the CI state bucket, which only a listener with the GitHub webhook route opens.
func listenerCopiedBuckets(ci bool) []string {
	return slices.DeleteFunc(bus.CopiedBuckets(), func(bucket string) bool { return bucket == bus.CIStateBucket && !ci })
}

// readMissingBuckets asks the server for each bucket's stream and answers those it says it does
// not hold. A read that fails any other way is returned as the error, after every bucket was
// asked, and says nothing of that bucket.
func readMissingBuckets(js nats.JetStreamContext, buckets []string) ([]string, error) {
	var missing []string
	var errs []error
	for _, bucket := range buckets {
		ctx, cancel := context.WithTimeout(context.Background(), bucketCheckTimeout)
		_, err := js.StreamInfo("KV_"+bucket, nats.Context(ctx))
		cancel()
		switch {
		case errors.Is(err, nats.ErrStreamNotFound):
			missing = append(missing, bucket)
		case err != nil:
			errs = append(errs, fmt.Errorf("read KV_%s: %w", bucket, err))
		}
	}
	return missing, errors.Join(errs...)
}

// logMissingBuckets logs missingBucketLine for each bucket of names.
func logMissingBuckets(logger *logging.Logger, names []string) {
	for _, bucket := range names {
		logger.Error(missingBucketLine,
			slog.String("bucket", bucket),
			slog.String("action", "stop every listener of the deployment, then run `envoy-infra buckets create`; no listener creates a bucket it copies (INFRA-4094)"))
	}
}

// awaitCopiedBuckets returns once every bucket of buckets exists, or ctx ends. A start runs it
// before it opens a store, so a missing bucket holds the start at its gates, which answer every
// route 503, while /healthz names the bucket (missing) and the log carries missingBucketLine every
// missingBucketLogInterval. With throwaway (throwawayNATSEnv) it creates each missing bucket
// instead, with the configuration bus.CopiedBucketConfig gives at replicas copies.
func awaitCopiedBuckets(ctx context.Context, logger *logging.Logger, js nats.JetStreamContext, conn *nats.Conn, buckets []string, missing *missingBuckets, throwaway bool, replicas int) error {
	var logged time.Time
	for {
		names, err := readMissingBuckets(js, buckets)
		if throwaway && len(names) > 0 {
			for _, bucket := range names {
				config, _ := bus.CopiedBucketConfig(bucket, replicas)
				if _, err := bus.EnsureKeyValue(conn, js, &config); err != nil {
					return fmt.Errorf("create %s on the throwaway server (%s=1): %w", bucket, throwawayNATSEnv, err)
				}
				logger.Warn("listener created a missing KV bucket on a throwaway NATS server",
					slog.String("bucket", bucket), slog.String("env", throwawayNATSEnv+"=1"))
			}
			continue
		}
		missing.record(names)
		if len(names) == 0 && err == nil {
			return nil
		}
		if len(names) > 0 && time.Since(logged) >= missingBucketLogInterval {
			logMissingBuckets(logger, names)
			logged = time.Now()
		}
		if err != nil {
			logger.Warn("listener could not read whether its KV buckets exist; it asks again", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(bucketWaitInterval):
		}
	}
}
