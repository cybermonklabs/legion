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
// /api/v1/delivery/pipeline) answers, value for value, what the base head answered when it was
// recorded, the recorder's stripped fields left out. Integers and strings must be equal; floats
// within deliveryOracleTolerance of each other, relative to the larger. The change failure rate
// and the targets are compared like every other field.
//
// The Pipeline is compared on the days the two answers share alone. The base head answered
// ?from=&to= with the window's own days; INFRA-1077's cut (0a919c56) made the route read `at`
// alone, the seven UTC days ending on at's, and none of the recorded windows is seven days long.
// The recorder is gone (below), so no answer of seven days can be recorded at the base head
// either. So each recorded Pipeline request is sent as ?at= the window's last instant, whose last
// shown day is the window's, and the two answers are compared on what both state of the days both
// show (sharedPipelineDays): each per-day cell of the PR-checks runs, the three deploy cards and
// the two lead times, each card's job, the failing jobs, which both read over the 24 hours before
// the window's end, and the targets. The totals and the slowest jobs span every shown day, which
// differ, and are left out. The test fails where it compared no Pipeline cell, read no Pipeline
// answer, or compared no change-failure value.
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
// The one named exception is a rule INFRA-2916 changed, a deploy's with_prs: a deploy is one with
// pull requests when it ships any pull request, whenever that pull request merged, where the base
// head counted only those merged inside the window. So each measures answer's deploys_with_prs
// must be the base head's plus exactly the deploys of the window whose every shipped pull request
// merged before it, counted from the fixture's own containment (deploysShippingOnlyEarlier), and
// with_prs_per_day the base head's plus that count over the window's days (takeWithPRs). Those two
// fields are then left out of the value-for-value comparison, and every other field it reads must
// be the base head's.
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

	requests, values, differing, moved, movedWindows, failureValues, pipelines, pipelineCells := 0, 0, 0, 0, 0, 0, 0, 0
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
				// The change failure rate is compared with every other field below; this counts
				// the values of it that comparison reads, where the recorded answer holds one.
				failure := func(tree any) any {
					return tree.(map[string]any)["measures"].(map[string]any)["change_failure_rate"]
				}
				if rate, held := failure(want).(map[string]any); held && len(rate) > 0 {
					read, _ := compareDeliveryOracleTrees(".measures.change_failure_rate", failure(got), rate)
					failureValues += read
				}
			}
			if pipeline {
				var cells int
				var difference string
				got, want, cells, difference = sharedPipelineDays(got, want)
				pipelines, pipelineCells = pipelines+1, pipelineCells+cells
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
		"deploys_with_prs moved in %d of the %d windows, by %d deploys in all; %d change-failure values compared; "+
		"%d Pipeline answers read, %d Pipeline day cells compared on the days both answers show",
		recorded.Fixture.Spec.Project, recorded.Fixture.Spec.Seed, recorded.Fixture.Spec.Days, recorded.Fixture.Spec.Now.Format(time.DateOnly),
		recorded.Head, len(recorded.Windows), requests, values, differing, movedWindows, len(recorded.Windows), moved, failureValues,
		pipelines, pipelineCells)
	if failureValues == 0 {
		t.Error("no measures answer held a change-failure value to compare, so the comparison never read the change failure rate")
	}
	if pipelines == 0 {
		t.Error("no Pipeline answer was read, so the Pipeline's oracle compared nothing")
	}
	if pipelineCells == 0 {
		t.Error("no Pipeline day cell was compared, so the Pipeline's oracle compared nothing")
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
// pipelineDaySeries its cells of those days and a card's job, the failing jobs, and the targets,
// which are no day's and every day's alike. Both answers' last shown day must be the window's, so
// the shared days are want's last days, as many as both show. It answers the two parts, how many
// day cells want's part holds, and, where the shown days do not end together, what is off.
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
		kept := map[string]any{
			"pr_checks.failing_jobs": answer["pr_checks"].(map[string]any)["failing_jobs"],
			"targets":                answer["targets"],
		}
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
