// Package cistore holds the ingest-side aggregation of GitHub CI observations.

// Each check_run and check_suite webhook folds into a per-commit State record
// in a JetStream KV bucket via compare-and-swap rather than being published
// raw; concurrent observations of one commit share one write (see update). A
// summary loop (see loop.go) emits one checks envelope when a commit is quiet
// and its recorded CI work is complete; it runs a pass only when a cached record
// changed or one it waits on falls due. All coordination state lives in
// KV, so aggregation is durable, restart-safe, and correct across listener
// replicas; the in-memory state is a rebuildable WatchAll read-cache and the
// queue of observations waiting for their commit's write.
package cistore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/logging"
)

// recordBudget bounds how long one write of a commit's record retries. It retries a lost
// compare-and-swap, and a transient KV error (kvErrorLasts), from a fresh read. Concurrent
// observations of one commit within this process are combined into one write (update), so the
// writers that conflict on the KV revision are the few that remain: the summary loop's settlement
// transitions and another listener task during a deploy. Each backs off with jitter before
// retrying, so a bounded time budget lets them serialize rather than a fixed attempt count. The
// transient errors it outlasts are a NATS reconnect (a request refused while the connection
// reconnects, since the reconnect buffer is off, and a request that was in flight when the
// connection went away, given up on at the rewatch that follows the reconnect: kvCall) and a
// JetStream 503 while a server restarts.
// A caller that queues behind a write in progress waits for that write and then its own, so a
// Record can take two budgets, and recordWait holds it to that, every KV call included. The webhook
// handler records a check_run once for each PR it lists, one after another, under one context for
// the delivery (webhook.GitHubHandler), so a delivery listing k PRs can take up to 2k budgets
// unless that context ends first; that needs a write to lose its compare-and-swap for its whole
// budget, and after combining only the summary loop's transitions and another listener task's
// batches write the same record. The summary loop's transitions (casState) answer no request and
// still wait the JetStream MaxWait.
const recordBudget = 2 * time.Second
const recordBackoffCap = 50 * time.Millisecond

// recordWait bounds one Record or RecordSuite as a whole: the wait for a write it queues behind and
// its own write, two budgets (update). nats.go's KV calls take no context and wait up to the
// JetStream MaxWait (10 s, Open), so each is waited for only until this wait, or the caller's
// context, ends (kvCall): a silent bus fails the record then rather than holding it, and every
// caller queued behind it, for the MaxWait. Within that wait a call is still waited for past its
// write's own budget, so a server that stalls for less and then answers loses nothing.
const recordWait = 2 * recordBudget

// checkRunID is a GitHub check-run id. Records written before ids were
// validated at ingress carry it as a decimal string; new records carry a
// number. Id-less legacy checks omit the field when they are persisted again.
type checkRunID uint64

func (id *checkRunID) UnmarshalJSON(raw []byte) error {
	var n uint64
	if err := json.Unmarshal(raw, &n); err == nil && n > 0 {
		*id = checkRunID(n)
		return nil
	}
	var legacy string
	if err := json.Unmarshal(raw, &legacy); err == nil {
		if n, err := strconv.ParseUint(legacy, 10, 64); err == nil && n > 0 {
			*id = checkRunID(n)
			return nil
		}
	}
	return fmt.Errorf("cistore: check run ID %s is not a positive integer", raw)
}

// Check is the last-known state of a single named check for a commit.
type Check struct {
	Name       string     `json:"name,omitempty"`
	CheckRunID checkRunID `json:"check_run_id,omitempty"`
	URL        string     `json:"url"`
	Status     string     `json:"status"`     // queued|in_progress|completed
	Conclusion string     `json:"conclusion"` // success|failure|... ("" until completed)
	// ObservedAt orders observations of this run (completion, else start).
	ObservedAt string `json:"observed_at"`
}

// UnmarshalJSON reads records written by deployed listeners: their checks
// carry updated_at instead of observed_at. The retired updated_at becomes
// observed_at.
func (check *Check) UnmarshalJSON(data []byte) error {
	type checkAlias Check
	*check = Check{}
	wire := struct {
		*checkAlias
		ObservedAt *string `json:"observed_at"`
		UpdatedAt  string  `json:"updated_at"`
	}{checkAlias: (*checkAlias)(check)}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.ObservedAt != nil {
		check.ObservedAt = *wire.ObservedAt
		return nil
	}
	check.ObservedAt = wire.UpdatedAt
	return nil
}

// Suite is the last-known state of one GitHub check suite for a commit. A record written before
// the listener stopped taking a suite before it completes (contracts.GithubCIObservations) may
// hold a run_count field of its suites, which this release no longer reads and the next write of
// the record drops.
type Suite struct {
	ID         string `json:"id"`
	AppID      string `json:"app_id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	ObservedAt string `json:"observed_at"`
}

// Status is the newest terminal commit status of one context on a commit (recordStatus). A status
// is an entry of its own kind: a check run of its name is another entry, it never stands for a
// check run nor for the tester or architect check run that carries a verdict, and it takes no
// part in whether the head is ready to settle (settlementReady). Dispatch's CI reconcile records
// it from GitHub's read of the head; no webhook does, since a status event names no pull request.
type Status struct {
	Context string `json:"context"`
	State   string `json:"state"` // success|failure|error
	// CreatedAt is GitHub's created_at, which orders two statuses of one context.
	CreatedAt   string `json:"created_at"`
	URL         string `json:"url"`
	Description string `json:"description"`
	// ObservedAt is when the record took this status, in Unix milliseconds (as LastEventAt).
	ObservedAt int64 `json:"observed_at"`
}

// SettlementClaim is the durable right to publish one settlement snapshot.
// Claims prevent replicas from publishing the same episode concurrently.
// Verify says the claim waits for a GitHub read before it publishes (verify.go),
// which gives it a longer life before another replica may reclaim it
// (claimWindow). A listener of an earlier release ignores the field and drops
// it at its next write of the claim.
type SettlementClaim struct {
	Hash       string `json:"hash"`
	Generation uint64 `json:"generation"`
	ClaimedAt  int64  `json:"claimed_at"`
	Verify     bool   `json:"verify,omitempty"`
}

// State is the aggregated set of checks, suites and commit statuses for one (owner, repo, PR
// number, head SHA).
type State struct {
	Owner  string           `json:"owner"`
	Repo   string           `json:"repo"`
	Number string           `json:"number"`
	SHA    string           `json:"sha"`
	Checks map[string]Check `json:"checks"`
	Suites map[string]Suite `json:"suites"`
	// Statuses holds the newest terminal commit status of each context, keyed by context
	// (recordStatus), and is left out of a record that holds none.
	Statuses       map[string]Status `json:"statuses,omitempty"`
	LastEventAt    int64             `json:"last_event_at"`
	Generation     uint64            `json:"generation"`
	EmittedCount   uint64            `json:"emitted_count"`
	SettledEmitted bool              `json:"settled_emitted"`
	Claim          *SettlementClaim  `json:"claim,omitempty"`
	// Overflowed says the listener never settles this head again, because the head is past what it
	// can publish: a check run or check suite was refused because it would take the record past
	// maxRecordBytes or the record's settlement past maxSettlementBytes even with no commit status
	// in the record (write), or NATS refused the settlement itself (markOverflowed, from the
	// summary loop). A commit status never marks a record overflowed: at a bound it gives way to
	// the check runs (write). A head that settled before it overflowed keeps its last settlement;
	// nothing supersedes it.
	Overflowed bool `json:"overflowed,omitempty"`
	// Schema is recordSchema on every record this listener writes (encodeRecord). A record last
	// written by a listener that settled only a pull request's head has none (legacyBacklog). An
	// older listener ignores the field, and its own writes drop it. A listener of a release before
	// commit statuses drops Statuses the same way, at its next write of the record, and the
	// reconcile writes them back only once GitHub's answer for the head changes, since a pass
	// writes nothing while the head's fingerprint holds.
	Schema int `json:"schema,omitempty"`
	// VerifiedRev is the newest revision of this head's verify request (verifyKey) that Dispatch's
	// CI reconcile answered: it read the request at that revision, then read the head from GitHub
	// whole, wrote what it read into this record, and then wrote VerifiedRev (Writer.MarkVerified).
	// VerifiedStream is the identity of the bucket's stream that revision is of (streamIdentity),
	// since a bucket deleted and created again numbers its revisions from 1 again. Within one
	// stream VerifiedRev only grows. A listener that asked with ENVOY_CI_VERIFY on publishes only
	// once both answer its own request in the stream the bucket is held in (ClaimStillHeld). Hash
	// takes neither: an answer moves no snapshot. A listener of an earlier release drops both at
	// its next write of the record, as it drops Schema, which costs at most one more verify read.
	VerifiedRev    uint64 `json:"verified_rev,omitempty"`
	VerifiedStream int64  `json:"verified_stream,omitempty"`
	// VerifyStopped is the snapshot (Hash) of the last settlement episode the summary loop gave up
	// after verifyMaxAsks requests Dispatch did not answer (giveUp), VerifyStops how many episodes
	// of that snapshot it gave up in a row, and VerifyRetryAt the millisecond from which a listener
	// claims the head again: the give-up's backoff, verifyBackoff doubled at each give-up of the
	// snapshot after its first, up to verifyBackoffMax (verifyBackoffAfter). VerifyRetryAt is zero
	// for a give-up made once the head's last observation was the bucket's TTL old: that head is
	// given up for good, and its record expires as an unanswered head's did before the backoff. While
	// the record still hashes to VerifyStopped and VerifyRetryAt has not come, no listener claims the
	// head (verifyStopped); a new observation, which moves the snapshot, ends the stop at once. A
	// listener of an earlier release drops all three at its next write.
	VerifyStopped string `json:"verify_stopped,omitempty"`
	VerifyStops   int    `json:"verify_stops,omitempty"`
	VerifyRetryAt int64  `json:"verify_retry_at,omitempty"`
}

// recordSchema stamps every record this listener writes.
const recordSchema = 1

// encodeRecord stamps st with recordSchema and encodes it. Every write of a record goes through
// it, so a record without the stamp was last written by a head-gated listener.
func encodeRecord(st *State) ([]byte, error) {
	st.Schema = recordSchema
	return json.Marshal(st)
}

// handoverGrace is how long past its debounce a record without a schema can still settle, so every
// such record admitted is at most debounce+handoverGrace old. Five minutes is the smallest round
// figure above the startup floor in the table in
// docs/solutions/architecture-patterns/envoy-ci-summary.md, the time a stop-then-start compose
// deploy of a listener with the GitHub webhook route takes from the old listener's SIGTERM to the
// new one's first pass, so a head that finished during a degraded handover still settles; a wider
// grace would only make admitted settlements later. That doc holds the derivation, its constants and the dated measurements.
const handoverGrace = 5 * time.Minute

// legacyBacklog reports whether st is a head-gated listener's leftover: it has no schema and its
// last event is at least debounce plus handoverGrace before now. A record without a schema settles
// only in [debounce, debounce+handoverGrace) after its last event; past that it never settles, and
// it stays settled_emitted false with no schema until the bucket's seven-day TTL expires it, which
// is expected. An observation that changes the record stamps it, and the commit then settles as any
// other; a stamped record is never held back, so a settlement pending across a restart still
// publishes. Seven days after the last head-gated listener stops, no record without a schema
// remains.
//
// A record without a schema whose last_event_at is ahead of this listener's clock publishes once
// wall time reaches its band. Raising ENVOY_CI_DEBOUNCE moves the band's far edge with it, so a
// restart with a larger debounce admits the records whose last event falls in the added slice.
func legacyBacklog(st State, now int64, debounce time.Duration) bool {
	return st.Schema == 0 && now-st.LastEventAt >= (debounce+handoverGrace).Milliseconds()
}

// due reports whether st is waiting to settle at now: unsettled, not given up at now
// (verifyStopped), ready (settlementReady), quiet for debounce since its last event, and not a
// head-gated listener's leftover (legacyBacklog).
func due(st State, now int64, debounce time.Duration) bool {
	return !st.SettledEmitted &&
		!verifyStopped(st, now) &&
		settlementReady(st) &&
		now-st.LastEventAt >= debounce.Milliseconds() &&
		!legacyBacklog(st, now, debounce)
}

// UnmarshalJSON maps the retired resettled marker to the durable fact that
// this record emitted at least one settlement.
func (state *State) UnmarshalJSON(data []byte) error {
	type stateAlias State
	*state = State{}
	wire := struct {
		*stateAlias
		Resettled bool `json:"resettled"`
	}{stateAlias: (*stateAlias)(state)}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if (wire.Resettled || state.SettledEmitted) && state.EmittedCount == 0 {
		state.EmittedCount = 1
	}
	return nil
}

// retiredHeadKind marks the head record an earlier listener kept per pull request under its own
// key, to settle only a pull request's head. Settlements do not depend on the head, so nothing
// writes or reads one; a record left in the bucket waits out its TTL uncached.
const retiredHeadKind = "head"

// keyCleaner writes one segment of a KV key: every key this store builds joins its segments with
// '.', and a KV key's tokens must not be empty, so a '.' in a segment is written '='. A GitHub
// repository name may begin with a dot, end with one or hold two in a row (`sjawhar/.github`), and
// no GitHub owner, name, number or sha holds '=', so the spelling is lossless: `foo.bar` and
// `foo_bar` keep distinct keys, and a segment without a dot is written as it is. The rest guards
// against stray wildcard, space and slash characters.
var keyCleaner = strings.NewReplacer(".", "=", "*", "_", ">", "_", " ", "_", "/", "_")

// Key derives a KV-safe key from the commit identity, each segment through keyCleaner. The key is
// never parsed back (State carries the identity fields), so it only needs to be unique and valid
// per commit.
func Key(owner, repo, number, sha string) string {
	return keyCleaner.Replace(owner) + "." +
		keyCleaner.Replace(repo) + ".pr" +
		keyCleaner.Replace(number) + "." +
		keyCleaner.Replace(sha)
}

// Hash is a stable, order-independent fingerprint of the CI state. It includes
// check-run attempts and check-suite state so a re-run becomes a distinct
// delivery even when it resolves to the same conclusion. It includes each
// commit status's context and state alone, as a status's own entry beside the
// checks', so a new context or a changed state moves the snapshot, a new time,
// link or description does not, and a record with no status hashes as it did
// before the record held statuses.
func (s State) Hash() string {
	h := sha256.New()
	checkNames := make([]string, 0, len(s.Checks))
	for name := range s.Checks {
		checkNames = append(checkNames, name)
	}
	sort.Strings(checkNames)
	for _, name := range checkNames {
		check := s.Checks[name]
		h.Write([]byte("check\x00" + name + "\x00" + strconv.FormatUint(uint64(check.CheckRunID), 10) + "\x00" + check.Status + "\x00" + check.Conclusion + "\x01"))
	}
	suiteIDs := make([]string, 0, len(s.Suites))
	for id := range s.Suites {
		suiteIDs = append(suiteIDs, id)
	}
	sort.Strings(suiteIDs)
	for _, id := range suiteIDs {
		suite := s.Suites[id]
		h.Write([]byte("suite\x00" + id + "\x00" + suite.AppID + "\x00" + suite.Status + "\x00" + suite.Conclusion + "\x01"))
	}
	for _, name := range statusContexts(s.Statuses) {
		h.Write([]byte("status\x00" + name + "\x00" + s.Statuses[name].State + "\x01"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// statusContexts returns the contexts of statuses in order, nil when it holds none.
func statusContexts(statuses map[string]Status) []string {
	if len(statuses) == 0 {
		return nil
	}
	contexts := make([]string, 0, len(statuses))
	for name := range statuses {
		contexts = append(contexts, name)
	}
	sort.Strings(contexts)
	return contexts
}

type Store struct {
	mu sync.RWMutex
	// cache is the copy of the bucket: its records, revision fence, marks and read epoch
	// (kvwatch.Copy, decodeState), which every write site reaches through. A record it removes is
	// a change for the summary loop (removedLocked).
	cache kvwatch.Copy[State]
	// watcher feeds the cache and holds the handle the store writes through. Every watcher it
	// installs reads the whole bucket again (ciCache): the cache keeps every record through the
	// read and removes those the read found no evidence of only once it completes. The summary
	// loop reads only the cache (no KV fallback) and learns of its changes through the change
	// feed, so a dead watcher silently stops summaries; Ping exposes its terminal error to the
	// listener, which re-establishes the watcher or exits for Docker to restart.
	watcher *kvwatch.Watcher

	// kv is the one seam a store reaches its bucket through: every record read and write takes
	// its handle from it (write, ClaimStillHeld, HasRecord), and casState takes the same handle
	// with the epoch it fences on from the watcher itself (kvwatch.Watcher.Handle). The
	// listener's store (Open) takes the cache watcher's handle, which Rewatch moves to the new
	// connection on every reconnect, so each attempt takes the handle installed last; a writer
	// (OpenWriter) has no watcher and holds one bound handle instead.
	kv func() bus.KeyValue

	// The change feed, guarded by mu. feedAttached says a summary loop consumes it
	// (attachSummary). While it is set, changed holds the keys whose cached record changed since
	// the loop last took them (takeChanges), and cacheReset says the cache was emptied for a
	// recreated bucket since then. changedWake carries one wake-up for any number of changes; a
	// send never blocks.
	feedAttached bool
	changed      map[string]struct{}
	cacheReset   bool
	changedWake  chan struct{}

	// rewatchMu guards rewatched, which Rewatch closes and replaces. Rewatch runs on every NATS
	// reconnect and when the listener's self-health rebuilds a watcher, so a write's KV call still
	// waiting when it closes may have been sent on a connection that went away (kvCall).
	rewatchMu sync.Mutex
	rewatched chan struct{}

	// combineMu guards combiners, which holds, for each commit's record, the mutations waiting to
	// be written to it (update).
	combineMu sync.Mutex
	combiners map[string]*keyCombiner

	// logger writes the store's JSON lines, which an alarm can count (write).
	logger *logging.Logger

	// verify says the summary loop asks Dispatch's CI reconcile for a GitHub read of a head
	// before it publishes the head's settlement (WithVerify, verify.go). Off, it publishes as
	// soon as it holds the claim, as every listener before ENVOY_CI_VERIFY did.
	verify bool
	// ttl is the expiry the store opened the bucket with (WithTTL): a head given up once its last
	// observation is this old is given up for good (stopVerify). Zero, in a writer's store, which
	// gives nothing up.
	ttl time.Duration
	// now is the clock a claim is stamped by (ClaimSettlement, refreshClaim); nil is time.Now. A
	// test sets it to drive the summary loop, which is passed the same clock's time, through the
	// verify timeouts and backoffs without waiting them out.
	now func() time.Time
	// casHook, when set, runs in each write attempt after the attempt read the record and before
	// its compare-and-swap (write): a test makes another write land there, so the attempt loses
	// its compare-and-swap and retries. Nil in every store but a test's.
	casHook func(key string)
}

type openOpts struct {
	replicas int
	ttl      time.Duration
	verify   bool
}

// Option configures Open.
type Option func(*openOpts)

// WithReplicas sets the deployment's replica count (bus.ReplicasEnvVar, default 1) for the CI
// state bucket: the count a new bucket is created at, and the count an existing bucket with fewer
// copies is raised to at open (bus.EnsureKeyValue) and by the listener's keeper after it
// (bus.WithKeptBuckets). It stays there while every program of the deployment carries one count.
func WithReplicas(n int) Option {
	return func(o *openOpts) {
		if n > 0 {
			o.replicas = n
		}
	}
}

// WithTTL sets the per-key expiry so stale per-commit state auto-cleans.
func WithTTL(d time.Duration) Option {
	return func(o *openOpts) {
		if d > 0 {
			o.ttl = d
		}
	}
}

// WithVerify turns on the listener's verify-before-announce (ENVOY_CI_VERIFY=on): the summary
// loop publishes a head's settlement only after Dispatch's CI reconcile has read the head from
// GitHub after the loop asked (verify.go).
func WithVerify(on bool) Option {
	return func(o *openOpts) {
		o.verify = on
	}
}

// Open connects the CI-state KV bucket and starts the WatchAll cache. The bucket is one a
// listener copies, which bus.EnsureKeyValue creates only through a test fixture's connection
// (bus.CreateCopiedBucketsOn), and otherwise answers bus.ErrBucketMissing. Mirrors store.Open:
// the cache is populated asynchronously by watch() so Open never blocks on per-key Gets. The store
// and its cache watcher log through logger. ctx bounds the bucket's open (bus.EnsureKeyValue).
func Open(ctx context.Context, nc *nats.Conn, logger *logging.Logger, opts ...Option) (*Store, error) {
	o := openOpts{replicas: 1, ttl: bus.CIStateBucketMaxAge}
	for _, f := range opts {
		f(&o)
	}
	js, err := nc.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		return nil, err
	}
	kv, err := bus.EnsureKeyValue(ctx, nc, js, &nats.KeyValueConfig{
		Bucket:   bus.CIStateBucket,
		Replicas: o.replicas,
		Storage:  nats.FileStorage,
		TTL:      o.ttl,
	})
	if err != nil {
		return nil, err
	}
	s := &Store{
		combiners:   map[string]*keyCombiner{},
		rewatched:   make(chan struct{}),
		changedWake: make(chan struct{}, 1),
		logger:      logger,
		verify:      o.verify,
		ttl:         o.ttl,
	}
	s.cache = newCICopy(s)
	s.watcher = kvwatch.New("cistore", kv, ciCache{s}, kvwatch.WithLogger(logger.Slog()))
	// The cache watcher holds the handle, so the seam asks it on every call: Rewatch moves the
	// watcher's handle to the new connection on every reconnect. It reads the field rather than
	// holding the watcher, as the calls it replaced did, so a watcher rebuilt over the same store
	// is the one asked.
	s.kv = func() bus.KeyValue { return s.watcher.KV() }
	s.watcher.Start()
	return s, nil
}

// Writer is the CI record bucket as a process that only writes it holds it: exactly RecordRuns,
// RecordSuite, RecordStatus, HasRecord, VerifyRequests, MarkVerified and DoneVerifying, and no way
// to reach the store behind it. Everything else on Store belongs to the listener's store and would
// panic here: the cache, the summary loop, Ping, Rewatch, StopWatch, EndWatch, WaitForCacheReady
// and the settlement transitions all go through the watcher a writer has not got, so the type a
// caller is given offers none of them rather than a comment telling it which to leave alone.
type Writer struct {
	store *Store
}

// OpenWriter binds the existing CI state bucket for a process that writes records and reads none
// of them into a cache: no watcher, no cache, no change feed and no summary loop, and it creates
// nothing — the listener's Open stays the one owner of the bucket, its TTL and its copies. It
// fails with what bus.OpenKeyValue gives when the bucket does not exist, so a caller binds it
// again later rather than making one of its own.
//
// It takes the JetStream context beside the connection, as bus.OpenKeyValue does, so a caller
// binding through a bus.Client passes natsClient.JS() and natsClient.Connection() and reads both
// under the client's own lock: the client replaces both when it reconnects a closed connection,
// and reading either field directly would race that replacement. The two must come from one
// client, since the handle's watches go through the connection's own jetstream handle
// (bus.WrapKeyValue).
//
// The store behind a writer touches no cached state (only the watcher's hooks, ciCache, and
// casState write the cache, and a writer reaches neither), so a writer holds nothing per head and
// grows with nothing.
// It never calls Rewatch either, so the channel nextRewatch hands its KV calls never closes and a
// write of its own is never given up on for a rewatch (kvCall); it is given up on only when its
// caller's context ends. The handle stays usable across a reconnect, since nats.go reconnects the
// same *nats.Conn under it.
func OpenWriter(conn *nats.Conn, js nats.JetStreamContext, logger *logging.Logger) (*Writer, error) {
	kv, err := bus.OpenKeyValue(context.Background(), conn, js, bus.CIStateBucket)
	if err != nil {
		return nil, err
	}
	s := &Store{
		combiners: map[string]*keyCombiner{},
		rewatched: make(chan struct{}),
		logger:    logger,
	}
	// One handle for the store's life: nothing moves it, since a writer has no watcher to
	// rewatch. The cache stays its zero value, since nothing a writer calls reads or writes it.
	s.kv = func() bus.KeyValue { return kv }
	return &Writer{store: s}, nil
}

// RecordRuns folds every check run of one head into the head's record in one compare-and-swap
// (recordRuns), and reports whether that changed it: runs the record already holds, or ones its own
// rules refuse as older, change nothing and are no news for a caller counting what a pass did.
// The runs land together or none of them does, so a write that fails never leaves the record
// holding some of a head's finished runs without the rest.
func (w *Writer) RecordRuns(ctx context.Context, observations []contracts.CIObservation) (bool, error) {
	return w.store.recordRuns(ctx, observations)
}

// RecordSuite folds one check_suite observation into the head's record and reports whether that
// changed it, as RecordRuns does.
func (w *Writer) RecordSuite(ctx context.Context, observation contracts.CIObservation) (bool, error) {
	return w.store.recordSuite(ctx, observation)
}

// RecordStatus folds one commit status into the head's record and reports whether that changed
// it, as RecordRuns does (recordStatus). A status the record already holds, an older one than the
// record keeps for its context, and one the record's bounds refuse (write) change nothing. It
// refuses, with an error, a status with no context or in a state other than success, failure and
// error (contracts.TerminalStatusState): contracts.GithubStatusObservation maps no other, so a
// caller that sends one has skipped that mapping.
func (w *Writer) RecordStatus(ctx context.Context, observation contracts.StatusObservation) (bool, error) {
	return w.store.recordStatus(ctx, observation)
}

// HasRecord answers whether the bucket holds the record of one head.
func (w *Writer) HasRecord(owner, repo, number, sha string) (bool, error) {
	return w.store.HasRecord(owner, repo, number, sha)
}

// HasRecord answers whether the bucket holds the record of one head, with one KV Get. A caller
// that remembers what it last wrote cannot answer this from memory: the bucket's TTL expires a
// key under an unchanged head, and a record may be deleted, after which the head must be read in
// full again. It reads the bucket and never a cache, so on the listener's store it answers from
// what the bucket holds rather than from this process's own writes.
func (s *Store) HasRecord(owner, repo, number, sha string) (bool, error) {
	if _, err := s.kv().Get(Key(owner, repo, number, sha)); err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Ping verifies the KV bucket is reachable and its cache watcher is alive.
func (s *Store) Ping() error {
	if err := s.watcher.Check(); err != nil {
		return err
	}
	return s.watcher.Err()
}

// Rewatch moves the cache's watcher and the handle the store writes through to conn
// (kvwatch.Watcher.Rewatch), and then gives up on every write's KV call still waiting for an
// answer (kvCall), since it runs on every reconnect: their retries take the handle it installed.
// It gives them up even when the move fails, since after a reconnect the connection they were sent
// on went away either way. The move waits on nothing past ctx, the reconnect hook's run
// (kvwatch.Watcher.Rewatch).
func (s *Store) Rewatch(ctx context.Context, conn *nats.Conn) error {
	err := s.watcher.Rewatch(ctx, conn)
	s.rewatchMu.Lock()
	close(s.rewatched)
	s.rewatched = make(chan struct{})
	s.rewatchMu.Unlock()
	return err
}

// ciCache is the store as the cache its watcher feeds (kvwatch.Cache). The hooks are its own
// type's, not the store's methods, so nothing but the watcher calls them.
type ciCache struct{ s *Store }

// BeginRead starts a read of the CI bucket (kvwatch.Copy.BeginRead). Every record stays.
func (c ciCache) BeginRead(read kvwatch.Read) {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache.BeginRead(read)
	if read.Recreated && s.feedAttached {
		// The bucket was deleted and created again, so nothing the loop knew of it still stands:
		// it drops what it knew at its next pass (takeChanges), and this read tells it afresh.
		s.cacheReset = true
		s.changed = nil
		s.wakeSummary()
	}
}

// Apply takes one record the read delivered (kvwatch.Copy.Delivered), and logs a value it could not
// decode, which evicted the record. A record that moves the copy on and that the copy keeps is a
// change for the summary loop (markStoredLocked); a removal reports itself (removedLocked). A
// verify request (verifyKey) is Dispatch's to read, and the copy keeps no entry of it.
func (c ciCache) Apply(entry kvwatch.Entry, read kvwatch.Read) {
	if isVerifyKey(entry.Key) {
		return
	}
	s := c.s
	s.mu.Lock()
	moved := s.movedLocked(read.Epoch, entry.Key, entry.Revision)
	malformed := s.cache.Delivered(entry)
	s.markStoredLocked(entry.Key, moved)
	s.mu.Unlock()
	if malformed != nil {
		s.logger.Warn("cistore watch evicted malformed value",
			slog.String("key", entry.Key),
			slog.Uint64("revision", entry.Revision),
			slog.String("error", malformed.Error()),
		)
	}
}

// CompleteRead removes every record and fence entry the read found no evidence of: the bucket no
// longer holds it.
func (c ciCache) CompleteRead(kvwatch.Read) {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache.CompleteRead()
}

// newCICopy is the copy of the CI bucket s keeps: the one place its records are decoded, and the
// one place a record the bucket no longer holds is reported to the summary loop. Every removal
// the copy makes for the bucket's sake comes through Removed -- a delete or purge marker, a
// retired head, a record it cannot decode, and a read's sweep -- so the loop hears of each once.
func newCICopy(s *Store) kvwatch.Copy[State] {
	return kvwatch.NewCopy(kvwatch.Rows[State]{Decode: decodeState, Removed: s.removedLocked})
}

// decodeState is the state a put record of the bucket leaves in the cache. A retired head record,
// and a state stored under a key other than the one its identity builds, are not kept: the summary
// loop addresses a record by the key its identity builds, so a record stored under any other key
// could never be claimed, and it waits out its TTL uncached.
func decodeState(entry kvwatch.Entry) (State, bool, error) {
	var marker struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(entry.Value, &marker); err == nil && marker.Kind == retiredHeadKind {
		return State{}, false, nil
	}
	var st State
	if err := json.Unmarshal(entry.Value, &st); err != nil {
		return State{}, false, err
	}
	if entry.Key != Key(st.Owner, st.Repo, st.Number, st.SHA) {
		return State{}, false, nil
	}
	return st, true, nil
}

// removedLocked is what the copy records beside it when it removes a record for the bucket's sake
// (kvwatch.Rows.Removed): the key is a change for the summary loop. Callers hold s.mu.
func (s *Store) removedLocked(key string, _ State) {
	s.markChangedLocked(key)
}

// movedLocked reports whether a record of key at revision, applied through a handle of epoch,
// moves the copy on from what it holds: a key it holds nothing for, or one whose fence is older.
// A record the copy already holds at that revision is no change for the summary loop -- the
// watcher delivers every write-through again, and every read of the whole bucket delivers every
// record it holds, so counting those would settle the whole fleet after each reconnect. Callers
// hold s.mu.
func (s *Store) movedLocked(epoch uint64, key string, revision uint64) bool {
	if _, cached := s.cache.Get(key); !cached {
		return true
	}
	return revision > s.cache.FenceFor(epoch, key)
}

// markStoredLocked marks key changed for the summary loop when the record just applied moved the
// copy on (moved) and the copy kept it. A record the copy keeps none of -- a retired head, a state
// under a foreign key, a value that does not decode, a delete or purge marker -- leaves the copy
// as it was, unless it took away a record the copy held, and that removal reports itself
// (removedLocked). Marking one here would mark a key the cache holds nothing for, so every read
// of the whole bucket that streamed past one, after a reconnect or a Rewatch, would wake the loop
// for a pass with nothing to look at. Callers hold s.mu.
func (s *Store) markStoredLocked(key string, moved bool) {
	if !moved {
		return
	}
	if _, kept := s.cache.Get(key); kept {
		s.markChangedLocked(key)
	}
}

// markChangedLocked adds key to the change feed and wakes the summary loop, when one is attached.
func (s *Store) markChangedLocked(key string) {
	if !s.feedAttached {
		return
	}
	if s.changed == nil {
		s.changed = map[string]struct{}{}
	}
	s.changed[key] = struct{}{}
	s.wakeSummary()
}

// wakeSummary sends the summary loop one wake-up, unless one is already waiting.
func (s *Store) wakeSummary() {
	select {
	case s.changedWake <- struct{}{}:
	default:
	}
}

// attachSummary starts the change feed for a summary loop and returns the channel that wakes it.
// Every record the cache holds counts as changed, so the loop's first pass looks at each once. A
// store feeds one summary loop at a time: a second attach panics, since two loops would split the
// feed and leave records unsettled.
func (s *Store) attachSummary() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.feedAttached {
		panic("cistore: a second summary loop attached to one store")
	}
	s.feedAttached = true
	s.changed = make(map[string]struct{}, s.cache.Len())
	for key := range s.cache.All() {
		s.changed[key] = struct{}{}
	}
	s.wakeSummary()
	return s.changedWake
}

// detachSummary stops the change feed; a later attachSummary starts it again from the whole cache.
func (s *Store) detachSummary() {
	s.mu.Lock()
	s.feedAttached = false
	s.changed = nil
	s.cacheReset = false
	s.mu.Unlock()
}

// takeChanges returns the keys whose cached record changed since the last call, and whether the
// bucket was recreated under the cache since then, and clears both. The caller owns the map
// it returns.
func (s *Store) takeChanges() (keys map[string]struct{}, reset bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys, s.changed = s.changed, nil
	reset, s.cacheReset = s.cacheReset, false
	return keys, reset
}

// cachedState returns key's cached record, and whether the cache holds one.
func (s *Store) cachedState(key string) (State, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cache.Get(key)
}

// WatchErr is the cache watcher's terminal error, or nil while it runs.
func (s *Store) WatchErr() error {
	return s.watcher.Err()
}

// NeedsRead reports whether the copy does not follow its bucket with no read under way to make it,
// which only a Rewatch repairs (kvwatch.Watcher.NeedsRead).
func (s *Store) NeedsRead() bool {
	return s.watcher.NeedsRead()
}

// Disconnected says the connection the cache's watcher reads through was lost
// (kvwatch.Watcher.Disconnected).
func (s *Store) Disconnected() {
	s.watcher.Disconnected()
}

// StopWatch retires the cache's watcher for a shutdown (kvwatch.Watcher.Stop).
func (s *Store) StopWatch() {
	s.watcher.Stop()
}

// EndWatch ends the cache's watchers at shutdown, within ctx, while the connection still carries
// requests (kvwatch.Watcher.End).
func (s *Store) EndWatch(ctx context.Context) {
	s.watcher.End(ctx)
}

// WaitForCacheReady blocks until the watcher has delivered every existing key, or until ctx is
// done.
func (s *Store) WaitForCacheReady(ctx context.Context) error {
	return s.watcher.WaitReady(ctx)
}

// Record folds one check observation into the per-commit state via CAS, waiting for it no longer
// than ctx allows (update). It answers the error alone, which is the shape webhook.CIRecorder
// takes and all the webhook handler has a use for.
func (s *Store) Record(ctx context.Context, observation contracts.CIObservation) error {
	_, err := s.update(ctx, observation.Owner, observation.Repo, observation.Number, observation.SHA, "", func(st *State) bool {
		return applyCheckRun(st, observation)
	})
	return err
}

// recordRuns folds every check run of observations, all of one head, into that head's record as
// one mutation, so one compare-and-swap writes them all or none (update, write), and reports
// whether any of them changed the record. Each run goes through the record's own rules
// (applyCheckRun), in the order given, exactly as it would written alone. A write past the
// record's bounds is refused whole, as one webhook's observation past them is: the record is
// written as it was, marked Overflowed, so it never settles on part of the head (write, atBounds).
// Runs of more than one head are refused before anything is read, and no run is no write.
func (s *Store) recordRuns(ctx context.Context, observations []contracts.CIObservation) (bool, error) {
	if len(observations) == 0 {
		return false, nil
	}
	head := observations[0]
	for _, observation := range observations[1:] {
		if observation.Owner != head.Owner || observation.Repo != head.Repo || observation.Number != head.Number || observation.SHA != head.SHA {
			return false, fmt.Errorf("cistore: check runs of %s/%s#%s@%s and %s/%s#%s@%s in one write; a write carries the runs of one head",
				head.Owner, head.Repo, head.Number, head.SHA, observation.Owner, observation.Repo, observation.Number, observation.SHA)
		}
	}
	return s.update(ctx, head.Owner, head.Repo, head.Number, head.SHA, "", func(st *State) bool {
		changed := false
		for _, observation := range observations {
			if applyCheckRun(st, observation) {
				changed = true
			}
		}
		return changed
	})
}

// applyCheckRun folds one check run into st and reports whether that changed it. A run older than
// the one st holds of its name (checkRunIDIsOlder), and an observation of the same run that may
// not replace the held one (observationMayReplace, which ranks a finished run over a running or
// queued one), change nothing, and neither does one st already holds.
func applyCheckRun(st *State, observation contracts.CIObservation) bool {
	if st.Checks == nil {
		st.Checks = map[string]Check{}
	}
	key := observation.CheckName
	if current, ok := st.Checks[key]; ok {
		if checkRunIDIsOlder(observation.CheckRunID, uint64(current.CheckRunID)) ||
			(observation.CheckRunID == uint64(current.CheckRunID) && !observationMayReplace(observation.ObservedAt, current.ObservedAt, observation.Status, current.Status)) {
			return false
		}
	}
	next := Check{
		Name:       observation.CheckName,
		CheckRunID: checkRunID(observation.CheckRunID),
		URL:        observation.URL,
		Status:     observation.Status,
		Conclusion: observation.Conclusion,
		ObservedAt: observation.ObservedAt,
	}
	if current, ok := st.Checks[key]; ok && current == next {
		return false
	}
	st.Checks[key] = next
	rearm(st)
	st.LastEventAt = time.Now().UnixMilli()
	return true
}

// RecordSuite folds a check_suite observation into the per-commit state, waiting for it no longer
// than ctx allows (update). It answers the error alone, as Record does.
func (s *Store) RecordSuite(ctx context.Context, observation contracts.CIObservation) error {
	_, err := s.recordSuite(ctx, observation)
	return err
}

// recordSuite is the one body both forms of RecordSuite share, and reports whether the
// observation changed the head's record: an observation that repeats the stored suite changes
// nothing.
func (s *Store) recordSuite(ctx context.Context, observation contracts.CIObservation) (bool, error) {
	return s.update(ctx, observation.Owner, observation.Repo, observation.Number, observation.SHA, "", func(st *State) bool {
		if st.Suites == nil {
			st.Suites = map[string]Suite{}
		}
		next := Suite{
			ID:         observation.SuiteID,
			AppID:      observation.AppID,
			Status:     observation.Status,
			Conclusion: observation.Conclusion,
			ObservedAt: observation.ObservedAt,
		}
		if current, ok := st.Suites[observation.SuiteID]; ok {
			if !observationMayReplace(observation.ObservedAt, current.ObservedAt, observation.Status, current.Status) || current == next {
				return false
			}
		}
		st.Suites[observation.SuiteID] = next
		rearm(st)
		st.LastEventAt = time.Now().UnixMilli()
		return true
	})
}

// recordStatus folds one commit status into the head's record and reports whether that changed
// it. The record keeps the newest status of each context by GitHub's created_at (statusMayReplace),
// so an older status than the kept one changes nothing. A new context, or a newer status in
// another state, re-arms a settled head and restarts its quiet period, as a late check run does,
// so the head settles again and that settlement names it. A newer status in the same state is
// recorded, its time, link and description replacing the kept ones, and moves neither the
// snapshot (Hash) nor the quiet period, so no settlement follows it. At the record's bounds a
// status gives way and is not recorded (write). A status with no context, or in a state the record
// does not keep (contracts.TerminalStatusState), is refused before anything is read.
func (s *Store) recordStatus(ctx context.Context, observation contracts.StatusObservation) (bool, error) {
	if observation.Context == "" || !contracts.TerminalStatusState(observation.State) {
		return false, fmt.Errorf("cistore: commit status %q in state %q not recorded: the record keeps a status in success, failure or error alone",
			observation.Context, observation.State)
	}
	name := observation.Context
	return s.update(ctx, observation.Owner, observation.Repo, observation.Number, observation.SHA, name, func(st *State) bool {
		next := Status{
			Context:     name,
			State:       observation.State,
			CreatedAt:   observation.CreatedAt,
			URL:         observation.URL,
			Description: observation.Description,
		}
		current, held := st.Statuses[name]
		if held {
			if !statusMayReplace(observation.CreatedAt, current.CreatedAt) {
				return false
			}
			next.ObservedAt = current.ObservedAt
			if current == next {
				return false
			}
		}
		now := time.Now().UnixMilli()
		next.ObservedAt = now
		if st.Statuses == nil {
			st.Statuses = map[string]Status{}
		}
		st.Statuses[name] = next
		if !held || current.State != next.State {
			rearm(st)
			st.LastEventAt = now
		}
		return true
	})
}

// statusMayReplace reports whether a commit status created at incomingAt may replace the kept
// status of its context, created at storedAt: an older one may not. Every status the record keeps
// is terminal, so at an equal time, or with neither time readable, the later one read wins, as
// observationMayReplace orders two observations of one rank.
func statusMayReplace(incomingAt, storedAt string) bool {
	return observationMayReplace(incomingAt, storedAt, "completed", "completed")
}

func rearm(st *State) {
	if !st.SettledEmitted && (st.Claim == nil || st.Claim.Generation != st.Generation) {
		return
	}
	bumpGeneration(st)
	st.SettledEmitted = false
}

// bumpGeneration advances the delivery identity after a mutation invalidates a
// settlement snapshot. ClaimSettlement and MarkSettled deliberately do not
// call it: claiming reserves the current snapshot, while marking preserves the
// identity of the snapshot that was published.
func bumpGeneration(st *State) {
	st.Generation++
}

func observationMayReplace(incomingAt, storedAt, incomingStatus, storedStatus string) bool {
	incoming, incomingErr := time.Parse(time.RFC3339, incomingAt)
	stored, storedErr := time.Parse(time.RFC3339, storedAt)
	incomingValid := incomingErr == nil
	storedValid := storedErr == nil
	switch {
	case incomingValid && storedValid:
		if incoming.Before(stored) {
			return false
		}
		if incoming.After(stored) {
			return true
		}
		return statusRank(incomingStatus) >= statusRank(storedStatus)
	case !incomingValid && storedValid:
		return false
	case incomingValid:
		return true
	default:
		// A missing timestamp cannot order two observations, so preserve receipt
		// order for two timestamp-less values. A timestamp-less incoming never
		// displaces a stored observation with a usable timestamp.
		return true
	}
}

func statusRank(status string) int {
	if status == "completed" {
		return 1
	}
	return 0
}

func checkRunIDIsOlder(incoming, stored uint64) bool {
	return incoming < stored
}

// casBackoff returns a full-jitter, capped-exponential backoff for CAS retries.
func casBackoff(attempt int) time.Duration {
	base := time.Millisecond << attempt
	if base <= 0 || base > recordBackoffCap {
		base = recordBackoffCap
	}
	return time.Duration(rand.Int64N(int64(base) + 1))
}

// casState reads key's record, has apply change it, and writes it back at the revision it read,
// retrying a lost compare-and-swap within recordBudget. A written record reaches the cache only
// while the bucket the write went to is the one the watcher reads (kvwatch.Watcher.Handle): a
// revision of a bucket since recreated would fence out the new bucket's, which number from 1
// again.
//
// The first read is a direct get, which any copy of a replicated bucket near enough to current
// answers (bus.KeyValue.Get). A write over a copy's stale answer is refused by the compare-and-swap,
// but a decline is not: apply declining on a record a copy has not brought up to date would answer
// "no" for a record the bucket holds otherwise, and a mark of a published settlement that declines
// so leaves its claim standing, which a later pass reclaims and publishes again. So a decline
// stands only on the stream leader's read (bus.KeyValue.GetFromLeader), which holds every write
// the bucket took, and every read after a lost compare-and-swap is the leader's too.
func (s *Store) casState(key string, apply func(st *State) (ok bool, err error)) (State, bool, error) {
	kv, epoch := s.watcher.Handle()
	deadline := time.Now().Add(recordBudget)
	fromLeader := false
	for attempt := 0; ; attempt++ {
		var entry nats.KeyValueEntry
		var err error
		if fromLeader {
			entry, err = kv.GetFromLeader(context.Background(), key)
		} else {
			entry, err = kv.Get(key)
		}
		if err != nil {
			return State{}, false, err
		}
		var state State
		if err := json.Unmarshal(entry.Value(), &state); err != nil {
			return State{}, false, err
		}
		ok, err := apply(&state)
		if err != nil {
			return State{}, false, err
		}
		if !ok {
			if !fromLeader {
				fromLeader = true
				continue
			}
			return state, false, nil
		}
		buf, err := encodeRecord(&state)
		if err != nil {
			return State{}, false, err
		}
		revision, err := kv.Update(key, buf, entry.Revision())
		if err == nil {
			s.mu.Lock()
			moved := s.movedLocked(epoch, key, revision)
			s.cache.Put(s.watcher, epoch, key, state, revision)
			if moved {
				s.markChangedLocked(key)
			}
			s.mu.Unlock()
			return state, true, nil
		}
		if !bus.IsCASConflict(err) {
			return State{}, false, err
		}
		if time.Now().After(deadline) {
			return State{}, false, errors.New("cistore: state transition exceeded CAS budget")
		}
		fromLeader = true
		time.Sleep(casBackoff(attempt))
	}
}

// ClaimSettlement atomically acquires the right to publish a ready state
// generation without changing that generation, after re-reading the durable
// state. It applies due to the durable record, not the cache snapshot. A store
// with verify on marks the claim Verify, so every replica gives it the longer
// life a claim waiting for a GitHub read needs (claimWindow).
func (s *Store) ClaimSettlement(key, expectedHash string, expectedGeneration uint64, now int64, debounce time.Duration) (State, bool, error) {
	state, claimed, err := s.casState(key, func(state *State) (bool, error) {
		if state.Claim != nil ||
			state.Generation != expectedGeneration ||
			state.Hash() != expectedHash ||
			!due(*state, now, debounce) {
			return false, nil
		}
		state.Claim = &SettlementClaim{
			Hash:       expectedHash,
			Generation: expectedGeneration,
			ClaimedAt:  s.clock().UnixMilli(),
			Verify:     s.verify,
		}
		return true, nil
	})
	if err != nil || !claimed {
		return State{}, false, err
	}
	return state, true, nil
}

// ClaimStillHeld is the authorization point of a settlement: the last check before the summary
// loop publishes one (publish), and the one it makes on a fresh read of the durable record rather
// than on the cache. It answers whether this exact snapshot still holds the settlement right: the
// claim of generation is on the record with the snapshot hash, the record still hashes to it, and
// no settlement of it has been marked. The read is the stream leader's (bus.KeyValue.GetFromLeader):
// a copy of a replicated bucket that has not applied a reclaim, or a re-arm that replaced the
// claimed snapshot, would answer that the claim still holds, and the listener would publish a
// snapshot it no longer has the right to.
//
// requested is the revision of the verify request the claim's holder wrote for this claim
// (verify.go), and zero where it wrote none, which is a listener with ENVOY_CI_VERIFY off: that
// check is the one every listener before verify made, and now and debounce are not read. A
// request also needs the claim live at now, with more than claimMargin of its life left
// (claimLive), so an answer for a claim that has run out authorizes nothing whether or not a
// replica has reclaimed it yet.
// It also needs the record's VerifiedRev at or past requested and its VerifiedStream the identity
// of the stream the bucket is held in now (read fresh, streamIdentity), so Dispatch read the head
// from GitHub after the request, in the stream the request is of; VerifiedRev below the record's
// own revision, which holds of every answer of the record's stream, since the answer's write lands
// after the request it answers; and the record still ready to settle (settlementReady) on what
// that read merged into it. A webhook the listener fails to write after this read, and a run
// GitHub creates after it, come after the settlement, as a run created after a settlement always
// did: their observation re-arms the record when it lands, and the head settles again,
// superseding this one.
func (s *Store) ClaimStillHeld(key string, generation uint64, hash string, requested uint64, now int64, debounce time.Duration) (bool, error) {
	kv := s.kv()
	entry, err := kv.GetFromLeader(context.Background(), key)
	if err != nil {
		return false, err
	}
	var state State
	if err := json.Unmarshal(entry.Value(), &state); err != nil {
		return false, err
	}
	held := !state.SettledEmitted &&
		state.Generation == generation &&
		state.Claim != nil &&
		state.Claim.Generation == generation &&
		state.Claim.Hash == hash &&
		state.Hash() == hash
	if requested == 0 {
		return held, nil
	}
	if !held || !claimLive(state.Claim, now, debounce) || state.VerifiedRev < requested || state.VerifiedRev >= entry.Revision() || !settlementReady(state) {
		return false, nil
	}
	read, cancel := context.WithTimeout(context.Background(), recordWait)
	defer cancel()
	stream, err := streamIdentity(read, kv)
	if err != nil {
		return false, err
	}
	return state.VerifiedStream == stream, nil
}

// ReclaimSettlement releases a claim from a replica that died before publish.
// The caller determines the stale threshold from its configured debounce and
// the claim's kind (claimWindow).
func (s *Store) ReclaimSettlement(key string, generation uint64, staleBefore int64) (bool, error) {
	_, reclaimed, err := s.casState(key, func(state *State) (bool, error) {
		if state.Claim == nil ||
			state.Claim.Generation != generation ||
			(state.Generation == generation && state.Claim.ClaimedAt >= staleBefore) {
			return false, nil
		}
		state.Claim = nil
		bumpGeneration(state)
		return true, nil
	})
	return reclaimed, err
}

// ReleaseClaim clears this generation's claim after a failed publication.
func (s *Store) ReleaseClaim(key string, generation uint64) (bool, error) {
	_, released, err := s.casState(key, func(state *State) (bool, error) {
		if state.Claim == nil || state.Claim.Generation != generation {
			return false, nil
		}
		state.Claim = nil
		bumpGeneration(state)
		return true, nil
	})
	return released, err
}

// MarkSettled records a successfully published claim without changing its
// generation. A re-arm that happened after publication keeps its newer snapshot
// unsettled, but it must not erase the fact that the obsolete snapshot emitted.
func (s *Store) MarkSettled(key string, generation uint64) (bool, error) {
	_, marked, err := s.casState(key, func(state *State) (bool, error) {
		if state.Claim == nil || state.Claim.Generation != generation {
			return false, nil
		}
		state.EmittedCount++
		if state.Generation == generation && state.Claim.Hash == state.Hash() {
			state.SettledEmitted = true
		}
		state.Claim = nil
		return true, nil
	})
	return marked, err
}

// markOverflowed marks key's record overflowed, so it never settles: NATS refused its settlement,
// which it would refuse at every attempt.
func (s *Store) markOverflowed(key string) error {
	_, _, err := s.casState(key, func(state *State) (bool, error) {
		if state.Overflowed {
			return false, nil
		}
		state.Overflowed = true
		return true, nil
	})
	return err
}

// githubActionsAppID is the id of the GitHub App GitHub Actions runs as (15368), the same on every
// repository. An Actions suite that is not completed holds the head, run or none (suiteHolds). No
// path records such a suite, before or after this change: a repository or organization webhook
// receives only completed check suites, a check_run delivery records its run and not its suite,
// and Dispatch's CI reconcile skips every suite that is not completed. So the rule guards only a
// record written before this change, should one hold such a suite. Keeping a head from settling
// before its workflow's first run is a separate change.
const githubActionsAppID = "15368"

// settlementReady reports whether st can settle: every check it holds is terminal, one check has
// a run id, no suite holds it (suiteHolds), and the record holds every check of its commit, which
// an overflowed one does not.
func settlementReady(st State) bool {
	if st.Overflowed || !terminal(st) {
		return false
	}
	if !hasCheckRunID(st) {
		return false
	}
	for _, suite := range st.Suites {
		if suiteHolds(suite) {
			return false
		}
	}
	return true
}

// suiteHolds reports whether suite holds its head from settling: it is not completed, and it is
// GitHub Actions' (githubActionsAppID) or the record names no App for it, which fails safe, since
// such a suite may be Actions'; no writer records one today. Every recorded run holds the head
// until it finishes, whatever its suite (terminal). Another App's suite that is not completed
// holds nothing: GitHub creates a suite for each App that may write checks, on every push,
// whether or not the App ever adds a run, and a suite completes only through its runs, so the
// suite of an App that adds none stays queued for ever.
//
// No new suite that is not completed enters a record: the webhook route takes no requested or
// rerequested check_suite delivery (contracts.GithubCIObservations), which is the only delivery
// that carries one, and Dispatch's CI reconcile records only completed suites. So this rule
// governs the records written before that, for their seven-day life (WithTTL). A head such a
// suite held settles at the first summary pass that finds its record in the store's copy
// (attachSummary, StartSummaryLoop), with no new webhook.
func suiteHolds(suite Suite) bool {
	if suite.Status == "completed" {
		return false
	}
	return suite.AppID == "" || suite.AppID == githubActionsAppID
}

// hasCheckRunID reports whether one of st's checks has a run id; a record whose checks have none
// does not settle (settlementReady), and its held line names it (logHeldLocked).
func hasCheckRunID(st State) bool {
	for _, check := range st.Checks {
		if check.CheckRunID > 0 {
			return true
		}
	}
	return false
}

func terminal(st State) bool {
	if len(st.Checks) == 0 {
		return false
	}
	for _, check := range st.Checks {
		switch classify(check) {
		case catRunning, catQueued:
			return false
		}
	}
	return true
}
