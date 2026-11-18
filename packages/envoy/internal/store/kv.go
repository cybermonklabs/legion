package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

type Registry struct {
	// kvMu guards roleKV, which Rewatch moves to a replacement connection. The interest bucket's
	// handle is the watcher's (kvwatch.Watcher.KV).
	kvMu                  sync.RWMutex
	roleKV                bus.KeyValue
	log                   *slog.Logger
	clock                 clock.Clock
	openedAt              time.Time
	restoredRoleRevisions map[string]uint64
	mu                    sync.RWMutex
	// cache is the copy of the interest bucket: its interests, revision fence, marks and read epoch
	// (kvwatch.Copy, newInterestCopy), which every write site reaches through.
	cache kvwatch.Copy[Interest]
	// index indexes the cached interests' topic patterns by subject token (subjectIndex), which is
	// what Match walks rather than every cached interest. The copy's own hooks keep it
	// in step (indexInterestLocked, unindexInterestLocked), so a recreated bucket's read takes its
	// departed sessions out of it at the sweep that removes their rows.
	index *subjectIndex
	// watcher feeds the cache from the interest bucket. Every watcher it installs reads the whole
	// bucket again (interestCache): the cache keeps every interest through the read and removes
	// the ones the read found no evidence of only once it completes. Once a read has completed the
	// cache is consistent with the bucket, and Match answers every "is this session subscribed?"
	// question without falling through.
	watcher *kvwatch.Watcher
	// roleWatcher feeds roleHolders from the role bucket, so a session's roles are read from the
	// claims with no bucket call (RolesHeldBy), which answers only while its copy can be served
	// (kvwatch.Watcher.Servable). A role's holder is never read from it: every act that turns on
	// who holds a role reads that role's claim record (RoleClaim). holdersMu guards
	// roleHolders, the copy of the role bucket: the session each role's claim names, its revision
	// fence, which keeps a delayed watcher record from undoing a claim this registry wrote or read,
	// its marks and its read epoch (kvwatch.Copy, newRoleHolderCopy), so a claim write this registry
	// made through a handle on a bucket since recreated cannot land in the new bucket's view and
	// fence out its lower revisions. It guards roleRepair too, what the watcher's whole reads of the
	// bucket repaired in that copy (roleCache, RoleClaimRepair), which each read's hooks write
	// beside the copy they repair.
	roleWatcher *kvwatch.Watcher
	holdersMu   sync.RWMutex
	roleHolders kvwatch.Copy[string]
	roleRepair  roleClaimRepair
	// renewalFailures is this registry's record of the role claim renewals that failed, by role
	// and holder session (RenewalFailedWithin), under a mutex of its own (role_renewal.go).
	renewalFailures renewalFailures
	// legacySubjects counts the old-form names callers give the registry (LegacySubjectCounter);
	// nil counts nothing.
	legacySubjects LegacySubjectCounter
}

// OpenOption configures the registry.
type OpenOption func(*openOpts)

type openOpts struct {
	replicas       int
	clock          clock.Clock
	log            *slog.Logger
	interestWatch  []kvwatch.Option
	roleWatch      []kvwatch.Option
	legacySubjects LegacySubjectCounter
	// requestWait is WithRequestWait's wait, zero when no option set one.
	requestWait time.Duration
}

// WithLogger sets the logger the registry and its cache watcher write through. The listener passes
// its own, so the registry's lines and the watcher's (internal/kvwatch) are the JSON records with
// machine_id that the rest of its output is; the default is slog.Default(), which is what every
// other caller and the tests use. The listener must not set that default instead: it also routes
// the stdlib log package's output into the same handler, and the deployed CloudWatch metric filters
// for publish failures, webhook refusals and dropped stream subjects are space-delimited text
// patterns anchored on that package's date and time prefix
// (defined in the deployment repository's listener infrastructure), so JSON there silently stops three
// alarms. No deployed pattern matches a line from this package or from internal/kvwatch.
func WithLogger(log *slog.Logger) OpenOption {
	return func(o *openOpts) { o.log = log }
}

// WithReplicas sets the deployment's replica count (bus.ReplicasEnvVar, default 1) for the interest
// and role buckets: the count a new bucket is created at, and the count an existing bucket with
// fewer copies is raised to at open (bus.EnsureKeyValue) and by the listener's keeper after it
// (bus.WithKeptBuckets). It stays there while every program of the deployment carries one count.
func WithReplicas(n int) OpenOption {
	return func(o *openOpts) { o.replicas = n }
}

// WithClock sets the registry's clock: the time every interest and role claim records, the reaper's
// staleness window, and a restored role claim's grace window, which starts when Open returns. Tests
// drive a clocktest.Fake to end a window without waiting for it; there is one clock for the module.
func WithClock(c clock.Clock) OpenOption {
	return func(o *openOpts) { o.clock = c }
}

// WithRoleClaimWatch adds options to the role claim watcher's own, after them, so a test sets the
// watcher's AfterFunc (kvwatch.WithAfterFunc), its clock (kvwatch.WithNow) or the interval of its
// scheduled read (kvwatch.WithScheduledRead) and fires the scheduled read itself rather than wait
// a minute for it, without reaching into the registry. The listener passes none.
func WithRoleClaimWatch(options ...kvwatch.Option) OpenOption {
	return func(o *openOpts) { o.roleWatch = append(o.roleWatch, options...) }
}

// WithInterestWatch adds options to the interest registry's cache watcher's own, after them: the
// watcher's timers for a test (kvwatch.WithAfterFunc, kvwatch.WithNow), so its stream probe and its
// other waits run on a clock the test holds rather than on time.AfterFunc. No deployment sets it.
func WithInterestWatch(options ...kvwatch.Option) OpenOption {
	return func(o *openOpts) { o.interestWatch = append(o.interestWatch, options...) }
}

// WithRequestWait sets every wait the registry's own bucket calls and its open make, in place of
// the defaults: each JetStream request (the MaxWait of the JetStream context the buckets are
// opened through, 10 s, which every bucket call waits out: bus.Bounded), each bucket's lookup and
// create's wait for a reconnecting connection (bus.EnsureKeyValueWithin, 10 s), and the open's scan
// of the role bucket (roleRevisions), whose consumer create waits 5 s a send and is sent again for
// as long as the server leaves it unanswered, and whose next entry waits 10 s
// (kvwatch.ScanIdleTimeout); the scan as a whole then waits d. A test that writes through a
// registry of its own passes what its test binary's deadline leaves, so that no bound of the
// store's own ends a write the test then waits for; no deployment sets it. Open refuses a d that
// is not positive.
func WithRequestWait(d time.Duration) OpenOption {
	return func(o *openOpts) {
		o.requestWait = d
		if d <= 0 {
			// Recorded as refused, so Open tells a d that is not positive from no option at all.
			o.requestWait = -1
		}
	}
}

// LegacySubjectCounter counts each name a caller gave in its old form, under a kind label: an
// interest a registration names (LegacyTopicWarnings) or a stored row holds when it is read into a
// session's consumer filter (CountStoredLegacyTopics), or a role a claim names (ClaimedRole). The
// listener passes its envoy_listener_legacy_subjects_total counter, whose movement is what tells
// an operator a reader of the old form is still about.
type LegacySubjectCounter interface {
	Inc(labels ...[2]string)
}

// WithLegacySubjectCounter sets the counter of the old-form names callers give the registry
// (LegacySubjectCounter). A registry opened without one counts nothing and names the old forms
// all the same.
func WithLegacySubjectCounter(counter LegacySubjectCounter) OpenOption {
	return func(o *openOpts) { o.legacySubjects = counter }
}

func Open(conn *nats.Conn, options ...OpenOption) (*Registry, error) {
	opts := openOpts{replicas: 1, clock: clock.Real(), log: slog.Default()}
	for _, o := range options {
		o(&opts)
	}
	requestWait, scanIdle := 10*time.Second, kvwatch.ScanIdleTimeout
	scanCtx := context.Background()
	switch {
	case opts.requestWait < 0:
		return nil, errors.New("open the interest registry: WithRequestWait was given a wait that is not positive")
	case opts.requestWait > 0:
		requestWait, scanIdle = opts.requestWait, opts.requestWait
		// A scan whose context has a deadline creates its consumer within it rather than within
		// the 5 s of the bucket handle's own request wait (bus.KeyValue.WatchKeys). The deadline is
		// the wall clock's, as every JetStream request wait is: nats.go reads it off the context.
		var cancel context.CancelFunc
		scanCtx, cancel = context.WithTimeout(scanCtx, opts.requestWait)
		defer cancel()
	}
	js, err := conn.JetStream(nats.MaxWait(requestWait))
	if err != nil {
		return nil, err
	}
	// Each bucket's lookup and create wait for the connection while it reconnects, within this
	// open's own request wait (bus.EnsureKeyValueWithin): a start the connection drops under for a
	// moment goes on once it is back, while the listener still answers /healthz "starting", since
	// it publishes what /healthz reads only once this open and the durable's bind have returned.
	kv, err := bus.EnsureKeyValueWithin(conn, js, &nats.KeyValueConfig{Bucket: bus.InterestBucket, Replicas: opts.replicas, Storage: nats.FileStorage}, requestWait)
	if err != nil {
		return nil, err
	}
	roleKV, err := bus.EnsureKeyValueWithin(conn, js, &nats.KeyValueConfig{Bucket: bus.RoleClaimBucket, Replicas: opts.replicas, Storage: nats.FileStorage}, requestWait)
	if err != nil {
		return nil, err
	}
	// The scan's consumer create is sent again for as long as the server leaves it unanswered
	// (roleRevisions), so a server that answers late, as a loaded machine's does, delays this open,
	// and the start that waits for it answers /healthz "starting" meanwhile; a refusal the server
	// answers fails the open.
	restoredRoleRevisions, err := roleRevisions(scanCtx, roleKV, opts.clock, scanIdle, opts.log, requestWait)
	if err != nil {
		return nil, err
	}
	r := &Registry{
		roleKV:                roleKV,
		log:                   opts.log,
		index:                 newSubjectIndex(),
		clock:                 opts.clock,
		openedAt:              opts.clock.Now(),
		restoredRoleRevisions: restoredRoleRevisions,
		legacySubjects:        opts.legacySubjects,
	}
	r.cache = newInterestCopy(r)
	r.roleHolders = newRoleHolderCopy(r)
	r.watcher = r.newInterestWatcher(kv, opts.interestWatch...)
	r.roleWatcher = r.newRoleWatcher(roleKV, opts.roleWatch...)
	// No eager load: the watchers populate the caches asynchronously. A synchronous load of N
	// individual kv.Get() calls blocks indefinitely when the KV stream leader is on a remote node.
	r.watcher.Start()
	r.roleWatcher.Start()
	return r, nil
}

// newRoleWatcher returns the watcher that feeds the holder view from kv. A read whose scan timed
// out is read again by the watcher itself, so a view RolesHeldBy refuses becomes servable without
// waiting for a Rewatch, and the bucket is read whole again roleClaimRepairInterval after each read
// that completed, which repairs a copy that drifted from the bucket in either direction (roleCache
// counts what each such read repaired). A first watch whose failure was transient, as a loaded
// start's can be, is made again on the watcher's own clock (kvwatch.WithFirstWatchRetry), a WARN for
// each attempt, rather than the ERROR that leaves the view to the self-health rebuild. options
// follow the watcher's own, so one of them replaces the scheduled read's interval
// (WithRoleClaimWatch).
func (r *Registry) newRoleWatcher(kv bus.KeyValue, options ...kvwatch.Option) *kvwatch.Watcher {
	options = append([]kvwatch.Option{kvwatch.WithLogger(r.log), kvwatch.WithScheduledRead(roleClaimRepairInterval), kvwatch.WithFirstWatchRetry()}, options...)
	return kvwatch.New("role claims", kv, roleCache{r}, options...)
}

// newInterestWatcher returns the watcher that feeds the interest copy from kv. A first watch whose
// failure was transient is made again on the watcher's own clock (kvwatch.WithFirstWatchRetry), as
// the role claim watcher's is (newRoleWatcher). options follow the watcher's own
// (WithInterestWatch).
func (r *Registry) newInterestWatcher(kv bus.KeyValue, options ...kvwatch.Option) *kvwatch.Watcher {
	options = append([]kvwatch.Option{kvwatch.WithLogger(r.log), kvwatch.WithFirstWatchRetry()}, options...)
	return kvwatch.New("interest registry", kv, interestCache{r}, options...)
}

// Ping verifies both KV buckets are reachable through the current NATS connection and reports
// either watcher's terminal error, including a bucket recreated under it (kvwatch.Watcher.Check).
// /healthz and the listener monitor use failures to report an unavailable dependency while NATS
// reconnects.
func (r *Registry) Ping() error {
	if err := r.watcher.Check(); err != nil {
		return err
	}
	if err := r.roleWatcher.Check(); err != nil {
		return err
	}
	return r.WatchErr()
}

// Servable reports whether the copy List answers from can be served (kvwatch.Watcher.Servable):
// nil once a read of the interest bucket has completed and the copy follows it, or stopped
// following it no longer ago than the stale bound, and kvwatch.ErrRefilling otherwise. It reads the
// watcher's state only, never the cache. The role claim view has its own answer (RolesHeldBy).
func (r *Registry) Servable() error {
	return r.watcher.Servable()
}

// NeedsRead reports whether either copy does not follow its bucket with no read under way to make
// it, which only a Rewatch repairs (kvwatch.Watcher.NeedsRead).
func (r *Registry) NeedsRead() bool {
	return r.watcher.NeedsRead() || r.roleWatcher.NeedsRead()
}

// Disconnected says the connection both caches' watchers read through was lost
// (kvwatch.Watcher.Disconnected).
func (r *Registry) Disconnected() {
	r.watcher.Disconnected()
	r.roleWatcher.Disconnected()
}

// WatchErr is the interest watcher's terminal error, else the role claim watcher's, or nil while
// both run, so the listener can tell a rebuildable dead cache from a transient KV timeout.
func (r *Registry) WatchErr() error {
	if err := r.watcher.Err(); err != nil {
		return err
	}
	return r.roleWatcher.Err()
}

// StopWatch retires both watchers for a shutdown (kvwatch.Watcher.Stop).
func (r *Registry) StopWatch() {
	r.watcher.Stop()
	r.roleWatcher.Stop()
}

// EndWatch ends both watchers at shutdown, together and within ctx, while the connection still
// carries requests (kvwatch.Watcher.End).
func (r *Registry) EndWatch(ctx context.Context) {
	var ended sync.WaitGroup
	ended.Go(func() { r.watcher.End(ctx) })
	ended.Go(func() { r.roleWatcher.End(ctx) })
	ended.Wait()
}

func (r *Registry) interests() bus.KeyValue {
	return r.watcher.KV()
}

// logger is the logger Open was given, or the default one. A Registry the package's own unit tests
// build as a literal has no logger, and its zero value logging where slog.Default() points is the
// same thing every caller but the listener gets.
func (r *Registry) logger() *slog.Logger {
	if r.log == nil {
		return slog.Default()
	}
	return r.log
}

// Logger is the logger the registry logs through: the listener's own, which it passes to Open, so
// a route that reads the registry logs what it makes of a read there too.
func (r *Registry) Logger() *slog.Logger {
	return r.logger()
}

func (r *Registry) roles() bus.KeyValue {
	r.kvMu.RLock()
	defer r.kvMu.RUnlock()
	return r.roleKV
}

// Rewatch reopens both buckets on conn and replaces the cache's watcher with one there
// (kvwatch.Watcher.Rewatch). A NATS server restart loses the watcher's ordered consumer, and
// nats.go replaces it only once it notices the missed heartbeats, up to twenty seconds later;
// until then a cache misses every write another listener makes, and a drain deletes a consumer
// the server no longer has. The listener's reconnect hook calls this, so the caches follow their
// buckets from the reconnect on. A failure to open the role bucket or to rewatch the interest
// watcher moves nothing: both watchers and both handles stay where they were. The role claim
// watcher is rewatched last, once the interest watcher and the role handle have moved, and its
// failure is returned with the role claim view left on its previous watcher. The move waits on
// nothing past ctx, which a reconnect hook passes as the run's context (bus.OpenKeyValue,
// kvwatch.Watcher.Rewatch).
func (r *Registry) Rewatch(ctx context.Context, conn *nats.Conn) error {
	// The role bucket opens first, so a failure leaves both handles on the previous connection
	// rather than the interests on conn and the roles behind.
	js, err := conn.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		return fmt.Errorf("open role registry JetStream: %w", err)
	}
	roleKV, err := bus.OpenKeyValue(ctx, conn, js, r.roles().Bucket())
	if err != nil {
		return fmt.Errorf("open role KV bucket: %w", err)
	}
	if err := r.watcher.Rewatch(ctx, conn); err != nil {
		return err
	}
	r.kvMu.Lock()
	r.roleKV = roleKV
	r.kvMu.Unlock()
	return r.roleWatcher.Rewatch(ctx, conn)
}

// CachedInterest returns sessionID's interest from the cache alone, and whether the cache holds
// one; it never reads the bucket. It is Cached, under the name the listener's durable delivery
// reads the cache by (cmd/listener/durable.go).
func (r *Registry) CachedInterest(sessionID string) (Interest, bool) {
	return r.Cached(sessionID)
}
