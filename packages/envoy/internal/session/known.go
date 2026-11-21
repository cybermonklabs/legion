package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/ctxlock"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/store"
)

// KnownSessionRetention is how long a session stays known after its last registration, interest
// change or clean stop: the envoy_known_sessions bucket's TTL, and the window a record's own
// last_seen is read against (KnownSessions.Get). It is well past the 72 hours Envoy keeps a
// session's messages, so a suspended Legion session that resumes under the same id is still known
// when its queued messages are delivered.
const KnownSessionRetention = 30 * 24 * time.Hour

// KnownSessionRefreshAge is how young a stored record must be for a write of the same content to
// be skipped (KnownSessions.RecordWithin): a session heartbeats every 90 seconds, so an unchanged
// record is rewritten at most once per this, whatever the number of heartbeats and however many
// listeners serve the session. A record's last_seen therefore lags the session's last heartbeat by
// less than this, which nothing reads as liveness: the session entry (SessionBucket) is the store
// that answers that.
const KnownSessionRefreshAge = 10 * time.Minute

// knownSessionSweepInterval is how often the store drops the records that have aged out of
// KnownSessionRetention. One periodic pass is the whole expiry machinery here, rather than a timer
// per record as the session registry arms (expiry.go): that registry holds the live sessions, a
// few thousand at most, each with a deadline a read must not overshoot, while this store holds up
// to ~100,000 records whose deadline is 30 days away and whose only readers are a publish count
// and a send to a stopped session. 100,000 armed timers would cost more than the records do; a
// read filters a record past its deadline out on its own (Get, MatchEach), so the pass is only
// there to give the memory back, and an hour's delay in doing that changes no answer.
const knownSessionSweepInterval = time.Hour

// knownBucketOpenWait bounds the one JetStream request that tells an attempt to open the bucket
// whether this listener may open it, and so the whole of an attempt it may not: a bucket this
// listener's NATS user may not open is refused by the server dropping the request, which costs the
// wait in full (openKnownBucket). The listener's start can wait on this attempt, so the wait is
// what a start pays for a bucket it cannot open; 2 seconds is far past a healthy open, whose
// leader has already answered the JetStream requests of every bucket opened before this one, and
// far below the 10 seconds every other store of the listener waits, which no start can afford for
// a store no delivery depends on. An attempt this wait gives up on costs nothing but a retry.
const knownBucketOpenWait = 2 * time.Second

// knownBucketServeWait is the wait every bucket call made after the open gets: the handle the
// cache watcher reads and a write (RecordWithin) goes through, which its caller's own context
// bounds besides (bus.Bounded). It is the 10 seconds the listener's other stores give their bucket
// calls (store.Open, OpenSessionRegistry, kvwatch.Watcher.Rewatch): the short wait above is for
// the attempt a start waits on, and giving a write or a watch that wait would make a cluster that
// is merely slow look like one that refuses.
const knownBucketServeWait = 10 * time.Second

// knownBucketRetryFirst and knownBucketRetryMax are the waits between the attempts the store's
// one background repair makes (repairUntilStopped), of a bucket that did not open and of a move
// to a recovered connection that failed: the first, then twice the one before it up to the cap,
// until an attempt leaves nothing to repair or the store is stopped. A connection event starts the
// waits over and makes an attempt at once (Rewatch). A listener started before its hub's grant
// carries this bucket picks the bucket up within the cap of the grant arriving, with no restart.
// Past the cap a denied listener asks 12 times an hour, for as long as it runs: each is one
// dropped request and one permissions violation in the server's log, and in the listener's own
// the `envoy nats async error` ERROR its connection writes for that violation
// (internal/bus/connect.go) and no line of the store's, since the store logs only the first
// failure (OpenKnownSessions) and a success after it (awaitOpened, attemptRepair).
const (
	knownBucketRetryFirst = time.Second
	knownBucketRetryMax   = 5 * time.Minute
)

// KnownSession is one record of the envoy_known_sessions bucket, keyed by session id: a session
// that registered within KnownSessionRetention, whether it is registered now or not. Its Topics
// are the topics the session's interest row stores (store.Interest.Topics), which are the topics
// its session consumer's filter holds: a registration drops every topic that reaches no session,
// one no stream subject captures and no role lane (splitUncapturedTopics, cmd/listener/api.go),
// so a record counts exactly the sessions a stored publish reaches.
//
// The record holds nothing else, and not the client instance id in particular, which only a role
// claim's arbitration reads (cmd/listener/role_holder.go), and that reads the live session entry.
type KnownSession struct {
	SessionID    string   `json:"session_id"`
	Title        string   `json:"title"`
	Capabilities []string `json:"capabilities"`
	Topics       []string `json:"topics"`
	// LastSeen is when the record was last written, in unix milliseconds, as
	// SessionEntry.UpdatedAt is.
	LastSeen int64 `json:"last_seen"`
}

type KnownSessionsOption func(*knownSessionsOpts)

type knownSessionsOpts struct {
	replicas int
	log      *slog.Logger
	now      func() time.Time
	// retryFirst and retryMax are the waits between the background repair's attempts,
	// knownBucketRetryFirst and knownBucketRetryMax unless a test shortens them
	// (withKnownSessionRetry, export_test.go).
	retryFirst time.Duration
	retryMax   time.Duration
	// attemptBound is a test's bound on one attempt of the background repair in place of
	// repairAttemptWindow (withKnownSessionAttemptContext, export_test.go): a context the test
	// ends itself. Nil outside tests.
	attemptBound func(context.Context) (context.Context, context.CancelFunc)
	// openWait is the wait of each retry attempt's request for the bucket (knownJetStream),
	// knownBucketOpenWait unless a test lengthens it (withKnownSessionOpenWait, export_test.go);
	// the store's first attempt (OpenKnownSessions) always waits knownBucketOpenWait.
	openWait time.Duration
	// watch is what a test adds to the options the store's watcher is built with
	// (withKnownSessionWatch, export_test.go): a clock to move past the stale bound, or a short
	// scan idle timeout. No deployment sets it.
	watch []kvwatch.Option
}

// WithKnownSessionReplicas sets the copies the bucket is created with, as the deployment's replica
// count says (bus.EnsureKeyValue). The listener's keeper raises an existing bucket's copies
// (bus.ListenerBuckets).
func WithKnownSessionReplicas(n int) KnownSessionsOption {
	return func(o *knownSessionsOpts) { o.replicas = n }
}

// WithKnownSessionLogger sets the logger the store and its cache watcher write through. The
// listener passes its own, so their lines are JSON records with machine_id, as the session
// registry's are (WithSessionLogger).
func WithKnownSessionLogger(log *slog.Logger) KnownSessionsOption {
	return func(o *knownSessionsOpts) { o.log = log }
}

// WithKnownSessionClock sets the clock the store stamps each record's last_seen with and reads
// both the refresh age and the retention against; time.Now unless a test sets it. One clock drives
// all three, so a test that drives heartbeats through the listener's own write path controls the
// throttle with it. The cache watcher's stale bound is measured on its own clock (kvwatch.WithNow).
func WithKnownSessionClock(now func() time.Time) KnownSessionsOption {
	return func(o *knownSessionsOpts) { o.now = now }
}

// KnownSessions is a cache-backed view of the envoy_known_sessions bucket: every session that
// registered through any listener within KnownSessionRetention, keyed by session id. A long-lived
// watcher (kvwatch) keeps the records in memory (kvwatch.Copy) with a subject index of their
// topics (store.TopicMatcher), so every read - one record by id, or the records whose topics match
// a published topic - answers from memory and makes no JetStream call. A publish counts its
// recipients per request against up to ~100,000 records, which neither a per-key Get nor a linear
// scan can serve.
//
// The store answers only while its copy can be served (Opened): once the watcher's first read of
// the bucket has delivered every record, and for as long as the copy follows the bucket or
// stopped following it no longer ago than the watcher's stale bound, two minutes
// (kvwatch.Watcher.Servable), the rule every cache of the listener answers by. Past that bound,
// and before the bucket opens, it answers no record and no match, so a publish leaves its counts
// out and a send to a session with no entry answers 404, as on a listener that has no bucket.
//
// A store whose bucket is not open is the degraded listener: one whose NATS user may not open the
// bucket keeps the store, which answers nothing and whose writes do nothing, and opens the bucket
// as soon as an attempt of its own succeeds (repairUntilStopped). A nil *KnownSessions answers the
// same way, for a listener that could not build a JetStream context at all and for a test that
// builds the handlers without a store, so every method here is safe on a nil receiver and no call
// site carries a nil check.
type KnownSessions struct {
	now func() time.Time
	log *slog.Logger
	// replicas is the copies the bucket is created with, kept for the attempts that follow a
	// bucket that did not open; retryFirst and retryMax are the waits between them, and
	// watchOptions what a test adds to the watcher's options.
	replicas     int
	retryFirst   time.Duration
	retryMax     time.Duration
	watchOptions []kvwatch.Option
	// openWait is the wait of each retry attempt's request for the bucket (knownJetStream).
	openWait time.Duration

	// mu guards records and topics, which are kept in step: every record the copy stores or
	// removes for the bucket's sake is indexed or unindexed through its rows (newKnownSessions),
	// and the sweep removes from both.
	mu      sync.RWMutex
	records kvwatch.Copy[KnownSession]
	topics  *store.TopicMatcher

	// openMu guards conn, watcher, asked and covered. The watcher is installed when the bucket
	// opens, which is at OpenKnownSessions for a listener whose user may open it and at a later
	// attempt for one whose grant arrives while it runs.
	openMu sync.RWMutex
	// conn is the connection the bucket is opened on, the one the store was opened with until a
	// connection event moves the store to another (Rewatch).
	conn *nats.Conn
	// watcher feeds the records from the bucket and holds the handle the store writes through,
	// nil until the bucket opens.
	watcher *kvwatch.Watcher
	// asked counts the moves the listener has asked for, one per connection event (Rewatch), and
	// covered is the count the last attempt that succeeded carried out. They are counted rather
	// than compared as connections because nats.go reconnects in place: the connection such an
	// event hands back is the one the watcher is already on, and its subscriptions are gone all
	// the same. covered < asked is a store whose watcher and write handle are on a connection the
	// listener has left, which is what the background repair repairs and what a move that
	// succeeded takes away from it.
	asked   uint64
	covered uint64

	// repairMu serializes the attempts to repair the store, a reconnect hook's own (Rewatch) and
	// the background retry's, so the watcher is never moved by two callers at once; and it guards
	// moveFailed. It is a ctxlock.Mutex so that a reconnect hook's run waits for it only while the
	// run lasts (LockContext): the retry holds it through an attempt whose request takes no
	// context, and on a connection that dropped with that request unanswered the attempt waits out
	// the request's own bound, which would hold every other hook of the run behind it
	// (TestTheHooksMoveDoesNotWaitOutTheRetrysAttempt). The retry's attempt is bounded by
	// repairAttemptWindow, so it lets the lock go even while nats.go holds its connection lock for
	// a reconnect's handshake.
	repairMu ctxlock.Mutex
	// repairStateMu guards repairing and rearm, and is never held across an attempt, so arming and
	// retiring the retry wait for no attempt (armRepair, retireRepair). Exactly one retry runs, or
	// none: it is armed only while none runs, and only the goroutine itself retires it, after an
	// attempt that found nothing left to repair and while no arm came during that attempt (rearm).
	repairStateMu sync.Mutex
	repairing     bool
	// rearm is an arm that came while the retry ran: the retry makes another attempt rather than
	// retiring, as it would have had the arm waited for its attempt to end.
	rearm bool
	// attemptBound is a test's bound on one attempt of the retry in place of repairAttemptWindow
	// (attemptContext); nil outside tests.
	attemptBound func(context.Context) (context.Context, context.CancelFunc)
	// moveFailed is set by a move to a recovered connection that failed and cleared by the next
	// that succeeds, whichever caller makes it, which is the move that logs the store back
	// (attemptRepair).
	moveFailed bool

	// wake makes the retry attempt at once, from its first wait again, after a connection event
	// that found no bucket open (Rewatch), as the legion agent registry's retry is woken
	// (cmd/listener/legion_agents.go).
	wake chan struct{}
	// stopped ends the sweep goroutine and the background repair; stopOnce closes it once,
	// whichever of StopWatch and EndWatch comes first.
	stopped  chan struct{}
	stopOnce sync.Once
}

// newKnownSessions is a store with no bucket open yet: no watcher, an empty copy and index, and
// the background goroutines not started.
func newKnownSessions(conn *nats.Conn, opts knownSessionsOpts) *KnownSessions {
	s := &KnownSessions{
		now:          opts.now,
		log:          opts.log,
		replicas:     opts.replicas,
		retryFirst:   opts.retryFirst,
		retryMax:     opts.retryMax,
		attemptBound: opts.attemptBound,
		openWait:     opts.openWait,
		watchOptions: opts.watch,
		topics:       store.NewTopicMatcher(),
		conn:         conn,
		wake:         make(chan struct{}, 1),
		stopped:      make(chan struct{}),
	}
	s.records = kvwatch.NewCopy(kvwatch.Rows[KnownSession]{
		Decode:  decodeKnownSession,
		Stored:  func(key string, record KnownSession) { s.topics.Set(key, record.Topics) },
		Removed: func(key string, _ KnownSession) { s.topics.Remove(key) },
	})
	return s
}

// decodeKnownSession is the record a put of the bucket leaves in the copy.
func decodeKnownSession(entry kvwatch.Entry) (KnownSession, bool, error) {
	var record KnownSession
	if err := json.Unmarshal(entry.Value, &record); err != nil {
		return KnownSession{}, false, err
	}
	return record, true, nil
}

// OpenKnownSessions opens the envoy_known_sessions bucket, creating it with a 30-day TTL
// (KnownSessionRetention) on file storage, and starts the watcher that keeps its records in
// memory and the sweep that gives back the memory of the records that aged out.
//
// A bucket that does not open - a listener whose NATS user may not open it, on a hub whose grant
// does not carry the bucket - is no error and stops no start: the store is returned, logs the one
// WARN below, answers no record and writes nothing, and goes on attempting the bucket in the
// background until one succeeds (repairUntilStopped), at once after each connection event
// (Rewatch). The error is the JetStream context's alone, which leaves no store to open anything
// with; the caller then runs on a nil *KnownSessions, which answers as this store does and never
// opens. ctx bounds the first attempt, the one the start waits on (openKnownBucket).
func OpenKnownSessions(ctx context.Context, conn *nats.Conn, options ...KnownSessionsOption) (*KnownSessions, error) {
	opts := knownSessionsOpts{
		replicas:   1,
		now:        time.Now,
		retryFirst: knownBucketRetryFirst,
		retryMax:   knownBucketRetryMax,
		openWait:   knownBucketOpenWait,
	}
	for _, o := range options {
		o(&opts)
	}
	// A connection that gives no JetStream context can open nothing, now or later. It is this
	// function's one error, read here so the caller is answered it rather than meeting it on
	// every attempt; each attempt builds a context of its own, on the connection it runs against.
	if _, err := knownJetStream(conn, knownBucketOpenWait); err != nil {
		return nil, err
	}
	s := newKnownSessions(conn, opts)
	kv, err := openKnownBucket(ctx, conn, opts.replicas, knownBucketOpenWait)
	if err != nil {
		s.logger().Warn("the known-session bucket is not open; this listener answers no session's 30-day record and writes none until an attempt of its own opens the bucket",
			slog.String("bucket", bus.KnownSessionBucket), slog.String("error", err.Error()))
		s.armRepair()
	} else {
		s.startWatch(kv)
	}
	go s.sweepUntilStopped()
	return s, nil
}

// knownJetStream is the JetStream context one attempt to open the bucket asks through: its
// requests wait openWait, knownBucketOpenWait outside tests, rather than the 10 seconds nats.go
// gives its own by default.
func knownJetStream(conn *nats.Conn, openWait time.Duration) (nats.JetStreamContext, error) {
	return bus.JetStream(conn, nats.MaxWait(openWait))
}

// openKnownBucket makes one attempt to open the bucket on conn, creating it with the retention
// and the copies the deployment asks for when it is not there.
//
// The attempt asks for the bucket with one request first, waiting knownBucketOpenWait or until ctx
// ends, whichever comes first (bus.Bounded, as bus.OpenKeyValue waits), so the window of a
// retry's attempt (retryRepair) bounds that request too; only a bucket the server says it does
// not hold goes through bus.EnsureKeyValue's create. That function
// sends a lookup the server leaves unanswered again until ctx ends or the start's hang guard
// passes (its sentAgain), and reads a refusal only on a connection a bus.Client owns, so a user
// whose grant does not carry the bucket would cost an attempt that whole wait rather than one
// send; the one request costs a denied attempt what it costs, the wait, and a granted one an
// answer. A create the server answers only at the bucket's first leader election is waited for
// the same way, so an election slower than a few of these 2 s sends opens the bucket on this
// attempt rather than leaving the store to write nothing until a retry does.
//
// The handle it returns is opened on a context with the wait every other store of the listener
// gives its bucket calls, since the store's writes and its cache watcher go through that handle
// and must not give up on a cluster that is merely slow: a write is bounded by its caller's own
// context (RecordWithin, bus.Bounded), and the open's short wait is for the attempt a start waits
// on, not for the work that follows it.
func openKnownBucket(ctx context.Context, conn *nats.Conn, replicas int, openWait time.Duration) (bus.KeyValue, error) {
	opening, err := knownJetStream(conn, openWait)
	if err != nil {
		return bus.KeyValue{}, err
	}
	if _, _, err := bus.Bounded(ctx, func() (nats.KeyValue, error) { return opening.KeyValue(bus.KnownSessionBucket) }); errors.Is(err, nats.ErrBucketNotFound) {
		if _, err := bus.EnsureKeyValue(ctx, conn, opening, &nats.KeyValueConfig{
			Bucket:   bus.KnownSessionBucket,
			TTL:      KnownSessionRetention,
			Replicas: replicas,
			Storage:  nats.FileStorage,
		}); err != nil {
			return bus.KeyValue{}, err
		}
	} else if err != nil {
		return bus.KeyValue{}, err
	}
	serving, err := bus.JetStream(conn, nats.MaxWait(knownBucketServeWait))
	if err != nil {
		return bus.KeyValue{}, err
	}
	return bus.OpenKeyValue(ctx, conn, serving, bus.KnownSessionBucket)
}

// startWatch installs the watcher of the bucket kv is a handle on, and starts it: the watcher
// fills the records in the background, as the session registry's does, since a synchronous read
// of every key blocks for seconds when the bucket's leader is remote. A store already stopped
// installs nothing, so a bucket that opens during a shutdown leaves no watcher behind: StopWatch
// and EndWatch take openMu to read the watcher, so one installed here is theirs to stop and one
// installed after them is not installed at all.
//
// A read whose scan timed out completes nothing, so it leaves the store not open (Opened), and the
// watcher reads the bucket again itself (kvwatch.Watcher.readAgainLater): no rescan of the
// store's own is needed for a scan that stalled.
func (s *KnownSessions) startWatch(kv bus.KeyValue) {
	s.openMu.Lock()
	defer s.openMu.Unlock()
	select {
	case <-s.stopped:
		return
	default:
	}
	options := append([]kvwatch.Option{kvwatch.WithLogger(s.log)}, s.watchOptions...)
	s.watcher = kvwatch.New("known session store", kv, knownCache{s}, options...)
	s.watcher.Start()
}

// awaitOpened waits for the watcher an attempt installed to complete its first read, the state
// Opened reports, and logs the one line that says the store reads and writes the bucket from now
// on. Until that read the store answers for none of the sessions the bucket knows: the listener's
// start gates exactly this hazard with WaitForCacheReady before it serves, and nothing gates the
// attempt that opens the bucket later. The line is written after the wait, so it means what it
// says.
//
// The background retry runs it on a goroutine of its own once an attempt has opened the bucket
// (repairUntilStopped), and never waits on it, because the read it waits for can wait on that
// retry: a disconnect during the first read completes nothing, and the read that completes then
// is the one a move installs, the reconnect's own or, once that failed, the retry's. It waits
// until a read of the watcher completes, whichever move installed it, or until the store stops
// (untilStopped). The bucket opens once, so a store waits here once at most.
func (s *KnownSessions) awaitOpened() {
	watcher := s.watch()
	if watcher == nil {
		// The store stopped while the attempt ran, so startWatch installed nothing.
		return
	}
	ctx, cancel := s.untilStopped()
	defer cancel()
	if err := watcher.WaitComplete(ctx); err != nil {
		// The store stopped, or the watch ended before it had read the bucket. Opened stays false,
		// which leaves a publish's count out rather than answering it from a cache that holds part
		// of it. The retry went on without this wait and may have retired before the watch ended,
		// so a watch that ended arms it again, and its next attempt moves the watcher that ended
		// (attemptRepair); after a stop it arms nothing (armRepair).
		s.armRepair()
		return
	}
	s.logger().Info("the known-session bucket is open; this listener writes and reads the 30-day records from now on",
		slog.String("bucket", bus.KnownSessionBucket))
}

// untilStopped is a context that ends when the store does, for a background wait that must not
// outlive a shutdown, and its cancel, which the waiter calls once it is done. Its goroutine ends
// with either.
func (s *KnownSessions) untilStopped() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-s.stopped:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// watch is the store's watcher, or nil while its bucket is not open. A nil store has none either,
// so one read of this covers both.
func (s *KnownSessions) watch() *kvwatch.Watcher {
	if s == nil {
		return nil
	}
	s.openMu.RLock()
	defer s.openMu.RUnlock()
	return s.watcher
}

// Opened reports whether the store answers: its bucket is open and its copy can be served
// (kvwatch.Watcher.Servable). A copy can be served once the watcher's first read of the bucket has
// delivered every record, and for as long as it follows the bucket or stopped following it - a
// disconnect, a move to a recovered connection that failed - no longer ago than the watcher's
// stale bound, two minutes, the rule every cache of the listener answers by. A store whose NATS
// user may not open the bucket answers false until an attempt of its own opens it and that read
// completes, and a nil store for good.
//
// Get and MatchEach answer nothing while this is false, so a caller whose answer would mean
// something narrower than it names - a publish's recipient count, which these records are half of
// - reads this and leaves that answer out instead of sending the live sessions alone, and a send
// to a session with no entry answers 404 as it does on a listener with no bucket.
//
// The completed read is half of this on purpose. A bucket is opened in one request and read in a
// pass over every key it holds, and between the two the store holds no record of a session the
// bucket knows: a send would be refused `404 no live session` for a session Envoy does know, and a
// publish would count its recipients low, which are the two failures this store exists to remove.
// A scan that timed out completes nothing, and neither does one a disconnect overlapped, so the
// store reports itself open only once a read has delivered every record.
func (s *KnownSessions) Opened() bool {
	watcher := s.watch()
	return watcher != nil && watcher.Servable() == nil
}

// logger is the logger WithKnownSessionLogger set, or the default one.
func (s *KnownSessions) logger() *slog.Logger {
	if s.log == nil {
		return slog.Default()
	}
	return s.log
}

// Get returns sessionID's record, and whether the store holds one that has not aged out of
// KnownSessionRetention while it answers at all (Opened). It reads memory alone and makes no
// bucket call, so a caller on a request path pays nothing for it; a record past the retention
// answers as no record, whether or not the sweep has dropped it yet.
func (s *KnownSessions) Get(sessionID string) (KnownSession, bool) {
	if !s.Opened() {
		return KnownSession{}, false
	}
	return s.cached(sessionID)
}

// cached is the record the copy holds for sessionID within the retention, whether or not the
// store answers: what a write compares against and a clean stop rewrites. Callers hold no lock.
func (s *KnownSessions) cached(sessionID string) (KnownSession, bool) {
	cutoff := s.cutoff()
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.records.Get(sessionID)
	if !ok || record.LastSeen < cutoff {
		return KnownSession{}, false
	}
	return record, true
}

// MatchEach calls visit with each record whose topics match topic (routing.Match's rules), once
// per record and in no order, looked up in the store's subject index, while the store answers at
// all (Opened). A record past the retention is left out, as Get leaves it out. visit runs under
// the store's read lock, so it must not call back into the store or block. A publish's recipient
// count reads the store this way, the only read of the matching records there is: it holds a set
// of the session ids it has counted either way, so a list and a sort of the records would be work
// it threw away.
func (s *KnownSessions) MatchEach(topic string, visit func(KnownSession)) {
	if !s.Opened() {
		return
	}
	s.eachMatch(topic, visit)
}

// eachMatch is MatchEach over the copy, whether or not the store answers.
func (s *KnownSessions) eachMatch(topic string, visit func(KnownSession)) {
	cutoff := s.cutoff()
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.topics.Match(topic, func(key string) {
		if record, ok := s.records.Get(key); ok && record.LastSeen >= cutoff {
			visit(record)
		}
	})
}

// RecordWithin writes record, stamped with the store's clock, its bucket write waiting no longer
// than ctx (bus.Bounded): an HTTP route passes its request's own context. A record whose content
// equals the one the store holds is not written while that one is younger than
// KnownSessionRefreshAge, so a session's 90-second heartbeat costs one write per refresh age
// rather than one per heartbeat. What that skip compares is this listener's own copy of the record
// (cached), never the bucket: the record as this listener's watcher last delivered it, or as this
// listener's own write left it (store.Registry.UpsertWithin, by contrast, reads the stored row). A
// record another listener wrote is therefore compared as this listener's watcher last delivered
// it: the throttle holds across every listener that serves the session once each one's watcher
// has delivered the others' writes, and a write made before that compares an older copy, which
// costs at most one write of a record the bucket already holds.
//
// A store whose bucket is not open writes nothing and answers nil, as a nil store does: its
// reads answer no record either, so nothing it could write would be read.
func (s *KnownSessions) RecordWithin(ctx context.Context, record KnownSession) error {
	return s.writeWithin(ctx, record, true)
}

// writeWithin is the write both of this store's write paths make. throttled is RecordWithin's
// skip of an unchanged record younger than the refresh age; TouchWithin passes false, since a
// clean stop is one write per session lifetime and the whole point of it is to move last_seen.
//
// No caller fails its request over what this answers: a registration and a subscription change
// (RecordWithin) and a clean stop (TouchWithin) each answer whatever the write did, and the error
// is for the call site's WARN.
func (s *KnownSessions) writeWithin(ctx context.Context, record KnownSession, throttled bool) error {
	watcher := s.watch()
	if watcher == nil || record.SessionID == "" {
		return nil
	}
	now := s.now()
	record.LastSeen = now.UnixMilli()
	buf, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if held, ok := s.cached(record.SessionID); throttled && ok && now.UnixMilli()-held.LastSeen < KnownSessionRefreshAge.Milliseconds() {
		// What the skip asks is whether this write would store the record already there, so it
		// compares the two encodings rather than the fields one by one: a field added to
		// KnownSession, and the difference between a null and an empty list, are then covered by
		// the encoding itself. The stored record is stamped with this write's last_seen, the one
		// field a write of unchanged content would move.
		unchanged := held
		unchanged.LastSeen = record.LastSeen
		stored, err := json.Marshal(unchanged)
		if err != nil {
			return err
		}
		if bytes.Equal(buf, stored) {
			return nil
		}
	}
	kv, epoch := watcher.Handle()
	revision, answered, err := bus.Bounded(ctx, func() (uint64, error) { return kv.Put(ctx, record.SessionID, buf) })
	if !answered {
		return fmt.Errorf("write the known-session record of %s: %w", record.SessionID, err)
	}
	if err != nil {
		return err
	}
	// Write through, as the session registry's Put does: the next heartbeat then finds this
	// record whether or not the watcher has delivered it back yet, so the throttle holds from the
	// first write rather than from the first watch event. The copy takes it behind its revision
	// fence (kvwatch.Copy.Put): while this write was in flight the watcher can have applied a
	// newer revision another listener wrote - a re-registration of the same session through that
	// listener - and putting this record back over it would answer the older one until the next
	// write of the key. A write through a handle on a bucket since recreated is dropped there too.
	s.mu.Lock()
	s.records.Put(watcher, epoch, record.SessionID, record, revision)
	s.mu.Unlock()
	return nil
}

// TouchWithin moves sessionID's record to now, keeping its content, which is what a clean stop
// writes: the session stopped, and a publish must still count it as a recipient of the topics it
// followed. A session the store holds no record of writes nothing, since a stop carries no topics
// of its own to record.
//
// The write is made whatever the refresh-age throttle would say. A live session rewrites its
// record at least once per KnownSessionRefreshAge, so at a clean stop the stored record is almost
// always younger than that and an unchanged-content write would be skipped, leaving last_seen up
// to a refresh age before the stop - which a send to the stopped session answers and both tools
// print. A stop is one write per session lifetime, so the throttle buys nothing here.
func (s *KnownSessions) TouchWithin(ctx context.Context, sessionID string) error {
	if s == nil {
		return nil
	}
	held, ok := s.cached(sessionID)
	if !ok {
		return nil
	}
	return s.writeWithin(ctx, held, false)
}

// Rewatch moves the cache's watcher and the handle the store writes through to conn
// (kvwatch.Watcher.Rewatch), after a connection event; the listener calls it from a reconnect
// hook of its own. A store whose bucket is not open has no watcher to move: it keeps conn for the
// attempts that follow and wakes the retry to attempt the bucket at once, so a grant applied by a
// reload that reconnected the listener is taken without waiting out the retry's backoff.
//
// A move that failed is retried in the background, on the store's own backoff
// (repairUntilStopped): the store repairs a failed move itself, and the error is returned for the
// caller's WARN. Until a move succeeds the watcher is on a connection the listener has left: the
// copy answers for the watcher's stale bound and then Opened answers false over it, so a
// publish's recipient count is left out. The move that succeeds, this caller's or the retry's,
// logs the store back (attemptRepair).
//
// The move is made under the lock the retry's attempts take, so the hook's move and the retry's
// never run at once, and each reads the connection the store holds at the moment it runs, so
// neither can put the watcher back onto a connection the store has already left. A move that
// succeeds is what ends the retry: its next attempt finds every move the listener asked for made
// (covered, asked) and retires. The hook's move waits on nothing past ctx, the hook's run: not on
// its requests (kvwatch.Watcher.Rewatch), nor on repairMu, which the retry holds through an
// attempt of its own (repairMu's LockContext), so a move on a connection that drops ends at the
// drop; the retry's attempts are its own.
func (s *KnownSessions) Rewatch(ctx context.Context, conn *nats.Conn) error {
	if s == nil {
		return nil
	}
	s.openMu.Lock()
	s.conn = conn
	s.asked++
	watcher := s.watcher
	s.openMu.Unlock()
	if watcher == nil {
		// The bucket never opened, so the retry that is already attempting it covers this move
		// too: it opens on the connection just stored (attemptRepair).
		select {
		case s.wake <- struct{}{}:
		default:
		}
		s.armRepairWithin(ctx)
		return nil
	}
	if _, err := s.repairOnce(ctx); err != nil {
		// A move whose run is over (bus.RunEnded: its context ended, or conn left CONNECTED) is no
		// failure to repair in the background: the run is superseded, and the run the next
		// connection event brings moves the store again.
		if !bus.RunEnded(ctx, conn) {
			s.armRepairWithin(ctx)
		}
		return err
	}
	return nil
}

// Disconnected says the connection the cache's watcher reads through was lost
// (kvwatch.Watcher.Disconnected): the copy stops following its bucket and its stale episode
// begins, so the store answers for the stale bound and no longer unless a move reads the bucket
// again. The listener calls it from the bus's disconnect hook (bus.Client.AddDisconnectHook). A
// store whose bucket is not open has nothing to say it to.
func (s *KnownSessions) Disconnected() {
	if watcher := s.watch(); watcher != nil {
		watcher.Disconnected()
	}
}

// WaitForCacheReady blocks until the store answers (Opened), its watcher's first read having
// delivered every record (kvwatch.Watcher.WaitComplete), or until ctx is done. It waits for that
// state rather than for readiness, which a read whose scan timed out releases too: a caller
// released on such a scan would serve over a cache holding part of the bucket, which is the state
// this gate exists to keep away (store.Registry.WaitForCacheReady waits on the role claim view the
// same way, for the same reason). A watcher that ends on its own, and a stop, end it at once with
// their own error rather than at ctx.
//
// Callers bound ctx and proceed on error: the store answers nothing until Opened says the view is
// whole. A store whose bucket is not open waits for nothing and answers nil: it has no cache to
// fill, and the listener's start must not spend its caller's budget on a bucket it may not open.
func (s *KnownSessions) WaitForCacheReady(ctx context.Context) error {
	watcher := s.watch()
	if watcher == nil {
		return nil
	}
	return watcher.WaitComplete(ctx)
}

// StopWatch retires the cache's watcher and the sweep for a shutdown, and stops the background
// repair (repairUntilStopped).
func (s *KnownSessions) StopWatch() {
	if s == nil {
		return
	}
	s.stopSweep()
	if watcher := s.watch(); watcher != nil {
		watcher.Stop()
	}
}

// EndWatch ends the cache's watcher and the sweep at shutdown, within ctx, while the connection
// still carries requests (kvwatch.Watcher.End), and stops the background repair
// (repairUntilStopped). The listener adds it as a drain hook.
func (s *KnownSessions) EndWatch(ctx context.Context) {
	if s == nil {
		return
	}
	s.stopSweep()
	if watcher := s.watch(); watcher != nil {
		watcher.End(ctx)
	}
}

// stopSweep ends the sweep goroutine and the background repair, and keeps a bucket that opens
// after it from installing a watcher (startWatch).
func (s *KnownSessions) stopSweep() {
	s.stopOnce.Do(func() { close(s.stopped) })
}

// cutoff is the earliest last_seen a record may carry and still answer: anything older has aged
// out of KnownSessionRetention.
func (s *KnownSessions) cutoff() int64 {
	return s.now().Add(-KnownSessionRetention).UnixMilli()
}

// knownCache is the store as the cache its watcher feeds (kvwatch.Cache). The hooks are its own
// type's, not the store's methods, so nothing but the watcher calls them.
type knownCache struct{ s *KnownSessions }

// BeginRead starts a read of the bucket (kvwatch.Copy.BeginRead). Every record stays through it.
func (c knownCache) BeginRead(read kvwatch.Read) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	c.s.records.BeginRead(read)
}

// Apply takes one record the read delivered (kvwatch.Copy.Delivered), and logs a value it could
// not decode, which removed the session's record.
func (c knownCache) Apply(entry kvwatch.Entry, _ kvwatch.Read) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	if err := c.s.records.Delivered(entry); err != nil {
		c.s.logger().Warn("known session store evicted a malformed record",
			slog.String("key", entry.Key), slog.Uint64("revision", entry.Revision), slog.String("error", err.Error()))
	}
}

// CompleteRead removes every record the read found no evidence of, and its topics from the index:
// the bucket no longer holds that session's record.
func (c knownCache) CompleteRead(kvwatch.Read) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	c.s.records.CompleteRead()
}

// sweepUntilStopped runs the one periodic pass until the store is stopped.
func (s *KnownSessions) sweepUntilStopped() {
	ticker := time.NewTicker(knownSessionSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopped:
			return
		case <-ticker.C:
			s.sweep()
		}
	}
}

// sweep drops every record that has aged out of KnownSessionRetention, giving back the strings and
// topic lists it held and the index nodes its topics alone kept. The bucket's TTL removes such a
// record with no record of the removal a watcher delivers, so nothing else would ever drop it. The
// copy keeps the key's revision in its fence (kvwatch.Copy.Remove) until a completed read finds
// the bucket no longer holds the key.
func (s *KnownSessions) sweep() {
	cutoff := s.cutoff()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, record := range s.records.All() {
		if record.LastSeen < cutoff {
			s.records.Remove(key)
			s.topics.Remove(key)
		}
	}
}
