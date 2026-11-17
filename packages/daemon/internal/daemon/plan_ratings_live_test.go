//go:build e2e

// The live proof's driver for the rating of each plan: each retro of the whole daemon rates the
// plan its issue ran, the daemon writes the plan and the rating onto the issue in Dispatch, and a
// person lists the finished issues of the project with the real `dispatch plans` command.
// scripts/e2e/plan-ratings.sh builds this driver with `-tags e2e` and owns the nats-server and the
// PostgreSQL it runs against.
//
// Nothing of the record path is stood in for. Dispatch is the real server this checkout builds
// (startHandoffDispatch), on a database of its own on LEGION_TEST_PG_DSN's server, with NATS off.
// The daemon is the whole daemon (run, through startDaemonWithin) with dispatch_url at that
// server, so each plan and each rating it writes goes through its outbox and its Dispatch client to
// the real routes. The issues walk as the package's whole-daemon tests walk them (stepRecordsRig,
// step_records_test.go, and walkThreeRated, plan_ratings_test.go). The reader is the real
// `dispatch` CLI under bun, run as a person of the project with the personal token Dispatch
// minted for that person (stepRecordsLive.cli, step_records_live_test.go).
//
// What is stood in for lies outside the records, as for the step records' driver: GitHub's REST
// API, the Apps' tokens, Envoy's listener, and the panes, which the package's fake runtime
// launches and the driver plays.
package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// planRatingsLive is one run of the proof: the walk's rig over four issues of one project, and the
// person's CLI.
type planRatingsLive struct {
	*stepRecordsLive
}

// TestPlanRatingsLive drives the proof's steps against one booted daemon and one Dispatch, over
// four issues of one project: three walk to done and rate their plans, and the fourth walks to
// its retro, whose completion is refused until it rates the plan. The fourth goes no further than
// merging, so the read by project lists the three alone.
func TestPlanRatingsLive(t *testing.T) {
	fixtureenv.Live(t)
	root := liveCheckout(t)
	proof := newLiveProof(t, "scripts/e2e/plan-ratings.sh")
	requireBun(t)
	l := &planRatingsLive{&stepRecordsLive{stepRecordsRig: newStepRecordsRigWith(t, proof, startHandoffDispatch(t, root).scratchDispatch, 4), root: root, cliHome: t.TempDir()}}
	l.mintReader("plans-reader")
	keys := make([]string, 0, len(l.issues))
	for _, issue := range l.issues {
		keys = append(keys, issue.key)
	}
	l.note("Dispatch %s (database of its own, NATS off), project %s mapped to %s, issues %s; daemon at dispatch_url %s; the reader is the person's personal token",
		l.dispatch.base, l.cfg.Project, liveRepo, strings.Join(keys, ", "), l.cfg.DispatchURL)
	l.step("three-finished-issues-each-rated", l.readByProject)
	l.step("a-retro-with-no-rating-is-refused", l.ratingRequired)
}

// readByProject is v-records, the read by project, in the form this slice can run: the rig's first
// three issues walk to done, the first with one failed review, and their retros rate the plan 3, 4
// and 5 with a reason. `dispatch plans --project <PROJECT>` run as a person of the project then
// prints the three, newest close first, each with the default plan, its one rating with its
// reason, its time and its count of failed steps, 1, 0 and 0. The read after the merge,
// scripts/e2e/plan-ratings-read.sh, judges the same command's output for the first issue and
// passes, naming its rating and reason; its control names the fourth issue, which is not
// finished, and the script must fail on it. The control of the read deletes the failed review's
// record from Dispatch's own store, its node and every tuple that names it, and the same parse of
// the same command must then report the first issue's failed steps 0 where 1 is wanted.
func (l *planRatingsLive) readByProject() {
	t := l.t
	want := l.walkThreeRated()
	l.note("%s, %s and %s walked to done, the first with one failed review; their retros rated the plan %d, %d and %d",
		want[0].issue, want[1].issue, want[2].issue, want[0].rating, want[1].rating, want[2].rating)
	// The ratings and the closes ride the outbox, so the read by project holds them a moment after
	// the walks end: GET /api/v1/projects/{key}/plans is waited on for the whole of what it must
	// list before the CLI reads it once.
	l.waitPlans("the three finished issues, each rated", want)

	out, code := l.plans()
	if code != 0 {
		t.Fatalf("`dispatch plans --project %s` exited %d: %s", l.cfg.Project, code, out)
	}
	if err := plansPrintedHold(l.cfg.Project, out, want); err != nil {
		t.Fatalf("`dispatch plans --project %s` printed\n%s\nand the read does not hold the three: %v", l.cfg.Project, out, err)
	}
	l.note("`dispatch plans --project %s` as the person, exit 0:", l.cfg.Project)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		l.note("  %s", line)
	}

	reason, err := json.Marshal(want[0].reason)
	if err != nil {
		t.Fatal(err)
	}
	out, code = l.readScript(want[0].issue)
	if passed := fmt.Sprintf("PASS: %s: plan default (", want[0].issue); code != 0 || !strings.Contains(out, passed) ||
		!strings.Contains(out, fmt.Sprintf(", ratings %d/5 %s, time ", want[0].rating, reason)) {
		t.Fatalf("scripts/e2e/plan-ratings-read.sh %s %s exited %d:\n%s\nwant exit 0 and a PASS line naming the rating %d/5 %s", l.cfg.Project, want[0].issue, code, out, want[0].rating, reason)
	}
	l.note("scripts/e2e/plan-ratings-read.sh %s %s, exit 0: %s", l.cfg.Project, want[0].issue, lastLine(out))
	unfinished := l.issues[3].key
	out, code = l.readScript(unfinished)
	if failed := fmt.Sprintf("FAIL: the read lists 3 finished issues of %s and no line of %s", l.cfg.Project, unfinished); code != 1 || !strings.Contains(out, failed) {
		t.Fatalf("control: scripts/e2e/plan-ratings-read.sh %s %s exited %d:\n%s\nwant exit 1 and %q", l.cfg.Project, unfinished, code, out, failed)
	}
	l.note("control: scripts/e2e/plan-ratings-read.sh %s %s, which is not finished, exit 1: %s", l.cfg.Project, unfinished, lastLine(out))

	failed := want[0].steps.find("step", 4, 1).ID
	if failed == "" {
		t.Fatalf("the records of %s hold no record of step 4 attempt 1", want[0].issue)
	}
	l.deleteStepRecord(failed)
	l.note("control: deleted %s's failed review's record %s from Dispatch's database (its workflow_nodes row and its workflow_tuples rows)", want[0].issue, failed)
	out, code = l.plans()
	if code != 0 {
		t.Fatalf("control: `dispatch plans --project %s` after the delete exited %d: %s", l.cfg.Project, code, out)
	}
	err = plansPrintedHold(l.cfg.Project, out, want)
	if err == nil {
		t.Fatalf("control: the parse of `dispatch plans --project %s` passed with the failed review's record deleted:\n%s", l.cfg.Project, out)
	}
	if wanted := failedStepsPrinted(want[0].issue, 0, 1); !strings.Contains(err.Error(), wanted) {
		t.Fatalf("control: the parse with the failed review's record deleted reports %q, want it to say %q", err, wanted)
	}
	l.note("control: the same parse now fails: %v", err)
}

// ratingRequired is the rule of the rating for a plugin at the new contract: the rig's fourth issue
// walks to its retro, and the implementer's completion of the retro with no rating, from a claim
// at api.PlanRatingContract or later, is refused 400 PLAN_RATING_REQUIRED, and the issue stays in
// retro. The control sends the same completion with planRating 4 and a reason, which moves the
// issue to merging, so the refusal is the rating's and no refusal of every retro.
func (l *planRatingsLive) ratingRequired() {
	t := l.t
	l.on(l.issues[3].key)
	l.plan()
	l.implement()
	l.test()
	l.review("APPROVED", phase.Accepting)
	l.accept()
	l.note("planning, implementing, testing, an approving review and the architect's sign-off walked %s to its retro", l.key)

	c := l.worker(claim.RoleImplementer)
	c.take()
	if contract := l.d.claim(c.token).PluginContract; contract < api.PlanRatingContract {
		t.Fatalf("the implementer of %s registered plugin contract %d, below PlanRatingContract %d: this check proves nothing", l.key, contract, api.PlanRatingContract)
	}
	status, body := l.complete(c, l.retroCompletion(0, ""))
	if status != http.StatusBadRequest || !strings.Contains(body, "PLAN_RATING_REQUIRED") {
		t.Fatalf("the retro's completion of %s with no rating = %d %s, want 400 PLAN_RATING_REQUIRED", l.key, status, body)
	}
	if got := string(l.d.state().Issues[l.key].Phase); got != string(phase.Retro) {
		t.Fatalf("after the refused completion %s is in %s, want retro", l.key, got)
	}
	l.note("the retro's completion with no rating, from the implementer at plugin contract %d: %d %s; %s still in retro",
		l.d.claim(c.token).PluginContract, status, strings.TrimSpace(body), l.key)

	l.finish(c, "", l.retroCompletion(4, "The plan fit the work once its retro was asked for a rating."), phase.Merging)
	l.note("control: the same completion with planRating 4 and a reason: 200; %s in merging", l.key)
}

// plans runs `dispatch plans --project <PROJECT>` as the person and returns both streams and its
// exit code.
func (l *planRatingsLive) plans() (string, int) {
	l.t.Helper()
	return l.cli("plans", "--project", l.cfg.Project)
}

// readScript runs scripts/e2e/plan-ratings-read.sh <PROJECT> issue in the person's shell, as the
// operator runs it after the merge, and returns both streams and its exit code. The `dispatch` on
// its PATH runs this checkout's CLI under bun, as the installed command does.
func (l *planRatingsLive) readScript(issue string) (string, int) {
	t := l.t
	t.Helper()
	cli := filepath.Join(l.root, "packages", "envoy-client", "bin", "dispatch.ts")
	if strings.Contains(cli, "'") {
		t.Fatalf("the checkout's path %s holds a quotation mark the dispatch wrapper cannot quote", l.root)
	}
	bin := filepath.Join(l.cliHome, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "dispatch"), []byte("#!/bin/sh\nexec bun '"+cli+"' \"$@\"\n"), 0o700); err != nil {
		t.Fatalf("write the dispatch command of the person's shell: %v", err)
	}
	command := exec.Command("bash", filepath.Join(l.root, "scripts", "e2e", "plan-ratings-read.sh"), l.cfg.Project, issue)
	command.Env = l.personsShell(bin + string(os.PathListSeparator) + os.Getenv("PATH"))
	return l.run(command, "scripts/e2e/plan-ratings-read.sh "+l.cfg.Project+" "+issue)
}

// lastLine is the last line of out.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return lines[len(lines)-1]
}

// planLine is one finished issue as `dispatch plans` prints it: `<ISSUE> closed <closed_at> plan
// <plan> ratings <ratings> time <time> failed steps <n>` (envoy-client's plans.ts).
type planLine struct {
	text, issue, closed, plan, time string
	ratings                         []printedRating
	failed                          int
}

// printedRating is one rating as `dispatch plans` prints it: `<rating>/5 <reason as JSON>`.
type printedRating struct {
	rating int
	reason string
}

// planLinePattern reads one finished issue's line; the ratings are read apart (parseRatings).
var planLinePattern = regexp.MustCompile(`^(\S+) closed (\S+) plan (none|(?:default|owner) \(\d+ tasks: [^)]*\)) ratings (none|.*) time (-|\d+h\d+m) failed steps (\d+)$`)

// parsePlans reads the output of `dispatch plans --project project`: the header `<project>: <n>
// finished issues`, then exactly n lines of issues.
func parsePlans(project, out string) ([]planLine, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	header := strings.TrimPrefix(lines[0], project+": ")
	var count int
	if header == lines[0] {
		return nil, fmt.Errorf("the first line %q is no header of %s", lines[0], project)
	}
	if _, err := fmt.Sscanf(header, "%d finished issues", &count); err != nil {
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
		line := planLine{text: text, issue: match[1], closed: match[2], plan: match[3], ratings: ratings, time: match[5]}
		if _, err := fmt.Sscanf(match[6], "%d", &line.failed); err != nil {
			return nil, fmt.Errorf("the line %q: failed steps %v", text, err)
		}
		parsed = append(parsed, line)
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
		if len(rest) < 4 || rest[0] < '1' || rest[0] > '5' || rest[1:4] != "/5 " {
			return nil, fmt.Errorf("the ratings %q: %q is no `<n>/5 ` rating", text, rest)
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

// failedStepsPrinted is how plansPrintedHold names an issue whose printed count of failed steps is
// got, not want.
func failedStepsPrinted(issue string, got, want int) string {
	return fmt.Sprintf("%s prints failed steps %d, want %d", issue, got, want)
}

// plansPrintedHold says whether out, the output of `dispatch plans --project project`, lists exactly
// the issues of want, newest close first, each with the default plan, its rating with its reason
// (or none), its time, the hours and minutes from the earliest start to the latest end of its
// records, and its count of failed steps. The error names the first issue that is missing or wrong.
func plansPrintedHold(project, out string, want []ratedWant) error {
	lines, err := parsePlans(project, out)
	if err != nil {
		return err
	}
	if len(lines) != len(want) {
		return fmt.Errorf("`dispatch plans` lists %d finished issues, want %d", len(lines), len(want))
	}
	var before time.Time
	for _, line := range lines {
		closed, err := time.Parse(time.RFC3339Nano, line.closed)
		if err != nil {
			return fmt.Errorf("%s prints the close %q: %v", line.issue, line.closed, err)
		}
		if !before.IsZero() && closed.After(before) {
			return fmt.Errorf("%s closed at %s is printed after an issue closed at %s, want the newest close first", line.issue, closed, before)
		}
		before = closed
	}
	plan := fmt.Sprintf("default (1 tasks: %s)", strings.Join(workflow.DefaultSteps(), " "))
	for _, w := range want {
		at := slices.IndexFunc(lines, func(line planLine) bool { return line.issue == w.issue })
		if at < 0 {
			return fmt.Errorf("`dispatch plans` prints no line of %s", w.issue)
		}
		got := lines[at]
		started, ended := w.steps.span()
		minutes := int64(ended.Sub(started)/time.Second) / 60
		spent := fmt.Sprintf("%dh%dm", minutes/60, minutes%60)
		switch {
		case got.plan != plan:
			return fmt.Errorf("%s prints plan %s, want plan %s", w.issue, got.plan, plan)
		case w.rating == 0 && len(got.ratings) != 0:
			return fmt.Errorf("%s prints the ratings %+v, want none", w.issue, got.ratings)
		case w.rating != 0 && (len(got.ratings) != 1 || got.ratings[0] != printedRating{rating: w.rating, reason: w.reason}):
			return fmt.Errorf("%s prints the ratings %+v, want the one rating %d/5 %q", w.issue, got.ratings, w.rating, w.reason)
		case got.time != spent:
			return fmt.Errorf("%s prints time %s, want %s, from %s to %s", w.issue, got.time, spent, started, ended)
		case got.failed != w.failed:
			return fmt.Errorf("%s", failedStepsPrinted(w.issue, got.failed, w.failed))
		}
	}
	return nil
}
