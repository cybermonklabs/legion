package replay

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// Only a rule with the new P0 start reads the priority Dispatch resolves (spec R10): a recorded
// rule keeps its release's own admissions and order, by each issue's own priority. The window
// one-free-slot-opposite-order holds ACME-2, with no priority of its own, under ACMEAUX-1, at P1,
// so it resolves to P1, and ACME-1, at P2 of its own; one slot comes free at 10:00. After every
// fact of a replay under each rule that reads each issue's own priority — the three of the
// recorded windows and before-p0 — the daemon's record of ACME-2 carries no priority, and the slot
// goes to ACME-1, as the record gave it; under main, which starts a P0 outside the cap since
// cybermonklabs/legion#5488, it carries P1, the resolved one, and the slot goes to ACME-2.
func TestOnlyARuleWithTheNewP0StartReadsTheResolvedPriority(t *testing.T) {
	t.Parallel()
	dsn := replayDSN(t)
	window := fixtureWindow(t, "one-free-slot-opposite-order")
	for _, run := range []struct {
		rule  string
		want  record.Priority
		first string
	}{
		{"legion-v19.57.3", record.NoPriority, "ACME-1"},
		{"legion-v20.42.5", record.NoPriority, "ACME-1"},
		{"legion-v22.14.1", record.NoPriority, "ACME-1"},
		{record.MainRule.Name, record.P1, "ACME-2"},
		{"before-p0", record.NoPriority, "ACME-1"},
	} {
		t.Run(run.rule, func(t *testing.T) {
			t.Parallel()
			records := record.NewStore()
			var read []record.Priority
			look := func(ctx context.Context, pool *store.Pool, _ *shadow) error {
				return pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
					issue, err := records.Issue(ctx, tx, "ACME-2")
					if issue != nil {
						read = append(read, issue.Priority)
					}
					return err
				})
			}
			result, err := Run(context.Background(), RunConfig{Window: window, Rule: replayedRule(t, run.rule), Branch: MainBranch, Seed: 1, Slowdown: 1,
				PGDSN: dsn, Out: t.TempDir(), afterFact: look})
			if err != nil {
				t.Fatalf("replay the window under %s: %v", run.rule, err)
			}
			if len(read) == 0 {
				t.Fatalf("under %s no fact left a record of ACME-2 to read", run.rule)
			}
			for i, priority := range read {
				if priority != run.want {
					t.Errorf("under %s the daemon's record of ACME-2 carries the priority %q after its %d-th fact, want %q", run.rule, priority, i+1, run.want)
					break
				}
			}
			if got, want := result.Scorecard.FirstSlot, map[string]time.Time{run.first: clock(10, 0)}; len(got) != 1 || !got[run.first].Equal(want[run.first]) {
				t.Errorf("under %s the replay gives the first slots %v, want %s's at 10:00 alone", run.rule, got, run.first)
			}
		})
	}
}

// A rule that starts no P0 issue outside the cap keeps its admissions and its line on each issue's
// own priority (spec R10: each recorded rule keeps its release's own admissions and order, by each
// issue's own priority) on a window whose events carry the priority Dispatch resolved as on one
// whose events carry none. p0-recorded-descendant-update holds ACME-1 and ACME-2, neither with a
// priority of its own and each the root of its own tree: ACME-1 takes the one slot at 09:05, and
// ACME-2, filed at 09:10 under ACMEAUX-1, carries resolved_priority 1, and from Dispatch's update
// at 09:30 resolved_priority 0. After every fact of a replay under legion-v20.42.5, a recorded
// rule, and under before-p0, main with no P0 issue started outside the cap, each issue's record
// gives admission its own priority, none (record.Issue.AdmissionPriority, which the start outside
// the cap and the cap's count read); the line places each entry in the band its own priority
// gives, none, under before-p0, whose line stands by priority first, as under legion-v20.42.5,
// whose line stands by none; ACME-2 is the line after the last fact; and the one slot given is
// ACME-1's at 09:05, as the record gave it. A replay that hands such a rule the recorded
// resolved_priority gives ACME-2 P0 from 09:30, and under before-p0 places it in the P0 band.
func TestARuleWithNoP0StartReadsEachIssuesOwnPriorityWhereTheRecordCarriesTheResolvedOne(t *testing.T) {
	t.Parallel()
	dsn := replayDSN(t)
	window := fixtureWindow(t, "p0-recorded-descendant-update")
	for _, name := range []string{"legion-v20.42.5", "before-p0"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rule := replayedRule(t, name)
			records := record.NewStore()
			facts := 0
			seen := map[string]bool{}
			var failures, line []string
			fail := func(what string) {
				if !seen[what] {
					seen[what] = true
					failures = append(failures, fmt.Sprintf("from fact %d, %s", facts, what))
				}
			}
			look := func(ctx context.Context, pool *store.Pool, _ *shadow) error {
				facts++
				return pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
					issues, err := records.LiveIssues(ctx, tx, window.Project)
					if err != nil {
						return err
					}
					slots, err := records.Slots(ctx, tx)
					if err != nil {
						return err
					}
					lineFacts, err := record.ReadLineFactsUnder(ctx, tx, records, window.Project, issues, window.GateSteps, rule)
					if err != nil {
						return err
					}
					for _, issue := range issues {
						if got := issue.AdmissionPriority(); got != record.NoPriority {
							fail(fmt.Sprintf("%s's record gives admission the priority %q, want none, its own", issue.Key, got))
						}
					}
					entries, _ := record.Waiting(issues, slots, lineFacts)
					line = line[:0]
					for _, entry := range entries {
						line = append(line, entry.Issue.Key)
						if entry.Priority != record.NoPriority {
							fail(fmt.Sprintf("the line places %s in the band %q, want none, the band of its own priority", entry.Issue.Key, entry.Priority))
						}
					}
					return nil
				})
			}
			result, err := Run(context.Background(), RunConfig{Window: window, Rule: rule, Branch: MainBranch, Seed: 1, Slowdown: 1,
				PGDSN: dsn, Out: t.TempDir(), afterFact: look})
			if err != nil {
				t.Fatalf("replay the window under %s: %v", name, err)
			}
			if facts == 0 {
				t.Fatalf("the replay under %s applied no fact, so nothing was read", name)
			}
			for _, failure := range failures {
				t.Errorf("under %s, %s", name, failure)
			}
			if !slices.Equal(line, []string{"ACME-2"}) {
				t.Errorf("under %s the line after the last fact is %v, want [ACME-2]", name, line)
			}
			for card, got := range map[string]map[string]time.Time{"snapshot": result.Scorecard.FirstSlot, "line": result.LineScorecard.FirstSlot} {
				if len(got) != 1 || !got["ACME-1"].Equal(clock(9, 5)) {
					t.Errorf("under %s the %s scorecard gives the first slots %v, want ACME-1's at 09:05 alone", name, card, got)
				}
			}
		})
	}
}

// A replay of a window that lacks the history of an ancestor a priority rests on names that input
// and compares nothing that rests on it (spec R10). p0-inherited-ancestor-missing lacks ACMEAUX-1's
// events, and ACME-2's priority rests on it. Under main, a rule with the new P0 start since
// cybermonklabs/legion#5488, Run refuses the window with an error that names ACMEAUX-1 and ACME-2,
// and writes no scorecard. Under legion-v20.42.5, a recorded rule, it runs, its result and its
// run.json name ACMEAUX-1 (missingAncestors), and each of its two scorecards reads no wait of
// urgent work and no priority taken in today's line's order: urgentWaits holds no priority and
// priorityTakenBanded stands at 0.
func TestARunOnAWindowThatLacksAnAncestorNamesIt(t *testing.T) {
	t.Parallel()
	dsn := replayDSN(t)
	window := fixtureWindow(t, "p0-inherited-ancestor-missing")
	refused := t.TempDir()
	_, err := Run(context.Background(), RunConfig{Window: window, Rule: mainRule(t), Branch: MainBranch, Seed: 1, Slowdown: 1,
		PGDSN: dsn, Out: refused})
	if err == nil {
		t.Fatal("the replay under main ran, want a refusal naming ACMEAUX-1")
	}
	for _, named := range []string{"ACMEAUX-1", "ACME-2", record.MainRule.Name} {
		if !strings.Contains(err.Error(), named) {
			t.Errorf("the refusal %q does not name %s", err, named)
		}
	}
	if _, err := os.Stat(filepath.Join(refused, scorecardFile)); err == nil {
		t.Errorf("the refused replay wrote %s", scorecardFile)
	}
	out := t.TempDir()
	result, err := Run(context.Background(), RunConfig{Window: window, Rule: replayedRule(t, "legion-v20.42.5"), Branch: MainBranch, Seed: 1,
		Slowdown: 1, PGDSN: dsn, Out: out})
	if err != nil {
		t.Fatalf("replay the window under legion-v20.42.5: %v", err)
	}
	want := []string{"ACMEAUX-1"}
	if !slices.Equal(result.MissingAncestors, want) {
		t.Errorf("the run names the missing ancestors %v, want %v", result.MissingAncestors, want)
	}
	var ran struct {
		MissingAncestors []string `json:"missingAncestors"`
	}
	if err := readJSON(filepath.Join(out, runFile), &ran); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ran.MissingAncestors, want) {
		t.Errorf("run.json names the missing ancestors %v, want %v", ran.MissingAncestors, want)
	}
	for name, card := range map[string]Scorecard{"snapshot": result.Scorecard, "line": result.LineScorecard} {
		if card.UrgentWaits == nil || len(card.UrgentWaits) != 0 || card.PriorityTakenBanded != 0 {
			t.Errorf("the %s scorecard reads the urgent waits %v and priorityTakenBanded %v, want none and 0", name, card.UrgentWaits, card.PriorityTakenBanded)
		}
	}
	if result.LineScorecard.UrgentMeans == nil || len(result.LineScorecard.UrgentMeans) != 0 {
		t.Errorf("the line scorecard reads the urgent means %v, want none", result.LineScorecard.UrgentMeans)
	}
}

// A rule with the new P0 start, main among them since cybermonklabs/legion#5488, makes no run of a
// window that lacks the history of an ancestor a priority rests on, and no report reads one (spec
// R10: validate, forecast and compare refuse such a run): runWindows refuses the part of
// p0-inherited-ancestor-missing under main, naming the window, the rule and ACMEAUX-1, before it
// runs any run of the window, the recorded rule's among them; and ReadRuns refuses runs whose
// window's record names a missing ancestor and that hold a part under main, naming the same.
func TestARuleWithTheNewP0StartIsRefusedOnAWindowThatLacksAnAncestor(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	recorded := replayedRule(t, "legion-v20.42.5")
	spec := windowSpec{dir: filepath.Join(fixtureWindowsDir, "p0-inherited-ancestor-missing"), name: "p0-inherited-ancestor-missing",
		parts: []partSpec{{rule: recorded, branches: []BranchAt{MainBranchAt}}, {rule: mainRule(t), branches: []BranchAt{MainBranchAt}}}}
	err := runWindows(context.Background(), []windowSpec{spec}, runOptions{root: root, pgDSN: "postgres://unused", parallel: 1, perDraw: 1})
	if err == nil {
		t.Fatal("the runs of main on a window that lacks ACMEAUX-1 were made")
	}
	for _, named := range []string{"p0-inherited-ancestor-missing", record.MainRule.Name, "ACMEAUX-1"} {
		if !strings.Contains(err.Error(), named) {
			t.Errorf("the refusal %q does not name %s", err, named)
		}
	}
	if _, err := os.Stat(filepath.Join(root, spec.name, recorded.Name)); err == nil {
		t.Errorf("a run of %s was made before the refusal", recorded.Name)
	}

	dir := t.TempDir()
	windows := madeUpWindows("legion-v19.57.3", "legion-v20.42.5")
	windows[1].MissingAncestors = []string{"ACMEAUX-1"}
	writeMadeUpRuns(t, dir, windows, []record.SlotRule{mainRule(t), replayedRule(t, "legion-v19.57.3"), recorded},
		runPlan{branches: []BranchAt{MainBranchAt}, perDraw: 1}, madeUpRun)
	_, err = ReadRuns(dir)
	if err == nil {
		t.Fatal("runs of main on a window whose record names a missing ancestor are read")
	}
	for _, named := range []string{windows[1].Name, record.MainRule.Name, "ACMEAUX-1"} {
		if !strings.Contains(err.Error(), named) {
			t.Errorf("the refusal %q does not name %s", err, named)
		}
	}
}

// The reports of a window that lacks an ancestor's history give what it can give and name the
// missing input (spec R10), and the comparison, which needs main on each window, refuses it as a
// gap of its collection (spec R7). Of window-2, whose record and runs name ACMEAUX-1 missing, only
// the recorded rules run, since main reads the priority Dispatch resolves: the comparison of the two
// windows refuses, naming main, window-2 and ACMEAUX-1, and says to collect window-2 again; the
// validation of window-2 under its recorded rule prints no row of priorityTakenBanded and names
// ACMEAUX-1; and the forecast across the change compares no number of the wait of urgent work and
// names ACMEAUX-1.
func TestTheReportsLeaveOutThePriorityMeasuresOfAWindowThatLacksAnAncestor(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	windows := madeUpWindows("legion-v19.57.3", "legion-v20.42.5")
	windows[0].Scorecard.UrgentWaits = comparisonUrgentWaits(0.5)
	windows[1].MissingAncestors = []string{"ACMEAUX-1"}
	windows[1].Scorecard.PriorityTakenBanded = 0
	run := func(w windowRecord, r int, rule record.SlotRule, at BranchAt, seed int64) (Scorecard, Scorecard, runRecord) {
		snapshot, line, ran := madeUpRun(w, r, rule, at, seed)
		ran.MissingAncestors = append([]string{}, w.MissingAncestors...)
		if len(w.MissingAncestors) == 0 {
			snapshot.UrgentWaits, line.UrgentWaits = comparisonUrgentWaits(0.5), comparisonUrgentWaits(0.5)
		} else {
			snapshot.PriorityTakenBanded, line.PriorityTakenBanded = 0, 0
		}
		return snapshot, line, ran
	}
	// main reads the priority Dispatch resolves, so it makes no run of window-2, which lacks the
	// history of ACMEAUX-1: the recorded rules are the rules that run on both windows.
	writeMadeUpRuns(t, dir, windows, []record.SlotRule{replayedRule(t, "legion-v19.57.3"), replayedRule(t, "legion-v20.42.5")},
		runPlan{branches: []BranchAt{MainBranchAt}, perDraw: 1}, run)
	runs, err := ReadRuns(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runs.Comparison(filepath.Join(dir, "comparison-issues.md")); err == nil {
		t.Error("the comparison of a window that lacks ACMEAUX-1 was made, want a refusal: main, which it needs, runs no part of it")
	} else {
		for _, named := range []string{record.MainRule.Name, windows[1].Name, "ACMEAUX-1", "collect " + windows[1].Name + " again"} {
			if !strings.Contains(err.Error(), named) {
				t.Errorf("the comparison's refusal %q does not name %q", err, named)
			}
		}
	}
	validation, err := runs.Validation(windows[1].Name, windows[1].Rule)
	if err != nil {
		t.Fatal(err)
	}
	var validated strings.Builder
	if err := validation.Write(&validated, 2, func(BranchAt) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(validated.String(), "| priorityTakenBanded |") || !strings.Contains(validated.String(), "ACMEAUX-1") {
		t.Errorf("the validation of %s under %s prints priorityTakenBanded or does not name ACMEAUX-1:\n%s", windows[1].Name, windows[1].Rule,
			validated.String())
	}
	forecast, err := runs.Forecast(windows[0].Name, windows[1].Name, windows[1].Rule)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range forecast.Checks {
		if strings.HasPrefix(check.Quantity, "urgentWaits[") {
			t.Errorf("the forecast compares %s, which %s cannot give", check.Quantity, windows[1].Name)
		}
	}
	var written strings.Builder
	if err := forecast.Write(&written, 2); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(written.String(), "ACMEAUX-1") {
		t.Errorf("the forecast does not name ACMEAUX-1, the input it lacks:\n%s", written.String())
	}
}
