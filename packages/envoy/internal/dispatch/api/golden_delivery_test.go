package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliveryseed"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
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
		Spec       deliveryseed.Spec `json:"spec"`
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
// /api/v1/delivery/pipeline) answers, value for value, what the base head answered when it was
// recorded, the recorder's stripped fields left out. Integers and strings must be equal; floats
// within deliveryOracleTolerance of each other, relative to the larger.
//
// The Pipeline is compared on the days the two answers share alone. The base head answered
// ?from=&to= with the window's own days; INFRA-1077's cut (0a919c56) made the route read `at`
// alone, the seven UTC days ending on at's, and none of the recorded windows is seven days long.
// The recorder is gone (below), so no answer of seven days can be recorded at the base head
// either. So each recorded Pipeline request is sent as ?at= the window's last instant, whose last
// shown day is the window's, and the two answers are compared on what both state of the days both
// show (sharedPipelineDays): each per-day cell of the PR-checks runs, the three deploy cards and
// the two lead times, each card's job, and the failing jobs, which both read over the 24 hours
// before the window's end. The totals, the slowest jobs and the status span every shown day, which
// differ, and are left out. The test fails where it compared no Pipeline cell.
//
// The files were recorded once, on a checkout of the base head 02a7b3c6 that a recorder test and
// the delivery/deliverytest package it seeds through were copied into (that head holds neither),
// with `go test -count=1 -tags goldenrecord -run TestRecordDeliveryGolden ./internal/dispatch/api/`
// from packages/envoy against a database of its own: it seeded the history fixture.json names
// and drew its 20 windows from a math/rand/v2 PCG source of seed 18 (counting from 1, the first
// seed whose 20 draws hold a window of one day, one ending at the history's end and one of 80 days
// or more). The recorder is gone: run at any later head it would record that head's own answers,
// and the oracle would compare the code with itself.
//
// The three named exceptions are rules INFRA-2916 changed. A deploy's with_prs: a deploy is one
// with pull requests when it ships any pull request, whenever that pull request merged, where the
// base head counted only those merged inside the window. So each measures answer's
// deploys_with_prs must be the base head's plus exactly the deploys of the window whose every
// shipped pull request merged before it, counted from the fixture's own containment
// (deploysShippingOnlyEarlier), and with_prs_per_day the base head's plus that count over the
// window's days. The change failure rate while no failure source exists (root R11: 0 is a value,
// never a stand-in): the base head answered an object of zero flag counts and rates over the
// window's totals, and verdicts over them; this head answers that object with every count 0 and
// every rate null, which a page of the base head's build still reads, and both its verdicts null
// (takeFailureMeasures). And the Pipeline's slowest-jobs threshold (R5: a series'
// min_n is a person's to set, and every reader uses it): the base head ranked by the constant 50
// and stated nothing, this head ranks by the min_n of pr-ci's job series in the project the
// deploy repository is mapped to, 50 where none holds one, and states it in
// pr_checks.slowest_min_results, which must be 50 here, where no pr-ci eval is seeded, and which
// the base head's answer must not hold (takeSlowestMinResults). Those fields are then left out of
// the value-for-value comparison, and every other field it reads must be the base head's.
func TestTheDeliveryRoutesAnswerWhatTheyAnsweredAtTheBaseHead(t *testing.T) {
	t.Parallel()
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

	requests, values, differing, moved, movedWindows, failureValues, thresholds, pipelineCells := 0, 0, 0, 0, 0, 0, 0, 0
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
			pipeline := strings.HasPrefix(target, "/api/v1/delivery/pipeline?")
			request := target
			if pipeline {
				request = "/api/v1/delivery/pipeline?" + url.Values{"at": {window.To.Add(-time.Nanosecond).Format(time.RFC3339Nano)}}.Encode()
			}
			response := dispatchRequest(t, handler, http.MethodGet, request, nil, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s: status = %d, body = %s", request, response.Code, response.Body.String())
			}
			got := decodeDeliveryOracleTree(t, response.Body.Bytes())
			for _, field := range recorded.Stripped {
				if _, held := got.(map[string]any)[field]; !held {
					t.Fatalf("GET %s answers no %s to strip", request, field)
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
				failureValues += named
				if difference != "" {
					differing++
					t.Errorf("%s: GET %s: %s", window.File, target, difference)
				}
			}
			if pipeline {
				if difference := takeSlowestMinResults(got, want); difference != "" {
					differing++
					t.Errorf("%s: GET %s: %s", window.File, request, difference)
				}
				values, thresholds = values+1, thresholds+1
			}
			named, difference := takeTargets(got, want)
			values += named
			if difference != "" {
				differing++
				t.Errorf("%s: GET %s: %s", window.File, request, difference)
			}
			if pipeline {
				var cells int
				got, want, cells, difference = sharedPipelineDays(got, want)
				pipelineCells += cells
				if difference != "" {
					differing++
					t.Errorf("%s: GET %s for %s: %s", window.File, request, target, difference)
					continue
				}
			}
			compared, difference := compareDeliveryOracleTrees("", got, want)
			requests, values = requests+1, values+compared
			if difference != "" {
				differing++
				t.Errorf("%s: GET %s differs from the base head's answer to %s first at %s", window.File, request, target, difference)
			}
		}
		if earlier > 0 {
			moved, movedWindows = moved+earlier, movedWindows+1
		}
		t.Logf("%s..%s (%d days): %d deploys ship only pull requests merged before the window; the measures answers' with_prs moved by %s",
			window.From.Format(time.DateOnly), window.To.Format(time.DateOnly), window.Days, earlier, strings.Join(slices.Compact(withPRsMoved), ", "))
	}
	t.Logf("history %s (seed %d, %d days to %s), recorded at %s: %d windows, %d requests, %d values compared, %d requests differing; "+
		"deploys_with_prs moved in %d of the %d windows, by %d deploys in all; %d failure-measure values the base head answered are null or 0 here; "+
		"%d Pipeline answers state the slowest-jobs threshold %d the base head ranked by; %d Pipeline day cells compared on the days both answers show",
		recorded.Fixture.Spec.Project, recorded.Fixture.Spec.Seed, recorded.Fixture.Spec.Days, recorded.Fixture.Spec.Now.Format(time.DateOnly),
		recorded.Head, len(recorded.Windows), requests, values, differing, movedWindows, len(recorded.Windows), moved, failureValues,
		thresholds, measures.SlowestMinResults, pipelineCells)
	if failureValues == 0 {
		t.Error("no measures answer held a failure measure to compare, so the named difference was never read")
	}
	if thresholds == 0 {
		t.Error("no Pipeline answer was read, so its named difference was never read")
	}
	if pipelineCells == 0 {
		t.Error("no Pipeline day cell was compared, so the Pipeline's oracle compared nothing")
	}
}

// deploysShippingOnlyEarlier is how many of the fixture's deploys started in [from, to) ship pull
// requests every one of which merged before from, by the fixture's own containment
// (delivery.ContainingRun over every deploy the fixture judged): the deploys the base head's rule
// counted as shipping none in that window and INFRA-2916's counts as shipping some.
func deploysShippingOnlyEarlier(fixture deliveryseed.Fixture, from, to time.Time) int {
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
// both: got's status.change_failure_rate and status.change_failure_rate_upper_bound must each be
// null, and its measures.change_failure_rate the base head's object, field for field, with every
// rate null and every count 0; want's change failure rate must be the base head's stand-in, every
// flag count and rate 0 over a total that is a count, and each of its verdicts a boolean or null.
// It answers how many of the base head's values the check named and, where any is off, what is.
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
		if path[0] == "status" {
			if gotValue != nil {
				return named, fmt.Sprintf("%s is %v with no failure source, want null", strings.Join(path, "."), gotValue)
			}
			if _, verdict := wantValue.(bool); wantValue != nil && !verdict {
				return named, fmt.Sprintf("the base head's %s is %v, want a verdict or null", strings.Join(path, "."), wantValue)
			}
			named++
		} else {
			rates, ok := wantValue.(map[string]any)
			if !ok {
				return named, fmt.Sprintf("the base head's %s is %v, want its object of flag counts", strings.Join(path, "."), wantValue)
			}
			gotRates, ok := gotValue.(map[string]any)
			if !ok {
				return named, fmt.Sprintf("%s is %v with no failure source, want the base head's object with null rates", strings.Join(path, "."), gotValue)
			}
			for _, cut := range []string{"per_pr", "per_deploy"} {
				counts, ok := rates[cut].(map[string]any)
				if !ok {
					return named, fmt.Sprintf("the base head's change_failure_rate.%s is %v, want an object", cut, rates[cut])
				}
				gotCounts, ok := gotRates[cut].(map[string]any)
				if !ok || len(gotCounts) != len(counts) {
					return named, fmt.Sprintf("change_failure_rate.%s is %v, want the base head's fields %v", cut, gotRates[cut], slices.Sorted(maps.Keys(counts)))
				}
				for name, value := range counts {
					number, isNumber := value.(json.Number)
					if !isNumber {
						return named, fmt.Sprintf("the base head's change_failure_rate.%s.%s is %v, want a number", cut, name, value)
					}
					if name != "total" && number.String() != "0" {
						return named, fmt.Sprintf("the base head's change_failure_rate.%s.%s is %s with no flag stored, want its stand-in 0", cut, name, number)
					}
					gotField, held := gotCounts[name]
					if name == "rate" || name == "upper_bound_rate" {
						if !held || gotField != nil {
							return named, fmt.Sprintf("change_failure_rate.%s.%s is %v (held %t) with no failure source, want null", cut, name, gotField, held)
						}
					} else if count, isNumber := gotField.(json.Number); !held || !isNumber || count.String() != "0" {
						return named, fmt.Sprintf("change_failure_rate.%s.%s is %v (held %t) with no failure source, want 0", cut, name, gotField, held)
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

// takeSlowestMinResults checks got's pr_checks.slowest_min_results, a Pipeline answer's, against
// want, the base head's, which ranked by the constant measures.SlowestMinResults and stated no
// threshold: got's must be that 50, where no project holds a pr-ci eval to read another from, and
// want must hold none. It takes the field out of got and answers, where either is off, what is.
func takeSlowestMinResults(got, want any) string {
	gotChecks := got.(map[string]any)["pr_checks"].(map[string]any)
	if _, held := want.(map[string]any)["pr_checks"].(map[string]any)["slowest_min_results"]; held {
		return "the base head's pr_checks holds slowest_min_results, which it never stated"
	}
	value, held := gotChecks["slowest_min_results"]
	if number, isNumber := value.(json.Number); !held || !isNumber || number.String() != strconv.Itoa(measures.SlowestMinResults) {
		return fmt.Sprintf("pr_checks.slowest_min_results is %v (held %t), want %d, the threshold the base head ranked by", value, held, measures.SlowestMinResults)
	}
	delete(gotChecks, "slowest_min_results")
	return ""
}

// pipelineDaySeries are the per-day series of a Pipeline answer, each by its path: the PR-checks
// runs, the staging end-to-end, staging deploy and production deploy cards, and the two lead
// times. Each holds days, cells naming their day; a card also holds its job, and a staging card is
// null where the settings name none.
var pipelineDaySeries = [][]string{
	{"pr_checks", "run"}, {"staging_e2e"}, {"staging_apply"}, {"production_apply"},
	{"merge_to_production"}, {"opened_to_merge"},
}

// sharedPipelineDays is what got, a Pipeline answer read at the window's last instant, and want,
// the base head's answer of the window, both state of the days both show: of each series of
// pipelineDaySeries its cells of those days and a card's job, and the failing jobs. Both answers'
// last shown day must be the window's, so the shared days are want's last days, as many as both
// show. It answers the two parts, how many day cells want's part holds, and, where the shown days
// do not end together, what is off.
func sharedPipelineDays(got, want any) (any, any, int, string) {
	daysOf := func(tree any) []string {
		var days []string
		for _, day := range tree.(map[string]any)["days"].([]any) {
			days = append(days, day.(string))
		}
		return days
	}
	gotDays, wantDays := daysOf(got), daysOf(want)
	if len(gotDays) == 0 || len(wantDays) == 0 || gotDays[len(gotDays)-1] != wantDays[len(wantDays)-1] {
		return nil, nil, 0, fmt.Sprintf("the answer shows the days %v and the base head's %v, want both to end on the window's last day", gotDays, wantDays)
	}
	shared := map[string]bool{}
	for _, day := range wantDays {
		if slices.Contains(gotDays, day) {
			shared[day] = true
		}
	}
	part := func(tree any) (map[string]any, int) {
		answer := tree.(map[string]any)
		kept := map[string]any{"pr_checks.failing_jobs": answer["pr_checks"].(map[string]any)["failing_jobs"]}
		cells := 0
		for _, path := range pipelineDaySeries {
			series, _ := answer[path[0]].(map[string]any)
			for _, key := range path[1:] {
				series, _ = series[key].(map[string]any)
			}
			name := strings.Join(path, ".")
			if series == nil {
				kept[name] = nil
				continue
			}
			var days []any
			for _, cell := range series["days"].([]any) {
				if shared[cell.(map[string]any)["day"].(string)] {
					days = append(days, cell)
				}
			}
			entry := map[string]any{"days": days}
			if job, held := series["job"]; held {
				entry["job"] = job
			}
			kept[name] = entry
			cells += len(days)
		}
		return kept, cells
	}
	gotPart, _ := part(got)
	wantPart, cells := part(want)
	return gotPart, wantPart, cells, ""
}

// takeTargets checks got's targets, a measures or Pipeline answer's, against want's, the base
// head's, and takes them out of both: the base head stated each target as its value alone, and
// this answer states it as its eval holds it, so each must be want's value with the catalogue's
// inclusiveness (measures.DefaultTargets), the targets of the history's evals, which no person
// set. It answers how many of the base head's values the check named and, where any is off, what
// is.
func takeTargets(got, want any) (int, string) {
	gotTargets, _ := got.(map[string]any)["targets"].(map[string]any)
	wantTargets, _ := want.(map[string]any)["targets"].(map[string]any)
	catalogue := map[string]*measures.Target{
		"deploys_per_day": measures.DefaultTargets.DeploysPerDay, "change_failure_rate": measures.DefaultTargets.ChangeFailureRate,
		"merge_to_production_minutes":    measures.DefaultTargets.MergeToProductionMinutes,
		"opened_to_merge_median_minutes": measures.DefaultTargets.OpenedToMergeMedianMinutes,
		"deploy_run_success_rate":        measures.DefaultTargets.DeployRunSuccessRate, "unowned_p0": measures.DefaultTargets.UnownedP0,
	}
	if len(gotTargets) != len(catalogue) || len(wantTargets) != len(catalogue) {
		return 0, fmt.Sprintf("targets hold %d and the base head's %d, want the %d delivery targets", len(gotTargets), len(wantTargets), len(catalogue))
	}
	named := 0
	for key, inclusive := range catalogue {
		gotTarget, _ := gotTargets[key].(map[string]any)
		if gotTarget == nil || fmt.Sprint(gotTarget["value"]) != fmt.Sprint(wantTargets[key]) || gotTarget["inclusive"] != inclusive.Inclusive {
			return named, fmt.Sprintf("targets.%s is %v, want the base head's %v, inclusive %t", key, gotTargets[key], wantTargets[key], inclusive.Inclusive)
		}
		named++
	}
	delete(got.(map[string]any), "targets")
	delete(want.(map[string]any), "targets")
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
