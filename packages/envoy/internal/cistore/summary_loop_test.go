package cistore

import (
	"context"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// verdictNames names a verdict in a failure message.
var verdictNames = map[verdict]string{verdictIdle: "idle", verdictWait: "wait", verdictHeld: "held", verdictAttempt: "attempt"}

// predicates is what the predicates say of a record at one millisecond.
type predicates struct{ due, claimFresh, heldBack bool }

// heldBack reports whether st would be due but for legacyBacklog.
func heldBack(st State, now int64, debounce time.Duration) bool {
	return !st.SettledEmitted && settlementReady(st) && legacyBacklog(st, now, debounce)
}

func predicatesAt(st State, now int64, debounce time.Duration) predicates {
	return predicates{due(st, now, debounce), claimFresh(st, now, debounce), heldBack(st, now, debounce)}
}

// The summary loop queues a record by schedule's verdict and reads it again only when the verdict
// changes, so schedule must agree with the predicates a pass acts on at every millisecond: it
// attempts exactly the due records with no fresh claim, holds exactly the heldBack ones, calls a
// record idle only when no later millisecond makes it due or held, and wakes a waiting record at
// exactly the first millisecond at which the predicates change - a later wake would settle late, or
// count a held record late; an earlier one would find nothing changed.
func TestScheduleMatchesThePredicates(t *testing.T) {
	const last = int64(10_000_000)
	grace := handoverGrace.Milliseconds()
	ready := func(edits ...func(*State)) State {
		st := State{
			Owner: "example-org", Repo: "example-repo", Number: "42", SHA: "5c4ed0000000000000000000000000000000beef",
			Checks: map[string]Check{
				"build": {Name: "build", CheckRunID: 700, Status: "completed", Conclusion: "success", ObservedAt: "2026-09-25T10:00:00Z"},
			},
			Suites: map[string]Suite{
				"9001": {ID: "9001", AppID: "15368", Status: "completed", Conclusion: "success", ObservedAt: "2026-09-25T10:00:00Z"},
			},
			LastEventAt: last,
			Generation:  3,
			Schema:      recordSchema,
		}
		for _, edit := range edits {
			edit(&st)
		}
		return st
	}
	unstamped := func(st *State) { st.Schema = 0 }
	claimed := func(generation uint64, at int64) func(*State) {
		return func(st *State) { st.Claim = &SettlementClaim{Hash: "claimed", Generation: generation, ClaimedAt: at} }
	}
	const debounce = 20 * time.Millisecond
	d := last + debounce.Milliseconds()
	g := d + grace

	for _, tc := range []struct {
		name     string
		st       State
		debounce time.Duration
		want     []verdict // every verdict the windows meet
	}{
		{"settled", ready(func(st *State) { st.SettledEmitted = true; st.EmittedCount = 1 }), debounce, []verdict{verdictIdle}},
		{"not ready", ready(func(st *State) { st.Checks["build"] = Check{Name: "build", CheckRunID: 700, Status: "in_progress"} }), debounce, []verdict{verdictIdle}},
		{"overflowed", ready(func(st *State) { st.Overflowed = true }), debounce, []verdict{verdictIdle}},
		{"stamped and terminal", ready(), debounce, []verdict{verdictWait, verdictAttempt}},
		{"unstamped and terminal", ready(unstamped), debounce, []verdict{verdictWait, verdictAttempt, verdictHeld}},
		{"a fresh claim of the record's generation", ready(claimed(3, d+10)), debounce, []verdict{verdictWait, verdictAttempt}},
		{"a claim of an older generation", ready(claimed(2, d+10)), debounce, []verdict{verdictWait, verdictAttempt}},
		{"unstamped with a fresh claim that outlasts the grace", ready(unstamped, claimed(3, g-10)), debounce, []verdict{verdictWait, verdictHeld}},
		{"no debounce", ready(), 0, []verdict{verdictWait, verdictAttempt}},
		{"no debounce, unstamped and claimed", ready(unstamped, claimed(3, last+5)), 0, []verdict{verdictWait, verdictAttempt, verdictHeld}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wait := tc.debounce.Milliseconds()
			marks := []int64{last + wait, last + wait + grace}
			if claim := tc.st.Claim; claim != nil {
				marks = append(marks, claim.ClaimedAt, claim.ClaimedAt+2*wait+1)
			}
			// far is past every mark: an idle record must stay idle there too.
			far := last + 10*grace
			seen := map[verdict]bool{}
			for _, mark := range marks {
				for now := mark - 50; now <= mark+50; now++ {
					v, at := schedule(tc.st, now, tc.debounce)
					seen[v] = true
					p := predicatesAt(tc.st, now, tc.debounce)
					if attempt := p.due && !p.claimFresh; (v == verdictAttempt) != attempt {
						t.Fatalf("at %d: schedule = %s, but due && !claimFresh = %v", now, verdictNames[v], attempt)
					}
					if (v == verdictHeld) != p.heldBack {
						t.Fatalf("at %d: schedule = %s, but heldBack = %v", now, verdictNames[v], p.heldBack)
					}
					switch v {
					case verdictIdle:
						for _, later := range append(append([]int64(nil), marks...), far) {
							for _, q := range []int64{later - 1, later, later + 1} {
								if q < now {
									continue
								}
								if p := predicatesAt(tc.st, q, tc.debounce); p.due || p.heldBack {
									t.Fatalf("at %d: schedule = idle, but at %d the record is %+v", now, q, p)
								}
							}
						}
					case verdictWait:
						if at <= now {
							t.Fatalf("at %d: schedule waits until %d, not a later millisecond", now, at)
						}
						if before, beforeAt := schedule(tc.st, at-1, tc.debounce); before != verdictWait || beforeAt != at {
							t.Fatalf("at %d: schedule waits until %d, but at %d it is %s until %d", now, at, at-1, verdictNames[before], beforeAt)
						}
						if after, afterAt := schedule(tc.st, at, tc.debounce); after == verdictWait && afterAt == at {
							t.Fatalf("at %d: schedule waits until %d, and at %d it still waits until %d", now, at, at, afterAt)
						}
						if before, after := predicatesAt(tc.st, at-1, tc.debounce), predicatesAt(tc.st, at, tc.debounce); before == after {
							t.Fatalf("at %d: schedule waits until %d, but nothing changes there: %+v at %d and at %d", now, at, before, at-1, at)
						}
					}
				}
			}
			for _, v := range tc.want {
				if !seen[v] {
					t.Errorf("the windows never met verdict %s", verdictNames[v])
				}
			}
			if len(seen) != len(tc.want) {
				t.Errorf("the windows met %d verdicts, want %d", len(seen), len(tc.want))
			}
		})
	}
}

// publishTimes records the wall-clock millisecond of each publication pub makes.
func publishTimes(pub *recPub) func() []int64 {
	var mu sync.Mutex
	var at []int64
	pub.onPublish = func(contracts.Envelope) {
		mu.Lock()
		at = append(at, time.Now().UnixMilli())
		mu.Unlock()
	}
	return func() []int64 {
		mu.Lock()
		defer mu.Unlock()
		return append([]int64(nil), at...)
	}
}

// waitPublished polls until pub has published n envelopes, failing once the wall clock passes by
// (milliseconds).
func waitPublished(t *testing.T, pub *recPub, n int, what string) {
	t.Helper()
	testwait.Eventually(t, what, func() bool { return pub.count() >= n })
}

// gaugeReads is the value met serves on /metrics for the gauge name.
func gaugeReads(t *testing.T, met *metrics.Registry, name string) int64 {
	t.Helper()
	recorder := httptest.NewRecorder()
	met.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if value, ok := strings.CutPrefix(line, name+" "); ok {
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				t.Fatalf("gauge line %q: %v", line, err)
			}
			return n
		}
	}
	t.Fatalf("/metrics serves no gauge %s:\n%s", name, recorder.Body.String())
	return 0
}

// A record that falls due while nothing else changes settles on the first tick at or after its
// debounce runs out: the loop wakes for the deadline itself, not for a write.
func TestARecordFallingDueSettlesWithNoOtherChange(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	published := publishTimes(pub)
	const (
		debounce = 300 * time.Millisecond
		tick     = 100 * time.Millisecond
		owner    = "example-org"
		repo     = "example-repo"
		number   = "42"
		sha      = "abcdef1234567890abcdef1234567890abcdef12"
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartSummaryLoop(ctx, store, pub, debounce, tick, metrics.New().NewGauge("held", ""), logging.New("test"))

	if err := recordCheck(store, owner, repo, number, sha, "build", "820", "https://example.test/820", "completed", "success", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record the terminal check: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)
	due := getState(t, store, owner, repo, number, sha).LastEventAt + debounce.Milliseconds()

	waitPublished(t, pub, 1, "the record falling due")
	if at := published()[0]; at < due {
		t.Fatalf("the record settled at %d, before its debounce ran out at %d", at, due)
	}
	if got := pub.count(); got != 1 {
		t.Fatalf("the record published %d settlements, want 1", got)
	}
	if !getState(t, store, owner, repo, number, sha).SettledEmitted {
		t.Fatal("the record was not marked settled")
	}
}

// A claim a replica took and never released - the replica died - is reclaimed once it goes stale,
// and the record settles once, with nothing else written to wake the loop.
func TestAStaleClaimIsReclaimedWithNoOtherChange(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	published := publishTimes(pub)
	const (
		debounce = 200 * time.Millisecond
		tick     = 20 * time.Millisecond
		owner    = "example-org"
		repo     = "example-repo"
		number   = "42"
		sha      = "abcdef1234567890abcdef1234567890abcdef12"
	)
	if err := recordCheck(store, owner, repo, number, sha, "build", "821", "https://example.test/821", "completed", "success", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record the terminal check: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	state := getState(t, store, owner, repo, number, sha)
	claimedAt := time.Now().UnixMilli()
	setClaim(t, store, owner, repo, number, sha, &SettlementClaim{Hash: state.Hash(), Generation: state.Generation, ClaimedAt: claimedAt})
	stale := claimedAt + (2 * debounce).Milliseconds() + 1

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartSummaryLoop(ctx, store, pub, debounce, tick, metrics.New().NewGauge("held", ""), logging.New("test"))

	// The reclaim and the claim that follows it take two passes.
	waitPublished(t, pub, 1, "the record behind a dead replica's claim")
	if at := published()[0]; at < stale {
		t.Fatalf("the record settled at %d, while its claim was fresh until %d", at, stale)
	}
	// The loop publishes the settlement before its write that marks the record settled and
	// clears the claim lands, so the row waits for that write rather than reading the record as
	// the publish leaves it.
	settled := testwait.Until(t, "the record to read settled with no claim", func() (State, bool) {
		state := getState(t, store, owner, repo, number, sha)
		return state, state.SettledEmitted && state.Claim == nil
	})
	if got := pub.count(); got != 1 {
		t.Fatalf("the record published %d settlements, want 1; final state = %+v", got, settled)
	}
}

// An unsettled record a head-gated listener left inside the handover grace, which a fresh claim
// keeps from settling, is counted as held once the grace ends, with nothing written to wake the
// loop.
func TestARecordPastTheGraceIsCountedWithNoOtherChange(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	const (
		debounce = time.Second
		tick     = 20 * time.Millisecond
		gauge    = "envoy_ci_legacy_records_held"
	)
	now := time.Now().UnixMilli()
	// The grace ends 400 ms from now; the claim stays fresh for 2 s.
	graceEnds := now + 400
	putHeadGatedRecord(t, store, legacyHead, graceEnds-(debounce+handoverGrace).Milliseconds(), func(st *State) {
		st.Claim = &SettlementClaim{Hash: "claimed", Generation: st.Generation, ClaimedAt: now}
	})
	met := metrics.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartSummaryLoop(ctx, store, pub, debounce, tick, met.NewGauge(gauge, "held"), logging.New("test"))

	testwait.Eventually(t, "the grace ends and the record is counted as held", func() bool { return gaugeReads(t, met, gauge) == 1 })
	if at := time.Now().UnixMilli(); at < graceEnds {
		t.Fatalf("the gauge read 1 at %d, before the grace ended at %d", at, graceEnds)
	}
	if got := pub.count(); got != 0 {
		t.Fatalf("the claimed record published %d settlements, want none", got)
	}
}

// With nothing changed and nothing falling due, the loop runs no pass: not on its ticks, and not
// when a Rewatch replays every record at the revision the cache holds. A change wakes it again.
// Every pass sets the held gauge, so a value the test writes there survives only while no pass
// runs.
func TestTheSummaryLoopSleepsWhileNothingChanges(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	const (
		debounce = time.Hour
		tick     = 10 * time.Millisecond
		gauge    = "envoy_ci_legacy_records_held"
		noPass   = -1
	)
	now := time.Now()
	settled := func(st *State) { st.SettledEmitted = true; st.EmittedCount = 1 }
	putHeadGatedRecord(t, store, head{"example-org", "example-repo", "60", "5e7713000000000000000000000000000000beef"}, now.Add(-time.Minute).UnixMilli(), settled)
	putHeadGatedRecord(t, store, head{"example-org", "example-repo", "61", "5e7714000000000000000000000000000000beef"}, now.Add(-2*time.Minute).UnixMilli(), settled)
	waiting := head{"example-org", "example-repo", "62", "3a171e000000000000000000000000000000beef"}
	putHeadGatedRecord(t, store, waiting, now.UnixMilli(), nil)
	putHeadGatedRecord(t, store, head{"example-org", "example-repo", "63", "3a171f000000000000000000000000000000beef"}, now.UnixMilli(), nil)

	met := metrics.New()
	held := met.NewGauge(gauge, "held")
	held.Set(noPass)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartSummaryLoop(ctx, store, &recPub{}, debounce, tick, held, logging.New("test"))
	waitGauge := func(want int64, what string) {
		t.Helper()
		testwait.Eventually(t, what, func() bool { return gaugeReads(t, met, gauge) == want })
	}
	waitGauge(0, "the first pass")

	held.Set(noPass)
	testwait.Span(t, 50*tick, "no record changed and none is due, and the loop writes nothing while it sleeps, so only waiting the window and finding the gauge untouched shows it ran no pass")
	if got := gaugeReads(t, met, gauge); got != noPass {
		t.Fatalf("a pass ran (gauge %d) during 50 ticks with nothing changed", got)
	}

	if err := store.Rewatch(context.Background(), conn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	// The new watcher delivers every record before a key written after it started; a retired head
	// record is never cached, so it changes nothing for the loop. Once its revision is applied, the
	// replay has been.
	marker := head{"example-org", "example-repo", "64", "ead000000000000000000000000000000000beef"}.key()
	revision, err := store.watcher.KV().Put(marker, []byte(`{"kind":"`+retiredHeadKind+`"}`))
	if err != nil {
		t.Fatalf("store the marker: %v", err)
	}
	testwait.Eventually(t, "the rewatched cache applies the marker", func() bool {
		store.mu.RLock()
		defer store.mu.RUnlock()
		return store.cache.FenceFor(store.watcher.Epoch(), marker) == revision
	})
	testwait.Span(t, 50*tick, "a Rewatch replays the records unchanged and the loop writes nothing while it sleeps, so only waiting the window and finding the gauge untouched shows the replay woke no pass")
	if got := gaugeReads(t, met, gauge); got != noPass {
		t.Fatalf("a pass ran (gauge %d) after a Rewatch replayed the records unchanged", got)
	}

	setLastEventAt(t, store, waiting.owner, waiting.repo, waiting.number, waiting.sha, time.Now().UnixMilli())
	waitGauge(0, "the pass after one record changed")
}

// A settlement whose publication fails is retried at the next tick, not at once: each attempt
// writes the record (its claim, then its release), and a loop that ran a pass for each such change
// would retry as fast as the store writes.
func TestAFailingPublisherIsRetriedOncePerTick(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	fake := clocktest.New(time.Now())
	store := openStore(t, conn, WithClock(fake))
	// NATS fails every publication while it is unreachable.
	pub := &failingPub{err: errors.New("nats unavailable")}
	const (
		tick   = 50 * time.Millisecond
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)
	if err := recordCheck(store, owner, repo, number, sha, "build", "822", "https://example.test/822", "completed", "success", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record the terminal check: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartSummaryLoop(ctx, store, pub, 0, tick, metrics.New().NewGauge("held", ""), logging.New("test"))

	// Each tick runs one pass, which attempts the settlement once; the publisher fails it, so it is
	// retried at the next tick, not at once. Drive exactly passes ticks on the loop's clock and
	// expect exactly that many attempts, one a tick.
	const passes = 5
	for i := range passes {
		fake.AwaitArmed(t, "the summary loop's next tick", 1)
		fake.Advance(tick)
		testwait.Eventually(t, "the pass of this tick attempts its settlement", func() bool { return pub.count() >= i+1 })
	}
	if got := pub.count(); got != passes {
		t.Fatalf("%d publication attempts over %d ticks, want one a tick", got, passes)
	}
}

// recreateCIBucket makes the CI bucket again with the config openStore opens it with, so a test that
// deleted it finds it as the store expects. It holds the config apart from the tests that recreate
// the bucket, which also keeps a test's own count assertions clear of its TTL.
func recreateCIBucket(t *testing.T, js natsgo.JetStreamContext) {
	t.Helper()
	testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: bus.CIStateBucket, Replicas: 1, Storage: natsgo.FileStorage, TTL: time.Hour})
}

// A CI bucket deleted and created again holds none of the old bucket's records, and the store
// empties its cache for it, so the loop forgets what it knew of the old records too. The pass after
// the recreate counts as held only the new bucket's record, not the two the old bucket's pass held
// back, and keeps no queued deadline for the old bucket's waiting record: its queue is empty.
func TestARecreatedBucketForgetsTheOldBucketsRecords(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	logger := logging.New("test")
	// Long enough that the waiting record never falls due while the test runs.
	const debounce = time.Hour
	z := testSummarizer(store, debounce)
	stamped := func(st *State) { st.Schema = recordSchema }
	past := time.Now().Add(-debounce - handoverGrace - time.Minute).UnixMilli()
	putHeadGatedRecord(t, store, head{"example-org", "example-repo", "70", "01d070000000000000000000000000000000beef"}, past, nil)
	putHeadGatedRecord(t, store, head{"example-org", "example-repo", "71", "01d071000000000000000000000000000000beef"}, past, nil)
	queuedAt := time.Now().UnixMilli()
	putHeadGatedRecord(t, store, head{"example-org", "example-repo", "72", "9e0e72000000000000000000000000000000beef"}, queuedAt, stamped)

	held, next, queued := z.pass(pub, logger, time.Now().UnixMilli())
	if held != 2 {
		t.Fatalf("the pass over the old bucket held back %d records, want 2", held)
	}
	if due := queuedAt + debounce.Milliseconds(); !queued || next != due {
		t.Fatalf("the pass over the old bucket left next %d queued %v, want the waiting record queued at %d", next, queued, due)
	}
	if got := pub.count(); got != 0 {
		t.Fatalf("the pass over the old bucket published %d settlements, want none", got)
	}

	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue(bus.CIStateBucket); err != nil {
		t.Fatalf("delete the bucket: %v", err)
	}
	// The config openStore opens the bucket with.
	recreateCIBucket(t, js)
	if err := store.Rewatch(context.Background(), conn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	fresh := head{"example-org", "example-repo", "73", "f2e573000000000000000000000000000000beef"}
	putHeadGatedRecord(t, store, fresh, past, nil)
	if cached := store.List(); len(cached) != 1 {
		t.Fatalf("the cache after the recreate holds %d records, want only the new bucket's %s", len(cached), fresh.key())
	}

	held, next, queued = z.pass(pub, logger, time.Now().UnixMilli())
	if held != 1 {
		t.Fatalf("the pass over the recreated bucket held back %d records, want 1, the new bucket's", held)
	}
	if queued {
		t.Fatalf("the pass over the recreated bucket left a deadline queued at %d, want an empty queue", next)
	}
	if got := pub.count(); got != 0 {
		t.Fatalf("the pass over the recreated bucket published %d settlements, want none", got)
	}
}
