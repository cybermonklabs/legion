package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
)

// deliveryOracleTolerance is how far apart, relative to the larger, two floats may be and still
// be the same answer: the folds sum and sort the same values the old code did, so only a change
// of summation order could move a float, and that moves it in its last bits.
const deliveryOracleTolerance = 1e-9

// deliveryOracleFixture is testdata/golden_delivery/fixture.json as this test reads it: the
// history the answers were recorded over, the windows recorded, and the top-level fields the
// recorder left out of every answer.
type deliveryOracleFixture struct {
	Head    string `json:"head"`
	Fixture struct {
		Spec       deliverytest.Spec `json:"spec"`
		Repos      []string          `json:"repos"`
		DeployRepo string            `json:"deploy_repo"`
		Summary    string            `json:"summary"`
	} `json:"fixture"`
	Windows []struct {
		From time.Time `json:"from"`
		To   time.Time `json:"to"`
		Days int       `json:"days"`
		File string    `json:"file"`
	} `json:"windows"`
	Stripped []string `json:"stripped"`
}

// TestTheDeliveryRoutesAnswerWhatTheyAnsweredAtTheBaseHead is the oracle of the day parts and
// folds that measures.Compute and the pipeline functions answer through (T7 of
// dispatch://INFRA-2916's plan): over the deliverytest history of
// testdata/golden_delivery/fixture.json, every request each window file holds (GET
// /api/v1/delivery/measures with no facet and with repo= each repository, and GET
// /api/v1/delivery/pipeline) answers, value for value, what the base head answered when
// golden_delivery_record_test.go recorded it, the recorder's stripped fields left out. Integers
// and strings must be equal; floats within deliveryOracleTolerance of each other, relative to the
// larger.
//
// The two named exceptions are rules INFRA-2916 changed. A deploy's with_prs: a deploy is one with
// pull requests when it ships any pull request, whenever that pull request merged, where the base
// head counted only those merged inside the window. So each measures answer's deploys_with_prs
// must be the base head's plus exactly the deploys of the window whose every shipped pull request
// merged before it, counted from the fixture's own containment (deploysShippingOnlyEarlier), and
// with_prs_per_day the base head's plus that count over the window's days. And the change failure
// rate while no failure source exists (root R11: 0 is a value, never a stand-in): the base head
// answered an object of zero flag counts and rates over the window's totals, and verdicts over
// them; this head answers change_failure_rate null and both its verdicts null
// (takeFailureMeasures). Those fields are then left out of the value-for-value comparison, and
// every other field must be the base head's.
func TestTheDeliveryRoutesAnswerWhatTheyAnsweredAtTheBaseHead(t *testing.T) {
	dir := filepath.Join("testdata", "golden_delivery")
	var recorded deliveryOracleFixture
	readDeliveryOracleFile(t, filepath.Join(dir, "fixture.json"), &recorded)
	if len(recorded.Windows) == 0 || len(recorded.Stripped) == 0 {
		t.Fatalf("fixture.json holds %d windows and %d stripped fields, want both", len(recorded.Windows), len(recorded.Stripped))
	}

	handler, database := newTestHandlerWithStore(t)
	fixture := deliverytest.Seed(t, database.Pool, recorded.Fixture.Spec)
	// A history other than the recorded one would show as every measure moving; name it instead.
	if fixture.Summary() != recorded.Fixture.Summary || !slices.Equal(fixture.Repos, recorded.Fixture.Repos) {
		t.Fatalf("the seeded history is not the recorded one:\n seeded:   %s, repositories %v\n recorded: %s, repositories %v",
			fixture.Summary(), fixture.Repos, recorded.Fixture.Summary, recorded.Fixture.Repos)
	}

	requests, values, differing, moved, movedWindows, failuresNulled := 0, 0, 0, 0, 0, 0
	for _, window := range recorded.Windows {
		var file struct {
			Head    string                     `json:"head"`
			Answers map[string]json.RawMessage `json:"answers"`
		}
		readDeliveryOracleFile(t, filepath.Join(dir, window.File), &file)
		if file.Head != recorded.Head || len(file.Answers) == 0 {
			t.Fatalf("%s: head %q and %d answers, want head %q and the window's answers", window.File, file.Head, len(file.Answers), recorded.Head)
		}
		earlier := deploysShippingOnlyEarlier(fixture, window.From, window.To)
		var withPRsMoved []string
		for _, target := range slices.Sorted(maps.Keys(file.Answers)) {
			response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s: status = %d, body = %s", target, response.Code, response.Body.String())
			}
			got := decodeDeliveryOracleTree(t, response.Body.Bytes())
			for _, field := range recorded.Stripped {
				if _, held := got.(map[string]any)[field]; !held {
					t.Fatalf("GET %s answers no %s to strip", target, field)
				}
				delete(got.(map[string]any), field)
			}
			want := decodeDeliveryOracleTree(t, file.Answers[target])
			if strings.HasPrefix(target, "/api/v1/delivery/measures?") {
				change, difference := takeWithPRs(t, got, want, earlier, window.Days)
				values += 2
				withPRsMoved = append(withPRsMoved, change)
				if difference != "" {
					differing++
					t.Errorf("%s: GET %s: %s", window.File, target, difference)
				}
				named, difference := takeFailureMeasures(t, got, want)
				values += named
				failuresNulled += named
				if difference != "" {
					differing++
					t.Errorf("%s: GET %s: %s", window.File, target, difference)
				}
			}
			compared, difference := compareDeliveryOracleTrees("", got, want)
			requests, values = requests+1, values+compared
			if difference != "" {
				differing++
				t.Errorf("%s: GET %s differs from the base head's answer first at %s", window.File, target, difference)
			}
		}
		if earlier > 0 {
			moved, movedWindows = moved+earlier, movedWindows+1
		}
		t.Logf("%s..%s (%d days): %d deploys ship only pull requests merged before the window; the measures answers' with_prs moved by %s",
			window.From.Format(time.DateOnly), window.To.Format(time.DateOnly), window.Days, earlier, strings.Join(slices.Compact(withPRsMoved), ", "))
	}
	t.Logf("history %s (seed %d, %d days to %s), recorded at %s: %d windows, %d requests, %d values compared, %d requests differing; "+
		"deploys_with_prs moved in %d of the %d windows, by %d deploys in all; %d failure-measure values the base head answered are null",
		recorded.Fixture.Spec.Project, recorded.Fixture.Spec.Seed, recorded.Fixture.Spec.Days, recorded.Fixture.Spec.Now.Format(time.DateOnly),
		recorded.Head, len(recorded.Windows), requests, values, differing, movedWindows, len(recorded.Windows), moved, failuresNulled)
	if failuresNulled == 0 {
		t.Error("no measures answer held a failure measure to compare, so the named difference was never read")
	}
}

// deploysShippingOnlyEarlier is how many of the fixture's deploys started in [from, to) ship pull
// requests every one of which merged before from, by the fixture's own containment
// (delivery.ContainingRun over every deploy the fixture judged): the deploys the base head's rule
// counted as shipping none in that window and INFRA-2916's counts as shipping some.
func deploysShippingOnlyEarlier(fixture deliverytest.Fixture, from, to time.Time) int {
	// shipsInWindow holds every deploy that ships a pull request, true where one of them merged in
	// the window or after it.
	shipsInWindow := map[int64]bool{}
	for _, pr := range fixture.PullRequests {
		if apply := delivery.ContainingRun(pr, fixture.DeployRepo, fixture.Applies); apply != nil {
			shipsInWindow[apply.RunID] = shipsInWindow[apply.RunID] || !pr.MergedAt.Before(from)
		}
	}
	deploys := map[int64]bool{}
	for _, apply := range fixture.Applies {
		deploys[apply.RunID] = true
	}
	earlier := 0
	for _, run := range fixture.Runs {
		inWindow, ships := shipsInWindow[run.RunID]
		if run.Kind == delivery.DeliveryRunKindDeploy && deploys[run.RunID] && ships && !inWindow &&
			!run.StartedAt.Before(from) && run.StartedAt.Before(to) {
			earlier++
		}
	}
	return earlier
}

// takeWithPRs checks got's with_prs against want's, a measures answer's and the base head's, and
// takes both fields out of both: deploys_with_prs must be want's plus earlier, exactly, and
// with_prs_per_day want's plus earlier over days, within deliveryOracleTolerance. It answers the
// change it measured (`+<n> deploys, +<per day>/day`) and, where either field is off, what is.
func takeWithPRs(t *testing.T, got, want any, earlier, days int) (string, string) {
	t.Helper()
	frequency := func(tree any) map[string]any {
		return tree.(map[string]any)["measures"].(map[string]any)["deploy_frequency"].(map[string]any)
	}
	gotFrequency, wantFrequency := frequency(got), frequency(want)
	number := func(fields map[string]any, name string) json.Number {
		value, ok := fields[name].(json.Number)
		if !ok {
			t.Fatalf("deploy_frequency.%s = %v, want a number", name, fields[name])
		}
		delete(fields, name)
		return value
	}
	gotCount, err := number(gotFrequency, "deploys_with_prs").Int64()
	if err != nil {
		t.Fatalf("deploys_with_prs: %v", err)
	}
	wantCount, err := number(wantFrequency, "deploys_with_prs").Int64()
	if err != nil {
		t.Fatalf("the base head's deploys_with_prs: %v", err)
	}
	gotPerDay, err := number(gotFrequency, "with_prs_per_day").Float64()
	if err != nil {
		t.Fatalf("with_prs_per_day: %v", err)
	}
	wantPerDay, err := number(wantFrequency, "with_prs_per_day").Float64()
	if err != nil {
		t.Fatalf("the base head's with_prs_per_day: %v", err)
	}
	change := fmt.Sprintf("%+d deploys, %+.4f/day", gotCount-wantCount, gotPerDay-wantPerDay)
	expectedPerDay := wantPerDay + float64(earlier)/float64(days)
	if gotCount-wantCount != int64(earlier) ||
		math.Abs(gotPerDay-expectedPerDay) > deliveryOracleTolerance*math.Max(math.Abs(gotPerDay), math.Abs(expectedPerDay)) {
		return change, fmt.Sprintf("deploys_with_prs is %d and with_prs_per_day %g, want the base head's %d and %g plus the %d deploys that ship only pull requests merged before the window (%d and %g)",
			gotCount, gotPerDay, wantCount, wantPerDay, earlier, wantCount+int64(earlier), expectedPerDay)
	}
	return change, ""
}

// takeFailureMeasures checks got's change failure rate and its verdicts against want's, a
// measures answer's and the base head's, while no failure source exists, and takes them out of
// both: got's measures.change_failure_rate, status.change_failure_rate and
// status.change_failure_rate_upper_bound must each be null; want's change failure rate must be
// the base head's stand-in, every flag count and rate 0 over a total that is a count, and each of
// its verdicts a boolean or null. It answers how many of the base head's values the check named
// and, where any is off, what is.
func takeFailureMeasures(t *testing.T, got, want any) (int, string) {
	t.Helper()
	field := func(tree any, path ...string) (map[string]any, string) {
		parent := tree.(map[string]any)
		for _, key := range path[:len(path)-1] {
			parent = parent[key].(map[string]any)
		}
		return parent, path[len(path)-1]
	}
	named := 0
	for _, path := range [][]string{{"measures", "change_failure_rate"}, {"status", "change_failure_rate"}, {"status", "change_failure_rate_upper_bound"}} {
		gotParent, key := field(got, path...)
		wantParent, _ := field(want, path...)
		gotValue, gotHeld := gotParent[key]
		wantValue, wantHeld := wantParent[key]
		if !gotHeld || !wantHeld {
			return named, fmt.Sprintf("%s is held %t by this answer and %t by the base head's, want both", strings.Join(path, "."), gotHeld, wantHeld)
		}
		if gotValue != nil {
			return named, fmt.Sprintf("%s is %v with no failure source, want null", strings.Join(path, "."), gotValue)
		}
		if path[0] == "status" {
			if _, verdict := wantValue.(bool); wantValue != nil && !verdict {
				return named, fmt.Sprintf("the base head's %s is %v, want a verdict or null", strings.Join(path, "."), wantValue)
			}
			named++
		} else {
			rates, ok := wantValue.(map[string]any)
			if !ok {
				return named, fmt.Sprintf("the base head's %s is %v, want its object of flag counts", strings.Join(path, "."), wantValue)
			}
			for _, cut := range []string{"per_pr", "per_deploy"} {
				counts, ok := rates[cut].(map[string]any)
				if !ok {
					return named, fmt.Sprintf("the base head's change_failure_rate.%s is %v, want an object", cut, rates[cut])
				}
				for name, value := range counts {
					number, isNumber := value.(json.Number)
					if !isNumber {
						return named, fmt.Sprintf("the base head's change_failure_rate.%s.%s is %v, want a number", cut, name, value)
					}
					if name != "total" && number.String() != "0" {
						return named, fmt.Sprintf("the base head's change_failure_rate.%s.%s is %s with no flag stored, want its stand-in 0", cut, name, number)
					}
					named++
				}
			}
		}
		delete(gotParent, key)
		delete(wantParent, key)
	}
	return named, ""
}

func readDeliveryOracleFile(t *testing.T, path string, into any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

// decodeDeliveryOracleTree is a JSON text as a tree, each number kept as the text it was
// written as, so an integer is compared as one.
func decodeDeliveryOracleTree(t *testing.T, text []byte) any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(text))
	decoder.UseNumber()
	var tree any
	if err := decoder.Decode(&tree); err != nil {
		t.Fatalf("decode %s: %v", text, err)
	}
	return tree
}

// compareDeliveryOracleTrees walks got and want together, objects by sorted key and arrays by
// index, and answers how many leaf values it compared and, at the first that differs, its path
// from the root ($) with both values ("" when every value is the same). Two numbers written as
// integers must be equal; any other two numbers are floats, within deliveryOracleTolerance
// relative to the larger.
func compareDeliveryOracleTrees(path string, got, want any) (int, string) {
	differs := func(format string, args ...any) string {
		return fmt.Sprintf("$%s: "+format, append([]any{path}, args...)...)
	}
	switch want := want.(type) {
	case map[string]any:
		got, ok := got.(map[string]any)
		if !ok {
			return 0, differs("got %v, want an object", got)
		}
		if gotKeys, wantKeys := slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)); !slices.Equal(gotKeys, wantKeys) {
			return 0, differs("got the keys %v, want %v", gotKeys, wantKeys)
		}
		compared := 0
		for _, key := range slices.Sorted(maps.Keys(want)) {
			count, difference := compareDeliveryOracleTrees(path+"."+key, got[key], want[key])
			compared += count
			if difference != "" {
				return compared, difference
			}
		}
		return compared, ""
	case []any:
		got, ok := got.([]any)
		if !ok {
			return 0, differs("got %v, want an array", got)
		}
		if len(got) != len(want) {
			return 0, differs("got %d entries, want %d", len(got), len(want))
		}
		compared := 0
		for index := range want {
			count, difference := compareDeliveryOracleTrees(path+"["+strconv.Itoa(index)+"]", got[index], want[index])
			compared += count
			if difference != "" {
				return compared, difference
			}
		}
		return compared, ""
	case json.Number:
		got, ok := got.(json.Number)
		if !ok {
			return 1, differs("got %v, want the number %s", got, want)
		}
		gotInteger, gotErr := strconv.ParseInt(got.String(), 10, 64)
		wantInteger, wantErr := strconv.ParseInt(want.String(), 10, 64)
		if gotErr == nil && wantErr == nil {
			if gotInteger != wantInteger {
				return 1, differs("got %d, want %d", gotInteger, wantInteger)
			}
			return 1, ""
		}
		gotFloat, gotErr := got.Float64()
		wantFloat, wantErr := want.Float64()
		if gotErr != nil || wantErr != nil {
			return 1, differs("got %s, want %s, which do not both read as numbers", got, want)
		}
		if math.Abs(gotFloat-wantFloat) > deliveryOracleTolerance*math.Max(math.Abs(gotFloat), math.Abs(wantFloat)) {
			return 1, differs("got %s, want %s", got, want)
		}
		return 1, ""
	default:
		// A string, a boolean or null: equal or not.
		if got != want {
			return 1, differs("got %#v, want %#v", got, want)
		}
		return 1, ""
	}
}
