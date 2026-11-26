package admit

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/signals"
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

// fakeTimers stands in for time.AfterFunc: each timer armed waits for the test to fire it.
type fakeTimers struct {
	mu      sync.Mutex
	pending []*fakeTimer
}

type fakeTimer struct {
	delay   time.Duration
	fire    func()
	stopped bool
}

func (f *fakeTimers) after(delay time.Duration, fire func()) func() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	timer := &fakeTimer{delay: delay, fire: fire}
	f.pending = append(f.pending, timer)
	return func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		was := !timer.stopped
		timer.stopped = true
		return was
	}
}

// live is each timer armed and not stopped.
func (f *fakeTimers) live() []*fakeTimer {
	f.mu.Lock()
	defer f.mu.Unlock()
	var live []*fakeTimer
	for _, timer := range f.pending {
		if !timer.stopped {
			live = append(live, timer)
		}
	}
	return live
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
	// leaving its 6 hours, the idle holder's hour, the kill's and the refusal's hours.
	t.Run("earliest", func(t *testing.T) {
		pool := migratedPool(t)
		lt := newLiveTest(t, 2, nil, on(map[string]float64{signals.Idle: 0.5, signals.Queue: 2, signals.Provider: 1, signals.Refusals: 1, signals.Kills: 1}))
		lt.boot(t, pool, clockAt(5, 0, 0), "acme-owner@example.invalid")
		holderAt(t, pool, "ACME-340", clockAt(10, 0, 0), clockAt(11, 15, 0), false)
		lt.queued(t, pool, 7, clockAt(10, 20, 0), time.Time{})
		lt.queued(t, pool, 8, clockAt(4, 0, 0), clockAt(6, 10, 0))
		lt.kill(t, pool, 1, clockAt(11, 25, 0))
		lt.request(t, pool, "r1", clockAt(11, 59, 30), 7000, "")
		lt.request(t, pool, "r2", clockAt(11, 30, 10), 0, claim.RefusalOwn)
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
		want := map[string]string{signals.Idle: "12:15:00", signals.Queue: "12:10:00", signals.Provider: "12:01:00", signals.Refusals: "12:31:00", signals.Kills: "12:25:00"}
		if !mapsEqual(got, want) || deadline != "provider at 12:01:00" {
			t.Errorf("the next crossings at 12:00 = %v, deadline %s; want %v, deadline provider at 12:01:00", got, deadline, want)
		}
		if committed := committedDeadline(t, pool); !committed.Set || !committed.Due.Equal(clockAt(12, 1, 0)) {
			t.Errorf("the committed deadline is %+v, want 12:01:00", committed)
		}
		got, deadline = next(clockAt(12, 12, 0))
		want = map[string]string{signals.Idle: "12:15:00", signals.Queue: "12:20:00", signals.Refusals: "12:31:00", signals.Kills: "12:25:00"}
		if !mapsEqual(got, want) || deadline != "idle at 12:15:00" {
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

		timers := &fakeTimers{}
		timer := newDeadlineTimer(func() time.Time { return fixedNow }, timers.after, func(record.SignalDeadline) {})
		timer.Arm(record.SignalDeadline{Due: clockAt(12, 40, 0), Set: true, Fence: 7})
		timer.Arm(record.SignalDeadline{Due: clockAt(12, 25, 0), Set: true, Fence: 6})
		if live := timers.live(); len(live) != 1 || live[0].delay != 40*time.Minute {
			t.Errorf("after an older deadline's callback, the timers armed are %d, want the newest one alone, 40 minutes away", len(live))
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
		timers := &fakeTimers{}
		timer := newDeadlineTimer(func() time.Time { return clockAt(12, 5, 0) }, timers.after, func(record.SignalDeadline) {})
		timer.Arm(committedDeadline(t, pool))
		if live := timers.live(); len(live) != 1 || live[0].delay != 20*time.Minute {
			t.Errorf("the timer rebuilt at 12:05 holds %d timers, want one 20 minutes away, at the committed 12:25", len(live))
		}
		applyThrough(t, pool, restarted.admission, "daemon", "boot-again", intake.DaemonBooted{Boot: 2})
		if got := committedDeadline(t, pool); got != committed {
			t.Errorf("after the boot's pass the committed deadline is %+v, want %+v as it stood", got, committed)
		}
	})
	// stop: the daemon's stop cancels the timer, which fires nothing after it, and arms nothing
	// again.
	t.Run("stop", func(t *testing.T) {
		timers := &fakeTimers{}
		var fired []record.SignalDeadline
		timer := newDeadlineTimer(func() time.Time { return fixedNow }, timers.after, func(d record.SignalDeadline) { fired = append(fired, d) })
		timer.Arm(record.SignalDeadline{Due: clockAt(12, 25, 0), Set: true, Fence: 3})
		armed := timers.live()
		timer.Stop()
		if len(armed) != 1 || len(timers.live()) != 0 {
			t.Fatalf("the stop left %d of %d timers armed, want none", len(timers.live()), len(armed))
		}
		armed[0].fire()
		timer.Arm(record.SignalDeadline{Due: clockAt(12, 40, 0), Set: true, Fence: 4})
		if len(fired) != 0 || len(timers.live()) != 0 {
			t.Errorf("after the stop %d deadlines fired and %d timers are armed, want none of either", len(fired), len(timers.live()))
		}
	})
}

// handlerFunc is an intake.Handler of one function.
type handlerFunc func(context.Context, pgx.Tx, intake.Fact) (intake.Result, error)

func (f handlerFunc) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	return f(ctx, tx, fact)
}

// mapsEqual says whether two maps of words hold the same.
func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

// The architect's rule for a report of model requests and a turn edge (design "The pass
// triggers"): each keeps its fact and sets the deadline again, and runs the pass only where a
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
		lt.request(t, pool, "r1", clockAt(12, 0, 15), 7000, "")
		if got := committedDeadline(t, pool); !got.Set || !got.Due.Equal(clockAt(12, 1, 0)) {
			t.Errorf("after the report the committed deadline is %+v, want 12:01:00, where the minute under way is the one the headroom reads", got)
		}
		if got := lt.passCount(); got != passes {
			t.Errorf("the turn edge and the report ran %d passes, want none", got-passes)
		}
		inTx(t, pool, func(tx pgx.Tx) {
			held, err := record.NewStore().ModelRequestsHeld(context.Background(), tx, acmeProject, acmeToken(t, "ACME-9", claim.RoleImplementer), 1, []string{"r1"})
			if err != nil || !slices.Equal(held, []string{"r1"}) {
				t.Errorf("the requests kept of the report = %v, %v; want r1", held, err)
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
		lt.request(t, pool, "r1", clockAt(12, 25, 0), 10, "")
		if got := lt.passCount(); got != passes+1 {
			t.Errorf("the report ran %d passes, want 1", got-passes)
		}
		if got := openedWord(t, pool, gateQueued); got != "the trees open of [ACME-301] are [ACME-301]" {
			t.Errorf("after the report's pass: %s, want the root started", got)
		}
	})
}
