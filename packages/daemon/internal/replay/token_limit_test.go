package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/signals"
)

// suppliedTokenLimit is the project's token limit, in uncached input tokens a minute, that the
// tests of noLimitWindow give a run (RunConfig.TokenLimit).
const suppliedTokenLimit = 6000

// noLimitWindow is the window signal-crosses-with-no-event with a record of the provider from
// 10:20 to 10:55 that holds the project's uncached input tokens and no limit of the project's own
// in any minute: 12,000 tokens a minute from 10:20 to 10:39, and 3,000 from 10:40 on. Its record
// holds ACME-1 alone in flight up to 10:31, and three issues from 10:31 on, when ACME-2 and ACME-3
// take their slots.
func noLimitWindow(t *testing.T) Window {
	t.Helper()
	w, _ := readFixture(t, "signal-crosses-with-no-event")
	for minute := clock(10, 20); minute.Before(clock(10, 55)); minute = minute.Add(time.Minute) {
		line := ProviderMinute{Minute: minute, Uncached: 3000}
		if minute.Before(clock(10, 40)) {
			line.Uncached = 12000
		}
		w.Provider = append(w.Provider, line)
	}
	recorded := map[string][]Span{SignalProvider: {{From: clock(10, 20), To: clock(10, 55)}}}
	for signal, spans := range w.Signals {
		recorded[signal] = spans
	}
	w.Signals = recorded
	return w
}

// runFields is the run.json a run wrote into out, by its fields.
func runFields(t *testing.T, out string) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := readJSON(filepath.Join(out, runFile), &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

// A run takes the project's token limit from its caller in each minute whose provider record holds
// none (RunConfig.TokenLimit, assumption 49). On noLimitWindow under signals with a limit of 1 on
// the provider's headroom, worked by hand:
//
//   - With no token limit, no minute holds a limit, so the tokens model gives no daily test only
//     because the window records no limit of the project's own, and gives no limit for the
//     provider: run.json's unlimited names the provider with the model, no pass reads the
//     headroom, each probe reads it unrecorded, the run keeps no deadline and does not name
//     assumption 49, and run.json names no token limit.
//   - With a token limit of 6,000 a minute the tokens model limits the provider, whose headroom is
//     the modelled tokens of the minute before over 6,000. At 10:31 it reads 10:30's 12,000 at the
//     record's one issue in flight and the replay's one: 12,000 / 6,000 = 2. The tick at 10:31
//     takes ACME-1's slot back and gives ACME-2, a P0 issue, its slot, but not ACME-3, below P0,
//     since the headroom binds the pass at its limit of 1. So from 10:31 the replay holds two
//     issues in flight, ACME-1 and ACME-2, where the record holds three. At 10:35, 10:34's 12,000
//     modelled at 2 of 3 is 8,000, which reads 8,000 / 6,000 and still binds; 10:40's 3,000
//     modelled so is 2,000, which reads 2,000 / 6,000 at 10:41, under the limit. The deadline set
//     at 10:31 is due at 10:41, and its pass gives ACME-3 its slot. At 10:50, 10:49's 3,000 at three
//     issues in flight in the record and in the replay reads 3,000 / 6,000 = 0.5. The run names
//     assumption 49, and run.json names the token limit.
func TestARunTakesTheTokenLimitWhereTheRecordHoldsNone(t *testing.T) {
	t.Parallel()
	w := noLimitWindow(t)
	dsn := replayDSN(t)
	run := func(t *testing.T, tokenLimit int64) (RunResult, []signals.Snapshot, map[string]json.RawMessage) {
		t.Helper()
		var snapshots []signals.Snapshot
		out := t.TempDir()
		result, err := Run(context.Background(), RunConfig{Window: w, Rule: replayedRule(t, "signals"), Seed: 1, Slowdown: 1, PGDSN: dsn, Out: out,
			Limits: map[string]float64{SignalProvider: 1}, Probes: []time.Time{clock(10, 31), clock(10, 35), clock(10, 41), clock(10, 50)},
			TokenLimit: tokenLimit, onSnapshot: func(s signals.Snapshot) { snapshots = append(snapshots, s) }})
		if err != nil {
			t.Fatalf("replay under signals with the token limit %d: %v", tokenLimit, err)
		}
		return result, snapshots, runFields(t, out)
	}

	t.Run("no token limit", func(t *testing.T) {
		t.Parallel()
		result, snapshots, ran := run(t, 0)
		if want := map[string]string{SignalProvider: ModelTokens}; !reflect.DeepEqual(result.Unlimited, want) {
			t.Errorf("the run names the signals with no limit %v, want %v", result.Unlimited, want)
		}
		for _, s := range snapshots {
			if reading, read := s.Readings[SignalProvider]; read {
				t.Errorf("the pass at %s reads the provider's headroom %+v, which has no limit", stamp(s.At), reading)
			}
		}
		why := "no minute holds a limit, and no token limit is given"
		checkProbed(t, result, SignalProvider, []probed{noValueAt(clock(10, 31), why), noValueAt(clock(10, 35), why), noValueAt(clock(10, 41), why),
			noValueAt(clock(10, 50), why)})
		if len(result.Deadlines) != 0 {
			t.Errorf("the run kept the deadlines %+v, want none", result.Deadlines)
		}
		if slices.Contains(result.Assumptions, assumeSuppliedTokenLimit) {
			t.Errorf("the run names the assumptions %v, with %d, and was given no token limit", result.Assumptions, assumeSuppliedTokenLimit)
		}
		if limit, named := ran["tokenLimit"]; named {
			t.Errorf("run.json names the token limit %s, and the run was given none", limit)
		}
	})

	t.Run("a token limit", func(t *testing.T) {
		t.Parallel()
		result, snapshots, ran := run(t, suppliedTokenLimit)
		if len(result.Unlimited) != 0 {
			t.Errorf("the run names the signals with no limit %v, want none: the tokens model limits the provider under the token limit", result.Unlimited)
		}
		checkProbed(t, result, SignalProvider, []probed{
			valueAt(clock(10, 31), 2, "10:30's 12,000 at one issue in flight in the record and in the replay, of 6,000"),
			valueAt(clock(10, 35), 8000.0/6000, "10:34's 12,000 at the replay's 2 issues in flight of the record's 3 is 8,000, of 6,000"),
			valueAt(clock(10, 41), 2000.0/6000, "10:40's 3,000 at the replay's 2 of the record's 3 is 2,000, of 6,000"),
			valueAt(clock(10, 50), 0.5, "10:49's 3,000 at three issues in flight in the record and in the replay, of 6,000"),
		})
		tick := slices.IndexFunc(snapshots, func(s signals.Snapshot) bool { return s.At.Equal(clock(10, 31)) })
		if tick < 0 {
			t.Fatal("no pass read the signals at 10:31")
		}
		reading, read := snapshots[tick].Readings[SignalProvider]
		if !read || reading.Limit == nil || *reading.Limit != 1 || !reading.Value.Recorded || reading.Value.Value != 2 || !reading.Value.Binds(*reading.Limit) {
			t.Errorf("the pass at 10:31 reads the provider's headroom %+v, want 2 against the limit 1, which binds", reading)
		}
		pass := func(issue string, at time.Time, action string, kind signals.PassKind) SlotPass {
			return SlotPass{Issue: issue, At: at, Action: action, PassKind: kind}
		}
		if want := []SlotPass{pass("ACME-1", clock(10, 31), slotTake, signals.PassTick), pass("ACME-2", clock(10, 31), slotGive, signals.PassTick),
			pass("ACME-3", clock(10, 41), slotGive, signals.PassDeadline)}; !reflect.DeepEqual(result.SlotPasses, want) {
			t.Errorf("the slots moved %+v, want %+v", result.SlotPasses, want)
		}
		if want := []DeadlineEntry{
			{Action: deadlineSet, At: clock(10, 31), Due: clock(10, 41), Signal: SignalProvider},
			{Action: deadlineFired, At: clock(10, 41), Due: clock(10, 41), Signal: SignalProvider},
		}; !reflect.DeepEqual(result.Deadlines, want) {
			t.Errorf("the deadlines are %+v, want %+v", result.Deadlines, want)
		}
		if !slices.Contains(result.Assumptions, assumeSuppliedTokenLimit) {
			t.Errorf("the run names the assumptions %v, which lack %d, the token limit it modelled minutes against", result.Assumptions, assumeSuppliedTokenLimit)
		}
		if limit := string(ran["tokenLimit"]); limit != "6000" {
			t.Errorf("run.json names the token limit %q, want 6000", limit)
		}
	})
}

// A limit of the project's own that the record holds stands under a token limit, which a run and the
// record's own reading of the signals (ReadSignalRecord) take only in a minute whose record holds
// none, and a minute outside the record's span stays unrecorded, whatever the token limit
// (assumptions 47 and 49). On providerWindow, under a token limit of 12,000, worked by hand: at
// 09:30 the minute before, 09:29, lies before the record's span, which begins at 09:30; at 09:31
// and at 10:02 the minutes before hold the recorded limit of 6,000, against which 3,000 reads 0.5
// and 6,600 reads 1.1; the limit taken away at 11:30 and the limit of one model's own from 11:45
// are no limit of the project's own, so at 11:31 and at 11:46 the 3,000 of the minute before reads
// 3,000 / 12,000 = 0.25. The run, under main with a limit on every signal, reads the provider from
// the tokens model, which passes its daily test on the recorded limit, at no issue in flight in the
// record or the replay, so each minute keeps its recorded tokens; it names assumption 49. The
// record's reading refuses a token limit below zero.
func TestATokenLimitStandsOnlyWhereTheRecordHoldsNoLimitOfTheProjectsOwn(t *testing.T) {
	t.Parallel()
	const supplied = 12000
	want := []probed{
		noValueAt(clock(9, 30), "the minute before, 09:29, lies before the record's span"),
		valueAt(clock(9, 31), 0.5, "3,000 of the recorded 6,000, which stands"),
		valueAt(clock(10, 2), 1.1, "6,600 of the recorded 6,000, which stands"),
		valueAt(clock(11, 31), 0.25, "the limit was taken away at 11:30: 3,000 of the token limit, 12,000"),
		valueAt(clock(11, 46), 0.25, "the limit of 11:45 is one model's own: 3,000 of the token limit, 12,000"),
	}
	w := providerWindow(t)
	t.Run("the record", func(t *testing.T) {
		t.Parallel()
		r, err := ReadSignalRecord(w, signals.DefaultIdleAfter, supplied)
		if err != nil {
			t.Fatalf("ReadSignalRecord: %v", err)
		}
		for _, p := range want {
			got := providerAt(r, p.at)
			switch {
			case p.recorded && (!got.Recorded || got.Value != p.value):
				t.Errorf("the provider at %s reads %s, want %v: %s", stamp(p.at), describeValue(got), p.value, p.why)
			case !p.recorded && got.Recorded:
				t.Errorf("the provider at %s reads %v, want it unrecorded: %s", stamp(p.at), got.Value, p.why)
			}
		}
		if _, err := ReadSignalRecord(w, signals.DefaultIdleAfter, -1); err == nil || !strings.Contains(err.Error(), "token limit is -1") {
			t.Errorf("ReadSignalRecord with the token limit -1 answered %v, want a refusal naming it", err)
		}
	})
	t.Run("a run", func(t *testing.T) {
		t.Parallel()
		result := signalRunWith(t, RunConfig{Window: w, Rule: mainRule(t), Limits: everyLimit, Probes: probesOf(want), TokenLimit: supplied})
		checkProbed(t, result, SignalProvider, want)
		if !slices.Contains(result.Assumptions, assumeSuppliedTokenLimit) {
			t.Errorf("the run names the assumptions %v, which lack %d, the token limit it modelled minutes against", result.Assumptions, assumeSuppliedTokenLimit)
		}
	})
}

// Under a rule that admits by no signal the token limit changes nothing a run admits or measures
// (assumption 49). noLimitWindow replayed under main with no limit, once with a token limit of
// 6,000 and once with none, writes the same log.jsonl, scorecard.json and scorecard-line.json,
// byte for byte, and the same run.json, but for the fields that measure the run itself
// (hostFields: startedAt, wallSeconds, meanPassMillis, peakMemoryBytes and database) and
// tokenLimit, which the run given a token limit names and the other leaves out.
func TestATokenLimitChangesNothingUnderARuleThatReadsNoSignal(t *testing.T) {
	t.Parallel()
	w := noLimitWindow(t)
	dsn := replayDSN(t)
	var outs [2]string
	for i, tokenLimit := range []int64{0, suppliedTokenLimit} {
		outs[i] = t.TempDir()
		if _, err := Run(context.Background(), RunConfig{Window: w, Rule: mainRule(t), Seed: 1, Slowdown: 1, PGDSN: dsn, Out: outs[i],
			TokenLimit: tokenLimit}); err != nil {
			t.Fatalf("replay under main with the token limit %d: %v", tokenLimit, err)
		}
	}
	for _, name := range []string{logFile, scorecardFile, lineScorecardFile} {
		without, err := os.ReadFile(filepath.Join(outs[0], name))
		if err != nil {
			t.Fatal(err)
		}
		with, err := os.ReadFile(filepath.Join(outs[1], name))
		if err != nil {
			t.Fatal(err)
		}
		if line, a, b, differ := firstDifference(without, with); differ {
			t.Errorf("the runs with no token limit and with one write %s otherwise, first at line %d:\n  %s\n  %s", name, line, a, b)
		}
	}
	runs := [2]map[string]json.RawMessage{runFields(t, outs[0]), runFields(t, outs[1])}
	if limit, named := runs[0]["tokenLimit"]; named {
		t.Errorf("the run with no token limit names %s in run.json", limit)
	}
	if limit := string(runs[1]["tokenLimit"]); limit != "6000" {
		t.Errorf("the run with a token limit names the token limit %q in run.json, want 6000", limit)
	}
	var kept [2][]byte
	for i, fields := range runs {
		for _, field := range append(slices.Clone(hostFields), "tokenLimit") {
			delete(fields, field)
		}
		data, err := json.MarshalIndent(fields, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		kept[i] = data
	}
	if !bytes.Equal(kept[0], kept[1]) {
		line, a, b, _ := firstDifference(kept[0], kept[1])
		t.Errorf("the runs with no token limit and with one write run.json otherwise but for the host fields and tokenLimit, first at line %d:\n  %s\n  %s", line, a, b)
	}
}

// A directory of runs keeps each run with the token limit it ran with (run.json's tokenLimit), so a
// validation, a forecast or a comparison into it refuses a run there already that ran with another,
// as it refuses one made at another cap, naming both; a run.json written before runs named a token
// limit names none, which reads as none, the limit that run ran with. The test validates
// no-crossing-due on its main branch with one seed: with no token limit into one directory, whose
// run.json names none, and then with a token limit of 6,000 into it, which is refused; and with a
// token limit of 6,000 into another, whose run.json names it, again with it, which keeps the run,
// and then with none, which is refused.
func TestARunThereAlreadyWithAnotherTokenLimitIsRefused(t *testing.T) {
	t.Parallel()
	const name = "no-crossing-due"
	w, _ := readFixture(t, name)
	rule := replayedRule(t, partsRule(w.Parts))
	dsn := replayDSN(t)
	validate := func(out string, tokenLimit int64) error {
		_, err := RunValidate(context.Background(), ValidateConfig{Window: filepath.Join(fixtureWindowsDir, name), Rule: rule, PGDSN: dsn, Parallel: 1,
			Out: out, TokenLimit: tokenLimit, plan: &runPlan{branches: []BranchAt{MainBranchAt}, perDraw: 1}})
		return err
	}
	ranWith := func(out string) map[string]json.RawMessage {
		return runFields(t, runDir(out, name, rule.Name, MainBranchAt, 1))
	}

	none := t.TempDir()
	if err := validate(none, 0); err != nil {
		t.Fatalf("the validation with no token limit: %v", err)
	}
	if limit, named := ranWith(none)["tokenLimit"]; named {
		t.Errorf("the run with no token limit names the token limit %s in run.json", limit)
	}
	if err := validate(none, suppliedTokenLimit); err == nil || !strings.Contains(err.Error(), "ran with the token limit 0 (0 for none), and this one runs with 6000") {
		t.Errorf("a validation with the token limit 6000 into runs made with none answered %v, want a refusal naming both", err)
	}

	limited := t.TempDir()
	if err := validate(limited, suppliedTokenLimit); err != nil {
		t.Fatalf("the validation with the token limit 6000: %v", err)
	}
	if limit := string(ranWith(limited)["tokenLimit"]); limit != "6000" {
		t.Errorf("the run with the token limit 6000 names the token limit %q in run.json, want 6000", limit)
	}
	if err := validate(limited, suppliedTokenLimit); err != nil {
		t.Errorf("a validation with the token limit 6000 into runs made with it answered %v, want the runs kept", err)
	}
	if err := validate(limited, 0); err == nil || !strings.Contains(err.Error(), "ran with the token limit 6000 (0 for none), and this one runs with 0") {
		t.Errorf("a validation with no token limit into runs made with the token limit 6000 answered %v, want a refusal naming both", err)
	}
}
