package main

import (
	"context"
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
	"github.com/sjawhar/envoy/internal/session"
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
//
// The one exception is optionalCopiedBucket, the known-session bucket: a listener serves without
// it, missing at its start or lost later, and creates it no more than any other.

// optionalCopiedBucket is the one copied bucket a listener serves without: the known-session
// bucket, whose store no delivery depends on and which a hub can lack, since older listeners
// created it only once their NATS user held its grant. Missing at the start or lost later, it holds
// no start (awaitCopiedBuckets reads it not), /healthz answers 200 "degraded" naming it, the
// listener logs it once at WARN (missingBuckets.report) and not missingBucketLine, and the store
// answers no record and writes none (session.KnownSessions.RecordWithin) until it opens the bucket
// the operator created.
const optionalCopiedBucket = bus.KnownSessionBucket

// optionalBucketMissingLine is the WARN a listener logs, once in its life, for optionalCopiedBucket
// found missing.
const optionalBucketMissingLine = "listener KV bucket is missing; the listener serves without it, and its store answers no record and writes none until the bucket is there"

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
// bucketCheckTimeout bounds each read of a bucket's stream: a read that fails holds the start's
// turn (awaitCopiedBuckets), and at a tick changes nothing the listener holds missing.
const (
	bucketWaitInterval       = 2 * time.Second
	missingBucketLogInterval = 30 * time.Second
	bucketCheckTimeout       = 10 * time.Second
)

// missingBuckets is the copied buckets this listener holds missing: the start's wait and the
// self-health monitor apply each read they make to it (observe), and /healthz and the
// envoy_kv_buckets_missing gauge read it. A bucket stays in it from a read that found it missing
// until a read finds it present; a read that failed says nothing of its bucket and changes
// nothing. A nil one holds nothing missing, as a test's cache list that names no tracker does.
type missingBuckets struct {
	mu    sync.Mutex
	names []string
	// warned is whether this listener has logged optionalCopiedBucket missing (report,
	// foundMissingAtStart): it does so once in its life.
	warned bool
}

// foundMissingAtStart is the known-session store's open at the start finding optionalCopiedBucket
// missing (session.KnownSessions.BucketMissing): the bucket is held missing from before the
// listener serves, so /healthz names it from its first answer, and since the store's open logged
// its own WARN for it (session.OpenKnownSessions), report logs it no second time.
func (m *missingBuckets) foundMissingAtStart() {
	m.observe(bucketReads{missing: []string{optionalCopiedBucket}})
	m.mu.Lock()
	defer m.mu.Unlock()
	m.warned = true
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

// required is list without optionalCopiedBucket: the buckets a listener cannot serve without.
func (m *missingBuckets) required() []string {
	return slices.DeleteFunc(m.list(), func(bucket string) bool { return bucket == optionalCopiedBucket })
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

// count is how many buckets the latest check found missing, optionalCopiedBucket included: the
// envoy_kv_buckets_missing gauge.
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

// err is the required buckets found missing as the error the self-health monitor reports, nil when
// none: optionalCopiedBucket missing fails no tick, and is logged once instead (report).
func (m *missingBuckets) err() error {
	names := m.required()
	if len(names) == 0 {
		return nil
	}
	return &bucketsMissingError{buckets: names}
}

// writeMissingBuckets answers /healthz for a running listener whose copied buckets a read found
// missing: 200 "degraded", naming them in "error" and listing them in "missing_buckets", with what
// issuers answers (writeHealth). Degraded and not 503: a running listener still serves every route
// that needs no missing bucket, reads from its copy within the stale bound included, and answers
// each write to a missing bucket its 503, but for optionalCopiedBucket, whose store writes nothing;
// a 503 here would take it out of a load balancer's rotation, and have ECS replace the task with
// one that waits at its start for the same bucket and serves nothing.
func writeMissingBuckets(w http.ResponseWriter, names []string, issuers func() map[string]any) {
	writeHealth(w, http.StatusOK, map[string]any{
		"status":          "degraded",
		"error":           (&bucketsMissingError{buckets: names}).Error(),
		"missing_buckets": names,
	}, issuers)
}

// writeWaitingForBuckets answers /healthz while the start waits for copied buckets it found
// missing (awaitCopiedBuckets), optionalCopiedBucket never among them: 503 "waiting", naming them
// in "error" and listing them in "missing_buckets", with what issuers answers (writeHealth). The
// wait comes before both starting gates open (openWebhooks, openV1Routes), so every webhook and /v1
// route answers 503 "service starting" until they exist: the listener serves nothing, and each
// reader of /healthz's status is told so:
//   - GKE's readiness probe (curl -fsS, infra/gcp) takes the pod out of service, and a rollout
//     waits instead of replacing a pod that serves with one that serves nothing; its startup and
//     liveness probes read /livez, so the waiting pod is not restarted;
//   - the ECS container health check (curl -sf, infra/aws/rendering/listener.go) replaces the
//     task, whose replacement waits the same way, and a deployment keeps the task it replaces;
//   - the docker provider's wait for a healthy container (infra/host) fails `pulumi up`;
//   - a Docker or compose health check (curl -f) reads the container unhealthy, so
//     `docker compose up --wait` fails instead of passing a listener that serves nothing.
//
// None of them can repair the wait: only the operator's `envoy-infra buckets create` does, and the
// ERROR the wait logs for each bucket (missingBuckets.report) pages for it.
func writeWaitingForBuckets(w http.ResponseWriter, names []string, issuers func() map[string]any) {
	writeHealth(w, http.StatusServiceUnavailable, map[string]any{
		"status":          "waiting",
		"error":           (&bucketsMissingError{buckets: names}).Error() + "; the listener's start waits for it and serves no route until it exists",
		"missing_buckets": names,
	}, issuers)
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
// the CI state bucket, which only a listener with the GitHub webhook route opens. The self-health
// monitor reads each of them; the start waits for all but optionalCopiedBucket (awaitCopiedBuckets).
func listenerCopiedBuckets(ci bool) []string {
	return slices.DeleteFunc(bus.CopiedBuckets(), func(bucket string) bool { return bucket == bus.CIStateBucket && !ci })
}

// monitoredBuckets are the copied buckets the self-health monitor reads at a tick: copied, less the
// known-session bucket while that store cannot read it (session.KnownSessions.Unreadable), a hub
// whose users were rendered before that bucket's grant (#2643). There the server refuses and drops
// the read, so it can find the bucket neither present nor missing: it would cost the tick
// bucketCheckTimeout and log, every tick, the connection's `envoy nats async error` ERROR for
// the refused publish and checkCopiedBuckets' WARN for a failed read, ten times the store's own
// retry. The start's wait reads that bucket not at all (awaitCopiedBuckets). Once the store opens
// the bucket, or finds it missing, the monitor reads it again.
func monitoredBuckets(copied []string, known *session.KnownSessions) []string {
	if !known.Unreadable() {
		return copied
	}
	return slices.DeleteFunc(slices.Clone(copied), func(bucket string) bool { return bucket == bus.KnownSessionBucket })
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
// read says nothing. refused names those of unread whose JetStream context returned
// bus.ErrPublishDenied: the user's grant does not carry the bucket.
type bucketReads struct {
	present []string
	missing []string
	unread  map[string]error
	refused []string
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
// and never past ctx, so the shutdown's monitorCancel ends a tick's reads on a hub that does not
// answer and a stalled hub costs one read's timeout, not one per bucket. The listener client's
// JetStream context turns a refused StreamInfo request into bus.ErrPublishDenied. The reads come
// back in the order of buckets.
func readBuckets(ctx context.Context, js nats.JetStreamContext, buckets []string) bucketReads {
	errs := make([]error, len(buckets))
	var wg sync.WaitGroup
	for i, bucket := range buckets {
		wg.Go(func() {
			readCtx, cancel := context.WithTimeout(ctx, bucketCheckTimeout)
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
			if errors.Is(err, bus.ErrPublishDenied) {
				reads.refused = append(reads.refused, bucket)
			}
		}
	}
	return reads
}

// checkCopiedBuckets is the self-health monitor's read of the copied buckets, each tick: it reads
// every bucket, applies the reads to missing, and logs what missing holds (report) - a bucket this
// read could not read included, so the alarm keeps firing until a read finds the bucket present.
// It returns the reads, for the monitor to tell a store whose bucket a read found present
// (session.KnownSessions.BucketPresent). A tick ctx ended (the shutdown) says nothing, logs nothing
// and returns no read.
func checkCopiedBuckets(ctx context.Context, logger *logging.Logger, js nats.JetStreamContext, buckets []string, missing *missingBuckets) bucketReads {
	reads := readBuckets(ctx, js, buckets)
	if ctx.Err() != nil {
		return bucketReads{}
	}
	missing.observe(reads)
	missing.report(logger)
	if err := reads.err(); err != nil {
		logger.Warn("listener could not read whether its KV buckets exist; it keeps what it last read", slog.String("error", err.Error()))
	}
	return reads
}

// checkMonitoredBuckets is the self-health monitor's read of the copied buckets at a tick: those of
// copied it monitors (monitoredBuckets: the known-session bucket is left to its store while the
// store cannot read it), applied to missing (checkCopiedBuckets). A read that finds the
// known-session bucket there tells the store, which opens it at once if its last attempt found it
// missing (session.KnownSessions.BucketPresent), so it writes and answers its records again within
// a tick of the operator creating it, rather than at its retry's next attempt, up to five minutes
// later, while /healthz already reads the bucket there.
func checkMonitoredBuckets(ctx context.Context, logger *logging.Logger, js nats.JetStreamContext, copied []string, known *session.KnownSessions, missing *missingBuckets) {
	reads := checkCopiedBuckets(ctx, logger, js, monitoredBuckets(copied, known), missing)
	if slices.Contains(reads.present, bus.KnownSessionBucket) {
		known.BucketPresent(ctx)
	}
}

// report logs what missing holds: missingBucketLine at ERROR for each bucket a listener cannot
// serve without, every call, and optionalBucketMissingLine at WARN for optionalCopiedBucket, once in
// the listener's life.
func (m *missingBuckets) report(logger *logging.Logger) {
	for _, bucket := range m.required() {
		logger.Error(missingBucketLine,
			slog.String("bucket", bucket),
			slog.String("action", missingBucketRepair+"; no listener creates a bucket it copies"))
	}
	if m == nil {
		return
	}
	m.mu.Lock()
	warn := !m.warned && slices.Contains(m.names, optionalCopiedBucket)
	m.warned = m.warned || warn
	m.mu.Unlock()
	if warn {
		logger.Warn(optionalBucketMissingLine,
			slog.String("bucket", optionalCopiedBucket),
			slog.String("action", missingBucketRepair+"; no listener creates a bucket it copies"))
	}
}

// awaitCopiedBuckets returns once a read finds every bucket of buckets present, or ctx ends. A
// start runs it before it opens a store, so a missing bucket holds the start at its gates, which
// answer every route 503, while /healthz names the bucket (missing) and the log carries
// missingBucketLine every missingBucketLogInterval. A read of a bucket that fails holds the turn
// and is made again on the next, so a read the hub did not answer in time never lets the start go
// with a bucket missing. It creates nothing: only the operator's `envoy-infra buckets` does.
//
// It neither reads nor waits for optionalCopiedBucket, which a listener serves without: that
// bucket's store finds it at its own open (session.OpenKnownSessions), which no missing bucket and
// no refused or slow read of it holds, and the self-health monitor reads it from then on.
func awaitCopiedBuckets(ctx context.Context, logger *logging.Logger, js nats.JetStreamContext, buckets []string, missing *missingBuckets) error {
	buckets = slices.DeleteFunc(slices.Clone(buckets), func(bucket string) bool { return bucket == optionalCopiedBucket })
	var logged time.Time
	for {
		reads := readBuckets(ctx, js, buckets)
		if err := ctx.Err(); err != nil {
			return err
		}
		missing.observe(reads)
		if len(reads.missing) == 0 && len(reads.unread) == 0 {
			return nil
		}
		if missing.count() > 0 && time.Since(logged) >= missingBucketLogInterval {
			missing.report(logger)
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
