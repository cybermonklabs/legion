//go:build e2e

// The live proof's driver of the owner's step: the owner of an issue, its own architect, gives
// Legion a plan whose one step is its own (`implementer:owner`), or a plan with no step of the type
// implementation (`tester`), and the whole daemon tells the owner each step it does as one notice
// owner-step on the owner's role topic, starts no claim for that step, takes the owner's report of
// it through the architect's route POST /legion/v1/steps/complete as the owner's `complete_step`
// sends it, and ends a task with no pull request at done once the owner's retro ends, with no
// merge. scripts/e2e/owner-does-step.sh builds this driver with `-tags e2e` and owns the
// nats-server and the PostgreSQL it runs against.
//
// The rig is the owner's steps driver's (newOwnerStepsLive, owner_steps_live_test.go): the real
// Dispatch server this checkout builds, on a database of its own, the whole daemon booted with
// dispatch_url at it over issues seeded at admitted with no design gate and no pull request, each
// plan written with the real `dispatch artifact` as the owner's session and given with
// register_plan before the issue's gate opens, and the steps read back with the real `dispatch
// steps` and `dispatch plans` as a person of the project. The owner's pane is the architect's
// claimant (stepClaimant): it is told its step where Envoy's listener is told it (noticeListener,
// whose publishes to the owner's role topic the driver reads), and it reports as the owner's
// `complete_step` reports, in a turn of its own, with a grant minted for its session, and with the
// handoff record of a phase that ends with one written to Dispatch under that session.
//
// What is stood in for lies outside the owner's step: GitHub's REST API (stepRecordsGitHub, which
// lists the first issue's pull request from the owner's report of its step on, as it lists an
// implementer's from its completion on), the Apps' tokens, Envoy's listener, and the panes, which
// the package's fake runtime launches and the driver plays.
package daemon

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

var (
	// ownerDoesStepPlan is the plan of the driver's first issue: one task a, whose one step is the
	// owner's implementer step, so the retro is the owner's too.
	ownerDoesStepPlan = []string{"implementer:owner"}
	// ownerDoesStepTester is the plan of the second issue: one task a, whose one step is the
	// tester's, a task with no step of the type implementation, whose retro is the owner's.
	ownerDoesStepTester = []string{"tester"}
	// ownerDoesStepFresh is the plan of the third issue, the first's with no key: its step is a
	// fresh implementer's.
	ownerDoesStepFresh = []string{"implementer"}
)

// ownerDoesStepText is the text of the block every plan of the run holds (planOf), which the task
// of a step names last.
const ownerDoesStepText = "Make the change the issue asks for."

// ownerDoesStepLive is one run of the proof: the owner's steps driver over three issues.
type ownerDoesStepLive struct {
	*ownerStepsLive
}

// TestOwnerDoesStepLive drives the proof's checks against one booted daemon and one Dispatch, over
// three issues of one project: the first walks its owner's plan `implementer:owner` to merging, the
// third is given `implementer`, whose step is a fresh implementer's, and the second walks `tester`
// to done with no merge. Each control runs a check's own assertion over an issue it must fail on,
// prints `control: FAIL <check> over <KEY>` with what the assertion said, and fails the run unless
// the assertion failed for the reason it names.
func TestOwnerDoesStepLive(t *testing.T) {
	l := &ownerDoesStepLive{newOwnerStepsLive(t, "scripts/e2e/owner-does-step.sh", 3)}
	owned, alone, fresh := l.issues[0].key, l.issues[1].key, l.issues[2].key
	l.note("Dispatch %s (database of its own), project %s mapped to %s, issues %s, %s and %s; daemon at dispatch_url %s",
		l.dispatch.base, l.cfg.Project, liveRepo, owned, alone, fresh, l.cfg.DispatchURL)
	l.check("owner-step", func() {
		l.on(owned)
		l.ownerStep()
	})
	l.check("control-owner-step", func() {
		l.on(fresh)
		l.freshImplementer()
	})
	l.check("tester-alone", func() {
		l.on(alone)
		l.testerAlone()
	})
	l.check("control-tester-alone", func() {
		l.on(owned)
		l.mergesWithItsPullRequest()
	})
}

// given is the owner of the walk's issue writing plan.md with one task a naming steps through the
// real `dispatch artifact` command (planWritten) and giving version 1 of it with register_plan
// (planRegistered), and the issue's gate opening, after which it stands in first. It answers how
// many launches of the issue's claims the daemon had made before the gate opened (launched).
func (l *ownerDoesStepLive) given(steps []string, first phase.Phase) int {
	l.t.Helper()
	plan := l.planWritten(steps)
	l.planRegistered(plan, steps)
	since := l.launched()
	l.openGate(first)
	l.note("%s's spec approved and registered: its gate opened, and it stands in %s", l.key, first)
	return since
}

// launched is how many launches of the claims of the walk's issue the daemon has made, each a Spawn
// or a Resume the fake runtime recorded (launchesOf).
func (l *ownerDoesStepLive) launched() int {
	return len(l.launchesOf(l.key))
}

// ownerStep is owner-step: the first issue's owner gives `implementer:owner`, and is told step 1 as
// one notice owner-step, with no claim of the issue launched for it (ownerTold); its report of step
// 1 with `complete_step`, the pull request open on the issue's branch, moves the issue to the retro,
// which is the owner's and is told the same way; its report of the retro with a rating moves the
// issue to merging, and the merger is launched.
func (l *ownerDoesStepLive) ownerStep() {
	t := l.t
	t.Helper()
	since := l.given(ownerDoesStepPlan, phase.Implementing)
	told, err := l.ownerTold(since, 1, claim.RoleImplementer, phase.Implementing, "step", 1, ownerDoesStepText)
	if err != nil {
		t.Fatal(err)
	}
	l.note("the owner %s was told one owner-step (role %s, phase %s), and no claim of %s was launched; its task: %q",
		l.token(claim.RoleArchitect), told.Role, told.Phase, l.key, told.Reason)

	l.github.open("legion/"+l.key, l.number)
	owner := l.worker(claim.RoleArchitect)
	id, version := l.writeHandoff(owner.session, "implement")
	since = l.launched()
	status, body := l.ownerReports(api.CompleteStepRequest{Step: 1, Summary: "implemented", Commit: stepRecordsHead, HandoffID: id, HandoffVersion: version})
	if status != http.StatusOK {
		t.Fatalf("the owner's complete_step of step 1 of %s = %d %s, want 200", l.key, status, body)
	}
	l.waitPhase(phase.Retro)
	l.note("GitHub lists pull request #%d open on legion/%s; the owner's complete_step of step 1 (commit %s, its implement handoff version %d under its session %s): %d %s; %s stands in retro",
		l.number, l.key, stepRecordsHead[:12], version, owner.session, status, strings.TrimSpace(body), l.key)

	if told, err = l.ownerTold(since, 2, "", phase.Retro, "retro", 0); err != nil {
		t.Fatal(err)
	}
	l.note("the owner was told a second owner-step (role none, phase %s), and no claim of %s was launched; its task: %q", told.Phase, l.key, told.Reason)
	status, body = l.rateRetro()
	if status != http.StatusOK {
		t.Fatalf("the owner's complete_step of the retro of %s = %d %s, want 200", l.key, status, body)
	}
	l.waitPhase(phase.Merging)
	l.awaitLaunch(l.token(claim.RoleMerger))
	l.note("the owner's complete_step of the retro (step 0, rating %d %q): %d %s; %s stands in merging, and the merger %s was launched",
		deploymentRating, deploymentReason, status, strings.TrimSpace(body), l.key, l.token(claim.RoleMerger))
}

// freshImplementer is control-owner-step: the third issue's owner gives `implementer`, whose step
// is a fresh implementer's, and the check of owner-step over it must fail: the daemon launches the
// implementer, and the owner is told no owner-step.
func (l *ownerDoesStepLive) freshImplementer() {
	t := l.t
	t.Helper()
	since := l.given(ownerDoesStepFresh, phase.Implementing)
	_, err := l.ownerTold(since, 1, claim.RoleImplementer, phase.Implementing, "step", 1, ownerDoesStepText)
	if launch := string(l.token(claim.RoleImplementer)); err == nil || !strings.Contains(err.Error(), launch) || len(l.ownerNotices(record.OwnerStepNotice)) != 0 {
		t.Fatalf("control: the check of owner-step over %s, whose step is a fresh implementer's, says %v; want it to name the launch of %s and no owner-step", l.key, err, launch)
	}
	l.note("control: FAIL owner-step over %s, plan `implementer`: %v", l.key, err)
}

// testerAlone is tester-alone: the second issue's owner gives `tester`; the tester is prompted with
// its step and the text of its task's block, and its pass starts the retro, which is the owner's,
// told as one notice owner-step with no claim of the issue launched for it. The check of the task's
// end (endedAlone) must fail while the retro runs; the owner's report of the retro with a rating
// ends the issue, and the check then holds.
func (l *ownerDoesStepLive) testerAlone() {
	t := l.t
	t.Helper()
	l.given(ownerDoesStepTester, phase.Testing)
	tester := l.worker(claim.RoleTester)
	prompt := tester.take()
	if !strings.Contains(prompt.Message, ownerDoesStepText) {
		t.Fatalf("the tester of %s was prompted with %q, want it to carry its task block's text %q", l.key, prompt.Message, ownerDoesStepText)
	}
	l.note("the tester %s was prompted with the text of its task's block, %q", tester.token, ownerDoesStepText)
	since := l.launched()
	l.finish(tester, "test", testCompletion, phase.Retro)
	told, err := l.ownerTold(since, 1, "", phase.Retro, "retro", 0)
	if err != nil {
		t.Fatal(err)
	}
	l.note("the tester passed, and %s stands in retro; the owner %s was told one owner-step (role none, phase %s), and no claim of %s was launched; its task: %q",
		l.key, l.token(claim.RoleArchitect), told.Phase, l.key, told.Reason)

	if _, err := l.endedAlone(); err == nil {
		t.Fatalf("control: the check of tester-alone over %s, whose retro runs, says %v; want it to say that %s stands in retro", l.key, err, l.key)
	} else {
		l.note("control: FAIL tester-alone over %s before its retro ends: %v", l.key, err)
	}

	status, body := l.rateRetro()
	if status != http.StatusOK {
		t.Fatalf("the owner's complete_step of the retro of %s = %d %s, want 200", l.key, status, body)
	}
	l.note("the owner's complete_step of the retro (step 0, rating %d %q): %d %s", deploymentRating, deploymentReason, status, strings.TrimSpace(body))
	read, err := l.endedAlone()
	if err != nil {
		t.Fatal(err)
	}
	l.note("%s stands in done, and no merger was launched within %s", l.key, liveSettle)
	l.note("`dispatch steps --issue %s` as the person, exit 0:", l.key)
	for _, line := range strings.Split(strings.TrimSpace(read.steps), "\n") {
		l.note("  %s", line)
	}
	l.note("GET /api/v1/issues/%s/steps, the read `dispatch steps` makes: the retro names rating %d and the reason %q", l.key, deploymentRating, deploymentReason)
	l.note("`dispatch plans --project %s` as the person, exit 0, lists: %s", l.cfg.Project, read.plan)
}

// mergesWithItsPullRequest is control-tester-alone: the check of tester-alone over the first
// issue, whose task has a pull request and whose retro has moved it to merging, must fail on its
// merger's launch.
func (l *ownerDoesStepLive) mergesWithItsPullRequest() {
	t := l.t
	t.Helper()
	_, err := l.endedAlone()
	if err == nil {
		t.Fatalf("control: the check of tester-alone over %s, which has a pull request, says %v; want it to name the merger's launch", l.key, err)
	}
	l.note("control: FAIL tester-alone over %s, whose task has a pull request: %v", l.key, err)
}

// ownerTold is the check of owner-step at the step the walk's issue stands at: the owner, the
// issue's own architect, was told told notices owner-step on its role topic, the last naming role,
// the phase at and the run of kind and place, its task carrying each text of planted, which the
// plan itself gives, and the daemon launched no claim of the issue after the move into the step,
// since being the count of its launches before that move (launched): an owner's step starts no
// claim, neither a fresh one nor the owner's own relaunched as the step's role. It waits until the
// topic holds told such notices or such a launch is made, then watches liveSettle for the launch,
// or a notice more, which it must not see. It answers the last notice, or what does not hold.
func (l *ownerDoesStepLive) ownerTold(since, told int, role claim.Role, at phase.Phase, kind string, place int, planted ...string) (record.Notice, error) {
	l.t.Helper()
	fresh := func() []string {
		var launches []string
		for _, call := range l.launchesOf(l.key)[since:] {
			launches = append(launches, fmt.Sprintf("%s as %s (%s)", call.Spec.Claim, call.Spec.Role, call.Method))
		}
		return launches
	}
	testwait.Holds(stepRecordsWait, 100*time.Millisecond, func() bool { return len(l.ownerNotices(record.OwnerStepNotice)) >= told || len(fresh()) != 0 })
	testwait.Holds(liveSettle, 100*time.Millisecond, func() bool { return len(l.ownerNotices(record.OwnerStepNotice)) > told || len(fresh()) != 0 })
	notices, launches := l.ownerNotices(record.OwnerStepNotice), fresh()
	owner := l.token(claim.RoleArchitect)
	switch {
	case len(launches) != 0:
		return record.Notice{}, fmt.Errorf("%s launched %v for its step in %s, and its owner %s was told %d notices owner-step %+v; want the owner told its step and no claim launched for it",
			l.key, launches, at, owner, len(notices), notices)
	case len(notices) != told:
		return record.Notice{}, fmt.Errorf("the owner %s of %s was told %d notices owner-step %+v, want %d", owner, l.key, len(notices), notices, told)
	}
	last := notices[told-1]
	if last.Role != role || last.Phase != at {
		return last, fmt.Errorf("the owner %s of %s was told owner-step of role %q in %s, want role %q in %s", owner, l.key, last.Role, last.Phase, role, at)
	}
	if step := last.OwnerStep; step == nil || step.Task != "a" || step.Kind != kind || step.Place != place {
		return last, fmt.Errorf("the owner-step told to %s of %s names the run %+v, want task a's %s %d", owner, l.key, last.OwnerStep, kind, place)
	}
	for _, text := range planted {
		if !strings.Contains(last.Reason, text) {
			return last, fmt.Errorf("the owner-step told to %s of %s says %q, want it to carry %q", owner, l.key, last.Reason, text)
		}
	}
	return last, nil
}

// ownerDoesStepEnd is what the check of tester-alone read once it held: what `dispatch steps
// --issue <KEY>` printed, and the issue's line of `dispatch plans --project <PROJECT>`.
type ownerDoesStepEnd struct {
	steps, plan string
}

// endedAlone is the check of tester-alone over the walk's issue, a task with no pull request whose
// retro has ended: the daemon launched no merger of the issue, which it watches liveSettle for; the
// issue stands in done; `dispatch steps --issue <KEY>`, run as the person, prints task a's step 1
// tester, by the tester's claim, and its retro, by the owner, each ended passed, and no other run;
// the retro's record, as GET /api/v1/issues/{key}/steps answers the read that command makes, names
// the rating deploymentRating and deploymentReason; and `dispatch plans --project <PROJECT>`, run as
// the person, lists the issue closed with the owner's plan of one task, `tester`, that one rating
// and no failed step. The reads ride the outbox, so they are read again until they hold or
// stepRecordsWait passes. It answers what it read, or the first of these that does not hold.
func (l *ownerDoesStepLive) endedAlone() (ownerDoesStepEnd, error) {
	l.t.Helper()
	merger := l.token(claim.RoleMerger)
	testwait.Holds(liveSettle, 100*time.Millisecond, func() bool { return len(l.launchesOfRole(claim.RoleMerger)) != 0 })
	if launches := l.launchesOfRole(claim.RoleMerger); len(launches) != 0 {
		return ownerDoesStepEnd{}, fmt.Errorf("%s launched its merger %s %v, and stands in %s; want no merge of a task with no pull request",
			l.key, merger, launches, l.d.state().Issues[l.key].Phase)
	}
	if stands := string(l.d.state().Issues[l.key].Phase); stands != string(phase.Done) {
		return ownerDoesStepEnd{}, fmt.Errorf("%s stands in %s, want done", l.key, stands)
	}
	want := []stepWant{
		{kind: "step", step: 1, attempt: 1, role: "tester", typ: "testing", result: "passed"},
		{kind: "retro", attempt: 1, role: "implementer", result: "passed"},
	}
	agents := []claim.Token{l.token(claim.RoleTester), l.token(claim.RoleArchitect)}
	var read ownerDoesStepEnd
	var last error
	testwait.Holds(stepRecordsWait, time.Second, func() bool {
		read, last = l.readEnd(want, agents)
		return last == nil
	})
	return read, last
}

// readEnd is one read of endedAlone's three reads: `dispatch steps --issue <KEY>`, whose lines must
// be exactly the runs of want, each by the agent of agents at its place; GET
// /api/v1/issues/{key}/steps, whose retro names the rating; and `dispatch plans --project
// <PROJECT>`, whose line of the issue names the plan, the rating and no failed step.
func (l *ownerDoesStepLive) readEnd(want []stepWant, agents []claim.Token) (ownerDoesStepEnd, error) {
	l.t.Helper()
	var read ownerDoesStepEnd
	out, code := l.reads.steps()
	if code != 0 {
		return read, fmt.Errorf("`dispatch steps --issue %s` exited %d: %s", l.key, code, out)
	}
	read.steps = out
	if err := taskLinesHold(l.key, out, "a", want); err != nil {
		return read, fmt.Errorf("`dispatch steps --issue %s` printed\n%s\nand the read does not hold task a's walk: %w", l.key, out, err)
	}
	printed, err := parseSteps(l.key, out)
	if err != nil {
		return read, err
	}
	for place, w := range want {
		if got := findLine(printed.lines, w); got.agent != string(agents[place]) {
			return read, fmt.Errorf("%s prints %q: want the agent %s", lineName(w), got.text, agents[place])
		}
	}
	var steps deploymentStepsRead
	if err := l.personRead("/api/v1/issues/"+l.key+"/steps", &steps); err != nil {
		return read, err
	}
	if err := deploymentStepsHold(steps, "a", []deploymentStepWant{
		{kind: "step", step: 1, role: "tester", typ: "testing", model: stepRecordsModel},
		{kind: "retro", role: "implementer"},
	}, true); err != nil {
		return read, err
	}
	plans, code := l.reads.cli("plans", "--project", l.cfg.Project)
	if code != 0 {
		return read, fmt.Errorf("`dispatch plans --project %s` exited %d: %s", l.cfg.Project, code, plans)
	}
	lines, err := parsePlans(l.cfg.Project, plans)
	if err != nil {
		return read, err
	}
	at := slices.IndexFunc(lines, func(line planLine) bool { return line.issue == l.key })
	switch {
	case at < 0:
		return read, fmt.Errorf("`dispatch plans --project %s` printed\n%s\nwith no line of %s", l.cfg.Project, plans, l.key)
	case lines[at].plan != "owner (1 tasks: tester)":
		return read, fmt.Errorf("%s prints plan %s, want owner (1 tasks: tester)", l.key, lines[at].plan)
	case len(lines[at].ratings) != 1 || lines[at].ratings[0] != (printedRating{rating: deploymentRating, reason: deploymentReason}):
		return read, fmt.Errorf("%s prints the ratings %+v, want the one rating %d/5 %q", l.key, lines[at].ratings, deploymentRating, deploymentReason)
	case lines[at].failed != 0:
		return read, fmt.Errorf("%s prints failed steps %d, want 0", l.key, lines[at].failed)
	}
	read.plan = lines[at].text
	return read, nil
}
