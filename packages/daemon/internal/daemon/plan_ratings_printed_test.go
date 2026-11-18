// The parse of what `dispatch plans --project <PROJECT>` prints, and the check of the read
// script's PASS line, which the live proof's driver (plan_ratings_live_test.go) judges the real
// command's output with. They read no environment, so they and their tests carry no build tag:
// the package's ordinary test run runs each refusal, where the live proof's script selects only
// TestPlanRatingsLive.
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// readScriptPassed is whether out, the output of plan-ratings-read.sh for line's issue, holds its
// PASS line naming the issue's plan and ratings as `dispatch plans` printed them, which is how the
// script prints them: no encoding of the driver's own stands between the two.
func readScriptPassed(out string, line planLine) bool {
	return strings.Contains(out, fmt.Sprintf("PASS: %s: plan %s, ratings %s, time ", line.issue, line.plan, line.printedRatings))
}

// defaultPlanPrinted is the default plan as `dispatch plans` prints it: its source, its one task
// and that task's steps, workflow.DefaultSteps.
func defaultPlanPrinted() string {
	return fmt.Sprintf("default (1 tasks: %s)", strings.Join(workflow.DefaultSteps(), " "))
}

// plansPrintedWith is what `dispatch plans --project SR1` prints of one finished issue whose
// ratings print as ratings, the answer the tests below parse.
func plansPrintedWith(ratings string) string {
	return "SR1: 1 finished issues\nSR1-1 closed 2026-11-17T00:00:00Z plan " + defaultPlanPrinted() +
		" ratings " + ratings + " time 0h1m failed steps 0"
}

// The read script prints the ratings as the command printed them, and plans.ts prints each reason
// with JSON.stringify, which leaves `<`, `>` and `&` as they are, where Go's encoder escapes them;
// so the driver's check of the script's PASS line holds for a reason that holds them. The control
// is a PASS line naming another reason.
func TestTheReadScriptsPassIsReadAsTheCommandPrintedTheRatings(t *testing.T) {
	printed := plansPrintedWith(`4/5 "Done in <1 day & no rework."`)
	lines, err := parsePlans("SR1", printed)
	if err != nil {
		t.Fatal(err)
	}
	pass := fmt.Sprintf(`PASS: SR1-1: plan %s, ratings 4/5 "Done in <1 day & no rework.", time 0h1m, failed steps 0`, defaultPlanPrinted())
	if !readScriptPassed(printed+"\n"+pass+"\n", lines[0]) {
		t.Errorf("the script's PASS line %q was not read as naming the ratings of %q", pass, lines[0].text)
	}
	if other := strings.Replace(pass, "no rework", "a rework", 1); readScriptPassed(printed+"\n"+other+"\n", lines[0]) {
		t.Errorf("control: the PASS line %q, naming another reason, was read as naming the ratings of %q", other, lines[0].text)
	}
}

// planLine is one finished issue as `dispatch plans` prints it: `<ISSUE> closed <closed_at> plan
// <plan> ratings <ratings> time <time> failed steps <n>` (envoy-client's plans.ts). printedRatings
// is the ratings' text as printed, and ratings is that text read.
type planLine struct {
	text, issue, closed, plan, printedRatings, time string
	ratings                                         []printedRating
	failed                                          int
}

// printedRating is one rating as `dispatch plans` prints it: `<rating>/5 <reason as JSON>`.
type printedRating struct {
	rating int
	reason string
}

// planHeaderPattern reads the header `<project>: <n> finished issues`, with nothing after it, as
// plan-ratings-read.sh's awk anchors it.
var planHeaderPattern = regexp.MustCompile(`^(\S+): (\d+) finished issues$`)

// planLinePattern reads one finished issue's line; the ratings are read apart (parseRatings).
var planLinePattern = regexp.MustCompile(`^(\S+) closed (\S+) plan (none|(?:default|owner) \(\d+ tasks: [^)]*\)) ratings (none|.*) time (-|\d+h\d+m) failed steps (\d+)$`)

// parsePlans reads the output of `dispatch plans --project project`: the header `<project>: <n>
// finished issues`, then exactly n lines of issues.
func parsePlans(project, out string) ([]planLine, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	header := planHeaderPattern.FindStringSubmatch(lines[0])
	if header == nil || header[1] != project {
		return nil, fmt.Errorf("the first line %q is no header `%s: <n> finished issues`", lines[0], project)
	}
	count, err := strconv.Atoi(header[2])
	if err != nil {
		return nil, fmt.Errorf("the header %q: %v", lines[0], err)
	}
	var parsed []planLine
	for _, text := range lines[1:] {
		match := planLinePattern.FindStringSubmatch(text)
		if match == nil {
			return nil, fmt.Errorf("the line %q is no finished issue", text)
		}
		ratings, err := parseRatings(match[4])
		if err != nil {
			return nil, fmt.Errorf("the line %q: %v", text, err)
		}
		failed, err := strconv.Atoi(match[6])
		if err != nil {
			return nil, fmt.Errorf("the line %q: failed steps %v", text, err)
		}
		parsed = append(parsed, planLine{text: text, issue: match[1], closed: match[2], plan: match[3], printedRatings: match[4], ratings: ratings, time: match[5], failed: failed})
	}
	if len(parsed) != count {
		return nil, fmt.Errorf("the header says %d finished issues and %d lines follow it", count, len(parsed))
	}
	return parsed, nil
}

// parseRatings reads the ratings of one line: `none`, or each rating as `<n>/5 ` and one JSON
// string, separated by `, ` outside those strings.
func parseRatings(text string) ([]printedRating, error) {
	if text == "none" {
		return nil, nil
	}
	var ratings []printedRating
	for rest := text; ; {
		// The reason is a JSON string, as plans.ts prints it: a `null` in its place, which Decode
		// would take as an empty reason, is no rating, and neither is a space before the string,
		// which Decode would skip (plan-ratings-read.sh's ratingsOf refuses both).
		if len(rest) < 5 || rest[0] < '1' || rest[0] > '5' || rest[1:5] != `/5 "` {
			return nil, fmt.Errorf("the ratings %q: %q is no `<n>/5 \"<reason>\"` rating", text, rest)
		}
		decoder := json.NewDecoder(strings.NewReader(rest[4:]))
		var reason string
		if err := decoder.Decode(&reason); err != nil {
			return nil, fmt.Errorf("the ratings %q: the reason after %q: %v", text, rest[:4], err)
		}
		ratings = append(ratings, printedRating{rating: int(rest[0] - '0'), reason: reason})
		rest = rest[4+int(decoder.InputOffset()):]
		if rest == "" {
			return ratings, nil
		}
		if !strings.HasPrefix(rest, ", ") {
			return nil, fmt.Errorf("the ratings %q: %q follows a rating, want `, ` or the end", text, rest)
		}
		rest = rest[2:]
	}
}

// The parser of `dispatch plans` reads only what envoy-client's plans.ts prints, as
// plan-ratings-read.sh's awk does, so the driver and the read script judge the same line alike:
// a rating is `<n>/5 ` and then its reason as one JSON string, so a `null` in the reason's place
// or a space before it is no rating, and the header is `<project>: <n> finished issues` with
// nothing after it.
func TestThePlansParserReadsOnlyWhatTheCommandPrints(t *testing.T) {
	if parsed, err := parsePlans("SR1", plansPrintedWith(`4/5 "x"`)); err != nil || len(parsed) != 1 || len(parsed[0].ratings) != 1 || parsed[0].ratings[0] != (printedRating{rating: 4, reason: "x"}) {
		t.Fatalf("the line the command prints parsed as %+v, %v; want one rating 4/5 \"x\"", parsed, err)
	}
	for _, ratings := range []string{`4/5 null`, `4/5  "x"`} {
		if parsed, err := parsePlans("SR1", plansPrintedWith(ratings)); err == nil {
			t.Errorf("the ratings %q parsed as %+v, want an error: the command prints each reason as a JSON string right after `<n>/5 `", ratings, parsed)
		}
	}
	if parsed, err := parsePlans("SR1", strings.Replace(plansPrintedWith(`4/5 "x"`), "1 finished issues", "1 finished issues (of 4)", 1)); err == nil {
		t.Errorf("the header `SR1: 1 finished issues (of 4)` parsed as %+v, want an error: the command prints the header with nothing after it", parsed)
	}
}

// The printed read holds each issue to its own plan: a planned issue to the default plan, and an
// issue whose run started before the release that writes each run's plan (ratedWant.unplanned) to
// `plan none`, as the read by project holds it to no plan (plansHold). Each is read against both
// texts: the one naming its plan holds, and the other fails, naming the plan it wants. The issue's
// one record ends before the issue closes, as a walk's records do.
func TestThePrintedReadHoldsEachIssueToItsPlan(t *testing.T) {
	var steps stepRecordsRead
	if err := json.Unmarshal([]byte(`{"tasks":[{"task":"default","steps":[{"started_at":"2026-11-16T23:58:00Z","ended_at":"2026-11-16T23:59:00Z"}]}]}`), &steps); err != nil {
		t.Fatal(err)
	}
	planned := plansPrintedWith("none")
	unplanned := strings.Replace(planned, "plan "+defaultPlanPrinted(), "plan none", 1)
	for _, tc := range []struct {
		name              string
		unplanned         bool
		holds, fails, why string
	}{
		{name: "a planned issue", holds: planned, fails: unplanned, why: "want plan " + defaultPlanPrinted()},
		{name: "an unplanned issue", unplanned: true, holds: unplanned, fails: planned, why: "want plan none"},
	} {
		want := []ratedWant{{walkIssue: walkIssue{key: "SR1-1"}, steps: steps, unplanned: tc.unplanned}}
		if _, err := plansPrintedHold("SR1", tc.holds, want); err != nil {
			t.Errorf("%s printed with its plan: %v, want it held", tc.name, err)
		}
		if _, err := plansPrintedHold("SR1", tc.fails, want); err == nil || !strings.Contains(err.Error(), tc.why) {
			t.Errorf("control: %s printed with the other plan = %v, want an error saying %q", tc.name, err, tc.why)
		}
	}
}

// failedStepsPrinted is how plansPrintedHold names an issue whose printed count of failed steps is
// got, not want.
func failedStepsPrinted(issue string, got, want int) string {
	return fmt.Sprintf("%s prints failed steps %d, want %d", issue, got, want)
}

// plansPrintedHold says whether out, the output of `dispatch plans --project project`, lists exactly
// the issues of want, in want's order, which is newest close first, each with the default plan, or
// `none` for an unplanned one, its rating with its reason (or none), its time, the hours and
// minutes from the earliest start to the latest end of its records, and its count of failed steps.
// It answers the lines it held, so the line in each place is the issue of want's in that place, or
// an error naming the first issue that is missing or wrong.
func plansPrintedHold(project, out string, want []ratedWant) ([]planLine, error) {
	lines, err := parsePlans(project, out)
	if err != nil {
		return nil, err
	}
	if len(lines) != len(want) {
		return nil, fmt.Errorf("`dispatch plans` lists %d finished issues, want %d", len(lines), len(want))
	}
	var before time.Time
	for _, line := range lines {
		closed, err := time.Parse(time.RFC3339Nano, line.closed)
		if err != nil {
			return nil, fmt.Errorf("%s prints the close %q: %v", line.issue, line.closed, err)
		}
		if !before.IsZero() && closed.After(before) {
			return nil, fmt.Errorf("%s closed at %s is printed after an issue closed at %s, want the newest close first", line.issue, closed, before)
		}
		before = closed
	}
	for place, w := range want {
		plan := defaultPlanPrinted()
		if w.unplanned {
			plan = "none"
		}
		got := lines[place]
		if got.issue != w.key {
			return nil, fmt.Errorf("`dispatch plans` prints %s in place %d, want %s: want's order is newest close first", got.issue, place+1, w.key)
		}
		started, ended := w.steps.span()
		minutes := int64(ended.Sub(started) / time.Minute)
		spent := fmt.Sprintf("%dh%dm", minutes/60, minutes%60)
		switch {
		case got.plan != plan:
			return nil, fmt.Errorf("%s prints plan %s, want plan %s", w.key, got.plan, plan)
		case w.rating == nil && len(got.ratings) != 0:
			return nil, fmt.Errorf("%s prints the ratings %+v, want none", w.key, got.ratings)
		case w.rating != nil && (len(got.ratings) != 1 || got.ratings[0] != printedRating{rating: w.rating.Score, reason: w.rating.Reason}):
			return nil, fmt.Errorf("%s prints the ratings %+v, want the one rating %d/5 %q", w.key, got.ratings, w.rating.Score, w.rating.Reason)
		case got.time != spent:
			return nil, fmt.Errorf("%s prints time %s, want %s, from %s to %s", w.key, got.time, spent, started, ended)
		case got.failed != w.failed:
			return nil, errors.New(failedStepsPrinted(w.key, got.failed, w.failed))
		}
	}
	return lines, nil
}
