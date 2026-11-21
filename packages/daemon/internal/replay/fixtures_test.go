package replay

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
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
// whose every time is far from the wall clock's; and a segment that starts with 42 more issues in
// flight than the record had.
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
}

// fixtureExpect is a fixture window's expect.json: the Scorecard a replay must give under each
// rule it names, on the branch it names (the main branch where it names none); for the window of
// an issue that never starts, the same for each never-started scenario in place of the rules; and,
// for a window that holds every trigger, each issue's sequence of moves a replay under the
// window's recorded rule must make.
type fixtureExpect struct {
	Branch    string                          `json:"branch,omitempty"`
	Rules     map[string]Scorecard            `json:"rules,omitempty"`
	Scenarios map[string]map[string]Scorecard `json:"scenarios,omitempty"`
	Moves     map[string][]fixtureMove        `json:"moves,omitempty"`
}

// fixtureMove is one recorded move of an issue's phase, as a `workflow: phase changed` line names
// it.
type fixtureMove struct {
	From    phase.Phase          `json:"from"`
	To      phase.Phase          `json:"to"`
	Trigger workflow.TriggerKind `json:"trigger"`
}

// fixtureRun is one replay a fixture window asks for: a rule, a branch, and the Scorecard the
// replay must give. The run's seed and slowdown factor are the Scorecard's own, so a window whose
// fact rests on a factor (forty-two-more-in-flight) is replayed at that factor.
type fixtureRun struct {
	name   string
	rule   string
	branch Branch
	want   Scorecard
}

// Every fact of a window that a slot rule does not decide is replayed as the window records it, and
// each measure of R4 is read the way the windows' README.md defines it (spec R1, R2, R4; check k1).
// The test reads the directory of fixture windows, so a window added there is run with no change
// here, and runs every window — not a sample — under every rule its expect.json names (every
// never-started scenario, for never-starts), through Run on a database of its own on
// LEGION_TEST_PG_DSN's server. It fails naming the window, the rule and each measure whose value
// differs from the one worked out by hand, floats compared to within fixtureTolerance; and, for a
// window whose expect.json holds moves, each issue whose sequence of (from, to, trigger) in the
// replay's own log differs from the recorded one under the window's recorded rule, which a move
// the replay refused breaks. Each window is read with ReadWindow first, so a window whose files do
// not match the sha256 its window.json records fails before it is replayed.
func TestFixtureWindows(t *testing.T) {
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		t.Skip(testdb.SkipMessage)
	}
	names := fixtureWindowNames(t)
	if len(names) == 0 {
		t.Fatalf("%s holds no fixture window, so nothing was replayed", fixtureWindowsDir)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(fixtureWindowsDir, name)
			window, err := ReadWindow(dir)
			if err != nil {
				t.Fatalf("window %s: %v", name, err)
			}
			expect, err := readFixtureExpect(dir)
			if err != nil {
				t.Fatalf("window %s: %v", name, err)
			}
			runs, err := expect.runs()
			if err != nil {
				t.Fatalf("window %s: %v", name, err)
			}
			recorded := recordedRule(window)
			for _, run := range runs {
				t.Run(run.name, func(t *testing.T) {
					rule, ok := record.SlotRuleNamed(run.rule)
					if !ok {
						t.Fatalf("window %s, rule %s: the repository keeps no slot rule of that name (record.SlotRules)", name, run.rule)
					}
					slowdown := run.want.Slowdown
					if slowdown == 0 {
						slowdown = 1
					}
					out := t.TempDir()
					result, err := Run(context.Background(), RunConfig{Window: window, Rule: rule, Branch: run.branch,
						Seed: run.want.Seed, Slowdown: slowdown, PGDSN: dsn, Out: out})
					if err != nil {
						t.Fatalf("window %s, rule %s, branch %s: the replay failed: %v", name, run.rule, run.branch.Name, err)
					}
					for _, diff := range scorecardDiffs(result.Scorecard, run.want) {
						t.Errorf("window %s, rule %s, branch %s: %s", name, run.rule, run.branch.Name, diff)
					}
					if len(expect.Moves) == 0 || run.rule != recorded || run.branch.Name != MainBranch.Name {
						return
					}
					moved, err := replayedMoves(filepath.Join(out, logFile))
					if err != nil {
						t.Fatalf("window %s, rule %s: %v", name, run.rule, err)
					}
					for _, issue := range slices.Sorted(maps.Keys(union(moved, expect.Moves))) {
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

// Check k1 names its facts, and each needs a window that holds it (the plan's t2g), so the list
// of facts is held here as data and the test fails for a fact with no fixture window: no
// directory, or one missing its window.json, README.md or expect.json, one ReadWindow refuses (an
// input whose sha256 is not the one window.json records among the reasons), or one whose
// expect.json names no replay to run.
func TestTheFixtureWindowsCoverK1(t *testing.T) {
	for _, fact := range k1Facts {
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

// fixtureWindowNames is the name of every directory in fixtureWindowsDir, in order.
func fixtureWindowNames(t *testing.T) []string {
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
	return expect, nil
}

// runs is every replay e asks for, in order: each rule of Rules on e's branch, and each rule of
// each never-started scenario of Scenarios on the main branch with that scenario.
func (e fixtureExpect) runs() ([]fixtureRun, error) {
	var runs []fixtureRun
	branch, err := branchNamed(cmp.Or(e.Branch, MainBranch.Name))
	if err != nil {
		return nil, err
	}
	for _, rule := range slices.Sorted(maps.Keys(e.Rules)) {
		runs = append(runs, fixtureRun{name: rule, rule: rule, branch: branch, want: e.Rules[rule]})
	}
	for _, scenario := range slices.Sorted(maps.Keys(e.Scenarios)) {
		name := MainBranch.Name
		if scenario != MainBranch.NeverStarted {
			name = MainBranch.Name + "+neverStarted=" + scenario
		}
		branch, err := branchNamed(name)
		if err != nil {
			return nil, err
		}
		for _, rule := range slices.Sorted(maps.Keys(e.Scenarios[scenario])) {
			runs = append(runs, fixtureRun{name: scenario + "/" + rule, rule: rule, branch: branch, want: e.Scenarios[scenario][rule]})
		}
	}
	if len(runs) == 0 {
		return nil, fmt.Errorf("%s names no rule to replay the window under", fixtureExpectFile)
	}
	return runs, nil
}

// branchNamed is the branch a name says: "main", the main branch, or "main+<knob>=<value>", the
// main branch with the one knob whose JSON name is <knob> set to <value>.
func branchNamed(name string) (Branch, error) {
	branch := MainBranch
	branch.Name = name
	if name == MainBranch.Name {
		return branch, nil
	}
	change, ok := strings.CutPrefix(name, MainBranch.Name+"+")
	knob, value, set := strings.Cut(change, "=")
	if !ok || !set || value == "" {
		return Branch{}, fmt.Errorf("the branch %q is neither main nor main+<knob>=<value>", name)
	}
	fields := reflect.ValueOf(&branch).Elem()
	for i := range fields.NumField() {
		field := fields.Type().Field(i)
		if tag, _, _ := strings.Cut(field.Tag.Get("json"), ","); tag == knob && knob != "name" && field.Type.Kind() == reflect.String {
			fields.Field(i).SetString(value)
			return branch, nil
		}
	}
	return Branch{}, fmt.Errorf("the branch %q names no knob of a replay.Branch", name)
}

// recordedRule is the rule the daemon that recorded w ran: its parts' rule, or "" where its parts
// name more than one.
func recordedRule(w Window) string {
	rule := ""
	for i, part := range w.Parts {
		if i > 0 && part.Rule != rule {
			return ""
		}
		rule = part.Rule
	}
	return rule
}

// replayedMoves is each issue's sequence of moves in the replay log at path, in the order the log
// holds its `workflow: phase changed` lines.
func replayedMoves(path string) (map[string][]fixtureMove, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the replay's log: %w", err)
	}
	moves := make(map[string][]fixtureMove)
	for i, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var moved struct {
			Msg   string `json:"msg"`
			Issue string `json:"issue"`
			fixtureMove
		}
		if err := json.Unmarshal([]byte(line), &moved); err != nil {
			return nil, fmt.Errorf("read the replay's log line %d: %w", i+1, err)
		}
		if moved.Msg == "workflow: phase changed" {
			moves[moved.Issue] = append(moves[moved.Issue], moved.fixtureMove)
		}
	}
	return moves, nil
}

// describeMoves is a sequence of moves as a failure prints it.
func describeMoves(moves []fixtureMove) string {
	parts := make([]string, len(moves))
	for i, move := range moves {
		parts[i] = fmt.Sprintf("%s→%s by %s", move.From, move.To, move.Trigger)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// union is the keys of a and b together, each once.
func union[V any](a, b map[string]V) map[string]struct{} {
	keys := make(map[string]struct{}, len(a)+len(b))
	for key := range a {
		keys[key] = struct{}{}
	}
	for key := range b {
		keys[key] = struct{}{}
	}
	return keys
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
// a Scorecard holds is its assumptions, whose order says nothing.
func sortedCopy(v reflect.Value) []int64 {
	numbers := make([]int64, 0, v.Len())
	for i := range v.Len() {
		numbers = append(numbers, v.Index(i).Int())
	}
	slices.Sort(numbers)
	return numbers
}
