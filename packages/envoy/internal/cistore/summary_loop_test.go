package cistore

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// verdictNames names a verdict in a failure message.
var verdictNames = map[verdict]string{verdictIdle: "idle", verdictWait: "wait", verdictHeld: "held", verdictStuck: "stuck", verdictAttempt: "attempt"}

// predicates is what the predicates say of a record at one millisecond.
type predicates struct{ due, claimFresh, heldBack, stuck bool }

// heldBack reports whether st would be due but for legacyBacklog.
func heldBack(st State, now int64, debounce time.Duration) bool {
	return !st.SettledEmitted && settlementReady(st) && legacyBacklog(st, now, debounce)
}

// stuck reports whether st is unsettled, not ready and has had no event for heldAfter by now: the
// pass names what holds it.
func stuck(st State, now int64) bool {
	return !st.SettledEmitted && !settlementReady(st) && now-st.LastEventAt >= heldAfter.Milliseconds()
}

func predicatesAt(st State, now int64, debounce time.Duration) predicates {
	return predicates{due(st, now, debounce), claimFresh(st, now, debounce), heldBack(st, now, debounce), stuck(st, now)}
}

// The summary loop queues a record by schedule's verdict and reads it again only when the verdict
// changes, so schedule must agree with the predicates a pass acts on at every millisecond: it
// attempts exactly the due records with no fresh claim, holds exactly the heldBack ones, calls
// exactly the stuck ones stuck, calls a record idle or stuck only when no later millisecond makes
// it due or held or changes whether it is stuck, and wakes a waiting record at exactly the first
// millisecond at which the predicates change - a later wake would settle late, count a held record
// late, or name a held head late; an earlier one would find nothing changed. A record that is not
// ready waits until heldAfter past its last event and is then stuck, which the pass files idle.
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

	// Reasons are part of the observer contract, not arbitrary labels on a verdict. Pin each
	// one and its spelling independently so an attempted settlement cannot be reported idle.
	stopped := ready()
	stopped.VerifyStopped = stopped.Hash()
	retrying := stopped
	retrying.VerifyRetryAt = d + 200
	verifying := ready(claimed(3, d+10))
	verifying.Claim.Verify = true
	for _, tc := range []struct {
		name    string
		state   State
		now     int64
		reason  scheduleReason
		label   string
		verdict verdict
	}{
		{"settled", ready(func(st *State) { st.SettledEmitted = true }), d, reasonIdle, "idle", verdictIdle},
		{"given up", stopped, d, reasonIdle, "idle", verdictIdle},
		{"retry", retrying, d, reasonRetry, "retry", verdictWait},
		{"not ready", ready(func(st *State) { st.Checks["build"] = Check{Name: "build", Status: "in_progress"} }), d, reasonNotReady, "not-ready", verdictWait},
		{"debounce", ready(), last, reasonDebounce, "debounce", verdictWait},
		{"held", ready(unstamped), g + 1, reasonHeld, "held", verdictHeld},
		{"stuck", ready(func(st *State) { st.Checks["build"] = Check{Name: "build", Status: "in_progress"} }), last + heldAfter.Milliseconds(), reasonStuck, "stuck", verdictStuck},
		{"claim", ready(claimed(3, d+10)), d + 10, reasonClaim, "claim", verdictWait},
		{"verify", verifying, d + 10, reasonVerify, "verify", verdictWait},
		{"attempt", ready(), d, reasonAttempt, "attempt", verdictAttempt},
	} {
		t.Run("reason/"+tc.name, func(t *testing.T) {
			reason, _ := schedule(tc.state, tc.now, debounce)
			if reason != tc.reason || reason.String() != tc.label {
				t.Fatalf("schedule reason = %d (%q), want %d (%q)", reason, reason.String(), tc.reason, tc.label)
			}
			if got := reason.verdict(); got != tc.verdict {
				t.Fatalf("reason %s acts as %s, want %s", reason, verdictNames[got], verdictNames[tc.verdict])
			}
		})
	}

	for _, tc := range []struct {
		name     string
		st       State
		debounce time.Duration
		want     []verdict // every verdict the windows meet
	}{
		{"settled", ready(func(st *State) { st.SettledEmitted = true; st.EmittedCount = 1 }), debounce, []verdict{verdictIdle}},
		{"not ready", ready(func(st *State) { st.Checks["build"] = Check{Name: "build", CheckRunID: 700, Status: "in_progress"} }), debounce, []verdict{verdictWait, verdictStuck}},
		{"not ready and unstamped", ready(unstamped, func(st *State) { st.Suites["9002"] = Suite{ID: "9002", AppID: githubActionsAppID, Status: "queued"} }), debounce, []verdict{verdictWait, verdictStuck}},
		{"another App's empty suite beside the finished run", ready(func(st *State) {
			st.Suites["9003"] = Suite{ID: "9003", AppID: "5789269", Status: "queued"}
		}), debounce, []verdict{verdictWait, verdictAttempt}},
		{"overflowed", ready(func(st *State) { st.Overflowed = true }), debounce, []verdict{verdictWait, verdictStuck}},
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
			marks := []int64{last + wait, last + wait + grace, last + heldAfter.Milliseconds()}
			if claim := tc.st.Claim; claim != nil {
				marks = append(marks, claim.ClaimedAt, claim.ClaimedAt+2*wait+1)
			}
			// far is past every mark: an idle or stuck record must stay so there too.
			far := last + 10*grace
			seen := map[verdict]bool{}
			for _, mark := range marks {
				for now := mark - 50; now <= mark+50; now++ {
					reason, at := schedule(tc.st, now, tc.debounce)
					v := reason.verdict()
					seen[v] = true
					p := predicatesAt(tc.st, now, tc.debounce)
					if attempt := p.due && !p.claimFresh; (v == verdictAttempt) != attempt {
						t.Fatalf("at %d: schedule = %s, but due && !claimFresh = %v", now, verdictNames[v], attempt)
					}
					if (v == verdictHeld) != p.heldBack {
						t.Fatalf("at %d: schedule = %s, but heldBack = %v", now, verdictNames[v], p.heldBack)
					}
					if (v == verdictStuck) != p.stuck {
						t.Fatalf("at %d: schedule = %s, but stuck = %v", now, verdictNames[v], p.stuck)
					}
					switch v {
					case verdictIdle, verdictStuck:
						for _, later := range append(append([]int64(nil), marks...), far) {
							for _, q := range []int64{later - 1, later, later + 1} {
								if q < now {
									continue
								}
								if p := predicatesAt(tc.st, q, tc.debounce); p.due || p.heldBack || p.stuck != (v == verdictStuck) {
									t.Fatalf("at %d: schedule = %s, but at %d the record is %+v", now, verdictNames[v], q, p)
								}
							}
						}
					case verdictWait:
						if at <= now {
							t.Fatalf("at %d: schedule waits until %d, not a later millisecond", now, at)
						}
						if before, beforeAt := schedule(tc.st, at-1, tc.debounce); before.verdict() != verdictWait || beforeAt != at {
							t.Fatalf("at %d: schedule waits until %d, but at %d it is %s until %d", now, at, at-1, verdictNames[before.verdict()], beforeAt)
						}
						if after, afterAt := schedule(tc.st, at, tc.debounce); after.verdict() == verdictWait && afterAt == at {
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

// loopPasses counts the passes a summary loop ended (WithSummaryObserver), so a row that moved the
// loop's clock waits for the pass the move fired to end.
type loopPasses struct {
	mu    sync.Mutex
	ended int
}

func (p *loopPasses) SummaryScheduled(string, State, string) {}

func (p *loopPasses) SummaryPassEnded() {
	p.mu.Lock()
	p.ended++
	p.mu.Unlock()
}

func (p *loopPasses) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ended
}

// moveTicks moves fake n ticks, one at a time, and after each move that reached an instant the
// loop had armed waits for the pass that move fired to end (passes), so each pass the ticks ran has
// ended, and published what it publishes, when it returns.
func moveTicks(t *testing.T, fake *clocktest.Fake, passes *loopPasses, tick time.Duration, n int) {
	t.Helper()
	for range n {
		ended := passes.count()
		target := fake.Now().Add(tick)
		armed, _ := fake.SetWhen(func(time.Time, []time.Time) (time.Time, bool) { return target, true })
		if len(armed) > 0 && !armed[0].After(target) {
			testwait.Until(t, "the summary pass the move to "+target.Format(time.RFC3339Nano)+" fired to end", func() (int, bool) {
				got := passes.count() - ended
				return got, got >= 1
			})
		}
	}
}

// gridInstant is the first instant start + k*tick of a summary loop's grid at or after the
// millisecond at, read apart from the loop's own arithmetic (gridIndexAtOrAfter).
func gridInstant(start time.Time, tick time.Duration, at int64) time.Time {
	k := (time.UnixMilli(at).Sub(start) + tick - 1) / tick
	return start.Add(k * tick)
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

const (
	// loopDebounce and loopTick are the summary loop's debounce and tick as cmd/listener runs it:
	// ENVOY_CI_DEBOUNCE's default and the loop's one-second tick.
	loopDebounce = 5 * time.Second
	loopTick     = time.Second
)

// loopStart is where the fake of a row that runs the loop at loopDebounce and loopTick stands as
// the row starts the loop, so the loop's grid starts there.
var loopStart = time.Date(2026, 9, 7, 3, 0, 0, 0, time.UTC)

// A record that falls due while nothing else changes settles on the first tick at or after its
// debounce runs out: the loop wakes for the deadline itself, not for a write. The loop runs at
// cmd/listener's debounce and tick on a fake clock the test moves to each instant the loop arms,
// once it has armed it, and the record is stamped where the fake stands as the loop starts.
func TestARecordFallingDueSettlesWithNoOtherChange(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	fake := clocktest.New(loopStart)
	passes := &loopPasses{}
	store := openStore(t, conn, WithClock(fake), WithSummaryObserver(passes))
	pub := &recPub{}
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartSummaryLoop(ctx, store, pub, loopDebounce, loopTick, metrics.New().NewGauge("held", ""), logging.New("test"))

	if err := recordCheck(store, owner, repo, number, sha, "build", "820", "https://example.test/820", "completed", "success", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record the terminal check: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)
	due := getState(t, store, owner, repo, number, sha).LastEventAt + loopDebounce.Milliseconds()

	// The loop's grid runs from loopStart. Its first pass finds the record inside its debounce,
	// and the loop arms the first instant at or after the debounce's end with no write to wake it.
	fake.AwaitArmedAt(t, "the loop's first pass", loopStart.Add(loopTick))
	fake.Set(loopStart.Add(loopTick))
	dueAt := gridInstant(loopStart, loopTick, due)
	fake.AwaitArmedAt(t, "the loop's pass at the first instant past the record's debounce", dueAt)
	if got := pub.count(); got != 0 {
		t.Fatalf("the record published %d settlements before its debounce ran out at %d, want none", got, due)
	}
	fake.Set(dueAt)
	// The pass there attempts the record, and the loop arms the next instant to look at it again.
	fake.AwaitArmedAt(t, "the loop's look at the record after its attempt", dueAt.Add(loopTick))
	if got := pub.count(); got != 1 {
		t.Fatalf("the record falling due published %d settlements on the first tick at or after its debounce ran out at %d, want 1", got, due)
	}
	// Three more ticks, the look after the attempt among them, once the cache holds the record
	// the settlement marked.
	cachedAsStored(t, store, head{owner, repo, number, sha})
	moveTicks(t, fake, passes, loopTick, 3)
	if got := pub.count(); got != 1 {
		t.Fatalf("the record published %d settlements, want 1", got)
	}
	if !getState(t, store, owner, repo, number, sha).SettledEmitted {
		t.Fatal("the record was not marked settled")
	}
}

// A claim a replica took and never released - the replica died - is reclaimed once it goes stale,
// and the record settles once, with nothing else written to wake the loop. The loop runs at
// cmd/listener's debounce and tick on a fake clock the test moves to each instant the loop arms,
// once it has armed it, and the claim is stamped where the fake stands as the loop starts.
func TestAStaleClaimIsReclaimedWithNoOtherChange(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	fake := clocktest.New(loopStart)
	passes := &loopPasses{}
	store := openStore(t, conn, WithClock(fake), WithSummaryObserver(passes))
	pub := &recPub{}
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)
	h := head{owner, repo, number, sha}
	if err := recordCheck(store, owner, repo, number, sha, "build", "821", "https://example.test/821", "completed", "success", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record the terminal check: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	state := getState(t, store, owner, repo, number, sha)
	claimedAt := fake.Now().UnixMilli()
	setClaim(t, store, owner, repo, number, sha, &SettlementClaim{Hash: state.Hash(), Generation: state.Generation, ClaimedAt: claimedAt})
	stale := claimedAt + (2 * loopDebounce).Milliseconds() + 1
	// The loop's first pass reads the claim, so the cache holds it before the loop starts.
	cachedAsStored(t, store, h)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartSummaryLoop(ctx, store, pub, loopDebounce, loopTick, metrics.New().NewGauge("held", ""), logging.New("test"))

	// The loop's grid runs from loopStart. Its first pass finds the claim fresh, and the loop arms
	// the first instant at or after the claim goes stale with no write to wake it.
	fake.AwaitArmedAt(t, "the loop's first pass", loopStart.Add(loopTick))
	fake.Set(loopStart.Add(loopTick))
	reclaimAt := gridInstant(loopStart, loopTick, stale)
	fake.AwaitArmedAt(t, "the loop's pass at the first instant past the claim's staleness", reclaimAt)
	if got := pub.count(); got != 0 {
		t.Fatalf("the record published %d settlements while its claim was fresh until %d, want none", got, stale)
	}
	// The reclaim and the claim that follows it take two passes: the reclaim moves the record's
	// generation, so the claim the same pass then asks for is refused, and the loop arms the next
	// instant to look at the record again, where it reads the reclaimed record once the cache
	// holds it.
	fake.Set(reclaimAt)
	settleAt := reclaimAt.Add(loopTick)
	fake.AwaitArmedAt(t, "the loop's look at the record after the reclaim", settleAt)
	cachedAsStored(t, store, h)
	fake.Set(settleAt)
	fake.AwaitArmedAt(t, "the loop's look at the record after its attempt", settleAt.Add(loopTick))
	if got := pub.count(); got != 1 {
		t.Fatalf("the record behind a dead replica's claim published %d settlements by the second tick at or after its claim went stale at %d, want 1", got, stale)
	}
	// Three more ticks, the look after the attempt among them, once the cache holds the record
	// the settlement marked.
	cachedAsStored(t, store, h)
	moveTicks(t, fake, passes, loopTick, 3)
	if got := pub.count(); got != 1 {
		t.Fatalf("the record published %d settlements, want 1", got)
	}
	if settled := getState(t, store, owner, repo, number, sha); !settled.SettledEmitted || settled.Claim != nil {
		t.Fatalf("final state = %+v, want settled with no claim", settled)
	}
}

// An unsettled record a head-gated listener left inside the handover grace, which a fresh claim
// keeps from settling, is counted as held once the grace ends, with nothing written to wake the
// loop. The loop runs at cmd/listener's debounce and tick on a fake clock the test moves to each
// instant the loop arms, once it has armed it. Every pass sets the held gauge, so the value the
// test writes there first survives only until the first pass.
func TestARecordPastTheGraceIsCountedWithNoOtherChange(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	fake := clocktest.New(loopStart)
	store := openStore(t, conn, WithClock(fake))
	pub := &recPub{}
	const (
		gauge  = "envoy_ci_legacy_records_held"
		noPass = -1
	)
	now := loopStart.UnixMilli()
	// The grace ends 3.5 s from now, between two instants of the loop's grid; the claim stays
	// fresh for 10 s.
	graceEnds := now + 3500
	putHeadGatedRecord(t, store, legacyHead, graceEnds-(loopDebounce+handoverGrace).Milliseconds(), func(st *State) {
		st.Claim = &SettlementClaim{Hash: "claimed", Generation: st.Generation, ClaimedAt: now}
	})
	met := metrics.New()
	held := met.NewGauge(gauge, "held")
	held.Set(noPass)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartSummaryLoop(ctx, store, pub, loopDebounce, loopTick, held, logging.New("test"))

	// The loop's grid runs from loopStart. Its first pass holds nothing back, since the grace has
	// not ended, and the loop arms the first instant at or after the grace's end with no write to
	// wake it; the gauge is set before that arm.
	fake.AwaitArmedAt(t, "the loop's first pass", loopStart.Add(loopTick))
	fake.Set(loopStart.Add(loopTick))
	graceAt := gridInstant(loopStart, loopTick, graceEnds)
	fake.AwaitArmedAt(t, "the loop's pass at the first instant past the grace", graceAt)
	if got := gaugeReads(t, met, gauge); got != 0 {
		t.Fatalf("the gauge reads %d after the loop's pass before the grace ended at %d, want 0 held", got, graceEnds)
	}
	fake.Set(graceAt)
	testwait.Until(t, fmt.Sprintf("the gauge to read 1 held on the loop's pass at the first tick after the grace ended at %d", graceEnds), func() (int64, bool) {
		got := gaugeReads(t, met, gauge)
		return got, got == 1
	})
	if got := pub.count(); got != 0 {
		t.Fatalf("the claimed record published %d settlements, want none", got)
	}
}

// With nothing changed and nothing falling due, the loop runs no pass: not on its ticks, and not
// when a Rewatch replays every record at the revision the cache holds. A change wakes it again.
// Every pass sets the held gauge, so a value the test writes there survives only while no pass
// runs. The loop runs on a fake clock the test moves a tick at a time, and a pass runs only once a
// move reaches an instant the loop armed: after a move that reached none the gauge is read as the
// loop left it, and after one that reached one, once that pass has set it.
func TestTheSummaryLoopSleepsWhileNothingChanges(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	fake := clocktest.New(now)
	store := openStore(t, conn, WithClock(fake))
	const (
		debounce = time.Hour
		tick     = 10 * time.Millisecond
		gauge    = "envoy_ci_legacy_records_held"
		noPass   = -1
	)
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
		testwait.Until(t, fmt.Sprintf("%s: the gauge to read %d", what, want), func() (int64, bool) {
			got := gaugeReads(t, met, gauge)
			return got, got == want
		})
	}
	// The loop's grid runs from where the fake stands; its first pass is at the grid's first instant.
	fake.AwaitArmedAt(t, "the loop's first pass", now.Add(tick))
	fake.Set(now.Add(tick))
	waitGauge(0, "the first pass")

	held.Set(noPass)
	// The first pass ends by arming the loop's timer for the waiting records' debounce.
	fake.AwaitArmed(t, "the loop's next pass, after its first", 1)
	// quiet moves the fake 50 ticks, one at a time, reading the gauge after each, and then reads the
	// change feed: a change the loop had not armed for by the last move still waits there.
	quiet := func(what string) {
		t.Helper()
		for range 50 {
			target := fake.Now().Add(tick)
			armed, _ := fake.SetWhen(func(time.Time, []time.Time) (time.Time, bool) { return target, true })
			if len(armed) > 0 && !armed[0].After(target) {
				// The move reached the instant the loop armed, so a pass runs: the read below is
				// made once it has set the gauge.
				testwait.Until(t, fmt.Sprintf("the pass the move to %v fired to set the gauge", target.Sub(now)), func() (int64, bool) {
					got := gaugeReads(t, met, gauge)
					return got, got != noPass
				})
			}
			if got := gaugeReads(t, met, gauge); got != noPass {
				t.Fatalf("a pass ran (gauge %d) %s", got, what)
			}
		}
		store.mu.RLock()
		changed := len(store.changed)
		store.mu.RUnlock()
		if changed != 0 {
			t.Fatalf("the change feed holds %d records %s, each of which wakes a pass", changed, what)
		}
	}
	quiet("during 50 ticks with nothing changed")

	if err := store.Rewatch(context.Background(), conn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	// The new watcher delivers every record before a key written after it started; a retired head
	// record is never cached, so it changes nothing for the loop. Once its revision is applied, the
	// replay has been.
	marker := head{"example-org", "example-repo", "64", "ead000000000000000000000000000000000beef"}.key()
	revision, err := store.watcher.KV().WatchHandle().Put(callContext(t), marker, []byte(`{"kind":"`+retiredHeadKind+`"}`))
	if err != nil {
		t.Fatalf("store the marker: %v", err)
	}
	testwait.Until(t, fmt.Sprintf("the rewatched cache to apply the marker at revision %d", revision), func() (uint64, bool) {
		store.mu.RLock()
		fence := store.cache.FenceFor(store.watcher.Epoch(), marker)
		store.mu.RUnlock()
		return fence, fence == revision
	})
	quiet("after a Rewatch replayed the records unchanged")

	setLastEventAt(t, store, waiting.owner, waiting.repo, waiting.number, waiting.sha, fake.Now().UnixMilli())
	// The change wakes the loop for the next instant of its grid; the fake stands on one.
	next := fake.Now().Add(tick)
	fake.AwaitArmedAt(t, "the loop's pass after one record changed", next)
	fake.Set(next)
	waitGauge(0, "the pass after one record changed")
}

// A settlement whose publication fails is retried at the next tick, not at once: each attempt
// writes the record (its claim, then its release), and a loop that ran a pass for each such change
// would retry as fast as the store writes. The loop runs on a fake clock the test moves a tick at a
// time, each once the loop has armed it.
func TestAFailingPublisherIsRetriedOncePerTick(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	fake := standingFake()
	start := fake.Now()
	store := openStore(t, conn, WithClock(fake))
	// NATS fails every publication while it is unreachable.
	pub := &failingPub{err: errors.New("nats unavailable")}
	const (
		tick = 50 * time.Millisecond
		// ticks is how many ticks of the loop's grid the fake is moved through.
		ticks  = 5
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
	// The loop's grid runs from start, where the fake stands. The loop arms each instant once the
	// pass before it has ended, and the instant after the last move once the pass that move fired
	// has.
	for k := 1; k <= ticks+1; k++ {
		at := start.Add(time.Duration(k) * tick)
		fake.AwaitArmedAt(t, fmt.Sprintf("the loop's grid instant %d", k), at)
		if k <= ticks {
			fake.Set(at)
		}
	}
	attempts, elapsed := pub.count(), fake.Now().Sub(start)
	cancel()

	// Passes run only at the ticks after the loop started, one attempt each at most: the fake
	// reached ticks of them.
	if attempts > ticks {
		t.Fatalf("%d publication attempts in %v, want at most %d, one a tick", attempts, elapsed, ticks)
	}
	if attempts < 2 {
		t.Fatalf("%d publication attempts in %v, want the failed settlement retried", attempts, elapsed)
	}
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
	// Long enough that the waiting record is not due at the moment both passes run at.
	const debounce = time.Hour
	z := testSummarizer(store, debounce)
	stamped := func(st *State) { st.Schema = recordSchema }
	// now is the one moment the records are stamped from and both passes run at, so no reading of
	// a clock between them moves what a pass finds.
	now := time.Now().UnixMilli()
	past := now - (debounce + handoverGrace + time.Minute).Milliseconds()
	putHeadGatedRecord(t, store, head{"example-org", "example-repo", "70", "01d070000000000000000000000000000000beef"}, past, nil)
	putHeadGatedRecord(t, store, head{"example-org", "example-repo", "71", "01d071000000000000000000000000000000beef"}, past, nil)
	queuedAt := now
	putHeadGatedRecord(t, store, head{"example-org", "example-repo", "72", "9e0e72000000000000000000000000000000beef"}, queuedAt, stamped)

	held, next, queued := z.pass(pub, logger, now)
	if held != 2 {
		t.Fatalf("the pass over the old bucket held back %d records, want 2", held)
	}
	if due := queuedAt + debounce.Milliseconds(); !queued || next != due {
		t.Fatalf("the pass over the old bucket left next %d queued %v, want the waiting record queued at %d", next, queued, due)
	}
	if got := pub.count(); got != 0 {
		t.Fatalf("the pass over the old bucket published %d settlements, want none", got)
	}

	// The bucket is deleted through the jetstream package, whose call takes the test's deadline as
	// its context. testnats.RecreateKeyValue takes nats.go's older context, whose CreateKeyValue
	// takes none, so that context waits as long as testnats.MaxWait says rather than nats.go's
	// five seconds.
	if err := store.watcher.KV().Streams().DeleteKeyValue(callContext(t), bus.CIStateBucket); err != nil {
		t.Fatalf("delete the bucket: %v", err)
	}
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	// The config openStore opens the bucket with.
	testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: bus.CIStateBucket, Replicas: 1, Storage: natsgo.FileStorage, TTL: time.Hour})
	if err := store.Rewatch(context.Background(), conn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	fresh := head{"example-org", "example-repo", "73", "f2e573000000000000000000000000000000beef"}
	putHeadGatedRecord(t, store, fresh, past, nil)
	if cached := store.List(); len(cached) != 1 {
		t.Fatalf("the cache after the recreate holds %d records, want only the new bucket's %s", len(cached), fresh.key())
	}

	held, next, queued = z.pass(pub, logger, now)
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
