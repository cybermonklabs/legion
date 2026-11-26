package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/signals"
)

// everyLimit is a limit on each of R11's signals, each legal (signals.CheckLimit). Under a rule
// that admits by no signal they decide nothing, and a run writes each signal's value at its probes.
var everyLimit = map[string]float64{SignalIdle: 0.5, SignalQueue: 2, SignalProvider: 1, signals.Refusals: 4, SignalKills: 2, SignalApprovals: 3}

// signalRun is the run of w under rule, at seed 1 with no slowdown, with limits, d and probes, which
// must run.
func signalRun(t *testing.T, w Window, rule string, limits map[string]float64, idleAfter time.Duration, probes ...time.Time) RunResult {
	t.Helper()
	return signalRunWith(t, RunConfig{Window: w, Rule: replayedRule(t, rule), Limits: limits, IdleAfter: idleAfter, Probes: probes})
}

// signalRunWith is the run cfg names, at seed 1 with no slowdown on the replay's server, which must
// run.
func signalRunWith(t *testing.T, cfg RunConfig) RunResult {
	t.Helper()
	cfg.Seed, cfg.Slowdown, cfg.PGDSN, cfg.Out = 1, 1, replayDSN(t), t.TempDir()
	result, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("replay under %s: %v", cfg.Rule.Name, err)
	}
	return result
}

// probed is a signal's value a run writes at a probe: the moment and, recorded, the value, or
// unrecorded; and why, worked out by hand.
type probed struct {
	at       time.Time
	value    float64
	recorded bool
	why      string
}

// valueAt and noValueAt are a probed value recorded at value, and one unrecorded.
func valueAt(at time.Time, value float64, why string) probed {
	return probed{at: at, value: value, recorded: true, why: why}
}
func noValueAt(at time.Time, why string) probed { return probed{at: at, why: why} }

// checkProbed fails for each value of signal at a probe that result gives otherwise than worked
// out.
func checkProbed(t *testing.T, result RunResult, signal string, want []probed) {
	t.Helper()
	for _, w := range want {
		values, read := result.SignalValues[w.at.Format(time.RFC3339Nano)]
		if !read {
			t.Errorf("the run wrote no values at %s", stamp(w.at))
			continue
		}
		got, named := values[signal]
		switch {
		case !named:
			t.Errorf("the run wrote no %s at %s", signal, stamp(w.at))
		case w.recorded && (got == nil || *got != w.value):
			t.Errorf("%s at %s = %s, want %v: %s", signal, stamp(w.at), describeProbed(got), w.value, w.why)
		case !w.recorded && got != nil:
			t.Errorf("%s at %s = %v, want it unrecorded: %s", signal, stamp(w.at), *got, w.why)
		}
	}
}

// describeProbed is a probed value as a failure names it.
func describeProbed(value *float64) string {
	if value == nil {
		return "unrecorded"
	}
	return fmt.Sprint(*value)
}

// probesOf is the moment of each of want.
func probesOf(want []probed) []time.Time {
	moments := make([]time.Time, len(want))
	for i, w := range want {
		moments[i] = w.at
	}
	return moments
}

// idleCourseWindow is idleWindow with ACME-2's first turn ending at 09:55, before the window's
// start, as ACME-1's does, so that no turn stands at the start. The claim of a root's architect at
// admitted beside a phases row that names no entry, which no start state writes, is one the
// admission pass raises an entry for once the claim is idle, sending it work that the record's
// daemon never sent (seeder's waitingArchitects): the start state writes the claim of a turn that
// stands at the start, as idleWindow's ACME-2's ending at 10:00 does, and the stub the claim of a
// turn it runs, as ACME-2's from 11:30 to 12:00.
func idleCourseWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(10, 0), clock(14, 0))
	b.boot(clock(9, 0))
	promotedRoot(b, clock(9, 5), "ACME-1", "a1")
	promotedRoot(b, clock(9, 10), "ACME-2", "a2")
	b.turnEnds(clock(9, 30), "ACME-1", claim.RoleArchitect)
	b.turnEnds(clock(9, 55), "ACME-2", claim.RoleArchitect)
	promotedRoot(b, clock(10, 20), "ACME-3", "a3")
	b.turnStarts(clock(11, 30), "ACME-2", claim.RoleArchitect)
	b.turnEnds(clock(12, 0), "ACME-2", claim.RoleArchitect)
	b.turnEnds(clock(13, 0), "ACME-3", claim.RoleArchitect)
	b.window.Signals = map[string][]Span{SignalIdle: {{From: clock(9, 0), To: clock(14, 0)}}}
	return b.build()
}

// A replay reads each of R11's signals from its own course, never from the record (spec R11): the
// idle share from its own slot holders and the stub's claims, the approvals from its copy of
// Dispatch, and the provider's headroom and refusals, the kills and the merge queue from the load
// models at its own work in flight. On the windows of TestEachSignalAtAMoment, each replayed under
// main with a limit on every signal and a probe at each moment worked out there, the run writes
// each signal's value: the record's for the kills, the queue and the approvals, whose models give
// the record at the recorded course; the headroom's tokens as recorded, with no issue in flight in
// either; and the project's own refusals as the tokens model gives them. Its one refused minute is
// 10:01's, 6,600 tokens over the limit of 6,000. The record's own refusals per refused minute are
// 2, its 3 and 1 of 10:01 and 10:30, so the model gives 10:01 two of the project's own, and 10:30,
// under the limit, none. Beside them stand the provider's 2 of 09:10 and 1 of 10:01, kept as
// recorded. The idle share is read on idleCourseWindow, whose course is the record's until 12:00:
// each holder idle since the newest end of its turn before the start, ACME-1 at 09:30 and ACME-2
// at 09:55, as the record has them, not since their slots. At 12:00 ACME-2's turn ends, and the
// next pass, at 12:01, raises an entry for its architect, at admitted beside a phases row that
// names none, and sends it work that no recorded turn takes, so it works in the replay where the
// record has it idle.
func TestTheReplayReadsEachSignalFromItsOwnCourse(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		signal string
		window func(*testing.T) Window
		want   []probed
	}{
		{SignalIdle, idleCourseWindow, []probed{
			valueAt(clock(10, 29), 0, "ACME-1 is idle for 59 minutes, from its turn's end at 09:30, not from its slot at 09:05"),
			valueAt(clock(10, 30), 1.0/3, "ACME-1 is idle for an hour, ACME-2 for 35 minutes, and ACME-3 works"),
			valueAt(clock(11, 0), 2.0/3, "ACME-2 is idle for an hour too"),
			valueAt(clock(11, 30), 1.0/3, "ACME-2 works again"),
			valueAt(clock(13, 0), 1.0/3, "ACME-2 works on the work the pass of 12:01 sent it, where the record has it idle for an hour, and ACME-3's turn has just ended"),
			noValueAt(clock(14, 0), "the record's span ends at 14:00"),
		}},
		{SignalQueue, queueWindow, []probed{
			valueAt(clock(9, 0), 3, "#3 has waited 3 hours, above the median 1.25 of #2's two waits"),
			valueAt(clock(12, 0), 3, "the median of #3's 4 hours and #4's 2, and #5 has waited nothing"),
			valueAt(clock(15, 0), 3, "the median of #3's 4 hours, #4's 2 and #5's 3, with none in the queue"),
			noValueAt(clock(18, 0), "the record's span ends at 18:00"),
		}},
		{SignalProvider, providerWindow, []probed{
			noValueAt(clock(9, 30), "the minute before, 09:29, is before the limit's first observation"),
			valueAt(clock(9, 31), 0.5, "3,000 of 6,000"),
			valueAt(clock(10, 1), 0.8, "4,800 of 6,000"),
			valueAt(clock(10, 2), 1.1, "6,600 of 6,000"),
			noValueAt(clock(11, 31), "the limit was taken away at 11:30"),
			noValueAt(clock(11, 46), "the limit of 11:45 is one model's own"),
		}},
		{signals.Refusals, providerWindow, []probed{
			noValueAt(clock(9, 0), "the record holds no minute before 09:00"),
			valueAt(clock(9, 11), 2, "the provider's 2 of 09:10"),
			valueAt(clock(10, 2), 5, "the provider's 2 of 09:10 and 1 of 10:01, and the model's 2 of the project's own in 10:01"),
			valueAt(clock(10, 11), 3, "09:10's left the hour at 10:11"),
			valueAt(clock(10, 31), 3, "10:01's 3, and none in 10:30, whose tokens the limit does not refuse in the model"),
		}},
		{SignalKills, killsWindow, []probed{
			valueAt(clock(9, 39), 0, "no kill yet"),
			valueAt(clock(9, 40), 1, "a kill counts from its own moment"),
			valueAt(clock(10, 40), 2, "09:40's kill left the hour, and 09:50's and 10:35's count"),
			valueAt(clock(10, 50), 1, "09:50's kill left the hour"),
			noValueAt(clock(12, 0), "the record's span ends at 12:00"),
		}},
		{SignalApprovals, approvalsWindow, []probed{
			valueAt(clock(9, 35), 3, "A1, A4 and A5; A2 waits on ACME-2's assignee and A3 on the person it was asked of"),
			valueAt(clock(9, 45), 4, "ACME-2's assignee is now the default assignee"),
			valueAt(clock(10, 15), 2, "A4 and A5 wait on their agents"),
			valueAt(clock(10, 45), 4, "A4 was handed back and the agent replied on A5"),
			valueAt(clock(11, 0), 3, "the default assignee moved, so A2 no longer counts"),
			valueAt(clock(12, 30), 1, "A4 was answered and A5 declined"),
			noValueAt(clock(13, 0), "the record's span ends at 13:00"),
		}},
	} {
		t.Run(c.signal, func(t *testing.T) {
			t.Parallel()
			result := signalRun(t, c.window(t), "main", everyLimit, time.Hour, probesOf(c.want)...)
			checkProbed(t, result, c.signal, c.want)
		})
	}
}

// A pass's snapshot holds each signal's reading with its limit, read from the replay at the pass's
// instant, and the back-off's setbacks (readSignals). On killsWindow, replayed under signals-backoff
// with a limit on the kills alone, each pass's snapshot names the back-off's mode and a reading of
// the kills alone, limit 2, valued as the run's probe of the kills at that instant gives it; its
// setbacks are the kills by the pass, 09:40's, 09:50's and 10:35's, each one, those the allowance
// still reads. Under main no pass reads the signals.
func TestAPassSnapshotReadsTheReplaysSignals(t *testing.T) {
	t.Parallel()
	w := killsWindow(t)
	var snapshots []signals.Snapshot
	limits := map[string]float64{SignalKills: 2}
	result := signalRunWith(t, RunConfig{Window: w, Rule: replayedRule(t, "signals-backoff"), Limits: limits,
		onSnapshot: func(s signals.Snapshot) { snapshots = append(snapshots, s) }})
	if len(snapshots) == 0 {
		t.Fatal("no pass read the signals under signals-backoff")
	}
	kills := []time.Time{clock(9, 40), clock(9, 50), clock(10, 35)}
	for _, s := range snapshots {
		reading, read := s.Readings[SignalKills]
		if s.Mode != signals.Backoff || len(s.Readings) != 1 || !read || reading.Limit == nil || *reading.Limit != 2 {
			t.Errorf("the snapshot at %s is %+v, want the back-off's mode and the kills alone, limit 2", stamp(s.At), s)
			continue
		}
		count := 0
		var setbacks []signals.Setback
		for _, kill := range kills {
			if !kill.After(s.At) {
				setbacks = append(setbacks, signals.Setback{At: kill, Count: 1})
			}
			if !kill.After(s.At) && kill.After(s.At.Add(-time.Hour)) {
				count++
			}
		}
		if !reading.Recorded || reading.Value.Value != float64(count) {
			t.Errorf("the kills at %s read %s, want %d", stamp(s.At), describeValue(reading.Value), count)
		}
		if signals.Allowance(4, s.Setbacks, s.At) != signals.Allowance(4, setbacks, s.At) {
			t.Errorf("the setbacks at %s are %+v, which fold otherwise than the kills by then, %+v", stamp(s.At), s.Setbacks, setbacks)
		}
	}
	if len(result.Deadlines) == 0 {
		t.Error("the run under signals-backoff set no deadline")
	}
	var none []signals.Snapshot
	signalRunWith(t, RunConfig{Window: w, Rule: mainRule(t), Limits: limits, onSnapshot: func(s signals.Snapshot) { none = append(none, s) }})
	if len(none) != 0 {
		t.Errorf("under main the passes read %d snapshots, want none", len(none))
	}
}

// runFacts is the facts a run applied, as its run.json gives them.
func runFacts(t *testing.T, out string) int {
	t.Helper()
	var ran runRecord
	if err := readJSON(filepath.Join(out, runFile), &ran); err != nil {
		t.Fatal(err)
	}
	return ran.Facts
}

// The project's one deadline is set, moved and fired (spec R11), on no-crossing-due under signals
// with its limit of 2 kills. The second kill at 09:50 sets it for 10:40, when 09:40's leaves the
// hour; the third at 10:35 moves it to 10:50, since the count at 10:40 would still be 2; at 10:50 it
// fires, and nothing is due after. A kill runs no pass, the replay leaving its fact out (assumption
// 31): the kill moments 09:40, 09:50 and 10:35 compute the deadline and apply no fact, so the run
// applies one fact more than the same window under main, the deadline's own at 10:50.
func TestTheDeadlineIsSetMovedAndFiredAndAKillRunsNoPass(t *testing.T) {
	t.Parallel()
	w, _ := readFixture(t, "no-crossing-due")
	dsn := replayDSN(t)
	run := func(rule string, limits map[string]float64) (RunResult, int) {
		out := t.TempDir()
		result, err := Run(context.Background(), RunConfig{Window: w, Rule: replayedRule(t, rule), Seed: 1, Slowdown: 1, PGDSN: dsn, Out: out, Limits: limits})
		if err != nil {
			t.Fatalf("replay under %s: %v", rule, err)
		}
		return result, runFacts(t, out)
	}
	signalled, signalFacts := run("signals", map[string]float64{SignalKills: 2})
	_, mainFacts := run("main", nil)
	want := []DeadlineEntry{
		{Action: deadlineSet, At: clock(9, 50), Due: clock(10, 40), Signal: SignalKills},
		{Action: deadlineMoved, At: clock(10, 35), Due: clock(10, 50), Signal: SignalKills, Previous: clock(10, 40)},
		{Action: deadlineFired, At: clock(10, 50), Due: clock(10, 50), Signal: SignalKills},
	}
	if !reflect.DeepEqual(signalled.Deadlines, want) {
		t.Errorf("the deadlines are %+v, want %+v", signalled.Deadlines, want)
	}
	if signalFacts != mainFacts+1 {
		t.Errorf("the run under signals applied %d facts and under main %d, want one more, the deadline's alone", signalFacts, mainFacts)
	}
}

// downtimeWindow is no-crossing-due's course with the daemon down from 10:30 to 10:45: a window from
// 09:00 to 12:00 under a cap of 1, with no issue, the daemon booting at 09:00, stopping at 10:30 and
// booting again at 10:45, and kills of a pod of no issue at 09:40, 09:50 and 10:35, recorded over the
// whole span.
func downtimeWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(12, 0))
	b.capped(1)
	b.boot(clock(9, 0))
	b.stopped(clock(10, 30))
	b.boot(clock(10, 45))
	pod := "legion-acme-acme-1"
	for i, at := range []time.Time{clock(9, 40), clock(9, 50), clock(10, 35)} {
		b.window.Launcher = append(b.window.Launcher, LauncherLine{Time: at, Pod: pod, Text: guardKillText(100+i, "architect")})
	}
	b.window.Signals = map[string][]Span{SignalKills: {{From: clock(9, 0), To: clock(12, 0)}}}
	return b.build()
}

// The daemon keeps its one timer in memory (spec R11), so a deadline that falls while it is down
// does not fire, and the boot's first pass computes it again. On downtimeWindow under signals with
// a limit of 2 kills, 09:50's kill sets the deadline for 10:40, which falls in the downtime and does
// not fire; the kill at 10:35, while the daemon is down, computes nothing. The boot at 10:45 drops the
// deadline, and its first pass reads 2 kills, 09:50's and 10:35's, and sets it for 10:50, when
// 09:50's leaves the hour, and there it fires.
func TestADeadlineThatFallsWhileTheDaemonIsDownDoesNotFire(t *testing.T) {
	t.Parallel()
	result := signalRun(t, downtimeWindow(t), "signals", map[string]float64{SignalKills: 2}, 0)
	want := []DeadlineEntry{
		{Action: deadlineSet, At: clock(9, 50), Due: clock(10, 40), Signal: SignalKills},
		{Action: deadlineDropped, At: clock(10, 45), Due: clock(10, 40), Signal: SignalKills},
		{Action: deadlineSet, At: clock(10, 45), Due: clock(10, 50), Signal: SignalKills},
		{Action: deadlineFired, At: clock(10, 50), Due: clock(10, 50), Signal: SignalKills},
	}
	if !reflect.DeepEqual(result.Deadlines, want) {
		t.Errorf("the deadlines are %+v, want %+v", result.Deadlines, want)
	}
}

// The replay's ticks are tick passes (spec R11): on signal-crosses-with-no-event, the tick at
// 10:31 takes back ACME-1's slot and gives ACME-2, a P0 issue, its slot under main and under signals
// alike, each as a pass of the tick. Under main it gives ACME-3, below P0, its slot at the tick too.
// Under signals no tick gives a slot below P0: with no limit, ACME-3 starts at an event's pass, and
// with a limit of 2 kills at the deadline's pass at 10:40.
func TestTheReplaysTicksAreTickPasses(t *testing.T) {
	t.Parallel()
	w, _ := readFixture(t, "signal-crosses-with-no-event")
	tick := clock(10, 31)
	pass := func(issue string, at time.Time, action string, kind signals.PassKind) SlotPass {
		return SlotPass{Issue: issue, At: at, Action: action, PassKind: kind}
	}
	main := signalRun(t, w, "main", nil, 0)
	if want := []SlotPass{pass("ACME-1", tick, slotTake, signals.PassTick), pass("ACME-2", tick, slotGive, signals.PassTick),
		pass("ACME-3", tick, slotGive, signals.PassTick)}; !reflect.DeepEqual(main.SlotPasses, want) {
		t.Errorf("under main the slots moved %+v, want %+v", main.SlotPasses, want)
	}
	limited := signalRun(t, w, "signals", map[string]float64{SignalKills: 2}, 0)
	if want := []SlotPass{pass("ACME-1", tick, slotTake, signals.PassTick), pass("ACME-2", tick, slotGive, signals.PassTick),
		pass("ACME-3", clock(10, 40), slotGive, signals.PassDeadline)}; !reflect.DeepEqual(limited.SlotPasses, want) {
		t.Errorf("under signals with a limit the slots moved %+v, want %+v", limited.SlotPasses, want)
	}
	free := signalRun(t, w, "signals", nil, 0)
	if len(free.SlotPasses) != 3 || !reflect.DeepEqual(free.SlotPasses[:2], limited.SlotPasses[:2]) ||
		free.SlotPasses[2].Issue != "ACME-3" || free.SlotPasses[2].Action != slotGive || free.SlotPasses[2].PassKind != signals.PassEvent {
		t.Errorf("under signals with no limit the slots moved %+v, want the tick's two moves and ACME-3 given its slot at an event's pass", free.SlotPasses)
	}
	if len(free.Deadlines) != 0 {
		t.Errorf("with no limit the run kept the deadlines %+v, want none", free.Deadlines)
	}
}

// Run refuses a limit that signals.CheckLimit refuses and a d below zero, naming it, before it
// replays: each run is given the replay's own server, on which it would otherwise run.
func TestARunRefusesAnIllegalLimit(t *testing.T) {
	t.Parallel()
	w, _ := readFixture(t, "no-crossing-due")
	dsn := replayDSN(t)
	for _, c := range []struct {
		cfg  RunConfig
		want string
	}{
		{RunConfig{Limits: map[string]float64{SignalKills: 1.5}}, "no whole number"},
		{RunConfig{Limits: map[string]float64{"memory": 1}}, "no signal a limit can name"},
		{RunConfig{Limits: map[string]float64{SignalIdle: 0}}, "not above 0"},
		{RunConfig{IdleAfter: -time.Minute}, "d is below zero"},
	} {
		cfg := c.cfg
		cfg.Window, cfg.Rule, cfg.Seed, cfg.Slowdown, cfg.PGDSN, cfg.Out = w, mainRule(t), 1, 1, dsn, t.TempDir()
		if _, err := Run(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Run with the limits %v and d %s answered %v, want a refusal naming %q", cfg.Limits, cfg.IdleAfter, err, c.want)
		}
	}
}

// A signal whose load model gives no limit has none in the run, which names it (spec R11): on
// signal-crosses-with-no-event, whose window holds no record of the merge queue, the queue's model
// is not fitted, so a limit on the queue is none and run.json's unlimited names the model.
func TestASignalWhoseModelGivesNoLimitHasNone(t *testing.T) {
	t.Parallel()
	w, _ := readFixture(t, "signal-crosses-with-no-event")
	result := signalRun(t, w, "signals", map[string]float64{SignalQueue: 1, SignalKills: 2}, 0)
	if want := map[string]string{SignalQueue: ModelQueue}; !reflect.DeepEqual(result.Unlimited, want) {
		t.Errorf("the run names the signals with no limit %v, want %v", result.Unlimited, want)
	}
	raw, err := json.Marshal(result.Deadlines)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(result.Deadlines, func(e DeadlineEntry) bool { return e.Signal == SignalKills }) {
		t.Errorf("the deadlines %s hold none of the kills, whose limit is in force", raw)
	}
}
