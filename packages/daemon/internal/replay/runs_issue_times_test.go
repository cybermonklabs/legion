package replay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// issueTimesRuns writes under dir a directory of runs of the window name, whose record gives ACME-1
// its first slot at recordedAt: one part under rule with one seed of the main branch and one of
// main+timing=wall, each run's line scorecard giving ACME-1's first slot at replayedAt, or none where
// replayedAt is zero. With named, the window's record names rule as the rule its daemon ran;
// otherwise it names none, as window.json with no parts does. The record's and each run's line
// scorecard hold the readings only a line timeline gives (lineReadings).
func issueTimesRuns(t *testing.T, dir, name, rule string, named bool, from, to, recordedAt, replayedAt time.Time) []BranchAt {
	t.Helper()
	rec := windowRecord{Name: name, Project: "ACME", From: from, To: to,
		Scorecard: lineReadings(Scorecard{From: from, To: to, FirstSlot: map[string]time.Time{"ACME-1": recordedAt}, DoneAt: map[string]time.Time{},
			UrgentWaits: map[string]UrgentWait{}}),
		WaitsGaveBack: []waitGaveBack{}}
	if named {
		rec.Rule = rule
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	wall, err := BranchNamed("main+timing=wall")
	must(err)
	branches := []BranchAt{MainBranchAt, {Branch: wall, Slowdown: 1}}
	must(writeJSON(filepath.Join(dir, name, recordFile), rec))
	part := partRecord{Window: name, Rule: rule, BreakEven: []partRun{}, Parallel: 1}
	for _, at := range branches {
		part.Branches = append(part.Branches, partRun{Branch: at.Branch.Name, Slowdown: at.Slowdown, Seeds: []int64{1}})
		run := runDir(dir, name, rule, at, 1)
		line := lineReadings(Scorecard{From: from, To: to, FirstSlot: map[string]time.Time{}, DoneAt: map[string]time.Time{}, UrgentWaits: map[string]UrgentWait{}})
		if !replayedAt.IsZero() {
			line.FirstSlot["ACME-1"] = replayedAt
		}
		must(writeJSON(filepath.Join(run, runFile), runRecord{Rule: rule, Branch: at.Branch, Seed: 1, Slowdown: 1,
			SignalSummaries: map[string]SignalSummary{}, SuppliedRefused: map[string]int{}}))
		must(writeJSON(filepath.Join(run, scorecardFile), Scorecard{From: from, To: to, UrgentWaits: map[string]UrgentWait{}}))
		must(writeJSON(filepath.Join(run, lineScorecardFile), line))
	}
	must(writeJSON(filepath.Join(dir, name, rule, partFile), part))
	return branches
}

// A validation read from runs keeps each replay's per-issue times when the window names no rule
// its daemon ran (window.json with no parts, so its record's rule is ""): the validation's own rule
// is then the part it reads, and each issue's error is the replay's time against the record's. A
// window collected without parts dropped those times, so every issue read as one the replay never
// placed and its error stood at the window's end less the record's time. ACME-1 takes its first
// slot at 01:00 in the record and at 01:10 in the one seed: its error is 10 minutes, not the 23
// hours from 01:00 to the window's end. The main branch of every part keeps its times for the issues
// file of a comparison whatever the window names (readPart), so the part holds a second branch,
// main+timing=wall, whose times only the window's rule kept, and the test reads both.
func TestAValidationOfAWindowNamingNoRuleKeepsTheReplaysIssueTimes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const window, rule = "no-parts", "legion-v20.42.5"
	from := time.Date(2001, time.March, 5, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	recordedAt, replayedAt := from.Add(time.Hour), from.Add(time.Hour+10*time.Minute)
	branches := issueTimesRuns(t, dir, window, rule, false, from, to, recordedAt, replayedAt)

	runs, err := ReadRuns(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := runs.Rule(window); err != nil || got != "" {
		t.Fatalf("the fixture's window names the rule %q (%v), want none", got, err)
	}
	v, err := runs.Validation(window, rule)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Branches) != len(branches) {
		t.Fatalf("the validation holds %d branches, want %d", len(v.Branches), len(branches))
	}
	for _, b := range v.Branches {
		if len(b.Issues) != 1 {
			t.Errorf("on %s the validation holds the issues %+v, want ACME-1 alone", b.Branch.Label(), b.Issues)
			continue
		}
		if got := b.Issues[0].FirstSlot; got != 10 {
			t.Errorf("on %s ACME-1's first-slot error is %v minutes, want 10: the replay's time against the record's, not the window's end against the record's (%v)",
				b.Branch.Label(), got, to.Sub(recordedAt).Minutes())
		}
	}
}

// A validation whose replays give no issue a first slot or a done on any branch, while the record
// gives some, is refused, with an error that names the window: its issues' errors would all stand
// at the window's end less the record's times, a validation of nothing the replays did. The part of
// the window no-times, under the rule its record names, holds a run of the main branch and one of
// main+timing=wall, neither giving ACME-1 the first slot the record gives it at 01:00.
func TestAValidationWhoseReplaysGiveNoIssueTimeIsRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const window, rule = "no-times", "legion-v20.42.5"
	from := time.Date(2001, time.March, 5, 0, 0, 0, 0, time.UTC)
	issueTimesRuns(t, dir, window, rule, true, from, from.Add(24*time.Hour), from.Add(time.Hour), time.Time{})

	runs, err := ReadRuns(dir)
	if err != nil {
		t.Fatal(err)
	}
	v, err := runs.Validation(window, rule)
	if err == nil {
		t.Fatalf("the validation of %s, whose replays give no issue a time, is not refused: %+v", window, v.Branches)
	}
	if !strings.Contains(err.Error(), window) {
		t.Errorf("the refusal %q does not name the window %s", err, window)
	}
}

// A run written before its scorecards held the wait of urgent work and priority taken in today's
// line's order (urgentWaits, priorityTakenBanded) is refused by every report read from runs, with
// an error that names the run and says it must be run again: read, it would give 0 in each of those
// measures, and the decision would judge a rule on priority taken it never measured. So is a
// window's record.json written before its scorecard held them. The test writes the runs of
// issueTimesRuns and then takes urgentWaits out of one run's scorecard.json, and, apart, out of the
// window's record.json.
func TestRunsWrittenBeforeTheUrgentWaitsAreRefused(t *testing.T) {
	t.Parallel()
	const window, rule = "old-runs", "legion-v20.42.5"
	from := time.Date(2001, time.March, 5, 0, 0, 0, 0, time.UTC)
	strip := func(t *testing.T, path string) {
		t.Helper()
		var fields map[string]any
		if err := readJSON(path, &fields); err != nil {
			t.Fatal(err)
		}
		scorecard := fields
		if inner, ok := fields["scorecard"].(map[string]any); ok {
			scorecard = inner
		}
		delete(scorecard, "urgentWaits")
		delete(scorecard, "priorityTakenBanded")
		if err := writeJSON(path, fields); err != nil {
			t.Fatal(err)
		}
	}
	for _, old := range []struct {
		name string
		file func(dir string) string
	}{
		{"a run's scorecard.json", func(dir string) string {
			return filepath.Join(runDir(dir, window, rule, MainBranchAt, 1), scorecardFile)
		}},
		{"a run's scorecard-line.json", func(dir string) string {
			return filepath.Join(runDir(dir, window, rule, MainBranchAt, 1), lineScorecardFile)
		}},
		{"the window's record.json", func(dir string) string { return filepath.Join(dir, window, recordFile) }},
	} {
		t.Run(old.name, func(t *testing.T) {
			dir := t.TempDir()
			issueTimesRuns(t, dir, window, rule, true, from, from.Add(24*time.Hour), from.Add(time.Hour), from.Add(time.Hour))
			if _, err := ReadRuns(dir); err != nil {
				t.Fatalf("the runs as written are refused: %v", err)
			}
			path := old.file(dir)
			if _, err := os.Stat(path); err != nil {
				t.Fatal(err)
			}
			strip(t, path)
			_, err := ReadRuns(dir)
			if err == nil {
				t.Fatalf("runs with %s written before the urgent waits are read", old.name)
			}
			named := filepath.Dir(path)
			if old.name == "the window's record.json" {
				named = path
			}
			for _, want := range []string{named, "urgentWaits", "priorityTakenBanded", "again"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not say %q", err, want)
				}
			}
		})
	}
}

// lineReadings is card with the readings only a line timeline gives (holdsLineReadings), each
// empty: no issue filed, no slot given out, no count in flight and no urgent work.
func lineReadings(card Scorecard) Scorecard {
	card.Filed, card.SlotsGiven, card.DailyMaxima, card.UrgentMeans = map[string]time.Time{}, []time.Time{}, map[string]float64{}, map[string]UrgentMean{}
	return card
}

// A run whose line scorecard was written before it held the readings only a line timeline gives —
// the issues filed (filed), the moments each slot was given out (slotsGiven), the daily maxima
// (dailyMaxima) and the means of the wait of urgent work (urgentMeans), which R5's branch of the
// recorded conflicts, the cap backtest and the forecast read — is refused by every report read
// from runs, with an error that names the run and says it must be run again: read, it would name
// every issue the record filed as left out, count no slot in any span of one cap, and forecast 0 in
// each of those numbers. So is a window's record.json written before its scorecard held them. The
// test writes the runs of issueTimesRuns and then takes each reading out of the main branch's line
// scorecard, and, apart, out of the window's record.json.
func TestRunsWrittenBeforeTheLineReadingsAreRefused(t *testing.T) {
	t.Parallel()
	const window, rule = "old-line-runs", "legion-v20.42.5"
	from := time.Date(2001, time.March, 5, 0, 0, 0, 0, time.UTC)
	for _, reading := range []string{"filed", "slotsGiven", "dailyMaxima", "urgentMeans"} {
		for _, old := range []struct {
			name string
			file func(dir string) string
		}{
			{"a run's scorecard-line.json", func(dir string) string {
				return filepath.Join(runDir(dir, window, rule, MainBranchAt, 1), lineScorecardFile)
			}},
			{"the window's record.json", func(dir string) string { return filepath.Join(dir, window, recordFile) }},
		} {
			t.Run(reading+" out of "+old.name, func(t *testing.T) {
				dir := t.TempDir()
				issueTimesRuns(t, dir, window, rule, true, from, from.Add(24*time.Hour), from.Add(time.Hour), from.Add(time.Hour))
				if _, err := ReadRuns(dir); err != nil {
					t.Fatalf("the runs as written are refused: %v", err)
				}
				path := old.file(dir)
				patchJSON(t, path, func(fields map[string]any) {
					if inner, ok := fields["scorecard"].(map[string]any); ok {
						fields = inner
					}
					delete(fields, reading)
				})
				_, err := ReadRuns(dir)
				if err == nil {
					t.Fatalf("runs with %s written before it held %s are read", old.name, reading)
				}
				named := filepath.Dir(path)
				if old.name == "the window's record.json" {
					named = path
				}
				for _, want := range []string{named, reading, "again"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the refusal %q does not say %q", err, want)
					}
				}
			})
		}
	}
}
