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

	"github.com/sjawhar/envoy/steprecord"

	"github.com/sjawhar/legion/daemon/internal/record"
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
	t.Parallel()
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

// printedRating is one rating as `dispatch plans` prints it: `<rating>/<most> <reason as JSON>`,
// most the top of the daemon's scale (steprecord.RatingMost), the contract's LEGION_PLAN_RATING_MOST,
// which plans.ts prints each rating out of.
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

// ratingHeadPattern reads the head of one printed rating, `<rating>/<most> "`: a whole number, then
// the top of the daemon's scale, then the quotation mark that opens the reason.
var ratingHeadPattern = regexp.MustCompile(fmt.Sprintf(`^([1-9][0-9]*)/%d "`, steprecord.RatingMost))

// parseRatings reads the ratings of one line: `none`, or each rating as `<n>/<most> `, n on the
// daemon's scale (steprecord.RatingLeast to steprecord.RatingMost), and one JSON string, separated
// by `, ` outside those strings.
func parseRatings(text string) ([]printedRating, error) {
	if text == "none" {
		return nil, nil
	}
	var ratings []printedRating
	for rest := text; ; {
		// The reason is a JSON string, as plans.ts prints it: a `null` in its place, which Decode
		// would take as an empty reason, is no rating, and neither is a space before the string,
		// which Decode would skip (plan-ratings-read.sh's ratingsOf refuses both).
		head := ratingHeadPattern.FindStringSubmatch(rest)
		if head == nil {
			return nil, fmt.Errorf("the ratings %q: %q is no `<n>/%d \"<reason>\"` rating", text, rest, steprecord.RatingMost)
		}
		rating, err := strconv.Atoi(head[1])
		if err != nil || rating < steprecord.RatingLeast || rating > steprecord.RatingMost {
			return nil, fmt.Errorf("the ratings %q: %s is no rating of the scale %d to %d", text, head[1], steprecord.RatingLeast, steprecord.RatingMost)
		}
		// The reason's JSON string starts at its opening quotation mark, the head's last byte.
		opened := len(head[0]) - 1
		decoder := json.NewDecoder(strings.NewReader(rest[opened:]))
		var reason string
		if err := decoder.Decode(&reason); err != nil {
			return nil, fmt.Errorf("the ratings %q: the reason after %q: %v", text, rest[:opened], err)
		}
		ratings = append(ratings, printedRating{rating: rating, reason: reason})
		rest = rest[opened+int(decoder.InputOffset()):]
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
// a rating is `<n>/<most> ` and then its reason as one JSON string, so a `null` in the reason's
// place or a space before it is no rating, and the header is `<project>: <n> finished issues` with
// nothing after it. The scale is the daemon's (steprecord.RatingLeast to steprecord.RatingMost):
// its top is a rating, and a rating past either end, or out of another number, is none.
func TestThePlansParserReadsOnlyWhatTheCommandPrints(t *testing.T) {
	t.Parallel()
	if parsed, err := parsePlans("SR1", plansPrintedWith(`4/5 "x"`)); err != nil || len(parsed) != 1 || len(parsed[0].ratings) != 1 || parsed[0].ratings[0] != (printedRating{rating: 4, reason: "x"}) {
		t.Fatalf("the line the command prints parsed as %+v, %v; want one rating 4/5 \"x\"", parsed, err)
	}
	most, least := steprecord.RatingMost, steprecord.RatingLeast
	top := fmt.Sprintf(`%d/%d "x"`, most, most)
	if parsed, err := parsePlans("SR1", plansPrintedWith(top)); err != nil || len(parsed) != 1 || len(parsed[0].ratings) != 1 || parsed[0].ratings[0] != (printedRating{rating: most, reason: "x"}) {
		t.Errorf("the top of the daemon's scale, %s, parsed as %+v, %v; want one rating %d", top, parsed, err, most)
	}
	for _, ratings := range []string{
		fmt.Sprintf(`4/%d null`, most), fmt.Sprintf(`4/%d  "x"`, most),
		fmt.Sprintf(`%d/%d "x"`, most+1, most), fmt.Sprintf(`%d/%d "x"`, least-1, most), fmt.Sprintf(`%d/%d "x"`, least, most+2),
	} {
		if parsed, err := parsePlans("SR1", plansPrintedWith(ratings)); err == nil {
			t.Errorf("the ratings %q parsed as %+v, want an error: the command prints each rating on the daemon's scale out of its top, and its reason as a JSON string right after it", ratings, parsed)
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
	t.Parallel()
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

// The printed read holds two issues to their order, their ratings, their times and their failed
// steps: a newer issue, rated, with one failed step and records more than an hour apart, printed
// before an older one with no rating. Each control changes one thing of the printed text and wants
// the error that names it: the two lines swapped, the rated issue printed with no rating, the
// unrated one printed with a rating, the time printed past its minutes, and the failed step
// printed as none.
func TestThePrintedReadHoldsTwoIssuesToTheirOrderRatingsTimesAndFailedSteps(t *testing.T) {
	t.Parallel()
	records := func(started, ended string) stepRecordsRead {
		t.Helper()
		var steps stepRecordsRead
		text := fmt.Sprintf(`{"tasks":[{"task":"default","steps":[{"started_at":%q,"ended_at":%q}]}]}`, started, ended)
		if err := json.Unmarshal([]byte(text), &steps); err != nil {
			t.Fatal(err)
		}
		return steps
	}
	want := []ratedWant{
		{walkIssue: walkIssue{key: "SR1-2"}, rating: &record.PlanRating{Rating: 4, Reason: "The plan fit the work."}, failed: 1, steps: records("2026-11-16T22:50:00Z", "2026-11-16T23:55:00Z")},
		{walkIssue: walkIssue{key: "SR1-1"}, steps: records("2026-11-16T23:58:00Z", "2026-11-16T23:59:00Z")},
	}
	newer := "SR1-2 closed 2026-11-17T01:00:00Z plan " + defaultPlanPrinted() + ` ratings 4/5 "The plan fit the work." time 1h5m failed steps 1`
	older := "SR1-1 closed 2026-11-17T00:00:00Z plan " + defaultPlanPrinted() + " ratings none time 0h1m failed steps 0"
	printed := func(lines ...string) string { return "SR1: 2 finished issues\n" + strings.Join(lines, "\n") }
	if _, err := plansPrintedHold("SR1", printed(newer, older), want); err != nil {
		t.Fatalf("the two issues printed newest close first: %v, want them held", err)
	}
	for _, control := range []struct{ name, out, why string }{
		{"the older issue printed first", printed(older, newer), "want the newest close first"},
		{"the rated issue printed with no rating", printed(strings.Replace(newer, `ratings 4/5 "The plan fit the work."`, "ratings none", 1), older), "want the one rating"},
		{"the unrated issue printed with a rating", printed(newer, strings.Replace(older, "ratings none", `ratings 2/5 "x"`, 1)), "want none"},
		{"the rated issue's time printed past its minutes", printed(strings.Replace(newer, "time 1h5m", "time 1h65m", 1), older), "prints time 1h65m, want 1h5m"},
		{"the rated issue's failed step printed as none", printed(strings.Replace(newer, "failed steps 1", "failed steps 0", 1), older), failedStepsPrinted("SR1-2", 0, 1)},
	} {
		if _, err := plansPrintedHold("SR1", control.out, want); err == nil || !strings.Contains(err.Error(), control.why) {
			t.Errorf("control: %s = %v, want an error saying %q", control.name, err, control.why)
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
		case w.rating != nil && (len(got.ratings) != 1 || got.ratings[0] != printedRating{rating: w.rating.Rating, reason: w.rating.Reason}):
			return nil, fmt.Errorf("%s prints the ratings %+v, want the one rating %d/%d %q", w.key, got.ratings, w.rating.Rating, steprecord.RatingMost, w.rating.Reason)
		case got.time != spent:
			return nil, fmt.Errorf("%s prints time %s, want %s, from %s to %s", w.key, got.time, spent, started, ended)
		case got.failed != w.failed:
			return nil, errors.New(failedStepsPrinted(w.key, got.failed, w.failed))
		}
	}
	return lines, nil
}
