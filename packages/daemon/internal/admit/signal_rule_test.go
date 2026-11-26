package admit

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/signals"
)

// Admission by signals (spec R11, R12): a pass admits an entry below P0 that takes a slot of the cap
// by the rule's own half (record.SlotRule.SignalsLet) from the snapshot its caller supplied
// (Admission.UseSignals) and the kind of the fact it runs for. The rows below are the table of the
// slot rule's readers (slotRuleReaders) for the choice Signals and the tests of what holds whatever
// the snapshot says: no slot is taken back for a signal, a tick's releases and P0 starts stand, a
// signal with no record or no limit holds nothing, and a pass with no snapshot admits under the cap.

// killsSnapshot is the snapshot a pass reads where the kills read kills against a limit, and no
// other signal has a reading.
func killsSnapshot(kills, limit float64) signals.Snapshot {
	return signals.Snapshot{Readings: map[string]signals.Reading{
		signals.Kills: {Value: signals.Value{Recorded: true, Value: kills}, Limit: new(limit)},
	}}
}

// signalAdmission is an admission of cap over acmeProject judging by rule, which reads each issue's
// priority as priorities names it (prioritized), and whose every pass reads snapshot, with
// admission by signals on.
func signalAdmission(t *testing.T, cap int, rule record.SlotRule, priorities map[string]record.Priority, snapshot signals.Snapshot) *Admission {
	t.Helper()
	admission := prioritizedAdmission(t, cap, rule, priorities)
	admission.UseSignals(func(context.Context, pgx.Tx, time.Time) (signals.Snapshot, bool, error) { return snapshot, true, nil })
	return admission
}

// aPass is one kind of admission pass, applied through the daemon's own chain of handlers: the pass
// of an event (the resume's fact, which promotes as every fact but the tick does), of a signal's
// deadline (intake.SignalDeadline) and of the controller's tick.
type aPass struct {
	name  string
	apply func(t *testing.T, pool *pgxpool.Pool, admission *Admission)
}

// passesOfEveryKind is the three kinds of pass (signals.PassKind), each under an event id of its
// own, so each applies once to a pool.
func passesOfEveryKind() []aPass {
	return []aPass{
		{"an event", func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
			t.Helper()
			applyThrough(t, pool, admission, "pause", "project-resumed", intake.ProjectResumed{})
		}},
		{"a deadline", func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
			t.Helper()
			applyThrough(t, pool, admission, "signals", "signal-deadline", intake.SignalDeadline{At: fixedNow})
		}},
		{"a tick", func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
			t.Helper()
			tickThrough(t, pool, admission, "tick")
		}},
	}
}

// signalRuleReaders is the rows of the table of the slot rule's readers (slotRuleReaders) for
// admission by signals, under the main rule and under signals and signals-backoff: a snapshot with
// one signal over its limit starts no issue below P0 and a P0 issue still starts; a snapshot under
// every limit starts the next issue at an event's pass and at a deadline's pass and none below P0 at
// a tick's pass; and the back-off's allowance holds a start the ceiling lets. Each row's main run
// gives the cap its answer, the snapshot's mode being off.
func signalRuleReaders() []slotRuleReader {
	return []slotRuleReader{
		{
			reader:  "admit's capHold under signals: a signal at its limit holds every start below P0, and a P0 issue still starts",
			changes: []string{"Signals"},
			rule:    keptSlotRule("signals"),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const ranked, urgent = "ACME-260", "ACME-261"
				acmeQueuedRoot(t, pool, ranked, "b")
				acmeQueuedRoot(t, pool, urgent, "c")
				admission := signalAdmission(t, 3, rule, map[string]record.Priority{ranked: record.P1, urgent: record.P0}, killsSnapshot(2, 2))
				applyThrough(t, pool, admission, "pause", "project-resumed", intake.ProjectResumed{})
				return openedWord(t, pool, ranked, urgent)
			},
			main:    "the trees open of [ACME-260 ACME-261] are [ACME-260 ACME-261]",
			changed: "the trees open of [ACME-260 ACME-261] are [ACME-261]",
			why: "the cap of 3 lets both start; under a rule that admits by signals, the kills at their limit of 2 hold the issue at P1, " +
				"and the P0 issue starts outside the cap, which only the P0 bound holds",
		},
		{
			reader:  "admit's capHold under signals: a snapshot under every limit starts at an event's and a deadline's pass and not at a tick's",
			changes: []string{"Signals"},
			rule:    keptSlotRule("signals"),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				const queued = "ACME-262"
				var words []string
				for _, pass := range passesOfEveryKind() {
					pool := migratedPool(t)
					acmeQueuedRoot(t, pool, queued, "b")
					pass.apply(t, pool, signalAdmission(t, 3, rule, nil, killsSnapshot(1, 2)))
					words = append(words, pass.name+": "+openedWord(t, pool, queued))
				}
				return strings.Join(words, "; ")
			},
			main: "an event: the trees open of [ACME-262] are [ACME-262]; a deadline: the trees open of [ACME-262] are [ACME-262]; " +
				"a tick: the trees open of [ACME-262] are [ACME-262]",
			changed: "an event: the trees open of [ACME-262] are [ACME-262]; a deadline: the trees open of [ACME-262] are [ACME-262]; " +
				"a tick: the trees open of [ACME-262] are []",
			why: "the daemon's own rule starts the next issue at a tick as at any pass; under a rule that admits by signals the tick " +
				"starts none below P0, which an event or a signal's deadline starts",
		},
		{
			reader:  "admit's capHold under signals-backoff: the allowance holds a start the ceiling lets",
			changes: []string{"Signals"},
			rule:    keptSlotRule("signals-backoff"),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const first, second, queued = "ACME-265", "ACME-266", "ACME-267"
				acmeRoot(t, pool, first, phase.Implementing, "in_progress", "a", nil, nil)
				takeSlot(t, pool, first)
				acmeRoot(t, pool, second, phase.Implementing, "in_progress", "b", nil, nil)
				takeSlot(t, pool, second)
				acmeQueuedRoot(t, pool, queued, "c")
				snapshot := killsSnapshot(0, 2)
				snapshot.Setbacks = []signals.Setback{{At: fixedNow.Add(-10 * time.Minute), Count: 1}}
				applyThrough(t, pool, signalAdmission(t, 3, rule, nil, snapshot), "pause", "project-resumed", intake.ProjectResumed{})
				return openedWord(t, pool, queued)
			},
			main:    "the trees open of [ACME-267] are [ACME-267]",
			changed: "the trees open of [ACME-267] are []",
			why: "two holders leave a row of the cap of 3 free; after a refusal by the project's own limit the back-off's allowance is " +
				"2, which the two holders fill",
		},
	}
}

// The handler chain applies an intake.SignalDeadline and runs the admission pass of it and nothing
// else: the mirror writes no row of Dispatch's copy and the engine moves no issue, and the pass
// promotes as at any event, so the waiting root takes the free slot, with no tick notice queued
// although a controller is registered.
func TestASignalDeadlineFactRunsOnlyThePass(t *testing.T) {
	pool := migratedPool(t)
	const holder, queued = "ACME-270", "ACME-271"
	registerControllerFor(t, pool, acmeProject)
	acmeRoot(t, pool, holder, phase.Implementing, "in_progress", "a", nil, nil)
	takeSlot(t, pool, holder)
	acmeQueuedRoot(t, pool, queued, "b")
	count := func(query string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(context.Background(), query).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", query, err)
		}
		return n
	}
	mirrored := count(`select count(*) from dispatch_issues`)
	applyThrough(t, pool, ruledAdmission(t, 2, record.MainRule, silent()), "signals", "signal-deadline", intake.SignalDeadline{At: fixedNow})
	if got := openedWord(t, pool, queued); got != "the trees open of [ACME-271] are [ACME-271]" {
		t.Errorf("after the deadline's pass: %s, want the waiting root started, as at any event", got)
	}
	if got := phaseWord(t, pool, holder) + "; " + slotWord(t, pool, holder); got != "ACME-270 stands in implementing; ACME-270 holds a slot" {
		t.Errorf("after the deadline's pass: %s, want the holder left as it stood", got)
	}
	if got := count(`select count(*) from dispatch_issues`); got != mirrored {
		t.Errorf("the mirror holds %d rows of Dispatch's issues after the deadline, want the %d it held: it writes none for the fact", got, mirrored)
	}
	if ticks := count(`select count(*) from outbox where kind = 'controller_notice' and payload->>'kind' = 'tick'`); ticks != 0 {
		t.Errorf("tick notices after the deadline's pass = %d, want none: it is no controller tick", ticks)
	}
}

// A signal that goes over its limit takes no slot back from a holder at any kind of pass, whether the
// signal is over its limit or every one is under it: three holders fill the ceiling of 3, and none
// leaves, and the root waiting for a slot starts at none.
func TestASignalOverItsLimitTakesNoSlotBack(t *testing.T) {
	holders := []string{"ACME-275", "ACME-276", "ACME-277"}
	const queued = "ACME-278"
	for _, snapshot := range []struct {
		name string
		s    signals.Snapshot
	}{{"a signal at its limit", killsSnapshot(2, 2)}, {"every signal under its limit", killsSnapshot(1, 2)}} {
		pool := migratedPool(t)
		for i, key := range holders {
			acmeRoot(t, pool, key, phase.Implementing, "in_progress", fmt.Sprintf("a%d", i), nil, nil)
			takeSlot(t, pool, key)
		}
		acmeQueuedRoot(t, pool, queued, "d")
		admission := signalAdmission(t, 3, keptSlotRule("signals"), nil, snapshot.s)
		for _, pass := range passesOfEveryKind() {
			pass.apply(t, pool, admission)
			var words []string
			for _, key := range holders {
				words = append(words, slotWord(t, pool, key))
			}
			got := strings.Join(words, "; ") + "; " + openedWord(t, pool, queued)
			want := "ACME-275 holds a slot; ACME-276 holds a slot; ACME-277 holds a slot; the trees open of [ACME-278] are []"
			if got != want {
				t.Errorf("%s, after %s: %s\nwant %s", snapshot.name, pass.name, got, want)
			}
		}
	}
}

// A tick's pass under a rule that admits by signals gives back each slot the daemon's own rule gives
// back and starts each P0 issue that R10 starts, and starts no issue below P0: the holder that waits
// on an unfinished link gives its slot back, the P0 root starts outside the cap, and the root at P1
// waits for a pass of an event or of a signal's deadline, which then starts it.
func TestASignalRuleGivesBackAndStartsAtATickAsTheDaemonsRuleDoes(t *testing.T) {
	pool := migratedPool(t)
	const waiter, blocker, ranked, urgent = "ACME-280", "ACME-281", "ACME-282", "ACME-283"
	acmeRoot(t, pool, waiter, phase.Implementing, "in_progress", "a", []string{blocker}, nil)
	takeSlot(t, pool, waiter)
	seedMirrorIn(t, pool, acmeProject, blocker, "in_progress")
	acmeQueuedRoot(t, pool, ranked, "b")
	acmeQueuedRoot(t, pool, urgent, "c")
	admission := signalAdmission(t, 3, keptSlotRule("signals"), map[string]record.Priority{ranked: record.P1, urgent: record.P0}, killsSnapshot(1, 2))
	tickThrough(t, pool, admission, "tick")
	if got, want := slotWord(t, pool, waiter)+"; "+openedWord(t, pool, ranked, urgent),
		"ACME-280 holds no slot; the trees open of [ACME-282 ACME-283] are [ACME-283]"; got != want {
		t.Errorf("after the tick: %s\nwant %s", got, want)
	}
	applyThrough(t, pool, admission, "signals", "signal-deadline", intake.SignalDeadline{At: fixedNow})
	if got, want := openedWord(t, pool, ranked, urgent), "the trees open of [ACME-282 ACME-283] are [ACME-282 ACME-283]"; got != want {
		t.Errorf("after the deadline's pass: %s, want %s", got, want)
	}
}

// A signal with no record in the span, a signal with a record and no limit, and a snapshot with no
// reading hold no start, and a signal at its limit holds it: the pass of an event starts the waiting
// root in the first three cases and not in the last.
func TestAnUnrecordedSignalOrAnUnsetLimitHoldsNoStart(t *testing.T) {
	for _, tc := range []struct {
		name     string
		snapshot signals.Snapshot
		starts   bool
	}{
		{"a signal at its limit", killsSnapshot(2, 2), false},
		{"a signal under its limit", killsSnapshot(1, 2), true},
		{"a signal with no record in the span", signals.Snapshot{Readings: map[string]signals.Reading{
			signals.Kills: {Value: signals.Value{Value: 9}, Limit: new(2.0)}}}, true},
		{"a signal with a record and no limit", signals.Snapshot{Readings: map[string]signals.Reading{
			signals.Kills: {Value: signals.Value{Recorded: true, Value: 9}}}}, true},
		{"no reading of any signal", signals.Snapshot{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			const queued = "ACME-285"
			acmeQueuedRoot(t, pool, queued, "a")
			applyThrough(t, pool, signalAdmission(t, 3, keptSlotRule("signals"), nil, tc.snapshot), "pause", "project-resumed", intake.ProjectResumed{})
			if got := treeOpenForProject(t, pool, acmeProject, queued); got != tc.starts {
				t.Errorf("the waiting root's tree is open after the pass of an event: %t, want %t", got, tc.starts)
			}
		})
	}
}

// The back-off starts an issue below P0 only while the holders below P0 are fewer than its allowance,
// which falls by 90% rounded down, at least 1, at each refusal by the project's own limit and each
// kill, and rises by 1 for each hour with neither, up to the ceiling of 3.
func TestTheBackoffRuleStartsWithinItsAllowance(t *testing.T) {
	refusal := func(ago time.Duration, count int64) signals.Setback {
		return signals.Setback{At: fixedNow.Add(-ago), Count: count}
	}
	for _, tc := range []struct {
		name     string
		holders  int
		setbacks []signals.Setback
		pass     int
		starts   bool
	}{
		{"allowance 2 after a refusal, two holders", 2, []signals.Setback{refusal(10*time.Minute, 1)}, 0, false},
		{"allowance 2 after a refusal, one holder", 1, []signals.Setback{refusal(10*time.Minute, 1)}, 0, true},
		{"an hour with no refusal or kill gives the allowance back at a deadline", 2, []signals.Setback{refusal(61*time.Minute, 1)}, 1, true},
		{"allowance 1 and a kill stays 1, one holder", 1, []signals.Setback{refusal(20*time.Minute, 2), refusal(10*time.Minute, 1)}, 0, false},
		{"allowance 1 and a kill stays 1, no holder", 0, []signals.Setback{refusal(20*time.Minute, 2), refusal(10*time.Minute, 1)}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			for i := range tc.holders {
				key := fmt.Sprintf("ACME-%d", 290+i)
				acmeRoot(t, pool, key, phase.Implementing, "in_progress", fmt.Sprintf("a%d", i), nil, nil)
				takeSlot(t, pool, key)
			}
			const queued = "ACME-295"
			acmeQueuedRoot(t, pool, queued, "z")
			snapshot := killsSnapshot(0, 2)
			snapshot.Setbacks = tc.setbacks
			passes := passesOfEveryKind()
			passes[tc.pass].apply(t, pool, signalAdmission(t, 3, keptSlotRule("signals-backoff"), nil, snapshot))
			if got := treeOpenForProject(t, pool, acmeProject, queued); got != tc.starts {
				t.Errorf("the waiting root's tree is open after %s: %t, want %t", passes[tc.pass].name, got, tc.starts)
			}
		})
	}
}

// Admission by signals is opt-in: a pass whose admission was given no signals, or whose signals say
// they are off, admits under the cap whatever rule it judges by, so a tick starts the waiting root
// under signals as under the daemon's own rule.
func TestAPassWithNoSignalsOrSignalsOffAdmitsUnderTheCap(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(*Admission)
	}{
		{"no signals registered", func(*Admission) {}},
		{"signals registered and off", func(a *Admission) {
			a.UseSignals(func(context.Context, pgx.Tx, time.Time) (signals.Snapshot, bool, error) {
				return killsSnapshot(2, 2), false, nil
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			const queued = "ACME-296"
			acmeQueuedRoot(t, pool, queued, "a")
			admission := ruledAdmission(t, 3, keptSlotRule("signals"), silent())
			tc.configure(admission)
			tickThrough(t, pool, admission, "tick")
			if !treeOpenForProject(t, pool, acmeProject, queued) {
				t.Errorf("the waiting root's tree is not open after a tick: the cap lets it start, and no signal is on")
			}
		})
	}
}

// A reader of the signals that fails ends the pass with its error, wrapped with what the pass was
// reading, and the fact's transaction rolls back: the resume is not marked applied, so it applies
// again on its next delivery, and the root waiting for a free slot of the cap does not start.
func TestASignalReaderErrorEndsThePassAndChangesNothing(t *testing.T) {
	pool := migratedPool(t)
	const queued = "ACME-297"
	acmeQueuedRoot(t, pool, queued, "a")
	admission := ruledAdmission(t, 3, keptSlotRule("signals"), silent())
	unread := errors.New("the signal facts are unreadable")
	admission.UseSignals(func(context.Context, pgx.Tx, time.Time) (signals.Snapshot, bool, error) {
		return signals.Snapshot{}, false, unread
	})
	_, err := intake.ApplyFact(context.Background(), pool, "pause", "project-resumed", intake.ProjectResumed{},
		Handlers(record.NewStore(), acmeProject, admission.engine, admission)...)
	if !errors.Is(err, unread) || !strings.Contains(err.Error(), "read the signals") {
		t.Errorf("the resume's pass ended with %v, want the reader's error, wrapped with what the pass was reading", err)
	}
	var marked int
	if err := pool.QueryRow(context.Background(), `select count(*) from processed_events where source = 'pause' and event_id = 'project-resumed'`).Scan(&marked); err != nil {
		t.Fatalf("read the resume's mark: %v", err)
	}
	if marked != 0 {
		t.Errorf("the resume is marked applied %d times, want its transaction rolled back", marked)
	}
	if treeOpenForProject(t, pool, acmeProject, queued) {
		t.Errorf("the waiting root's tree is open after a pass whose signals could not be read, want nothing changed")
	}
}

// A reader of the signals reads through the pass's own transaction, so it sees what the fact whose
// pass it is wrote before the pass: the row intake.ApplyFact writes to mark the fact applied, which
// no other transaction sees until the fact commits.
func TestTheSignalReaderReadsThroughThePassTransaction(t *testing.T) {
	pool := migratedPool(t)
	admission := ruledAdmission(t, 3, keptSlotRule("signals"), silent())
	var read []string
	admission.UseSignals(func(ctx context.Context, tx pgx.Tx, _ time.Time) (signals.Snapshot, bool, error) {
		if tx == nil {
			read = append(read, "no transaction")
			return signals.Snapshot{}, false, nil
		}
		var marked int
		if err := tx.QueryRow(ctx, `select count(*) from processed_events where source = 'signals' and event_id = 'signal-deadline'`).Scan(&marked); err != nil {
			return signals.Snapshot{}, false, err
		}
		read = append(read, fmt.Sprintf("%d mark of the deadline", marked))
		return signals.Snapshot{}, false, nil
	})
	applyThrough(t, pool, admission, "signals", "signal-deadline", intake.SignalDeadline{At: fixedNow})
	if want := []string{"1 mark of the deadline"}; !slices.Equal(read, want) {
		t.Errorf("the reader read %q, want %q: the mark the deadline's fact wrote before its pass, through the pass's transaction", read, want)
	}
}

// A pass view names the kind of the fact whose pass it is: an event's, a signal's deadline and the
// tick, which a replay records beside each slot a pass gives out.
func TestAPassViewNamesTheKindOfItsPass(t *testing.T) {
	pool := migratedPool(t)
	admission := ruledAdmission(t, 1, record.MainRule, silent())
	var kinds []signals.PassKind
	admission.UsePassObserver(func(view PassView) { kinds = append(kinds, view.Kind) })
	for _, pass := range passesOfEveryKind() {
		pass.apply(t, pool, admission)
	}
	if want := []signals.PassKind{signals.PassEvent, signals.PassDeadline, signals.PassTick}; !slices.Equal(kinds, want) {
		t.Errorf("the kinds of the passes viewed = %v, want %v", kinds, want)
	}
}
