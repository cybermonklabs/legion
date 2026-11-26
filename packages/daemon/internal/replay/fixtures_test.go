package replay

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/signals"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// fixtureWindowsDir holds one directory for each fixture window: a recorded window in the format of
// window.go, written by hand around one fact, with a README.md that works out every expected value
// by hand from the window's files and an expect.json that holds those values. The directory's own
// README.md gives the conventions every window is worked by.
const fixtureWindowsDir = "testdata/windows"

// fixtureExpectFile is the file of a fixture window that holds its expected values.
const fixtureExpectFile = "expect.json"

// fixtureTolerance is how far a measure of a replay may stand from its hand-worked value and still
// equal it: the hand works in minutes, and a measure in hours or a share of a span carries the
// float's own rounding.
const fixtureTolerance = 1e-6

// k1Facts is every fact check k1 of the tool's verification names, each the name of the fixture
// window that holds it: a phase that goes on while its issue waits on a link, with only its next
// start held; a design gate that holds a child's first phase; a link added and taken off inside
// the window; a child moved out from under its parent; a child filed after its parent starts,
// which moves with that start; a conflict drawn from the window's fitted rate; a review that asks
// for changes and a failed test, each kept as recorded; an issue that reopens and works again; a
// send-back from accepting at a full cap; two waiters on one blocker; a priority inversion of a
// holder below a waiting issue; an issue that never starts, under each named scenario; a window
// whose every time is far from the wall clock's; a segment that starts with 42 more issues in
// flight than the record had; a wait on another issue that lasts one minute, under the daemon's
// rule and under a rule whose wait keeps the slot for a grace; and a tree root that holds its slot
// with no turn while its children run, the shapes two deployments reported.
var k1Facts = []string{
	"phase-goes-on-during-link-wait",
	"gate-holds-child-first-phase",
	"link-added-and-removed",
	"child-moved-out",
	"child-filed-after-parent-start",
	"conflict-from-fitted-rate",
	"review-and-test-kept",
	"reopen-works-again",
	"send-back-at-full-cap",
	"two-waiters-one-blocker",
	"priority-inversion",
	"never-starts",
	"clock-far-from-now",
	"forty-two-more-in-flight",
	"one-minute-wait",
	"root-idle-while-children-run",
}

// k1GitFacts is each fact check k1 names of the branch that asks git and of the conflict generator
// (spec R2), each the name of the fixture window that holds it: on the branch that asks git, a read
// that the store answers with a conflict and a read that the store answers clean
// (git-store-verdicts), and one read of each kind that git cannot answer, which the generator
// answers (git-unanswered-reads); a merge into a base whose committer's time and GitHub merge event
// differ (merge-event-time); on the branch that draws every conflict, a stretch that goes on past
// a skipped conflict loop (drawn-skipped-loop), a stretch under way at a daemon boot in an earlier
// phase than the one it ends in (drawn-boot-earlier-phase), and a stretch whose base takes a merge
// that changes a path of its profile beside one whose base takes only merges that change none
// (drawn-touching-merges); and work that the record never ran, drawn from a recorded issue that
// holds a conflict, on the main branch and on the branch that draws (drawn-work-conflict). They
// stand apart from k1Facts, which is also the older corpus the record, seed and trace cross-checks
// replay (fixtureWindowNames): these windows hold git inputs and run on other branches than main.
var k1GitFacts = []string{
	"git-store-verdicts",
	"git-unanswered-reads",
	"merge-event-time",
	"drawn-skipped-loop",
	"drawn-boot-earlier-phase",
	"drawn-touching-merges",
	"drawn-work-conflict",
}

// fixtureExpect is a fixture window's expect.json: the Scorecard a replay must give under each
// rule it names, on the branch it names (the main branch where it names none); the text the
// refusal of each run it names refused must hold; for the window of an issue that never starts,
// the same for each never-started scenario in place of the rules; for a window whose fact is how
// two branches differ, the same for each branch it names in place of the rules; and, for a window
// that holds every trigger, each issue's sequence of moves a replay under the window's recorded
// rule must make.
type fixtureExpect struct {
	Branch    string                          `json:"branch,omitempty"`
	Rules     map[string]Scorecard            `json:"rules,omitempty"`
	Scenarios map[string]map[string]Scorecard `json:"scenarios,omitempty"`
	// Branches is, by the name of a branch (BranchNamed), the Scorecard a replay on that branch must
	// give under each rule: the runs of a window on more than one branch (drawn-work-conflict), each
	// named <branch>/<rule>.
	Branches map[string]map[string]Scorecard `json:"branches,omitempty"`
	Moves    map[string][]fixtureMove        `json:"moves,omitempty"`
	// Checks supplements the scorecard with fields from run.json and structured log lines.
	// Keys are rule names. In a log line only the named attributes have to match. A rule's run.json
	// checks are its run's inputs too: the run of each window under that rule takes the limits
	// its limits give (RunConfig.Limits), and the moment of each value its signalValues give as its
	// probes (RunConfig.Probes), and run.json echoes both, so the check holds the echo as well as
	// the values (fixtureChecks.inputs).
	Checks map[string]fixtureChecks `json:"checks,omitempty"`
	// Refused is, by rule, the text that Run's refusal of a replay of the window under that rule
	// must hold, on the window's branch: a run R10 makes no comparison on, such as one whose rule
	// starts a P0 issue outside the cap on a window that lacks the history of an ancestor the
	// priority of an issue rests on (p0-inherited-ancestor-missing). A rule it names has no
	// Scorecard in Rules and no Checks.
	Refused map[string]string `json:"refused,omitempty"`
}

type fixtureChecks struct {
	Run map[string]json.RawMessage `json:"run,omitempty"`
	Log []map[string]any           `json:"log,omitempty"`
}

// inputs is what c gives its run as inputs (fixtureExpect.Checks): the limits its run.json limits
// name, nil for none, and the moment of each value its signalValues name, each an RFC 3339 time.
func (c fixtureChecks) inputs() (map[string]float64, []time.Time, error) {
	var limits map[string]float64
	if raw, given := c.Run["limits"]; given {
		if err := json.Unmarshal(raw, &limits); err != nil {
			return nil, nil, fmt.Errorf("the limits of run.json's checks: %w", err)
		}
	}
	var values map[string]json.RawMessage
	if raw, given := c.Run["signalValues"]; given {
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, nil, fmt.Errorf("the signalValues of run.json's checks: %w", err)
		}
	}
	var probes []time.Time
	for _, key := range slices.Sorted(maps.Keys(values)) {
		at, err := time.Parse(time.RFC3339, key)
		if err != nil {
			return nil, nil, fmt.Errorf("the signalValues of run.json's checks name %q, no RFC 3339 time", key)
		}
		probes = append(probes, at)
	}
	return limits, probes, nil
}

// fixtureMove is one recorded move of an issue's phase, as a `workflow: phase changed` line names
// it.
type fixtureMove struct {
	From    phase.Phase          `json:"from"`
	To      phase.Phase          `json:"to"`
	Trigger workflow.TriggerKind `json:"trigger"`
}

// fixtureRun is one replay a fixture window asks for: a rule, a branch, and the Scorecard the
// replay must give, or, for a run expect.json names refused, the text Run's refusal must hold
// (refusal) in its place. The run's seed and slowdown factor are the Scorecard's own, so a window
// whose fact rests on a factor (forty-two-more-in-flight) is replayed at that factor.
type fixtureRun struct {
	name    string
	rule    string
	branch  Branch
	want    Scorecard
	refusal string
}

// refusingNoMove is, by fixture window, the rule under which TestFixtureWindows also fails for each
// move the replay of that window refused: under legion-v20.42.5, whose conflicting head moves back,
// conflict-from-fitted-rate's drawn conflict moves ACME-1 back to implementing by the rule's
// trigger through the daemon's own path, and its loop's work is drawn from the window's recorded
// loops (spec R2; the fixture windows' README, "Conflicts").
var refusingNoMove = map[string]string{"conflict-from-fitted-rate": "legion-v20.42.5"}

// Every fact of a window that a slot rule does not decide is replayed as the window records it, and
// each measure of R4 is read the way the windows' README.md defines it (spec R1, R2, R4; check k1).
// The test reads the directory of fixture windows, so a window added there is run with no change
// here, and runs every window — not a sample — under every rule its expect.json names (every
// never-started scenario, for never-starts, and every branch, for a window that names branches),
// through Run on a database of its own on
// LEGION_TEST_PG_DSN's server. It fails naming the window, the rule and each measure whose value
// differs from the one worked out by hand, floats compared to within fixtureTolerance; for a run
// expect.json names refused, a replay Run makes rather than refuses, or refuses with an error that
// does not hold the text expect.json gives; for a
// window whose expect.json holds moves, each issue whose sequence of (from, to, trigger) in the
// replay's own log differs from the recorded one under the window's recorded rule, which a move
// the replay refused breaks; for a run refusingNoMove names, each move its replay refused; and, for
// a run under main, each measure in which its two timelines part by more than
// TestSnapshotAndLineTimelinesAgree allows (checkTimelines). Each window is read with ReadWindow
// first, so a window whose files do not match the sha256 its window.json records fails before it
// is replayed.
func TestFixtureWindows(t *testing.T) {
	t.Parallel()
	dsn := replayDSN(t)
	names := allFixtureWindowNames(t)
	if len(names) == 0 {
		t.Fatalf("%s holds no fixture window, so nothing was replayed", fixtureWindowsDir)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			skipUntilBuilt(t)
			window, expect := readFixture(t, name)
			runs, err := expect.runs()
			if err != nil {
				t.Fatalf("window %s: %v", name, err)
			}
			recorded := partsRule(window.Parts)
			for _, run := range runs {
				t.Run(run.name, func(t *testing.T) {
					t.Parallel()
					skipUntilBuilt(t)
					out := t.TempDir()
					limits, probes, err := expect.Checks[run.rule].inputs()
					if err != nil {
						t.Fatalf("window %s, rule %s: %v", name, run.rule, err)
					}
					result, err := Run(context.Background(), RunConfig{Window: window, Rule: replayedRule(t, run.rule), Branch: run.branch,
						Seed: cmp.Or(run.want.Seed, 1), Slowdown: cmp.Or(run.want.Slowdown, 1), PGDSN: dsn, Out: out, Limits: limits, Probes: probes})
					if run.refusal != "" {
						switch {
						case err == nil:
							t.Errorf("window %s, rule %s, branch %s: the replay ran, want a refusal naming %q", name, run.rule, run.branch.Name, run.refusal)
						case !strings.Contains(err.Error(), run.refusal):
							t.Errorf("window %s, rule %s, branch %s: the replay was refused with %q, want a refusal naming %q", name, run.rule,
								run.branch.Name, err, run.refusal)
						}
						return
					}
					if err != nil {
						t.Fatalf("window %s, rule %s, branch %s: the replay failed: %v", name, run.rule, run.branch.Name, err)
					}
					for _, diff := range scorecardDiffs(result.Scorecard, run.want) {
						t.Errorf("window %s, rule %s, branch %s: %s", name, run.rule, run.branch.Name, diff)
					}
					checkFixtureOutput(t, out, expect.Checks[run.rule])
					if refused := result.Divergence.Refused; refusingNoMove[name] == run.rule && len(refused) > 0 {
						t.Errorf("window %s, rule %s, branch %s: the replay refused %+v", name, run.rule, run.branch.Name, refused)
					}
					if run.rule == record.MainRule.Name {
						checkTimelines(t, result)
					}
					if len(expect.Moves) == 0 || run.rule != recorded || run.branch.Name != MainBranch.Name {
						return
					}
					moved := replayedMoves(replayLog(t, out))
					for _, issue := range slices.Sorted(maps.Keys(unionKeys(moved, expect.Moves))) {
						if got, want := moved[issue], expect.Moves[issue]; !slices.Equal(got, want) {
							t.Errorf("window %s, rule %s: %s moved %s in the replay, and the record moved it %s",
								name, run.rule, issue, describeMoves(got), describeMoves(want))
						}
					}
				})
			}
		})
	}
}

func checkFixtureOutput(t *testing.T, out string, want fixtureChecks) {
	t.Helper()
	if len(want.Run) != 0 {
		var got map[string]any
		if err := readJSON(filepath.Join(out, runFile), &got); err != nil {
			t.Fatal(err)
		}
		for _, name := range slices.Sorted(maps.Keys(want.Run)) {
			var value any
			if err := json.Unmarshal(want.Run[name], &value); err != nil {
				t.Fatalf("invalid expectation for run.json %s: %v", name, err)
			}
			if measured, ok := got[name]; !ok {
				t.Errorf("run.json has no %q field", name)
			} else if !reflect.DeepEqual(measured, value) {
				t.Errorf("run.json %s = %v, want %v", name, measured, value)
			}
		}
	}
	if len(want.Log) == 0 {
		return
	}
	raw, err := os.ReadFile(filepath.Join(out, logFile))
	if err != nil {
		t.Fatal(err)
	}
	var lines []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
		var fields map[string]any
		if err := json.Unmarshal(line, &fields); err != nil {
			t.Fatalf("read replay log: %v", err)
		}
		lines = append(lines, fields)
	}
	for _, fields := range want.Log {
		if !slices.ContainsFunc(lines, func(line map[string]any) bool {
			for name, value := range fields {
				if got, ok := line[name]; !ok || !reflect.DeepEqual(got, value) {
					return false
				}
			}
			return len(fields) != 0
		}) {
			t.Errorf("replay log has no line with fields %v", fields)
		}
	}
}

// Check k1 names its facts, and each needs a window that holds it (the plan's t2g), so the list
// of facts is held here as data (k1Facts, and k1GitFacts for the branch that asks git and the
// conflict generator) and the test fails for a fact with no fixture window: no
// directory, or one missing its window.json, README.md or expect.json, one ReadWindow refuses (an
// input whose sha256 is not the one window.json records among the reasons), or one whose
// expect.json names no replay to run, or a branch BranchNamed refuses.
func TestTheFixtureWindowsCoverK1(t *testing.T) {
	t.Parallel()
	for _, fact := range slices.Concat(k1Facts, k1GitFacts) {
		dir := filepath.Join(fixtureWindowsDir, fact)
		missing := false
		for _, file := range []string{windowFile, "README.md", fixtureExpectFile} {
			if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
				t.Errorf("k1's fact %s has no fixture window: %v", fact, err)
				missing = true
			}
		}
		if missing {
			continue
		}
		if _, err := ReadWindow(dir); err != nil {
			t.Errorf("k1's fact %s: %v", fact, err)
		}
		expect, err := readFixtureExpect(dir)
		if err != nil {
			t.Errorf("k1's fact %s: %v", fact, err)
			continue
		}
		if _, err := expect.runs(); err != nil {
			t.Errorf("k1's fact %s: %v", fact, err)
		}
	}
}

// Each fixture window names each of R11's signals at a moment, once or more (check k1, the plan's
// t33): under a rule its expect.json names, its run.json checks give a signalValues whose moments
// name the signal, so that the replay's own reading of each signal at a moment, worked out by
// hand in the window's README, is checked on each window. A window with no record of a signal
// names it unrecorded (null). It fails naming each window and each signal that no moment names.
func TestEveryWindowHasEachSignalAtAMoment(t *testing.T) {
	t.Parallel()
	for _, name := range allFixtureWindowNames(t) {
		expect, err := readFixtureExpect(filepath.Join(fixtureWindowsDir, name))
		if err != nil {
			t.Errorf("window %s: %v", name, err)
			continue
		}
		named := map[string]bool{}
		for _, rule := range slices.Sorted(maps.Keys(expect.Checks)) {
			raw, given := expect.Checks[rule].Run["signalValues"]
			if !given {
				continue
			}
			var values map[string]map[string]*float64
			if err := json.Unmarshal(raw, &values); err != nil {
				t.Errorf("window %s, rule %s: the signalValues of run.json's checks: %v", name, rule, err)
				continue
			}
			for _, moment := range values {
				for signal := range moment {
					named[signal] = true
				}
			}
		}
		for _, signal := range signals.Names() {
			if !named[signal] {
				t.Errorf("window %s names the signal %s at no moment (checks.<rule>.run.signalValues)", name, signal)
			}
		}
	}
}

// readFixture reads the fixture window name and its expect.json, and fails the test where it reads
// either not.
func readFixture(t *testing.T, name string) (Window, fixtureExpect) {
	t.Helper()
	dir := filepath.Join(fixtureWindowsDir, name)
	window, err := ReadWindow(dir)
	if err != nil {
		t.Fatalf("window %s: %v", name, err)
	}
	expect, err := readFixtureExpect(dir)
	if err != nil {
		t.Fatalf("window %s: %v", name, err)
	}
	return window, expect
}

// fixtureWindowNames is the original format-1 corpus for the record, seed and trace
// cross-checks. TestFixtureWindows discovers every directory, including windows that
// deliberately require a reader or rule that does not exist yet.
func fixtureWindowNames(t *testing.T) []string {
	t.Helper()
	names := slices.Concat(k1Facts, []string{"every-trigger"})
	slices.Sort(names)
	return names
}

// allFixtureWindowNames is every directory in fixtureWindowsDir, in order.
func allFixtureWindowNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(fixtureWindowsDir)
	if err != nil {
		t.Fatalf("list the fixture windows: %v", err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names
}

// readFixtureExpect reads the expect.json of the fixture window in dir, refusing a field the
// format does not name, so a misspelt measure fails rather than reading as a zero.
func readFixtureExpect(dir string) (fixtureExpect, error) {
	raw, err := os.ReadFile(filepath.Join(dir, fixtureExpectFile))
	if err != nil {
		return fixtureExpect{}, err
	}
	var expect fixtureExpect
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&expect); err != nil {
		return fixtureExpect{}, fmt.Errorf("read %s: %w", filepath.Join(dir, fixtureExpectFile), err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fixtureExpect{}, fmt.Errorf("read %s: expected end of file after the fixture expectations", filepath.Join(dir, fixtureExpectFile))
	}
	return expect, nil
}

// runs is every replay e asks for, in order: each rule of Rules on e's branch, each rule of
// Refused on e's branch, each rule of each never-started scenario of Scenarios on the main
// branch with that scenario, and each rule of each branch of Branches on that branch. It refuses a
// rule Refused names with no text, or with a Scorecard in Rules or Checks beside it, since a
// refused run gives neither, and a branch BranchNamed refuses.
func (e fixtureExpect) runs() ([]fixtureRun, error) {
	var runs []fixtureRun
	branch, err := BranchNamed(cmp.Or(e.Branch, MainBranch.Name))
	if err != nil {
		return nil, err
	}
	for _, rule := range slices.Sorted(maps.Keys(e.Rules)) {
		runs = append(runs, fixtureRun{name: rule, rule: rule, branch: branch, want: e.Rules[rule]})
	}
	for _, rule := range slices.Sorted(maps.Keys(e.Refused)) {
		_, scored := e.Rules[rule]
		_, checked := e.Checks[rule]
		if e.Refused[rule] == "" || scored || checked {
			return nil, fmt.Errorf("%s names %q refused with the text %q, beside a Scorecard (%t) or output checks (%t)", fixtureExpectFile,
				rule, e.Refused[rule], scored, checked)
		}
		runs = append(runs, fixtureRun{name: rule, rule: rule, branch: branch, refusal: e.Refused[rule]})
	}
	for _, scenario := range slices.Sorted(maps.Keys(e.Scenarios)) {
		name := MainBranch.Name
		if scenario != MainBranch.NeverStarted {
			name = MainBranch.Name + "+neverStarted=" + scenario
		}
		branch, err := BranchNamed(name)
		if err != nil {
			return nil, err
		}
		for _, rule := range slices.Sorted(maps.Keys(e.Scenarios[scenario])) {
			runs = append(runs, fixtureRun{name: scenario + "/" + rule, rule: rule, branch: branch, want: e.Scenarios[scenario][rule]})
		}
	}
	for _, name := range slices.Sorted(maps.Keys(e.Branches)) {
		branch, err := BranchNamed(name)
		if err != nil {
			return nil, err
		}
		for _, rule := range slices.Sorted(maps.Keys(e.Branches[name])) {
			runs = append(runs, fixtureRun{name: name + "/" + rule, rule: rule, branch: branch, want: e.Branches[name][rule]})
		}
	}
	if len(runs) == 0 {
		return nil, fmt.Errorf("%s names no rule to replay the window under", fixtureExpectFile)
	}
	for _, rule := range slices.Sorted(maps.Keys(e.Checks)) {
		if !slices.ContainsFunc(runs, func(run fixtureRun) bool { return run.rule == rule }) {
			return nil, fmt.Errorf("%s has output checks for %q but no replay under that rule", fixtureExpectFile, rule)
		}
	}
	return runs, nil
}

// describeMoves is a sequence of moves as a failure prints it.
func describeMoves(moves []fixtureMove) string {
	parts := make([]string, len(moves))
	for i, move := range moves {
		parts[i] = fmt.Sprintf("%s→%s by %s", move.From, move.To, move.Trigger)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// scorecardDiffs is every measure in which got differs from want, each named by its JSON path in the
// Scorecard (inFlight.hours, loops[send_back].rounds, firstSlot[ACME-3]): a float that stands
// further than fixtureTolerance from its value, a time at another instant, an entry of a map that
// one of the two lacks, the assumptions as a set, and any other value that is not equal. It walks
// the Scorecard's own fields, so a measure the type gains is compared with no change here.
func scorecardDiffs(got, want Scorecard) []string {
	var diffs []string
	compareMeasure(&diffs, "", reflect.ValueOf(got), reflect.ValueOf(want))
	return diffs
}

// compareMeasure adds to diffs each way the value got at path differs from want.
func compareMeasure(diffs *[]string, path string, got, want reflect.Value) {
	differs := func(format string, args ...any) {
		*diffs = append(*diffs, path+": "+fmt.Sprintf(format, args...))
	}
	switch got.Kind() {
	case reflect.Struct:
		if got.Type() == reflect.TypeFor[time.Time]() {
			if g, w := got.Interface().(time.Time), want.Interface().(time.Time); !g.Equal(w) {
				differs("got %s, want %s", g.UTC().Format(time.RFC3339Nano), w.UTC().Format(time.RFC3339Nano))
			}
			return
		}
		for i := range got.NumField() {
			name, _, _ := strings.Cut(got.Type().Field(i).Tag.Get("json"), ",")
			if path != "" {
				name = path + "." + name
			}
			compareMeasure(diffs, name, got.Field(i), want.Field(i))
		}
	case reflect.Pointer:
		switch {
		case got.IsNil() && want.IsNil():
		case got.IsNil():
			differs("got none, want %+v", want.Elem().Interface())
		case want.IsNil():
			differs("got %+v, want none", got.Elem().Interface())
		default:
			compareMeasure(diffs, path, got.Elem(), want.Elem())
		}
	case reflect.Map:
		keys := make(map[string]reflect.Value)
		for _, key := range append(got.MapKeys(), want.MapKeys()...) {
			keys[key.String()] = key
		}
		for _, name := range slices.Sorted(maps.Keys(keys)) {
			g, w := got.MapIndex(keys[name]), want.MapIndex(keys[name])
			entry := path + "[" + name + "]"
			switch {
			case !g.IsValid():
				*diffs = append(*diffs, fmt.Sprintf("%s: got none, want %+v", entry, w.Interface()))
			case !w.IsValid():
				*diffs = append(*diffs, fmt.Sprintf("%s: got %+v, want none", entry, g.Interface()))
			default:
				compareMeasure(diffs, entry, g, w)
			}
		}
	case reflect.Slice:
		if got.Type().Elem() == reflect.TypeFor[time.Time]() {
			g, w := got.Interface().([]time.Time), want.Interface().([]time.Time)
			if !slices.EqualFunc(g, w, time.Time.Equal) {
				differs("got %v, want %v", g, w)
			}
			return
		}
		g, w := sortedCopy(got), sortedCopy(want)
		if !reflect.DeepEqual(g, w) {
			differs("got %v, want %v", g, w)
		}
	case reflect.Float64:
		if g, w := got.Float(), want.Float(); math.Abs(g-w) > fixtureTolerance {
			differs("got %v, want %v", g, w)
		}
	default:
		if g, w := got.Interface(), want.Interface(); !reflect.DeepEqual(g, w) {
			differs("got %v, want %v", g, w)
		}
	}
}

// sortedCopy is a slice of numbers as a set reads it: a sorted copy, empty for none. The one slice
// of numbers a Scorecard holds is its assumptions, whose order says nothing.
func sortedCopy(v reflect.Value) []int64 {
	numbers := make([]int64, 0, v.Len())
	for i := range v.Len() {
		numbers = append(numbers, v.Index(i).Int())
	}
	slices.Sort(numbers)
	return numbers
}
