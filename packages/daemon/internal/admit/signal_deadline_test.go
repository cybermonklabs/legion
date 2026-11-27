package admit

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/clock/clocktest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/signals"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// committedDeadline is the deadline the store holds of acmeProject.
func committedDeadline(t *testing.T, pool *pgxpool.Pool) record.SignalDeadline {
	t.Helper()
	var deadline record.SignalDeadline
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		if deadline, err = record.NewStore().SignalDeadline(context.Background(), tx, acmeProject); err != nil {
			t.Fatalf("read the committed deadline: %v", err)
		}
	})
	return deadline
}

// clockAt is the fixed clock's day at hh:mm:ss.
func clockAt(hh, mm, ss int) time.Time {
	return time.Date(fixedNow.Year(), fixedNow.Month(), fixedNow.Day(), hh, mm, ss, 0, time.UTC)
}

// The project's one deadline (plan section 4, the table's rows of TestSignalDeadline): from the
// committed inputs, the earliest next crossing of the series with a limit, written with a rising
// fence in the transaction of each fact whose pass computed another, armed once that fact commits,
// and fired as intake.SignalDeadline, whose stale or early firing runs no pass.
func TestSignalDeadline(t *testing.T) {
	// absent: off, with no limit, or with no crossing due, no deadline is committed or armed.
	t.Run("absent", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			sw   SignalSwitch
		}{
			{"admission by signals off", SignalSwitch{Limits: map[string]float64{signals.Kills: 1}}},
			{"no limit", on(nil)},
			{"no crossing due", on(map[string]float64{signals.Kills: 2})},
		} {
			t.Run(tc.name, func(t *testing.T) {
				pool := migratedPool(t)
				lt := newLiveTest(t, 2, nil, tc.sw)
				lt.boot(t, pool, fixedNow.Add(-7*time.Hour), "acme-owner@example.invalid")
				lt.kill(t, pool, 1, fixedNow.Add(-35*time.Minute))
				if got := committedDeadline(t, pool); got.Set {
					t.Errorf("the committed deadline is %s, want none", got.Due)
				}
				if armed, ok := lt.lastArmed(); ok && armed.Set {
					t.Errorf("a deadline at %s was armed, want none", armed.Due)
				}
			})
		}
	})
	// earliest: the next crossing of each series is its own, and the deadline is the earliest:
	// the minute boundary at which the headroom's minute stops binding, before the queue's exit
	// leaving its 6 hours, the idle holder's hour and the kill's hour.
	t.Run("earliest", func(t *testing.T) {
		pool := migratedPool(t)
		lt := newLiveTest(t, 2, nil, on(map[string]float64{signals.Idle: 0.5, signals.Queue: 2, signals.Provider: 1, signals.Kills: 1}))
		lt.boot(t, pool, clockAt(5, 0, 0), "acme-owner@example.invalid")
		holderAt(t, pool, "ACME-340", clockAt(10, 0, 0), clockAt(11, 15, 0), false)
		lt.queued(t, pool, 7, clockAt(10, 20, 0), time.Time{})
		lt.queued(t, pool, 8, clockAt(4, 0, 0), clockAt(6, 10, 0))
		lt.kill(t, pool, 1, clockAt(11, 25, 0))
		lt.request(t, pool, "r1", clockAt(11, 59, 30), 7000)
		lt.resume(t, pool, "resume")
		next := func(at time.Time) (map[string]string, string) {
			t.Helper()
			var reading LiveReading
			inTx(t, pool, func(tx pgx.Tx) {
				var err error
				if reading, err = lt.live.Reading(context.Background(), tx, at); err != nil {
					t.Fatalf("read the live signals at %s: %v", at, err)
				}
			})
			got := map[string]string{}
			for _, series := range reading.Series {
				if series.Due {
					got[series.Name] = series.Next.Format("15:04:05")
				}
			}
			deadline, due := reading.Deadline()
			if !due {
				return got, "none"
			}
			return got, deadline.Signal + " at " + deadline.At.Format("15:04:05")
		}
		got, deadline := next(fixedNow)
		want := map[string]string{signals.Idle: "12:15:00", signals.Queue: "12:10:00", signals.Provider: "12:01:00", signals.Kills: "12:25:00"}
		if !maps.Equal(got, want) || deadline != "provider at 12:01:00" {
			t.Errorf("the next crossings at 12:00 = %v, deadline %s; want %v, deadline provider at 12:01:00", got, deadline, want)
		}
		if committed := committedDeadline(t, pool); !committed.Set || !committed.Due.Equal(clockAt(12, 1, 0)) {
			t.Errorf("the committed deadline is %+v, want 12:01:00", committed)
		}
		got, deadline = next(clockAt(12, 12, 0))
		want = map[string]string{signals.Idle: "12:15:00", signals.Queue: "12:20:00", signals.Kills: "12:25:00"}
		if !maps.Equal(got, want) || deadline != "idle at 12:15:00" {
			t.Errorf("the next crossings at 12:12 with no new event = %v, deadline %s; want %v, deadline idle at 12:15:00 (the queue's wait grows to its limit at 12:20)",
				got, deadline, want)
		}
	})
	// replace: each event computes the deadline again: a kill that brings the kills to their limit
	// sets it, a pass that changes nothing keeps it and its fence, a kill that holds them at it
	// longer moves it later, a limit set on the queue's wait moves it earlier, and admission by
	// signals turned off takes it away, each change under a fence one higher and armed once its
	// fact commits.
	t.Run("replace", func(t *testing.T) {
		pool := migratedPool(t)
		lt := newLiveTest(t, 2, nil, on(map[string]float64{signals.Kills: 2}))
		lt.boot(t, pool, fixedNow.Add(-7*time.Hour), "acme-owner@example.invalid")
		lt.kill(t, pool, 1, clockAt(11, 25, 0))
		before := committedDeadline(t, pool)
		if before.Set {
			t.Fatalf("one kill under the limit of 2 commits the deadline %s, want none", before.Due)
		}
		steps := []struct {
			name  string
			apply func()
			due   time.Time
			moved bool
		}{
			{"a kill brings the kills to their limit", func() { lt.kill(t, pool, 2, clockAt(11, 40, 0)) }, clockAt(12, 25, 0), true},
			{"a pass that changes nothing", func() { lt.resume(t, pool, "resume") }, clockAt(12, 25, 0), false},
			{"a kill holds them at their limit longer", func() { lt.kill(t, pool, 3, clockAt(11, 50, 0)) }, clockAt(12, 40, 0), true},
			{"a limit on the queue's wait", func() {
				lt.settings.set(on(map[string]float64{signals.Kills: 2, signals.Queue: 2}))
				lt.queued(t, pool, 7, clockAt(10, 10, 0), time.Time{})
			}, clockAt(12, 10, 0), true},
			{"admission by signals off", func() {
				lt.settings.set(SignalSwitch{Limits: map[string]float64{signals.Kills: 2, signals.Queue: 2}})
				lt.resume(t, pool, "resume-off")
			}, time.Time{}, true},
		}
		fence := before.Fence
		for _, step := range steps {
			armedBefore := len(lt.armed)
			step.apply()
			got := committedDeadline(t, pool)
			if step.moved {
				fence++
			}
			want := record.SignalDeadline{Due: step.due, Set: !step.due.IsZero(), Fence: fence}
			if got != want {
				t.Errorf("after %s, the committed deadline is %+v, want %+v", step.name, got, want)
			}
			armed, _ := lt.lastArmed()
			switch {
			case step.moved && armed != want:
				t.Errorf("after %s, the deadline armed is %+v, want the committed %+v", step.name, armed, want)
			case !step.moved && len(lt.armed) != armedBefore:
				t.Errorf("after %s, %d deadlines were armed, want none: the deadline stands", step.name, len(lt.armed)-armedBefore)
			}
		}
	})
	// fenced: a firing whose fence or moment is not the committed deadline's runs no pass, one
	// before its moment runs none and stays unapplied, so its timer fires it again, and a fact
	// rolled back leaves the committed deadline and the timer as they stood; a timer armed for the
	// newest deadline is not replaced by a callback of an older one.
	t.Run("fenced", func(t *testing.T) {
		pool := migratedPool(t)
		lt := newLiveTest(t, 2, nil, on(map[string]float64{signals.Kills: 1}))
		lt.boot(t, pool, fixedNow.Add(-7*time.Hour), "acme-owner@example.invalid")
		lt.kill(t, pool, 1, clockAt(11, 25, 0))
		committed := committedDeadline(t, pool)
		if !committed.Set || !committed.Due.Equal(clockAt(12, 25, 0)) {
			t.Fatalf("the committed deadline is %+v, want 12:25:00", committed)
		}
		passes := lt.passCount()
		lt.at(clockAt(12, 25, 0))
		applyThrough(t, pool, lt.admission, "signals", "stale-fence", intake.SignalDeadline{At: committed.Due, Fence: committed.Fence + 5})
		applyThrough(t, pool, lt.admission, "signals", "stale-due", intake.SignalDeadline{At: clockAt(12, 20, 0), Fence: committed.Fence})
		lt.at(clockAt(12, 24, 0))
		result, err := intake.ApplyFact(context.Background(), pool, "signals", "early", intake.SignalDeadline{At: committed.Due, Fence: committed.Fence},
			Handlers(record.NewStore(), acmeProject, lt.admission.engine, lt.admission)...)
		if err != nil || !result.Unspent {
			t.Errorf("the early firing = %+v, %v; want it left unspent, so its id applies again", result, err)
		}
		if got := lt.passCount(); got != passes {
			t.Errorf("the stale and early firings ran %d passes, want none", got-passes)
		}
		armed := len(lt.armed)
		failing := handlerFunc(func(context.Context, pgx.Tx, intake.Fact) (intake.Result, error) {
			return intake.Result{}, errors.New("the transaction fails after admission")
		})
		lt.at(fixedNow)
		fact := guardKill(t, 2)
		fact.Report.At = clockAt(11, 50, 0)
		if _, err := intake.ApplyFact(context.Background(), pool, "launcher", "kill-rolled-back", fact,
			append(Handlers(record.NewStore(), acmeProject, lt.admission.engine, lt.admission), failing)...); err == nil {
			t.Fatalf("the kill's transaction committed, want it rolled back")
		}
		if got := committedDeadline(t, pool); got != committed || len(lt.armed) != armed {
			t.Errorf("after a rolled-back kill the committed deadline is %+v and %d were armed, want %+v and none", got, len(lt.armed)-armed, committed)
		}
		lt.at(clockAt(12, 25, 0))
		applyThrough(t, pool, lt.admission, "signals", "early", intake.SignalDeadline{At: committed.Due, Fence: committed.Fence})
		if got := lt.passCount(); got != passes+1 {
			t.Errorf("the firing at its moment, under the early one's id, ran %d passes, want 1", got-passes)
		}

		fake := clocktest.New(fixedNow)
		timer := newDeadlineTimer(fake, func(record.SignalDeadline) {})
		timer.Arm(record.SignalDeadline{Due: clockAt(12, 40, 0), Set: true, Fence: 7})
		timer.Arm(record.SignalDeadline{Due: clockAt(12, 25, 0), Set: true, Fence: 6})
		if armed := fake.Armed(); len(armed) != 1 || !armed[0].Equal(clockAt(12, 40, 0)) {
			t.Errorf("after an older deadline's callback, the timers armed are %v, want the newest one alone, at 12:40", armed)
		}
	})
	// fire: a crossing due with no other event runs exactly one pass, of a deadline, which starts
	// the root the kills held at their limit, and computes the deadline again: none is due, so none
	// is armed, and nothing fires on a period.
	t.Run("fire", func(t *testing.T) {
		pool := migratedPool(t)
		lt := newLiveTest(t, 2, nil, on(map[string]float64{signals.Kills: 1}))
		lt.boot(t, pool, fixedNow.Add(-7*time.Hour), "acme-owner@example.invalid")
		acmeQueuedRoot(t, pool, gateQueued, "b")
		lt.kill(t, pool, 1, clockAt(11, 25, 0))
		if got := openedWord(t, pool, gateQueued); got != "the trees open of [ACME-301] are []" {
			t.Fatalf("after the kill's pass: %s, want the root held by the kills at their limit", got)
		}
		committed := committedDeadline(t, pool)
		passes := lt.passCount()
		lt.at(committed.Due)
		applyThrough(t, pool, lt.admission, "signals", "fire", intake.SignalDeadline{At: committed.Due, Fence: committed.Fence})
		if got := lt.passes[passes:]; !slices.Equal(got, []signals.PassKind{signals.PassDeadline}) {
			t.Errorf("the firing ran the passes %v, want one deadline's", got)
		}
		if got := openedWord(t, pool, gateQueued); got != "the trees open of [ACME-301] are [ACME-301]" {
			t.Errorf("after the deadline's pass: %s, want the root started", got)
		}
		want := record.SignalDeadline{Fence: committed.Fence + 1}
		if got, armed := committedDeadline(t, pool), lt.armed[len(lt.armed)-1]; got != want || armed != want {
			t.Errorf("after the firing the committed deadline is %+v and the one armed %+v, want %+v: none is due", got, armed, want)
		}
	})
	// restart: a daemon that boots again arms the timer from the committed deadline, and its
	// boot's pass computes the same one, under the same fence.
	t.Run("restart", func(t *testing.T) {
		pool := migratedPool(t)
		lt := newLiveTest(t, 2, nil, on(map[string]float64{signals.Kills: 1}))
		lt.boot(t, pool, fixedNow.Add(-7*time.Hour), "acme-owner@example.invalid")
		lt.kill(t, pool, 1, clockAt(11, 25, 0))
		committed := committedDeadline(t, pool)
		restarted := newLiveTest(t, 2, nil, on(map[string]float64{signals.Kills: 1}))
		restarted.at(clockAt(12, 5, 0))
		fake := clocktest.New(clockAt(12, 5, 0))
		timer := newDeadlineTimer(fake, func(record.SignalDeadline) {})
		timer.Arm(committedDeadline(t, pool))
		if armed := fake.Armed(); len(armed) != 1 || !armed[0].Equal(clockAt(12, 25, 0)) {
			t.Errorf("the timer rebuilt at 12:05 holds %v, want one 20 minutes away, at the committed 12:25", armed)
		}
		applyThrough(t, pool, restarted.admission, "daemon", "boot-again", intake.DaemonBooted{Boot: 2})
		if got := committedDeadline(t, pool); got != committed {
			t.Errorf("after the boot's pass the committed deadline is %+v, want %+v as it stood", got, committed)
		}
	})
	// stop: the daemon's stop cancels the timer, which fires nothing after it, and arms nothing
	// again.
	t.Run("stop", func(t *testing.T) {
		fake := clocktest.New(fixedNow)
		var fired []record.SignalDeadline
		timer := newDeadlineTimer(fake, func(d record.SignalDeadline) { fired = append(fired, d) })
		timer.Arm(record.SignalDeadline{Due: clockAt(12, 25, 0), Set: true, Fence: 3})
		armed := fake.Armed()
		timer.Stop()
		if len(armed) != 1 || len(fake.Armed()) != 0 {
			t.Fatalf("the stop left %d of %d timers armed, want none", len(fake.Armed()), len(armed))
		}
		timer.Arm(record.SignalDeadline{Due: clockAt(12, 40, 0), Set: true, Fence: 4})
		fake.Advance(time.Hour)
		if len(fired) != 0 || len(fake.Armed()) != 0 {
			t.Errorf("after the stop %d deadlines fired and %d timers are armed, want none of either", len(fired), len(fake.Armed()))
		}
	})
}

// handlerFunc is an intake.Handler of one function.
type handlerFunc func(context.Context, pgx.Tx, intake.Fact) (intake.Result, error)

func (f handlerFunc) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	return f(ctx, tx, fact)
}

// The series-moved pass rule for a report of model requests and a turn edge (signalsMoved): each
// keeps its fact and sets the deadline again, and runs the pass only where a
// series' hold moved against the last committed pass's or one of the five but the headroom
// qualifies for a value entry against its last written value.
func TestSignalReportPredicate(t *testing.T) {
	// moves-nothing: a turn edge that starts an idle hour and a report whose tokens fall in the
	// minute under way change no hold and no value the last pass recorded: each runs no pass, the
	// report's request is kept, and each sets the deadline, to the idle holder's hour and then to
	// the minute boundary at which the report's minute is the one the headroom reads.
	t.Run("moves-nothing", func(t *testing.T) {
		pool := migratedPool(t)
		const holder = "ACME-350"
		lt := newLiveTest(t, 2, nil, on(map[string]float64{signals.Kills: 2, signals.Idle: 0.5, signals.Provider: 1}))
		lt.boot(t, pool, fixedNow.Add(-7*time.Hour), "acme-owner@example.invalid")
		holderAt(t, pool, holder, clockAt(10, 0, 0), clockAt(11, 50, 0), true)
		lt.kill(t, pool, 1, clockAt(11, 25, 0))
		lt.resume(t, pool, "resume")
		passes := lt.passCount()
		if got := committedDeadline(t, pool); got.Set {
			t.Fatalf("the committed deadline before the edge is %s, want none", got.Due)
		}
		lt.at(clockAt(12, 0, 20))
		idleClaim(t, pool, holder, holder, claim.RoleImplementer, clockAt(11, 50, 0), clockAt(12, 0, 10))
		applyThrough(t, pool, lt.admission, "supervise", "turn-end", intake.TurnEdge{Claim: acmeToken(t, holder, claim.RoleImplementer), Issue: holder,
			At: clockAt(12, 0, 10)})
		if got := committedDeadline(t, pool); !got.Set || !got.Due.Equal(clockAt(13, 0, 10)) {
			t.Errorf("after the turn's end the committed deadline is %+v, want 13:00:10, the hour the holder is idle", got)
		}
		lt.request(t, pool, "r1", clockAt(12, 0, 15), 7000)
		if got := committedDeadline(t, pool); !got.Set || !got.Due.Equal(clockAt(12, 1, 0)) {
			t.Errorf("after the report the committed deadline is %+v, want 12:01:00, where the minute under way is the one the headroom reads", got)
		}
		if got := lt.passCount(); got != passes {
			t.Errorf("the turn edge and the report ran %d passes, want none", got-passes)
		}
		var kept []string
		inTx(t, pool, func(tx pgx.Tx) {
			rows, err := tx.Query(context.Background(), `select id from model_requests where project = $1 and claim = $2 and generation = 1`,
				acmeProject, string(acmeToken(t, "ACME-9", claim.RoleImplementer)))
			if err == nil {
				kept, err = pgx.CollectRows(rows, pgx.RowTo[string])
			}
			if err != nil || !slices.Equal(kept, []string{"r1"}) {
				t.Errorf("the requests kept of the report = %v, %v; want r1", kept, err)
			}
		})
	})
	// after-crossing: a report that arrives once the kill left its hour, before the deadline's
	// firing, finds the kills' hold moved against the last committed pass, and its pass starts the
	// root they held.
	t.Run("after-crossing", func(t *testing.T) {
		pool := migratedPool(t)
		lt := newLiveTest(t, 2, nil, on(map[string]float64{signals.Kills: 1}))
		lt.boot(t, pool, fixedNow.Add(-7*time.Hour), "acme-owner@example.invalid")
		acmeQueuedRoot(t, pool, gateQueued, "b")
		lt.kill(t, pool, 1, clockAt(11, 25, 0))
		if got := openedWord(t, pool, gateQueued); got != "the trees open of [ACME-301] are []" {
			t.Fatalf("after the kill's pass: %s, want the root held", got)
		}
		passes := lt.passCount()
		lt.at(clockAt(12, 25, 5))
		lt.request(t, pool, "r1", clockAt(12, 25, 0), 10)
		if got := lt.passCount(); got != passes+1 {
			t.Errorf("the report ran %d passes, want 1", got-passes)
		}
		if got := openedWord(t, pool, gateQueued); got != "the trees open of [ACME-301] are [ACME-301]" {
			t.Errorf("after the report's pass: %s, want the root started", got)
		}
	})
}

// readsCounted is the admission's store, counting its reads of the provider's minutes and of the
// approvals: a reading of every live series makes each once (LiveSignals.Reading), and a read of
// the idle share alone makes neither.
type readsCounted struct {
	record.Store
	mu                 sync.Mutex
	minutes, approvals int
}

func (s *readsCounted) ProviderMinutes(ctx context.Context, tx pgx.Tx, project string, from, to time.Time, reporting int) (record.ProviderMinutes, error) {
	s.mu.Lock()
	s.minutes++
	s.mu.Unlock()
	return s.Store.ProviderMinutes(ctx, tx, project, from, to, reporting)
}

func (s *readsCounted) ApprovalsOf(ctx context.Context, tx pgx.Tx, project string) (record.Approvals, error) {
	s.mu.Lock()
	s.approvals++
	s.mu.Unlock()
	return s.Store.ApprovalsOf(ctx, tx, project)
}

// reads is how many of each read the store counted since the last call, which counts from zero
// again.
func (s *readsCounted) reads() (minutes, approvals int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	minutes, approvals = s.minutes, s.approvals
	s.minutes, s.approvals = 0, 0
	return minutes, approvals
}

// processedMarks is how many facts processed_events holds marked.
func processedMarks(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var marks int
	if err := pool.QueryRow(context.Background(), `select count(*) from processed_events`).Scan(&marks); err != nil {
		t.Fatalf("count the processed marks: %v", err)
	}
	return marks
}

// newestIdleValue is the idle share's newest value entry in acmeProject's record of changes, nil
// for none or for one the daemon could not compute.
func newestIdleValue(t *testing.T, pool *pgxpool.Pool) *float64 {
	t.Helper()
	var value *float64
	inTx(t, pool, func(tx pgx.Tx) {
		newest, err := store.NewestSeriesEntriesTx(context.Background(), tx, acmeProject)
		if err != nil {
			t.Fatalf("read the newest entries: %v", err)
		}
		if entry := newest[signals.Idle].Value; entry != nil {
			value = entry.Value
		}
	})
	return value
}

// A report of model requests and a turn edge keep no processed mark (intake's keepsMark), so
// neither adds a row to processed_events, which nothing prunes, and one applied again under the
// same id applies again and keeps nothing twice. While admission by signals is off, a report
// reads no series of the live signals, and a turn edge the idle share alone, the one series it
// moves (signalFactMoved): a turn's start that moves that share by its step runs the pass, which
// writes its value entry as every pass does while the rule is off, and the same edge told again
// moves nothing and runs none. With the rule on, a report reads every series, as it always did.
func TestASignalFactKeepsNoMarkAndReadsOnlyWhatItCanMove(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	const holder = "ACME-350"
	lt := newLiveTest(t, 2, nil, SignalSwitch{})
	counted := &readsCounted{Store: lt.admission.store}
	lt.admission.store = counted
	lt.boot(t, pool, fixedNow.Add(-7*time.Hour), "acme-owner@example.invalid")
	holderAt(t, pool, holder, clockAt(10, 0, 0), clockAt(10, 30, 0), false)
	lt.resume(t, pool, "resume")
	if got := newestIdleValue(t, pool); got == nil || *got != 1 {
		t.Fatalf("after the resume's pass the idle share's newest value is %v, want 1: its one holder is idle since 10:30", got)
	}
	marks, passes := processedMarks(t, pool), lt.passCount()

	// The holder's turn starts, which moves the idle share from 1 to 0.
	lt.at(clockAt(12, 0, 20))
	token := acmeToken(t, holder, claim.RoleImplementer)
	if err := claimStore(t, pool).PutClaim(ctx, supervise.Claim{Token: token, Project: acmeProjectToken(t), Tree: holder, Issue: holder,
		Role: claim.RoleImplementer, TreeEpoch: 1, Generation: 1, State: supervise.StateWorking, Session: "ses_implementer_" + holder,
		LastTurnStart: clockAt(12, 0, 10), LastTurnEnd: clockAt(10, 30, 0)}); err != nil {
		t.Fatalf("record the holder's turn: %v", err)
	}
	edge := intake.TurnEdge{Claim: token, Issue: holder, Start: true, At: clockAt(12, 0, 10)}
	applyThrough(t, pool, lt.admission, "supervise", "turn-start", edge)
	if got := lt.passCount(); got != passes+1 {
		t.Errorf("the turn's start ran %d passes, want 1: it moved the idle share by its step", got-passes)
	}
	if got := newestIdleValue(t, pool); got == nil || *got != 0 {
		t.Errorf("after the turn's start the idle share's newest value is %v, want 0, written by the pass it ran", got)
	}
	counted.reads()

	applyThrough(t, pool, lt.admission, "supervise", "turn-start", edge)
	if minutes, approvals := counted.reads(); minutes != 0 || approvals != 0 {
		t.Errorf("the turn edge told again read the provider's minutes %d times and the approvals %d, want neither: the rule is off, and the edge moves the idle share alone",
			minutes, approvals)
	}
	lt.request(t, pool, "r1", clockAt(12, 0, 15), 7000)
	lt.request(t, pool, "r1", clockAt(12, 0, 15), 7000)
	if minutes, approvals := counted.reads(); minutes != 0 || approvals != 0 {
		t.Errorf("the reports read the provider's minutes %d times and the approvals %d, want neither: the rule is off, and a report moves the headroom alone",
			minutes, approvals)
	}
	if got := lt.passCount(); got != passes+1 {
		t.Errorf("the edge told again and the reports ran %d passes, want none", got-passes-1)
	}
	var kept int
	inTx(t, pool, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `select count(*) from model_requests where project = $1 and id = 'r1'`, acmeProject).Scan(&kept); err != nil {
			t.Fatalf("count the requests kept: %v", err)
		}
	})
	if kept != 1 {
		t.Errorf("the report sent twice kept r1 %d times, want once", kept)
	}

	// With the rule on, a report reads every series.
	lt.settings.set(on(nil))
	lt.request(t, pool, "r2", clockAt(12, 0, 16), 10)
	if minutes, approvals := counted.reads(); minutes == 0 || approvals == 0 {
		t.Errorf("with the rule on the report read the provider's minutes %d times and the approvals %d, want every series read", minutes, approvals)
	}
	if got := processedMarks(t, pool); got != marks {
		t.Errorf("the turn edges and the reports left processed_events holding %d marks, want %d, the marks before them", got, marks)
	}
}
