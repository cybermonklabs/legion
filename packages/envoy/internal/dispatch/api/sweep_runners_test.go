package api

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/horizontest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// sweepRunnerSchedule is the pace a sweep's runners keep here: a short gap, so a pass that is
// owed runs at once and a test is about which runner judges what rather than about waiting.
var sweepRunnerSchedule = suggestionSchedule{minGap: 50 * time.Millisecond, page: suggestionOutcomePage}

// The statements a pass of the suggestion sweep is recognised by, each a substring of one of its
// queries: the cursor it holds for update (one per pass), the first pass's read of where to start
// (startSuggestionCursor), a later pass's page (readSuggestionPage) and the three rules, which
// run only over a page that holds something (judgeSuggestionPage).
const (
	cursorHeld    = "select event_id from suggestion_outcome_cursor for update"
	firstPassRead = "select coalesce((select max(id) from events where id <= $2::bigint), 0)"
	laterPageRead = "where id > $1 and id <= $4::bigint order by id limit $2"
	judgingRule   = "update write_suggestions ws"
)

// sweepLeases is the pool a leased loop takes its lease on, the one cmd/dispatch gives it.
func sweepLeases(t *testing.T, database *store.Store) *pgxpool.Pool {
	t.Helper()
	return storetest.Leases(t, database.Pool)
}

// countLeaseLines counts the lease lines carrying message for lease.
func countLeaseLines(logs *storetest.LockedLog, message, lease string) int {
	count := 0
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, `msg="`+message+`"`) && strings.Contains(line, "lease="+lease) {
			count++
		}
	}
	return count
}

// countStatements counts the recorded statements holding needle.
func countStatements(recorded []string, needle string) int {
	count := 0
	for _, sql := range recorded {
		if strings.Contains(sql, needle) {
			count++
		}
	}
	return count
}

// recordedPool is a process's pool with every statement the code under test makes recorded, which
// is how a sweep's passes are counted now that they take no lease to log.
func recordedPool(t *testing.T, database *store.Store) (*store.Pool, *storetest.StatementRecorder) {
	t.Helper()
	recorder := &storetest.StatementRecorder{}
	return storetest.Traced(t, database.Pool, recorder, nil), recorder
}

// sweepHorizon is one runner's events.Horizon, one per process as a server keeps one
// (api.Background's EventHorizon), observing through that process's own pool. It warns of a stall
// at horizontest.StallWarning, as a test server's horizon does, to the test's keeper of the pool's
// database, so a wait on what its passes judge names the holder (horizontest.PollInProcess).
func sweepHorizon(pool *store.Pool) *events.Horizon {
	horizon := events.NewHorizon(pool)
	horizon.SetStallWarning(horizontest.StallWarning, horizontest.StallWarning)
	horizon.SetLogger(horizontest.Logger(pool.Config().ConnConfig.Database))
	return horizon
}

// runSweep runs run on a goroutine until the test ends, and fails the test if it does not return
// once its context ends. That wait's one bound is the test binary's deadline (testwait.Receive):
// a run a loaded machine ends slowly passes, and one that never returns fails by name.
func runSweep(t *testing.T, run func(ctx context.Context)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		testwait.Receive(t, "the suggestion outcome sweep's return once its context ended", done)
	})
}

// Two envoy-dispatch processes sweep one database, each through the sweep's woken loop and each
// on a pool of its own (storetest.SecondProcess, as a second process has). The loop takes no
// lease: what keeps the two from judging one page twice is the suggestion_outcome_cursor row each
// page holds `for update` (judgeSuggestionPage). So both boot passes reach that row - a lease
// would have left one of them waiting outside it - and this test holds the row with a lock of its
// own to see both arrive. Once it lets go, one pass judges the page and advances the cursor, and
// the other, which was waiting on the row, reads the cursor that pass advanced and finds its page
// empty: one judging pass across the two, and both make progress rather than one giving its pass
// up to the other's.
func TestTwoSuggestionSweepRunnersNeverJudgeOnePageTwiceAndBothPass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	firstPool, firstStatements := recordedPool(t, database)
	secondPool, secondStatements := recordedPool(t, second)
	recorded := func() []string {
		return append(firstStatements.Statements(), secondStatements.Statements()...)
	}

	gate, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open the transaction holding the cursor: %v", err)
	}
	defer func() { _ = gate.Rollback(context.Background()) }()
	if _, err := gate.Exec(ctx, `lock table suggestion_outcome_cursor in exclusive mode`); err != nil {
		t.Fatalf("hold the cursor: %v", err)
	}

	runSweep(t, func(ctx context.Context) {
		runSuggestionOutcomeSweep(ctx, clock.Real(), firstPool, sweepHorizon(firstPool), nil, sweepRunnerSchedule)
	})
	runSweep(t, func(ctx context.Context) {
		runSuggestionOutcomeSweep(ctx, clock.Real(), secondPool, sweepHorizon(secondPool), nil, sweepRunnerSchedule)
	})
	storetest.WaitForLockWaiters(t, gate, 2)
	if err := gate.Rollback(context.Background()); err != nil {
		t.Fatalf("release the cursor: %v", err)
	}

	// The passes judge a page only once horizon.Safe proves it, so the wait is the horizon's.
	horizontest.PollInProcess(t, "one runner to judge the page and the other to read the cursor it advanced", database, func() bool {
		return countStatements(recorded(), firstPassRead) == 1 && countStatements(recorded(), laterPageRead) >= 1
	})

	var cursor *int64
	if err := database.Pool.QueryRow(ctx, `select event_id from suggestion_outcome_cursor`).Scan(&cursor); err != nil {
		t.Fatalf("read the cursor: %v", err)
	}
	if cursor == nil {
		t.Fatal("the cursor is still unset after both passes")
	}
	// Each runner held the cursor row for a pass of its own, and only one of the two judged: the
	// rules ran once over the page, not once per process.
	for runner, statements := range map[string]*storetest.StatementRecorder{"the first": firstStatements, "the second": secondStatements} {
		if held := countStatements(statements.Statements(), cursorHeld); held == 0 {
			t.Errorf("%s runner never held the cursor row: it made no pass of its own", runner)
		}
	}
	if judged := countStatements(recorded(), judgingRule); judged != len(suggestionRules(everySuggestion)) {
		t.Fatalf("the rules ran %d times across the two runners, want the %d of one judging pass: the page was judged twice",
			judged, len(suggestionRules(everySuggestion)))
	}
}

// One runner, nothing else on the database: every wake must earn a pass, each one gap after the
// one before at the soonest, and no wake earns more than one. A pass is counted by the cursor row
// it holds, one read per pass, since the loop takes no lease and logs nothing of its own. The loop
// runs on a fake clock: it holds each pass a wake earns to its gap there, and the row moves the
// clock to it. Once the last pass has returned, the loop decides what to arm next and reports that
// wait (suggestionSchedule.waiting, duty.WokenLoop.Waiting) before it waits: with no wake pending,
// the report is a rest, and the loop holds no timer on its clock, its own record that nothing it
// holds can start another pass.
func TestALoneSuggestionSweepRunnerPassesOnEveryWake(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	tracer := &sweepPassTracer{}
	pool := storetest.Traced(t, database.Pool, tracer, nil)
	wake := make(chan struct{}, 1)
	fake := clocktest.New(time.Date(2026, time.November, 5, 3, 0, 0, 0, time.UTC))
	schedule, waits := sweepRunnerSchedule, &testwait.Stream[sweepWait]{}
	schedule.waiting = func(at time.Time, ok bool) {
		waits.Add(sweepWait{at: at, armed: ok, passes: tracer.passes()})
	}
	runSweep(t, func(ctx context.Context) {
		runSuggestionOutcomeSweep(ctx, fake, pool, sweepHorizon(pool), wake, schedule)
	})

	const wakes = 5
	for woken := 0; woken <= wakes; woken++ {
		testwait.Until(t, "the pass the last wake earned, moving the loop's clock to the gap it holds", func() ([]time.Time, bool) {
			if tracer.passes() == woken+1 {
				return nil, true
			}
			armed := fake.Armed()
			if len(armed) > 0 {
				fake.Set(armed[0])
			}
			return armed, false
		})
		if woken < wakes {
			wake <- struct{}{}
		}
	}
	// The loop reports a wait only between passes, so the first report that counts every pass is
	// the one the last pass's return led to.
	last := testwait.Next(t, "the loop's report of its wait after its last pass", waits, func(w sweepWait) bool {
		return w.passes == wakes+1
	})
	if last.armed {
		t.Fatalf("a lone runner armed its timer for %s once its last pass had returned, with no wake pending; want a rest", last.at)
	}
	if armed, ran := fake.Armed(), tracer.passes(); ran != wakes+1 || len(armed) != 0 {
		t.Fatalf("a lone runner made %d passes for its start and %d wakes and holds timers for %v once it rested; want %d and none",
			ran, wakes, armed, wakes+1)
	}
}

// sweepWait is one wait a suggestion sweep's loop reported (suggestionSchedule.waiting): the moment
// its timer is armed for, false for armed when it rests, and the passes its tracer had counted.
type sweepWait struct {
	at     time.Time
	armed  bool
	passes int
}

// sweepPassTracer counts a suggestion sweep's passes on the pool it traces (storetest.Traced): one
// begun for each statement that holds the cursor row (cursorHeld). That a pass has ended is the
// loop's own report of the wait after it (suggestionSchedule.waiting).
type sweepPassTracer struct {
	mu    sync.Mutex
	begun int
}

func (p *sweepPassTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, cursorHeld) {
		p.mu.Lock()
		p.begun++
		p.mu.Unlock()
	}
	return ctx
}

func (p *sweepPassTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (p *sweepPassTracer) passes() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.begun
}
