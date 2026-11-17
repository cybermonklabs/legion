package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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
// and the listener that made the write answered its caller that it landed. So a
// listener that finds one missing fails closed: at its start it waits for the bucket and serves
// nothing but 503s (awaitCopiedBuckets, the starting gates), and once it serves, a write to the
// missing bucket answers the bus-unavailable 503 (writeStoreError) while /healthz names the bucket
// and the self-health monitor neither rebuilds nor restarts over it. Every check logs
// missingBucketLine at ERROR, which the AWS listener's bucket-missing alarm pages on
// (infra/aws/rendering/metricfilters.go). The operator makes the bucket with `envoy-infra buckets
// create` (missingBucketRepair).

// missingBucketLine is the ERROR a listener logs for each check that finds a copied bucket missing.
const missingBucketLine = "listener KV bucket is missing"

// missingBucketRepair is the repair a listener names for a missing bucket, in /healthz's error and
// in missingBucketLine's action: the runbook's create, which until the auth service's cutover
// no hub holds the key for but as `--break-glass`, run as a listener's NATS user, which
// stops no listener.
const missingBucketRepair = "make it with `envoy-infra buckets create`, with `--break-glass` until the auth service's cutover, by the runbook at packages/envoy/infra/README.md#envoy-infra-buckets"

// bucketWaitInterval is how often a start that found a copied bucket missing asks again, and
// missingBucketLogInterval how often it logs missingBucketLine while one stays missing: as often
// as the self-health monitor checks a serving listener, so the alarm reads one cadence.
const (
	bucketWaitInterval       = 2 * time.Second
	missingBucketLogInterval = 30 * time.Second
	bucketCheckTimeout       = 10 * time.Second
	// knownBucketCheckTimeout is the read of the known-session bucket's stream: a user whose grant
	// predates that bucket gets no answer at all, only the server's refusal, which nats.go does not
	// hand the request, and a start must not wait bucketCheckTimeout on it (awaitCopiedBuckets lets
	// that bucket's failed read through).
	knownBucketCheckTimeout = time.Second
)

// missingBuckets is the copied buckets this listener holds missing: the start's wait and the
// self-health monitor apply each read they make to it (observe), and /healthz and the
// envoy_kv_buckets_missing gauge read it. A bucket stays in it from a read that found it missing
// until a read finds it present; a read that failed says nothing of its bucket and changes
// nothing. A nil one holds nothing missing, as a test's cache list that names no tracker does.
type missingBuckets struct {
	mu    sync.Mutex
	names []string
}

// observe applies reads to the set: each bucket read missing is added, each read present removed,
// and each whose read failed left as it was.
func (m *missingBuckets) observe(reads bucketReads) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, bucket := range reads.missing {
		if !slices.Contains(m.names, bucket) {
			m.names = append(m.names, bucket)
		}
	}
	m.names = slices.DeleteFunc(m.names, func(bucket string) bool { return slices.Contains(reads.present, bucket) })
	slices.Sort(m.names)
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

// holdsAnyOf reports whether the latest check found one of buckets missing, read under the lock
// with no copy of the set: every self-health tick asks it for each cache.
func (m *missingBuckets) holdsAnyOf(buckets []string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return anyOf(buckets, m.names)
}

// count is how many buckets the latest check found missing: the envoy_kv_buckets_missing gauge.
func (m *missingBuckets) count() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.names)
}

// anyOf reports whether a and b share a bucket.
func anyOf(a, b []string) bool {
	return slices.ContainsFunc(a, func(bucket string) bool { return slices.Contains(b, bucket) })
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
	return fmt.Sprintf("%s %s: no listener creates a bucket it copies; %s",
		map[bool]string{true: "bucket", false: "buckets"}[len(e.buckets) == 1], strings.Join(e.buckets, ", "), missingBucketRepair)
}

// listenerCopiedBuckets are the copied buckets this listener opens: all of bus.CopiedBuckets but
// the CI state bucket, which only a listener with the GitHub webhook route opens.
func listenerCopiedBuckets(ci bool) []string {
	return slices.DeleteFunc(bus.CopiedBuckets(), func(bucket string) bool { return bucket == bus.CIStateBucket && !ci })
}

// openExisting opens a store with open, which opens its buckets only if they exist, and when one
// went missing after the start's wait found it (bus.ErrBucketMissing) waits for it again with
// wait (awaitCopiedBuckets) and opens it again, so a bucket deleted between the wait and the open
// holds the start at its gates, every route answering 503 and /healthz naming the bucket, and
// never ends it. Any other error is the open's, and wait's error ends the wait.
func openExisting[T any](wait func() error, open func() (T, error)) (T, error) {
	for {
		opened, err := open()
		if !errors.Is(err, bus.ErrBucketMissing) {
			return opened, err
		}
		if err := wait(); err != nil {
			var none T
			return none, err
		}
	}
}

// bucketReads is one read of each bucket's stream: the buckets the server holds (present), those
// it says it does not (missing), and those whose read failed, with the error (unread), of which the
// read says nothing.
type bucketReads struct {
	present []string
	missing []string
	unread  map[string]error
}

// err is the failed reads as one error, nil when none failed.
func (r bucketReads) err() error {
	var errs []error
	for _, bucket := range slices.Sorted(maps.Keys(r.unread)) {
		errs = append(errs, fmt.Errorf("read KV_%s: %w", bucket, r.unread[bucket]))
	}
	return errors.Join(errs...)
}

// readBuckets asks the server for every bucket's stream at once, within bucketCheckTimeout each
// (knownBucketCheckTimeout for the known-session bucket) and never past ctx, so the shutdown's
// monitorCancel ends a tick's reads on a hub that does not answer and a stalled hub costs one
// read's timeout, not one per bucket. The reads come back in the order of buckets.
func readBuckets(ctx context.Context, js nats.JetStreamContext, buckets []string) bucketReads {
	errs := make([]error, len(buckets))
	var wg sync.WaitGroup
	for i, bucket := range buckets {
		timeout := bucketCheckTimeout
		if bucket == bus.KnownSessionBucket {
			timeout = knownBucketCheckTimeout
		}
		wg.Go(func() {
			readCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			_, errs[i] = js.StreamInfo("KV_"+bucket, nats.Context(readCtx))
		})
	}
	wg.Wait()
	reads := bucketReads{unread: map[string]error{}}
	for i, bucket := range buckets {
		switch err := errs[i]; {
		case err == nil:
			reads.present = append(reads.present, bucket)
		case errors.Is(err, nats.ErrStreamNotFound):
			reads.missing = append(reads.missing, bucket)
		default:
			reads.unread[bucket] = err
		}
	}
	return reads
}

// checkCopiedBuckets is the self-health monitor's read of the copied buckets, each tick: it reads
// every bucket, applies the reads to missing, and logs missingBucketLine for every bucket missing
// holds - one this read could not read included, so the alarm keeps firing until a read finds the
// bucket present. A tick ctx ended (the shutdown) says nothing and logs nothing.
func checkCopiedBuckets(ctx context.Context, logger *logging.Logger, js nats.JetStreamContext, buckets []string, missing *missingBuckets) {
	reads := readBuckets(ctx, js, buckets)
	if ctx.Err() != nil {
		return
	}
	missing.observe(reads)
	logMissingBuckets(logger, missing.list())
	if err := reads.err(); err != nil {
		logger.Warn("listener could not read whether its KV buckets exist; it keeps what it last read", slog.String("error", err.Error()))
	}
}

// logMissingBuckets logs missingBucketLine for each bucket of names.
func logMissingBuckets(logger *logging.Logger, names []string) {
	for _, bucket := range names {
		logger.Error(missingBucketLine,
			slog.String("bucket", bucket),
			slog.String("action", missingBucketRepair+"; no listener creates a bucket it copies"))
	}
}

// awaitCopiedBuckets returns once a read finds every bucket of buckets present, or ctx ends. A
// start runs it before it opens a store, so a missing bucket holds the start at its gates, which
// answer every route 503, while /healthz names the bucket (missing) and the log carries
// missingBucketLine every missingBucketLogInterval. It creates nothing: only the operator's
// `envoy-infra buckets` does.
//
// The known-session bucket alone does not hold the start when its read fails rather than finding
// it missing: a hub whose users were rendered before that bucket's grant (#2643), and not applied
// since, lets the listener read no part of it, and its store then opens it once the grant arrives
// (session.OpenKnownSessions). A known-session bucket the server says it does not hold holds the
// start as every other does.
func awaitCopiedBuckets(ctx context.Context, logger *logging.Logger, js nats.JetStreamContext, buckets []string, missing *missingBuckets) error {
	var logged time.Time
	for {
		reads := readBuckets(ctx, js, buckets)
		if err := ctx.Err(); err != nil {
			return err
		}
		missing.observe(reads)
		delete(reads.unread, bus.KnownSessionBucket)
		if len(reads.missing) == 0 && len(reads.unread) == 0 {
			return nil
		}
		if held := missing.list(); len(held) > 0 && time.Since(logged) >= missingBucketLogInterval {
			logMissingBuckets(logger, held)
			logged = time.Now()
		}
		if err := reads.err(); err != nil {
			logger.Warn("listener could not read whether its KV buckets exist; it asks again", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(bucketWaitInterval):
		}
	}
}
