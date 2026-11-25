// An owner's plan names its steps in any order, and the whole daemon walks them in that order. Each
// test boots the whole daemon over the real Dispatch of this package's tests (ownerStepsRig,
// owner_steps_rig_test.go, or the store the release before leaves, inFlightRig,
// plan_in_flight_test.go), and every fact of GitHub reaches it through its own reads and intake.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A plan in another order than the default steps' runs to done: the issue whose plan is
// `implementer reviewer tester` launches its implementer, its reviewer and its tester in that order.
// The tester's step fails on its check's state at the head it tested and its completion through
// the completion route, the tester's row keeping the handoff record and that head, the tester's
// notice of the move back naming that head and the check as fields, and the implementer takes the
// task again in the session it registered, with no launch of its own, the task carrying the reason
// that notice gives and the version of the handoff record the tester's row holds. The tester's
// session writes its report three times before the record its completion names, so that record
// is version 4, a number the implementer's task carries only where it names the record. The
// reviewer and the tester then run again, the retro rates the plan, and the merge ends the walk at
// done. The control is a second issue given the same steps as `implementer tester reviewer`: it
// launches its tester before its reviewer, and the same check of the order must fail.
func TestAPlanInAnyOrderRunsToDone(t *testing.T) {
	t.Parallel()
	r := newOwnerStepsRig(t, newLiveProofOn(t, workflowNATS(t)), startScratchDispatchOn(t, scratchDispatchOptions{}), 2, config.Project{})
	walked, control := r.issues[0].key, r.issues[1].key
	order := []claim.Role{claim.RoleImplementer, claim.RoleReviewer, claim.RoleTester}
	r.givePlan("implementer", "reviewer", "tester")
	r.openGate(phase.Implementing)
	r.runStep("implementer", answerPass, phase.Reviewing)
	r.runStep("reviewer", answerPass, phase.Testing)
	tester := r.worker(claim.RoleTester)
	for range 3 {
		r.writeHandoff(tester.session, "test")
	}
	r.runStep("tester", answerFail, phase.Implementing)
	r.implementAfterTheTestersFail()
	r.runStep("reviewer", answerPass, phase.Testing)
	r.runStep("tester", answerPass, phase.Retro)
	r.retro()
	r.merge()
	if err := r.launchedInOrder(order); err != nil {
		t.Fatal(err)
	}

	r.on(control)
	r.givePlan("implementer", "tester", "reviewer")
	r.openGate(phase.Implementing)
	r.walkSteps([]string{"implementer", "tester", "reviewer"})
	err := r.launchedInOrder(order)
	if want := "in the order implementer, tester, reviewer, merger"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("control: the check of the order of %s's launches, whose plan is `implementer tester reviewer`, says %v, want it to say it launched them %s", control, err, want)
	}
	t.Logf("%s walked `implementer reviewer tester` to done; control: %v", walked, err)
}

// implementAfterTheTestersFail is the walk's implementer taking the task the tester's failed step
// returned to it: the tester's row holds its handoff record and the head it tested, and the
// tester's one phase-finished notice of the move back, as the architect's topic received it, names
// as fields that head and k1, the check that does not pass there, with its state and reason. The
// claim that implemented the task takes the task in the session it registered, with no launch
// after its first, and the task it takes carries the reason that notice gives and the version of
// the handoff record the tester's row holds: the reason is read from the notice and the version
// from the row, each found in the task as a value, and no word of the task is compared with a
// literal. The implementer's first task, taken before the tester wrote anything, and the reason
// carry no such version, nor is it the pull request's number, so a task that names no record of
// the tester's fails here. It implements again, and the task moves on to its reviewer.
func (r *ownerStepsRig) implementAfterTheTestersFail() {
	t := r.t
	t.Helper()
	tested := r.phaseOf(claim.RoleTester)
	if tested.HandoffRecord == "" || tested.HandoffCommit != stepRecordsHead {
		t.Fatalf("%s's tester row holds the handoff record %q and the head %q after its step failed, want a record and %s: this check proves nothing",
			r.key, tested.HandoffRecord, tested.HandoffCommit, stepRecordsHead)
	}
	var back []record.Notice
	testwait.Eventually(t, r.key+"'s tester's phase-finished notice of the move back on its architect's topic", func() bool {
		back = nil
		for _, notice := range r.testerNotices(r.key) {
			if notice.WaitsFor == "" {
				back = append(back, notice)
			}
		}
		return len(back) > 0
	})
	if want := []record.FailingCheck{{Check: "k1", State: "no", Reason: "fail"}}; len(back) != 1 || back[0].Phase != phase.Testing ||
		back[0].Head != stepRecordsHead || !slices.Equal(back[0].Failing, want) || len(back[0].Waiting) != 0 {
		t.Fatalf("%s's tester's phase-finished notices of the move back = %+v, want one with the head %s and failing %+v", r.key, back, stepRecordsHead, want)
	}
	implementer := r.worker(claim.RoleImplementer)
	task := implementer.take().Message
	if back[0].Reason == "" || !strings.Contains(task, back[0].Reason) {
		t.Fatalf("%s's implementer took the task %q, which does not carry the reason %q its tester's notice of the move back gives", r.key, task, back[0].Reason)
	}
	taken, version, pr := implementer.tasksTaken(), tested.HandoffVersion, r.pullRequest()
	if len(taken) < 2 || carriesNumber(taken[0], version) || carriesNumber(back[0].Reason, version) || (pr != nil && pr.Number == version) {
		t.Fatalf("%s's tester's row holds its record at version %d, which the implementer's first task %q, the reason %q or the pull request %+v carries already: this check proves nothing",
			r.key, version, taken[0], back[0].Reason, pr)
	}
	if !carriesNumber(task, version) {
		t.Fatalf("%s's implementer took the task %q, which does not carry %d, the version of the handoff record %q its tester's row holds", r.key, task, version, tested.HandoffRecord)
	}
	if launches := r.launchesOfRole(claim.RoleImplementer); len(launches) != 1 {
		t.Fatalf("%s's implementer was launched %v by the time it took the task again, want its first launch alone: it starts again in its session", r.key, launches)
	}
	if got := r.d.claim(implementer.token).Session; got != implementer.session {
		t.Fatalf("%s's implementer holds the session %q when it takes the task again, want %q, the one it implemented in", r.key, got, implementer.session)
	}
	r.finish(implementer, "implement", api.HandoffCompleteRequest{Summary: "implemented", Commit: stepRecordsHead}, phase.Reviewing)
}

// carriesNumber says whether s carries n as a number of its own: a run of digits that spells n
// with no letter or digit beside it, so the 4 of `#42`, of `k4`, of a commit or of an issue key's
// prefix is not n.
func carriesNumber(s string, n int) bool {
	return slices.Contains(strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }), strconv.Itoa(n))
}

// launchedInOrder says whether the daemon first launched the workers of the walk's issue in the
// order of steps, then its merger: each role's first launch on the issue, the architect's left
// out. The error names the order of the launches.
func (r *ownerStepsRig) launchedInOrder(steps []claim.Role) error {
	r.t.Helper()
	want := append(slices.Clone(steps), claim.RoleMerger)
	var got []claim.Role
	for _, launch := range r.launchesOf(r.key) {
		if role := launch.Spec.Role; role != claim.RoleArchitect && !slices.Contains(got, role) {
			got = append(got, role)
		}
	}
	if !slices.Equal(got, want) {
		return fmt.Errorf("%s's workers were first launched in the order %s, want %s", r.key, joinRoles(got), joinRoles(want))
	}
	return nil
}

// joinRoles is roles as a list, `a, b, c`.
func joinRoles(roles []claim.Role) string {
	names := make([]string, len(roles))
	for i, role := range roles {
		names[i] = string(role)
	}
	return strings.Join(names, ", ")
}

// A step that reads the implementer's pull request waits for an implementer step before it:
// register_plan of `tester implementer` is refused (testerFirstRefused). register_plan of
// `implementer planner` answers 200, and the issue walks it: no planner starts while the
// implementer works, GitHub lists the pull request from the implementer's completion on, the
// planner starts after that completion with the pull request recorded open, and the retro and the
// merge end the walk at done.
func TestAStepThatReadsAPullRequestWaitsForAnImplementer(t *testing.T) {
	t.Parallel()
	r := newOwnerStepsRig(t, newLiveProofOn(t, workflowNATS(t)), startScratchDispatchOn(t, scratchDispatchOptions{}), 1, config.Project{})
	r.testerFirstRefused()

	r.givePlan("implementer", "planner")
	r.openGate(phase.Implementing)
	implementer := r.worker(claim.RoleImplementer)
	implementer.take()
	if launches := r.launchesOfRole(claim.RolePlanner); len(launches) != 0 {
		t.Fatalf("%s's planner was launched %v while its implementer works, want none before the implementer's pull request opens", r.key, launches)
	}
	if pr := r.pullRequest(); pr != nil {
		t.Fatalf("%s records the pull request %+v while its implementer works, want none before GitHub lists one", r.key, *pr)
	}
	r.github.open("legion/"+r.key, r.number)
	r.finish(implementer, "implement", api.HandoffCompleteRequest{Summary: "implemented", Commit: stepRecordsHead}, phase.Planning)
	r.awaitLaunch(r.token(claim.RolePlanner))
	testwait.WithinPolling(t, stepRecordsWait, 100*time.Millisecond, r.key+"'s pull request recorded open while its planner starts", func() bool {
		pr := r.pullRequest()
		return pr != nil && pr.Number == r.number && pr.State == record.PullRequestOpen
	})
	r.runStep("planner", answerPass, phase.Retro)
	r.retro()
	r.merge()
}

// testerFirstReason is the one reason register_plan gives the plan `tester implementer`: its
// first step reads the pull request of an implementer no step before it names.
const testerFirstReason = "step 1 of task a names tester, and no step before it names the role implementer, whose pull request tester reads. This release of Legion does not carry it out yet."

// testerFirstRefused is the architect of the walk's issue giving the plan `tester implementer`
// (register_plan): it answers 409 PLAN_REFUSED with the one reason testerFirstReason for task a,
// and the issue holds no plan rows. It answers the refusal's body.
func (r *ownerStepsRig) testerFirstRefused() string {
	t := r.t
	t.Helper()
	status, body := r.registerPlan(r.document("tester-first.md", planOf("tester", "implementer")), 1)
	var refusal api.PlanRefusal
	if status == http.StatusConflict {
		if err := json.Unmarshal([]byte(body), &refusal); err != nil {
			t.Fatalf("decode the refusal of register_plan %q: %v", body, err)
		}
	}
	want := []api.PlanReason{{Task: "a", Reason: testerFirstReason}}
	if status != http.StatusConflict || refusal.Code != "PLAN_REFUSED" || !slices.Equal(refusal.Reasons, want) {
		t.Fatalf("register_plan of the plan `tester implementer` = %d %s, want 409 PLAN_REFUSED with the one reason %+v", status, body, want)
	}
	if rows := r.planRows(); rows != 0 {
		t.Fatalf("the refused plan left %d plan rows on %s, want none", rows, r.key)
	}
	return body
}

// pullRequest is the pull request the daemon's store records for the walk's issue, nil for none.
func (r *ownerStepsRig) pullRequest() *record.PullRequest {
	r.t.Helper()
	var pr *record.PullRequest
	if err := store.TransactWith(context.Background(), r.st.Pool(), pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		pr, err = r.records.PullRequest(ctx, tx, r.key)
		return err
	}); err != nil {
		r.t.Fatalf("read the pull request of %s: %v", r.key, err)
	}
	return pr
}

// releaseBeforeTaskText is the text of the one task of the plan the release before took.
const releaseBeforeTaskText = "Make the change the issue asks for."

// An issue of the release before goes on. The store holds every migration of the release of the
// owner's plan, which this release keeps, and an issue that the release before left in reviewing on
// the plan `implementer reviewer`, its rows written with plain SQL in that release's columns
// (seedPlanInReviewing): its task at step 2, the reviewer's, with the counts of one round that
// requested changes, and the claims of its architect and its reviewer each a launch that release
// began in the session it recorded. This release's daemon boots on it and resumes both. The
// reviewer's approval moves the issue into its retro, the step after the review on the plan's line
// `implementing reviewing retro merging`; the retro rates the plan 4, and the merge ends the walk at
// done. Each claim keeps its session, the task keeps its counts, and the read of the project on
// Dispatch lists the issue with the retro's rating and its reason. The control, every run, is a
// second issue seeded the same with its rows of issue_plans, plan_tasks and plan_steps deleted
// before the boot: it walks today's line, the approval moves it to accepting, and the same check of
// the move into the retro must fail on it.
func TestAnIssueOfTheReleaseBeforeGoesOn(t *testing.T) {
	t.Parallel()
	r := newReleaseBeforeRig(t)
	walked := r.walks[0]
	r.on(walked.key)
	if err := r.approvedIntoRetro(); err != nil {
		t.Fatal(err)
	}
	r.retro()
	r.merge()
	for role, kept := range walked.sessions {
		if got := r.d.claim(r.tokenOf(walked.key, role)).Session; got != kept.id {
			t.Fatalf("%s's %s holds the session %q at done, want %q, the one the release before recorded", walked.key, role, got, kept.id)
		}
	}
	var state, moves string
	var pass, rounds int
	if err := r.st.Pool().QueryRow(context.Background(), `select state, pass, rounds, backward_moves::text from plan_tasks where issue = $1 and task = 'a'`,
		walked.key).Scan(&state, &pass, &rounds, &moves); err != nil {
		t.Fatalf("read the task a of %s: %v", walked.key, err)
	}
	if state != string(record.TaskDone) || pass != 1 || rounds != 1 || moves != `{"reviewing-implementing": 1}` {
		t.Fatalf("%s's task a is %s with pass %d, rounds %d and the moves back %s at done, want done with the 1, 1 and %s the release before left",
			walked.key, state, pass, rounds, moves, `{"reviewing-implementing": 1}`)
	}
	r.waitPlansRead("the rating of "+walked.key+"'s retro", func(read projectPlansRead) error {
		for _, listed := range read.Issues {
			if listed.Issue != walked.key {
				continue
			}
			if len(listed.Ratings) != 1 || listed.Ratings[0].Rating != 4 || listed.Ratings[0].Reason != "The plan fit the work." {
				return fmt.Errorf("%s lists the ratings %+v, want the one rating 4 with the reason %q", walked.key, listed.Ratings, "The plan fit the work.")
			}
			return nil
		}
		return fmt.Errorf("the read lists %d finished issues and not %s", len(read.Issues), walked.key)
	})

	r.on(r.control.key)
	err := r.approvedIntoRetro()
	if want := "stands in accepting"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("control: the check of the move into the retro of %s, whose plan rows were deleted before the boot, says %v, want it to say it %s", r.control.key, err, want)
	}
	t.Logf("control: %v", err)
}

// approvedIntoRetro is the reviewer of the walk's issue taking its round in the session it was
// resumed in and a person approving the pull request, and says whether the approval moved the
// issue into its retro, the step after the review on the plan `implementer reviewer`. The error
// names where the issue stands instead.
func (r *inFlightRig) approvedIntoRetro() error {
	r.t.Helper()
	reviewer := r.worker(claim.RoleReviewer)
	reviewer.take()
	reviewer.endTurn()
	r.publishReview(r.number, "APPROVED")
	var stands string
	testwait.WithinPolling(r.t, stepRecordsWait, 100*time.Millisecond, r.key+" to leave reviewing at its reviewer's approval", func() bool {
		stands = string(r.d.state().Issues[r.key].Phase)
		return stands != "" && stands != string(phase.Reviewing)
	})
	if stands != string(phase.Retro) {
		return fmt.Errorf("%s stands in %s after its reviewer's approval, want retro, the step after the review on the plan implementer reviewer", r.key, stands)
	}
	return nil
}

// newReleaseBeforeRig is the store the release before leaves, every migration applied, seeded with
// the walk's issue and the control in reviewing on the plan `implementer reviewer`
// (seedPlanInReviewing), the control's plan rows deleted, with the daemon booted on it at the real
// Dispatch of this package's tests, and each issue's recorded claims resumed in their sessions.
func newReleaseBeforeRig(t *testing.T) *inFlightRig {
	t.Helper()
	proof := newLiveProofOn(t, workflowNATS(t))
	proof.cfg.Project = "RB" + randomSuffix(t)[:6]
	proof.cfg.Projects = map[string]config.Project{proof.cfg.Project: {}}
	proof.cfg.AdmissionCap = 16
	var err error
	if proof.project, err = claim.ProjectToken(proof.cfg.Project); err != nil {
		t.Fatal(err)
	}
	number, err := strconv.ParseInt(randomSuffix(t)[:6], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	r := &inFlightRig{stepRecordsRig: &stepRecordsRig{liveProof: proof, dispatch: startScratchDispatchOn(t, scratchDispatchOptions{}),
		github: newStepRecordsGitHub(t), claims: map[claim.Token]*stepClaimant{}, waits: make(chan claim.Token, 16)}}
	if err := os.WriteFile(r.cfg.DispatchTokenFile, []byte(r.dispatch.token+"\n"), 0o600); err != nil {
		t.Fatalf("write the Dispatch token file: %v", err)
	}
	r.cfg.DispatchURL = r.dispatch.base
	r.person(http.MethodPost, "/api/v1/projects", map[string]string{"key": r.cfg.Project, "name": "Release before"}, http.StatusCreated, nil)
	r.person(http.MethodPut, "/api/v1/projects/"+r.cfg.Project+"/default-repository", map[string]any{"repo": liveRepo}, http.StatusOK, nil)
	status := workflow.PhaseStatus(phase.Reviewing)
	// Two titles of the in-flight run, which Dispatch takes as two issues of no near-copy title.
	for place, title := range []string{inFlightTitles[3], inFlightTitles[7]} {
		var created createdIssue
		r.person(http.MethodPost, "/api/v1/issues", map[string]string{"project": r.cfg.Project, "title": title}, http.StatusCreated, &created)
		r.person(http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{"status": status, "labels": []string{"legion"}}, http.StatusOK, &created)
		issue := &inFlightIssue{walkIssue: walkIssue{key: created.Key, title: title, number: int(number) + 1 + place},
			stands: phase.Reviewing, sessions: map[claim.Role]inFlightSession{}}
		r.issues = append(r.issues, issue.walkIssue)
		r.github.open("legion/"+issue.key, issue.number)
		r.seedInFlight(issue, phase.Reviewing, status, created)
		r.seedPlanInReviewing(issue)
		r.walks = append(r.walks, issue)
	}
	r.control, r.walks = r.walks[1], r.walks[:1]
	for _, table := range []string{"plan_steps", "plan_tasks", "issue_plans"} {
		r.exec("delete from "+table+" where issue = $1", r.control.key)
	}
	// Dispatch holds no check of these issues: their heads' sets are empty, so a task's end waits
	// for no CI (noChecksAt).
	for _, issue := range r.issues {
		noChecksAt(t, r.st.Pool(), issue.key, liveRepo, stepRecordsHead)
	}
	r.boot()
	for _, issue := range []*inFlightIssue{r.walks[0], r.control} {
		r.on(issue.key)
		for role := range issue.sessions {
			r.resume(issue, role)
		}
	}
	return r
}

// seedPlanInReviewing writes, with plain SQL in the columns of the release of the owner's plan,
// what that release adds to an issue seedInFlight left in reviewing once its architect gave the
// plan `implementer reviewer` at version 1: the plan's rows, its task a running at step 2 with the
// counts of one round that requested changes (pass 1, rounds 1, one move back from reviewing to
// implementing), which that release keeps on the task and not on the implementer's phases row,
// each step with the type of its role, the step each phases row ran, and the claims' plugin
// contract, the owner's plan's.
func (r *inFlightRig) seedPlanInReviewing(issue *inFlightIssue) {
	r.t.Helper()
	key := issue.key
	r.exec(`insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at)
		values ($1, 1, 1, 'owner', $2, $3, now() - interval '4 hours')`, key, "plan-"+key, string(r.tokenOf(key, claim.RoleArchitect)))
	r.exec(`insert into plan_tasks (issue, generation, task, plan_version, block_text, state, running_kind, position, pass, rounds, backward_moves)
		values ($1, 1, 'a', 1, $2, 'running', 'step', 2, 1, 1, '{"reviewing-implementing": 1}')`, key, releaseBeforeTaskText)
	r.exec(`insert into plan_steps (issue, generation, task, position, role, type)
		values ($1, 1, 'a', 1, 'implementer', 'implementation'), ($1, 1, 'a', 2, 'reviewer', 'review')`, key)
	r.exec(`update phases set step_task = 'a', step_place = case role when 'implementer' then 1 else 2 end, pass = 0, rounds = 0
		where issue = $1`, key)
	r.exec(`update claims set plugin_contract = $2 where issue = $1`, key, api.OwnerPlanContract)
}
