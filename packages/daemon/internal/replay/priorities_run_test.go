package replay

import (
	"context"
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
// fact of a replay under each recorded rule — the three of the recorded windows, main and before-p0
// — the daemon's record of ACME-2 carries no priority, and the slot goes to ACME-1, as the record
// gave it; under p0-at-once it carries P1, the resolved one, and the slot goes to ACME-2.
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
		{record.MainRule.Name, record.NoPriority, "ACME-1"},
		{"before-p0", record.NoPriority, "ACME-1"},
		{"p0-at-once", record.P1, "ACME-2"},
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

// A replay of a window that lacks the history of an ancestor a priority rests on names that input
// and compares nothing that rests on it (spec R10). p0-inherited-ancestor-missing lacks ACMEAUX-1's
// events, and ACME-2's priority rests on it. Under p0-at-once, a rule with the new P0 start, Run
// refuses the window with an error that names ACMEAUX-1 and ACME-2, and writes no scorecard. Under
// main, a recorded rule, it runs, its result and its run.json name ACMEAUX-1 (missingAncestors),
// and each of its two scorecards reads no wait of urgent work and no priority taken in today's
// line's order: urgentWaits holds no priority and priorityTakenBanded stands at 0.
func TestARunOnAWindowThatLacksAnAncestorNamesIt(t *testing.T) {
	t.Parallel()
	dsn := replayDSN(t)
	window := fixtureWindow(t, "p0-inherited-ancestor-missing")
	refused := t.TempDir()
	_, err := Run(context.Background(), RunConfig{Window: window, Rule: replayedRule(t, "p0-at-once"), Branch: MainBranch, Seed: 1, Slowdown: 1,
		PGDSN: dsn, Out: refused})
	if err == nil {
		t.Fatal("the replay under p0-at-once ran, want a refusal naming ACMEAUX-1")
	}
	for _, named := range []string{"ACMEAUX-1", "ACME-2", "p0-at-once"} {
		if !strings.Contains(err.Error(), named) {
			t.Errorf("the refusal %q does not name %s", err, named)
		}
	}
	if _, err := os.Stat(filepath.Join(refused, scorecardFile)); err == nil {
		t.Errorf("the refused replay wrote %s", scorecardFile)
	}
	out := t.TempDir()
	result, err := Run(context.Background(), RunConfig{Window: window, Rule: mainRule(t), Branch: MainBranch, Seed: 1, Slowdown: 1, PGDSN: dsn, Out: out})
	if err != nil {
		t.Fatalf("replay the window under main: %v", err)
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

// A rule with the new P0 start makes no run of a window that lacks the history of an ancestor a
// priority rests on, and no report reads one (spec R10: validate, forecast and compare refuse such
// a run): runWindows refuses the part of p0-inherited-ancestor-missing under p0-at-once, naming
// the window, the rule and ACMEAUX-1, before it runs any run of the window, main's among them; and
// ReadRuns refuses runs whose window's record names a missing ancestor and that hold a part under
// p0-at-once, naming the same.
func TestARuleWithTheNewP0StartIsRefusedOnAWindowThatLacksAnAncestor(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	spec := windowSpec{dir: filepath.Join(fixtureWindowsDir, "p0-inherited-ancestor-missing"), name: "p0-inherited-ancestor-missing",
		parts: []partSpec{{rule: mainRule(t), branches: []BranchAt{MainBranchAt}}, {rule: replayedRule(t, "p0-at-once"), branches: []BranchAt{MainBranchAt}}}}
	err := runWindows(context.Background(), []windowSpec{spec}, runOptions{root: root, pgDSN: "postgres://unused", parallel: 1, perDraw: 1})
	if err == nil {
		t.Fatal("the runs of p0-at-once on a window that lacks ACMEAUX-1 were made")
	}
	for _, named := range []string{"p0-inherited-ancestor-missing", "p0-at-once", "ACMEAUX-1"} {
		if !strings.Contains(err.Error(), named) {
			t.Errorf("the refusal %q does not name %s", err, named)
		}
	}
	if _, err := os.Stat(filepath.Join(root, spec.name, record.MainRule.Name)); err == nil {
		t.Errorf("a run of main was made before the refusal")
	}

	dir := t.TempDir()
	windows := madeUpWindows("legion-v19.57.3", "legion-v20.42.5")
	windows[1].MissingAncestors = []string{"ACMEAUX-1"}
	writeMadeUpRuns(t, dir, windows, []record.SlotRule{mainRule(t), replayedRule(t, "legion-v19.57.3"), replayedRule(t, "legion-v20.42.5"),
		replayedRule(t, "p0-at-once")}, runPlan{branches: []BranchAt{MainBranchAt}, perDraw: 1}, madeUpRun)
	_, err = ReadRuns(dir)
	if err == nil {
		t.Fatal("runs of p0-at-once on a window whose record names a missing ancestor are read")
	}
	for _, named := range []string{windows[1].Name, "p0-at-once", "ACMEAUX-1"} {
		if !strings.Contains(err.Error(), named) {
			t.Errorf("the refusal %q does not name %s", err, named)
		}
	}
}

// The reports leave out what a window that lacks an ancestor's history cannot give, and name the
// missing input (spec R10): of window-2, whose record and runs name ACMEAUX-1 missing, the
// comparison prints no row of the wait of urgent work and none of priority taken in today's line's
// order, in its scorecard, its tables of every measure and its differences from main, and judges
// no rule on them there (none), while window-1 keeps every row; the validation of window-2 prints
// no row of priorityTakenBanded; the forecast across the change compares no number of the wait of
// urgent work; and each names ACMEAUX-1.
func TestTheReportsLeaveOutThePriorityMeasuresOfAWindowThatLacksAnAncestor(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	windows := madeUpWindows("legion-v19.57.3", "legion-v20.42.5")
	windows[0].Scorecard.UrgentWaits = comparisonUrgentWaits(0.5)
	windows[1].MissingAncestors = []string{"ACMEAUX-1"}
	windows[1].Scorecard.PriorityTakenBanded = 0
	rules := []record.SlotRule{mainRule(t), replayedRule(t, "legion-v19.57.3"), replayedRule(t, "legion-v20.42.5")}
	writeMadeUpRuns(t, dir, windows, rules, runPlan{branches: []BranchAt{MainBranchAt}, perDraw: 1},
		func(w windowRecord, r int, rule record.SlotRule, at BranchAt, seed int64) (Scorecard, Scorecard, runRecord) {
			snapshot, line, ran := madeUpRun(w, r, rule, at, seed)
			ran.MissingAncestors = append([]string{}, w.MissingAncestors...)
			if len(w.MissingAncestors) == 0 {
				snapshot.UrgentWaits, line.UrgentWaits = comparisonUrgentWaits(0.5), comparisonUrgentWaits(0.5)
			} else {
				snapshot.PriorityTakenBanded, line.PriorityTakenBanded = 0, 0
			}
			return snapshot, line, ran
		})
	runs, err := ReadRuns(dir)
	if err != nil {
		t.Fatal(err)
	}
	document, _, err := runs.Comparison(filepath.Join(dir, "comparison-issues.md"))
	if err != nil {
		t.Fatal(err)
	}
	// section is the part of document from its first heading head up to the next heading of its
	// level or above.
	section := func(text, head string) string {
		start := strings.Index(text, head)
		if start < 0 {
			t.Fatalf("the document holds no %q:\n%s", head, text)
		}
		rest := text[start+len(head):]
		level := strings.Repeat("#", strings.Index(head, " "))
		for _, next := range []string{"\n" + level + " ", "\n" + level[:len(level)-1] + " "} {
			if end := strings.Index(rest, next); end >= 0 {
				rest = rest[:end]
			}
		}
		return rest
	}
	scorecards := section(document, "## The scorecard of each window, read from its record")
	for _, w := range windows {
		lacks := len(w.MissingAncestors) > 0
		for name, part := range map[string]string{
			"its record's scorecard":       section(scorecards, "### "+w.Name+"\n"),
			"its table of every measure":   section(document, "### "+w.Name+", `"+MainBranchAt.Label()+"`"),
			"the validation of its window": section(document, "### "+w.Name+" under `"+w.Rule+"`"),
		} {
			for _, row := range []string{"| priorityTakenBanded |", "| P0 issues |", "| urgentWaits[P0].firstSlot.medianHours |"} {
				if name == "the validation of its window" && row != "| priorityTakenBanded |" {
					continue
				}
				if name == "its record's scorecard" && strings.HasPrefix(row, "| urgentWaits[") {
					continue
				}
				if held := strings.Contains(part, row); held == lacks {
					t.Errorf("%s's %s holds the row %q: %t, want %t", w.Name, name, row, held, !lacks)
				}
			}
			if named := strings.Contains(part, "ACMEAUX-1"); named != lacks {
				t.Errorf("%s's %s names ACMEAUX-1: %t, want %t", w.Name, name, named, lacks)
			}
		}
	}
	decision := runs.mustComparison(t).decision()
	for _, judged := range decision.Judged {
		if judged.Window != windows[1].Name {
			continue
		}
		for _, diff := range judged.Differences {
			if (diff.Measure == "priorityTakenBanded" || strings.HasPrefix(diff.Measure, "urgentWaits[")) && diff.Verdict != VerdictNone {
				t.Errorf("on %s the decision judges %s on %s: %s, want none", judged.Window, judged.Rule, diff.Measure, diff.Verdict)
			}
		}
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

// mustComparison is r read as a comparison (readComparison), which fails t where r holds none.
func (r Runs) mustComparison(t *testing.T) comparison {
	t.Helper()
	c, err := r.readComparison()
	if err != nil {
		t.Fatal(err)
	}
	return c
}
