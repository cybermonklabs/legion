// The whole daemon walks each issue of a project through its own steps when the settings define a
// role of the deployment's own, and an issue that the release before this one left mid-walk goes on
// under this release.
//
// Each test boots the whole daemon (startDaemonWithin) over the package's broker, the real store,
// the real outbox and the real Dispatch of this package's tests (startScratchDispatchOn), and walks
// each issue through the transports production has, as the step records' walks do (stepRecordsRig,
// step_records_test.go): the owner of an issue, the architect of its tree, gives its plan with
// register_plan before its gate opens, each role's agent works over the worker stream and the
// daemon's routes, a person's review and the merge reach the daemon as GitHub's envelopes on the
// stream, and the architect signs off through its own route. The settings' roles are checker, of
// the type testing, with a prompt file of its own and the model checker-model, and prober, of the
// same type, which names no model (deploymentRoles). What is stood in for lies outside the walk:
// GitHub's REST API (stepRecordsGitHub), and the panes, which the fake runtime launches and the
// test plays, each answering with the model its launch's spec named (stepClaimant.answer).
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const (
	// deploymentChecker is the role of the type testing the settings define, with a prompt file
	// and a model, and deploymentProber the one of the same type that names no model.
	deploymentChecker claim.Role = "checker"
	deploymentProber  claim.Role = "prober"
	// deploymentCheckerModel is the model the settings give checker, which its launch's spec names
	// and its agent answers with.
	deploymentCheckerModel = "checker-model"
	// deploymentCheckerPrompt is the text of checker's prompt file, which its launch's prompt holds.
	deploymentCheckerPrompt = "You are the checker of this deployment: run the issue's acceptance checks against the pull request's head and report pass or fail."
	// deploymentTaskText is the text of the one task of each plan an owner gives here, which every
	// task the walk sends a step of that plan holds.
	deploymentTaskText = "Make the change the issue asks for, and leave it ready for its checks."
	// deploymentRating and deploymentReason are the rating each retro here gives the plan, and its
	// reason (stepRecordsRig.retro).
	deploymentRating = 4
	deploymentReason = "The plan fit the work."
)

// v-steps, whole. The settings define checker (type testing, a prompt file, the model
// checker-model). Issue A gives no plan and walks the seven phases. Issue B gives `implementer
// checker`: it launches the implementer, then the checker, and no planner and no tester; the
// checker's spawn spec names checker-model and the prompt file's text; its task names B, holds the
// task's text and ` Step 1 (implementer) reported:`; after its pass Legion runs the retro, which
// rates the plan, and merges; the read `dispatch steps --issue B` makes names step 2 checker, type
// testing, model checker-model (the model that answered), and the retro with its rating and
// reason. Issue C gives `implementer reviewer tester`: the reviewer runs before the tester.
//
// Controls: (1) a role with no model (prober) has its step record name stand-in/model, the model
// its agent answered with for a launch that named none; (2) an issue given B's plan whose plan
// rows are deleted before its gate opens starts a planner, and the walk's assertion fails on it;
// (3) with checker taken out of the settings, register_plan of B's plan is refused naming checker.
func TestEachIssueWalksItsOwnStepsWithADeploymentRole(t *testing.T) {
	t.Parallel()
	roles := deploymentRoles(t)
	r := newDeploymentRolesRig(t, newLiveProofOn(t, workflowNATS(t)), startScratchDispatchOn(t, scratchDispatchOptions{}), roles,
		[]string{"Default walk lands its change", "Checker verifies widget output", "Reviewer precedes tester ordering",
			"Prober answers without model", "Deleted rows bring planner", "Refusal names absent role"})
	byDefault, checked, reordered, unmodelled, rowsDeleted, refused := r.issues[0].key, r.issues[1].key, r.issues[2].key,
		r.issues[3].key, r.issues[4].key, r.issues[5].key

	r.on(byDefault)
	if first := r.openGate(); first != phase.Planning {
		t.Fatalf("%s, given no plan, opened its gate at %s, want planning", byDefault, first)
	}
	r.plan()
	r.implement()
	r.test()
	r.review("APPROVED", phase.Accepting)
	r.accept()
	r.retro()
	r.merge()
	r.waitSteps("the seven records of "+byDefault+", given no plan", func(got stepRecordsRead) error { return stepsHold(got, stepRecordsSeven) })

	r.on(checked)
	r.givePlan("implementer", "checker")
	if err := r.walkImplementerChecker(); err != nil {
		t.Fatal(err)
	}
	waitDeploymentSteps(r.stepRecordsRig, "a", true, []deploymentStepWant{
		{kind: "step", step: 1, role: "implementer", typ: "implementation", model: stepRecordsModel},
		{kind: "step", step: 2, role: "checker", typ: "testing", model: deploymentCheckerModel},
		{kind: "retro", role: "implementer"},
		{kind: "merge", role: "merger"},
	})

	r.on(reordered)
	r.givePlan("implementer", "reviewer", "tester")
	if first := r.openGate(); first != phase.Implementing {
		t.Fatalf("%s, given `implementer reviewer tester`, opened its gate at %s, want implementing", reordered, first)
	}
	r.work(claim.RoleImplementer, "implement", api.HandoffCompleteRequest{Summary: "implemented", Commit: stepRecordsHead}, phase.Reviewing)
	r.review("APPROVED", phase.Testing)
	r.work(claim.RoleTester, "test", testCompletion, phase.Retro)
	r.retro()
	r.merge()
	waitDeploymentSteps(r.stepRecordsRig, "a", true, []deploymentStepWant{
		{kind: "step", step: 1, role: "implementer", typ: "implementation", model: stepRecordsModel},
		{kind: "step", step: 2, role: "reviewer", typ: "review", model: stepRecordsModel},
		{kind: "step", step: 3, role: "tester", typ: "testing", model: stepRecordsModel},
		{kind: "retro", role: "implementer"},
		{kind: "merge", role: "merger"},
	})

	t.Run("control 1: a role with no model", func(t *testing.T) {
		r.on(unmodelled)
		r.givePlan("implementer", "prober")
		if first := r.openGate(); first != phase.Implementing {
			t.Fatalf("%s, given `implementer prober`, opened its gate at %s, want implementing", unmodelled, first)
		}
		r.work(claim.RoleImplementer, "implement", api.HandoffCompleteRequest{Summary: "implemented", Commit: stepRecordsHead}, phase.Step)
		prober := r.worker(deploymentProber)
		if model := lastLaunch(t, r.rt, prober.token).Model; model != "" {
			t.Fatalf("the prober's spawn spec names the model %q, want none: the settings give prober none", model)
		}
		prober.take()
		r.finish(prober, "", api.HandoffCompleteRequest{Summary: "every probe holds", Commit: stepRecordsHead, Verdict: "pass"}, phase.Retro)
		r.retro()
		r.merge()
		waitDeploymentSteps(r.stepRecordsRig, "a", true, []deploymentStepWant{
			{kind: "step", step: 1, role: "implementer", typ: "implementation", model: stepRecordsModel},
			{kind: "step", step: 2, role: "prober", typ: "testing", model: stepRecordsModel},
			{kind: "retro", role: "implementer"},
			{kind: "merge", role: "merger"},
		})
	})

	t.Run("control 2: the plan rows deleted before the gate opens", func(t *testing.T) {
		r.on(rowsDeleted)
		r.givePlan("implementer", "checker")
		r.deletePlanRows()
		err := r.walkImplementerChecker()
		if want := rowsDeleted + "'s gate opened and its first step is planning"; err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("the walk's assertion on %s, whose plan rows were deleted before its gate opened, says %v; want it to say %q", rowsDeleted, err, want)
		}
		// The planner's start is the outbox's, which runs after the move to planning commits.
		r.awaitLaunch(r.token(claim.RolePlanner))
	})

	t.Run("control 3: checker taken out of the settings", func(t *testing.T) {
		withoutChecker := slices.DeleteFunc(slices.Clone(roles), func(role config.Role) bool { return role.Name == deploymentChecker })
		r.restartWith(withoutChecker, rowsDeleted, refused)
		r.on(refused)
		status, body := r.registerPlan(r.document("plan.md", deploymentPlanOf("implementer", "checker")), 1)
		if status != http.StatusConflict || !strings.Contains(body, "PLAN_REFUSED") || !strings.Contains(body, "checker") {
			t.Fatalf("register_plan of `implementer checker` with checker out of the settings = %d %s, want 409 PLAN_REFUSED naming checker", status, body)
		}
	})
}

// walkImplementerChecker is the walk of B's plan, `implementer checker`, on the walk's issue: its
// gate opens at implementing, the implementer works its step, the checker launches with the model
// and the prompt the settings give it, its task names the issue, the task's text and the
// implementer's report, and its pass runs the retro and the merge to done; no planner and no tester
// is launched. The error names the first thing that does not hold.
func (r *deploymentRolesRig) walkImplementerChecker() error {
	r.t.Helper()
	if first := r.openGate(); first != phase.Implementing {
		return fmt.Errorf("%s's gate opened and its first step is %s, want implementing, the first step of its plan", r.key, first)
	}
	r.work(claim.RoleImplementer, "implement", api.HandoffCompleteRequest{Summary: "implemented", Commit: stepRecordsHead}, phase.Step)
	checker := r.worker(deploymentChecker)
	spec := lastLaunch(r.t, r.rt, checker.token)
	if spec.Model != deploymentCheckerModel {
		return fmt.Errorf("the checker's spawn spec names the model %q, want %s", spec.Model, deploymentCheckerModel)
	}
	prompt, err := runtime.ComposePrompt(spec.Prompt)
	if err != nil {
		return fmt.Errorf("compose the checker's launch prompt: %w", err)
	}
	if !strings.Contains(prompt, deploymentCheckerPrompt) {
		return fmt.Errorf("the checker's launch prompt lacks its prompt file's text %q:\n%s", deploymentCheckerPrompt, prompt)
	}
	task := takeTask(checker)
	for _, want := range []string{r.key, deploymentTaskText, " Step 1 (implementer) reported:"} {
		if !strings.Contains(task, want) {
			return fmt.Errorf("the checker's task lacks %q:\n%s", want, task)
		}
	}
	r.finish(checker, "", api.HandoffCompleteRequest{Summary: "every check holds", Commit: stepRecordsHead, Verdict: "pass"}, phase.Retro)
	r.retro()
	r.merge()
	for _, role := range []claim.Role{claim.RolePlanner, claim.RoleTester} {
		if launches := r.launches(role); len(launches) != 0 {
			return fmt.Errorf("%s's %s was launched %v on the plan `implementer checker`, want never", r.key, role, launches)
		}
	}
	return nil
}

// An issue the release before this one left at testing goes on under this release. The store holds
// every migration of the release before, every one below this release's deployment_roles, applied
// by hand (storeOfTheEarlierRelease), and the record of one issue at testing, written with plain SQL
// in the columns of the release before: its task a at step 2 of the owner's plan `implementer
// tester reviewer`, the implementer's step done and the tester's claim a launch the earlier daemon
// began (seedEarlierReleaseIssue). This release's migrations then run on it, as a deploy runs them,
// and the whole daemon boots on it. The tester's pass moves the task to step 3 at reviewing, the
// issue goes on to done, its retro rates the plan 4 with its reason, and the read of its steps names
// the retro with that rating and that reason. The way back, the transaction in the header of the
// deployment_roles migration as an operator copies it out, then succeeds on that store with the
// daemon stopped.
//
// The fault: this release's walk keys its rows by place (Walk.row matches row.FromPlace ==
// w.Place), so an issue whose place the release before stored is not read would stop at its next
// report. The control is a mutation of the release's code, run by hand: Engine.walk answering the
// zero place in place of plan_tasks.position, as a store that lost the place the release before
// stored would answer; the tester's pass then moves nothing, and the assertion of step 3 fails.
func TestAnIssueOfTheEarlierReleaseGoesOnUnderThisRelease(t *testing.T) {
	t.Parallel()
	r := newEarlierReleaseRig(t)
	r.work(claim.RoleTester, "test", testCompletion, phase.Reviewing)
	if kind, position := r.taskPlace(); kind != "step" || position != 3 {
		t.Fatalf("after the tester's pass %s's task a stands at %s %d, want step 3", r.key, kind, position)
	}
	r.review("APPROVED", phase.Retro)
	r.retro()
	r.merge()
	waitDeploymentSteps(r.stepRecordsRig, "a", false, []deploymentStepWant{
		{kind: "step", step: 2, role: "tester", typ: "testing", model: stepRecordsModel},
		{kind: "step", step: 3, role: "reviewer", typ: "review", model: stepRecordsModel},
		{kind: "retro", role: "implementer"},
		{kind: "merge", role: "merger"},
	})

	r.d.stop()
	if err := runSQL(t, r.cfg.PostgresDSN, wayBackOf(t, deploymentRolesMigration)); err != nil {
		t.Fatalf("the way back of the deployment's roles on the store whose issue of the release before ran to done: %v", err)
	}
	var recorded int
	if err := r.st.Pool().QueryRow(context.Background(), `select count(*) from schema_version where version = $1`,
		r.migrationVersion).Scan(&recorded); err != nil {
		t.Fatalf("read the version row of the deployment's roles after the way back: %v", err)
	}
	if recorded != 0 {
		t.Fatalf("after the way back the store records the version %d %d times, want none", r.migrationVersion, recorded)
	}
}

// deploymentRoles is the settings' roles of these tests, each with a prompt file of its own under
// a directory of the test's: checker, of the type testing, with the model checker-model, and
// prober, of the same type, with none. Each works as the review App, the App the settings give a
// role that names none.
func deploymentRoles(t *testing.T) []config.Role {
	t.Helper()
	dir := t.TempDir()
	write := func(name, text string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	return []config.Role{
		{Name: deploymentChecker, Type: "testing", PromptFile: write("checker.md", deploymentCheckerPrompt), Model: deploymentCheckerModel, GitHubApp: config.RoleAppReview},
		{Name: deploymentProber, Type: "testing", PromptFile: write("prober.md", "You are the prober of this deployment: probe the change and report pass or fail."), GitHubApp: config.RoleAppReview},
	}
}

// deploymentRolesRig is the walk's rig over issues that stand at admitted with no design gate, so
// nothing of an issue starts before its owner has given its plan (givePlan) and its gate opens
// (openGate), with the id of each issue's spec, its primary document, by issue.
type deploymentRolesRig struct {
	*stepRecordsRig
	specs map[string]string
}

// newDeploymentRolesRig sets up dispatch, a running Dispatch, with the run's project, whose
// settings define roles, its repository mapping and one issue of each title, each created with a
// spec; seeds the daemon's store of proof with each issue at admitted with no design gate, on an
// open pull request of its own GitHub reports clean (seedAdmitted); boots the daemon at Dispatch;
// and registers each tree's architect. The walk's steps act on the first issue until on moves them.
func newDeploymentRolesRig(t *testing.T, proof liveProof, dispatch *scratchDispatch, roles []config.Role, titles []string) *deploymentRolesRig {
	t.Helper()
	// A Dispatch project key is at most ten characters, and the daemon's project is its Dispatch
	// project, so the run's project is a short one of its own.
	proof.cfg.Project = "DR" + randomSuffix(t)[:6]
	proof.cfg.Projects = map[string]config.Project{proof.cfg.Project: {}}
	proof.cfg.AdmissionCap = len(titles) + 2
	proof.cfg.Roles = roles
	var err error
	if proof.project, err = claim.ProjectToken(proof.cfg.Project); err != nil {
		t.Fatal(err)
	}
	number, err := strconv.ParseInt(randomSuffix(t)[:6], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	r := &deploymentRolesRig{stepRecordsRig: &stepRecordsRig{liveProof: proof, dispatch: dispatch, github: newStepRecordsGitHub(t),
		claims: map[claim.Token]*stepClaimant{}, waits: make(chan claim.Token, 16)}, specs: map[string]string{}}
	if err := os.WriteFile(r.cfg.DispatchTokenFile, []byte(r.dispatch.token+"\n"), 0o600); err != nil {
		t.Fatalf("write the Dispatch token file: %v", err)
	}
	r.cfg.DispatchURL = r.dispatch.base
	r.person(http.MethodPost, "/api/v1/projects", map[string]string{"key": r.cfg.Project, "name": "Deployment roles"}, http.StatusCreated, nil)
	r.person(http.MethodPut, "/api/v1/projects/"+r.cfg.Project+"/default-repository", map[string]any{"repo": liveRepo}, http.StatusOK, nil)
	for place, title := range titles {
		var created struct {
			Key               string `json:"key"`
			Rank              string `json:"rank"`
			LastSeq           int64  `json:"last_seq"`
			PrimaryArtifactID string `json:"primary_artifact_id"`
		}
		r.person(http.MethodPost, "/api/v1/issues", map[string]string{"project": r.cfg.Project, "title": title,
			"spec": "The issue's change, as its owner states it.\n"}, http.StatusCreated, &created)
		if created.PrimaryArtifactID == "" {
			t.Fatalf("Dispatch created %s with no primary document", created.Key)
		}
		spec := created.PrimaryArtifactID
		r.person(http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{"status": "in_progress", "labels": []string{"legion"}}, http.StatusOK, &created)
		r.specs[created.Key] = spec
		r.issues = append(r.issues, walkIssue{key: created.Key, number: int(number) + 1 + place})
		r.on(created.Key)
		r.github.open("legion/"+r.key, r.number)
		r.seedAdmitted(title, created.Rank, created.LastSeq)
	}
	r.boot()
	for _, issue := range r.issues {
		r.on(issue.key)
		r.worker(claim.RoleArchitect)
	}
	r.on(r.issues[0].key)
	return r
}

// seedAdmitted records the walk's issue as the daemon holds a root its architect has just begun:
// handed to Legion at admitted, its lifecycle under the workflow, no design gate registered, its
// pull request recorded open at stepRecordsHead, which GitHub reports clean, and its architect's
// claim a launch a previous daemon began and never recorded, which the boot relaunches.
func (r *deploymentRolesRig) seedAdmitted(title, rank string, seq int64) {
	t := r.t
	t.Helper()
	if err := store.Transact(context.Background(), r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: r.key, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: r.key, Project: r.cfg.Project,
			Title: title, Phase: phase.Admitted, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
			Rank: rank, LastDispatchSeq: seq, HandedOver: true,
		}); err != nil {
			return err
		}
		return r.records.PutPullRequest(ctx, tx, record.PullRequest{
			State: record.PullRequestOpen, Issue: r.key, Repo: liveRepo, Number: r.number, Branch: "legion/" + r.key,
			HeadSHA: stepRecordsHead, MergeState: record.MergeStateClean, Failing: []string{},
		})
	}); err != nil {
		t.Fatalf("seed %s: %v", r.key, err)
	}
	ctx := context.Background()
	if _, err := r.st.OpenTreeLifecycle(ctx, r.project, r.key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", r.key, err)
	}
	architect := r.token(claim.RoleArchitect)
	if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
		Token: architect, Project: r.project, Tree: r.key, Issue: r.key, Role: claim.RoleArchitect,
		Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
	}); err != nil {
		t.Fatalf("record the architect's unfinished launch on %s: %v", r.key, err)
	}
	writePrompt(t, r.cfg, architect)
}

// deploymentPlanOf is the text of a plan.md with one task a, whose steps attribute names steps and
// whose text is deploymentTaskText.
func deploymentPlanOf(steps ...string) string {
	return ":::plan_task{#a steps=\"" + strings.Join(steps, " ") + "\"}\n" + deploymentTaskText + "\n:::\n"
}

// document uploads markdown as the document name onto the walk's issue, as a person through
// Dispatch's route, and answers its id. Dispatch stores it at version 1.
func (r *deploymentRolesRig) document(name, markdown string) string {
	r.t.Helper()
	var created struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}
	r.person(http.MethodPost, "/api/v1/issues/"+r.key+"/artifacts", map[string]string{"name": name, "content": markdown}, http.StatusCreated, &created)
	if created.Artifact.ID == "" {
		r.t.Fatalf("Dispatch answered the upload of %s on %s with no artifact id", name, r.key)
	}
	return created.Artifact.ID
}

// registerPlan is the architect of the walk's issue giving Legion version of the document artifact
// as the issue's plan (register_plan), answered as the daemon's status and body.
func (r *deploymentRolesRig) registerPlan(artifact string, version int) (int, string) {
	r.t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/plans/register", api.PlanRegisterRequest{
		GrantID: r.worker(claim.RoleArchitect).grant(), Issue: r.key, ArtifactID: artifact, Version: version}, false)
	return status, string(body)
}

// givePlan writes plan.md on the walk's issue with one task a naming steps (deploymentPlanOf) and
// has its architect give version 1 of it, which the daemon must take.
func (r *deploymentRolesRig) givePlan(steps ...string) {
	r.t.Helper()
	if status, body := r.registerPlan(r.document("plan.md", deploymentPlanOf(steps...)), 1); status != http.StatusOK {
		r.t.Fatalf("register_plan of %s's plan %q = %d %s, want 200", r.key, strings.Join(steps, " "), status, body)
	}
}

// openGate is a person approving the walk's issue's spec at version 1 and its architect registering
// that version (register_gate), which carries the approval and so opens the issue's gate; the
// waiting line then starts the issue's first step. It answers the phase the issue stands in once it
// has left admitted.
func (r *deploymentRolesRig) openGate() phase.Phase {
	r.t.Helper()
	spec := r.specs[r.key]
	r.person(http.MethodPost, "/api/v1/artifacts/"+spec+"/reviews", map[string]any{"state": "approved"}, http.StatusCreated, nil)
	status, body := r.d.request(http.MethodPost, "/legion/v1/gates/register", api.GateRegisterRequest{
		GrantID: r.worker(claim.RoleArchitect).grant(), Issue: r.key, ArtifactID: spec, Version: 1}, false)
	if status != http.StatusOK {
		r.t.Fatalf("register_gate of %s = %d %s, want 200", r.key, status, body)
	}
	var stands string
	testwait.WithinPolling(r.t, stepRecordsWait, 100*time.Millisecond, r.key+" to leave admitted once its gate opened", func() bool {
		stands = string(r.d.state().Issues[r.key].Phase)
		return stands != "" && stands != string(phase.Admitted)
	})
	return phase.Phase(stands)
}

// launches is every launch of role's claim on the walk's issue the daemon running now has made, as
// its method, in order.
func (r *deploymentRolesRig) launches(role claim.Role) []string {
	token := r.token(role)
	var methods []string
	for _, call := range r.rt.Calls() {
		if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == token {
			methods = append(methods, call.Method)
		}
	}
	return methods
}

// deletePlanRows deletes the walk's issue's rows from plan_steps, plan_tasks and issue_plans, at
// every generation, from the daemon's store.
func (r *deploymentRolesRig) deletePlanRows() {
	r.t.Helper()
	for _, table := range []string{"plan_steps", "plan_tasks", "issue_plans"} {
		if _, err := r.st.Pool().Exec(context.Background(), "delete from "+table+" where issue = $1", r.key); err != nil {
			r.t.Fatalf("delete the %s rows of %s: %v", table, r.key, err)
		}
	}
}

// restartWith stops the daemon and boots it again on the same store, the same Dispatch and the
// same ports, with the settings defining roles. The pane of each claim of the issues live names
// outlives the daemon, as a tmux pane does, and its shim connects to the new daemon with the boot
// token it has held since its launch; every other claimant is forgotten, its issue done.
func (r *deploymentRolesRig) restartWith(roles []config.Role, live ...string) {
	r.t.Helper()
	r.d.stop()
	rebindHeldPorts(r.t, &r.cfg)
	r.cfg.Roles = roles
	r.boot()
	for token, c := range r.claims {
		if !slices.Contains(live, c.issue) {
			delete(r.claims, token)
			continue
		}
		c.reconnect()
	}
}

// takeTask waits for the daemon's next task of c's claim, starts the turn that works it with its
// model answering first, as stepClaimant.take does, and answers the task's text.
func takeTask(c *stepClaimant) string {
	c.r.t.Helper()
	var prompt shimwire.Prompt
	select {
	case prompt = <-c.prompts:
	case <-time.After(stepRecordsWait):
		c.r.t.Fatalf("%s was prompted with no task within %s", c.token, stepRecordsWait)
	}
	c.answer()
	c.send(shimwire.AgentStart{DeliveryID: prompt.DeliveryID})
	c.waitState(supervise.StateWorking)
	return prompt.Message
}

// deploymentStepWant is one record of a task's read: its kind, its place, its role and type, and,
// for a record of kind step, the model that answered it. Each is attempt 1, ended passed.
type deploymentStepWant struct {
	kind             string
	step             int
	role, typ, model string
}

// deploymentStepsRead is the read of an issue's steps, as Dispatch answers GET
// /api/v1/issues/{key}/steps, the read `dispatch steps` makes, with the rating a retro's record
// carries and its reason.
type deploymentStepsRead struct {
	Tasks []struct {
		Task  string               `json:"task"`
		Steps []deploymentStepRead `json:"steps"`
	} `json:"tasks"`
}

// deploymentStepRead is one record of the read: stepRecordRead's fields, with the rating a retro
// gives the plan and its reason.
type deploymentStepRead struct {
	stepRecordRead
	Rating       *int    `json:"rating"`
	RatingReason *string `json:"rating_reason"`
}

// waitDeploymentSteps waits for the read of r's issue to hold each record of want, of task, and,
// with exact, no other record; the retro carries deploymentRating and deploymentReason. It fails
// with deploymentStepsHold's last answer.
func waitDeploymentSteps(r *stepRecordsRig, task string, exact bool, want []deploymentStepWant) {
	r.t.Helper()
	var last error
	if !testwait.Holds(stepRecordsWait, time.Second, func() bool {
		var read deploymentStepsRead
		if last = r.personRead("/api/v1/issues/"+r.key+"/steps", &read); last != nil {
			return false
		}
		last = deploymentStepsHold(read, task, want, exact)
		return last == nil
	}) {
		r.t.Fatalf("the steps of %s never held task %s's walk: %v", r.key, task, last)
	}
}

// deploymentStepsHold says whether read holds each record of want, each of task, attempt 1, ended
// passed, with an agent, each of kind step with its type, its session and the model that answered
// it, and the retro with deploymentRating and deploymentReason; with exact, it holds no other
// record. The error names the first record that is missing or wrong.
func deploymentStepsHold(read deploymentStepsRead, task string, want []deploymentStepWant, exact bool) error {
	var records []deploymentStepRead
	for _, listed := range read.Tasks {
		records = append(records, listed.Steps...)
	}
	for _, w := range want {
		name := fmt.Sprintf("%s %d %s", w.kind, w.step, w.role)
		at := slices.IndexFunc(records, func(got deploymentStepRead) bool {
			return got.Kind == w.kind && got.Step == w.step && got.Attempt == 1
		})
		if at < 0 {
			return fmt.Errorf("the read holds no record of %s", name)
		}
		got := records[at]
		switch {
		case got.Task != task || got.Role != w.role || got.Type != w.typ || got.Agent == "":
			return fmt.Errorf("%s is task %q, role %q, type %q, agent %q; want task %s, role %s, type %q and an agent", name, got.Task, got.Role, got.Type, got.Agent, task, w.role, w.typ)
		case got.StartedAt == nil || got.EndedAt == nil || got.Result != "passed":
			return fmt.Errorf("%s started %v, ended %v with result %q; want a start, an end and passed", name, got.StartedAt, got.EndedAt, got.Result)
		case w.kind == "step" && (got.Session == "" || got.Model != w.model):
			return fmt.Errorf("%s names session %q and model %q, want its agent's session and the model %s", name, got.Session, got.Model, w.model)
		case w.kind == "retro" && (got.Rating == nil || *got.Rating != deploymentRating || got.RatingReason == nil || *got.RatingReason != deploymentReason):
			return fmt.Errorf("the retro names the rating %v and the reason %v, want %d and %q", deref(got.Rating), deref(got.RatingReason), deploymentRating, deploymentReason)
		}
	}
	if exact && len(records) != len(want) {
		return fmt.Errorf("the read holds %d records, want %d", len(records), len(want))
	}
	return nil
}

// deref is what p points at, or nil, as a value a message prints.
func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// deploymentRolesMigration ends the name of this release's migration of the deployment's roles,
// the first one of the release. It is found by its name rather than its number, so the store of the
// release before is the same whatever number a later merge gives it.
const deploymentRolesMigration = "_deployment_roles.up.sql"

// earlierReleaseRig is the walk's rig on the store the release before this one leaves, with its one
// issue at testing, and the version of the migration whose way back the test runs.
type earlierReleaseRig struct {
	*stepRecordsRig
	migrationVersion int
}

// newEarlierReleaseRig is the store the release before leaves (storeOfTheEarlierRelease), seeded with
// one issue at testing (seedEarlierReleaseIssue), migrated to this release's schema, with the daemon
// booted on it at the real Dispatch of this package's tests and the issue's architect and tester
// resumed in the sessions the earlier daemon recorded.
func newEarlierReleaseRig(t *testing.T) *earlierReleaseRig {
	t.Helper()
	proof := newLiveProofOn(t, workflowNATS(t))
	st, dsn, version := storeOfTheEarlierRelease(t)
	proof.st, proof.cfg.PostgresDSN = st, dsn
	proof.cfg.Project = "ER" + randomSuffix(t)[:6]
	proof.cfg.Projects = map[string]config.Project{proof.cfg.Project: {}}
	proof.cfg.AdmissionCap = 4
	var err error
	if proof.project, err = claim.ProjectToken(proof.cfg.Project); err != nil {
		t.Fatal(err)
	}
	number, err := strconv.ParseInt(randomSuffix(t)[:6], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	r := &earlierReleaseRig{stepRecordsRig: &stepRecordsRig{liveProof: proof, dispatch: startScratchDispatchOn(t, scratchDispatchOptions{}),
		github: newStepRecordsGitHub(t), claims: map[claim.Token]*stepClaimant{}, waits: make(chan claim.Token, 16)}, migrationVersion: version}
	if err := os.WriteFile(r.cfg.DispatchTokenFile, []byte(r.dispatch.token+"\n"), 0o600); err != nil {
		t.Fatalf("write the Dispatch token file: %v", err)
	}
	r.cfg.DispatchURL = r.dispatch.base
	r.person(http.MethodPost, "/api/v1/projects", map[string]string{"key": r.cfg.Project, "name": "Release before"}, http.StatusCreated, nil)
	r.person(http.MethodPut, "/api/v1/projects/"+r.cfg.Project+"/default-repository", map[string]any{"repo": liveRepo}, http.StatusOK, nil)
	const title = "An issue of the release before goes on"
	var created struct {
		Key     string `json:"key"`
		Rank    string `json:"rank"`
		LastSeq int64  `json:"last_seq"`
	}
	status := workflow.PhaseStatus(phase.Testing)
	r.person(http.MethodPost, "/api/v1/issues", map[string]string{"project": r.cfg.Project, "title": title}, http.StatusCreated, &created)
	r.person(http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{"status": status, "labels": []string{"legion"}}, http.StatusOK, &created)
	r.issues = append(r.issues, walkIssue{key: created.Key, number: int(number)})
	r.on(created.Key)
	r.github.open("legion/"+r.key, r.number)
	sessions := r.seedEarlierReleaseIssue(title, status, created.Rank, created.LastSeq)
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate the store the release before left to this release's schema: %v", err)
	}
	r.boot()
	for _, role := range []claim.Role{claim.RoleArchitect, claim.RoleTester} {
		r.resume(role, sessions[role])
	}
	return r
}

// storeOfTheEarlierRelease is a database of this test's own carrying every embedded migration whose
// version is below this release's first (deploymentRolesMigration), applied by hand in version
// order (applyMigrationsByHand): the schema the release before leaves. It answers the store, its URL
// and the version of this release's first migration.
func storeOfTheEarlierRelease(t *testing.T) (*store.Store, string, int) {
	t.Helper()
	dsn := testdb.Database(t, "legion_earlier_release")
	st, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open the store the release before leaves: %v", err)
	}
	t.Cleanup(st.Close)
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	first := 0
	for _, migration := range all {
		if strings.HasSuffix(migration.Name, deploymentRolesMigration) {
			first = migration.Version
		}
	}
	if first == 0 {
		t.Fatalf("no embedded migration ends %s, the first one of this release", deploymentRolesMigration)
	}
	var before []migrations.Migration
	for _, migration := range all {
		if migration.Version < first {
			before = append(before, migration)
		}
	}
	applyMigrationsByHand(t, st, before)
	return st, dsn, first
}

// earlierReleaseSession is a session the earlier daemon recorded on a claim: its id and its file.
type earlierReleaseSession struct {
	id, file string
}

// seedEarlierReleaseIssue writes the walk's issue as the release before leaves it at testing, with
// plain SQL in its columns: the root handed to Legion, its tree under the workflow, its approved
// design gate, its slot, its open pull request of the task a, the owner's plan `implementer tester
// reviewer` at version 1 with its task a running at step 2 (plan_tasks.running_kind step, position
// 2), the count of starts of steps 1 and 2, the implementer's phases row of its done step 1, the
// tester's row raised at entry 1 for step 2, and the claims of the architect and the tester, each a
// launch the earlier daemon began in the session it recorded, which the boot resumes. It answers
// those sessions.
func (r *earlierReleaseRig) seedEarlierReleaseIssue(title, status, rank string, seq int64) map[claim.Role]earlierReleaseSession {
	t := r.t
	t.Helper()
	ctx := context.Background()
	exec := func(what, statement string, args ...any) {
		t.Helper()
		if _, err := r.st.Pool().Exec(ctx, statement, args...); err != nil {
			t.Fatalf("write %s in the columns of the release before: %v", what, err)
		}
	}
	key := r.key
	implementer, tester := string(r.token(claim.RoleImplementer)), string(r.token(claim.RoleTester))
	exec("the root "+key, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq,
		handed_over, dispatch_status, repo, default_branch, github_repo)
		values ($1, $1, $2, $3, 'testing', 1, $4, $5, $6, true, $4, $7, 'main', $7)`,
		key, r.cfg.Project, title, status, rank, seq, liveRepo)
	exec("the tree of "+key, `insert into tree_lifecycles (project, tree, epoch, authority) values ($1, $2, 1, 'workflow')`, r.project, key)
	exec("the design gate of "+key, `insert into design_gates (issue, artifact_id, latest_version, approved_version, ever_opened, step)
		values ($1, $2, 1, 1, true, 'spec')`, key, "spec-"+key)
	exec("the slot of "+key, `insert into slots (issue, index, admitted_at) values ($1, 1, now() - interval '1 day')`, key)
	exec("the pull request of "+key, `insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at,
		failing, fix_attempts, blocked_attempts, check_runs, generation, snapshot, head_counted, state, mergeability, merge_sha)
		values ($1, $2, $3, $4, $5, now() - interval '3 hours', '[]', 0, 0, '[]', 1, '', '', 'open', 'MERGEABLE', '')`,
		key, liveRepo, r.number, claim.IssueBranch(key), stepRecordsHead)
	exec("the plan of "+key, `insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at)
		values ($1, 1, 1, 'owner', $2, $3, now() - interval '5 hours')`, key, "plan-"+key, string(r.token(claim.RoleArchitect)))
	exec("the task a of "+key, `insert into plan_tasks (issue, generation, task, plan_version, block_text, state, running_kind, position)
		values ($1, 1, 'a', 1, $2, 'running', 'step', 2)`, key, deploymentTaskText)
	exec("the steps of task a of "+key, `insert into plan_steps (issue, generation, task, position, role, type)
		values ($1, 1, 'a', 1, 'implementer', 'implementation'), ($1, 1, 'a', 2, 'tester', 'testing'), ($1, 1, 'a', 3, 'reviewer', 'review')`, key)
	exec("the implementer's row of "+key, `insert into phases (issue, role, claim, handoff_commit, rounds, pass, verdict, summary,
		last_handoff, completed_at, step_task, step_place) values ($1, 'implementer', $2, $3, 0, 0, '', 'implemented', $4,
		now() - interval '2 hours', 'a', 1)`, key, implementer, stepRecordsHead, "implement-record-"+key)
	// The tester's entry carries the resume task the earlier daemon stored for it, in the shape the
	// workflow composes a resume of testing (workflow's Engine.ResumeTask): the engine reads this
	// release's columns, which the store the release before left does not hold yet, so the text is
	// written here.
	testerTask := fmt.Sprintf("Continue %s. Issue: %s. Phase: %s. Working copy: branch %s. Resume the existing phase work.",
		title, key, phase.Testing, claim.IssueBranch(key))
	exec("the tester's row of "+key, `insert into phases (issue, role, claim, handoff_commit, rounds, verdict, summary,
		last_handoff, entry, entry_task, entry_raised_at, step_task, step_place) values ($1, 'tester', $2, '', 0, '', '', '', 1, $3,
		now() - interval '1 hour', 'a', 2)`, key, tester, testerTask)
	sessions := map[claim.Role]earlierReleaseSession{}
	for _, role := range []claim.Role{claim.RoleArchitect, claim.RoleTester} {
		token := r.token(role)
		kept := earlierReleaseSession{id: "ses_" + string(role) + "_" + strings.ToLower(key), file: filepath.Join(r.cfg.StateDir, key+"-"+string(role)+".jsonl")}
		sessions[role] = kept
		exec("the "+string(role)+"'s claim on "+key, `insert into claims (token, project, tree, issue, role, generation, session,
			session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, boot_token_hash, serving_generation,
			tree_epoch, plugin_contract) values ($1, $2, $3, $3, $4, 1, $5, $6, 'launching', 0, 0, 0, 0, $7, 1, 1, $8)`,
			string(token), r.project, key, string(role), kept.id, kept.file, supervise.HashBootToken("interrupted-"+string(token)),
			api.OwnerPlanContract)
		writePrompt(t, r.cfg, token)
	}
	return sessions
}

// resume is the pane of role's claim on the walk's issue coming up in the launch the boot made of
// it: the launch resumes the session the earlier daemon recorded, and the pane says hello with the
// launch's boot token, registers in that session and says ready, as the plugin of the deployed
// release does.
func (r *earlierReleaseRig) resume(role claim.Role, kept earlierReleaseSession) {
	t := r.t
	t.Helper()
	token := r.token(role)
	r.awaitLaunch(token)
	launch := lastLaunch(t, r.rt, token)
	if launch.ResumeSessionFile != kept.file {
		t.Fatalf("the boot launched %s with the session file %q, want a resume of %s, the one the earlier daemon recorded", token, launch.ResumeSessionFile, kept.file)
	}
	r.built.mu.Lock()
	address := r.built.address
	r.built.mu.Unlock()
	c := &stepClaimant{r: r.stepRecordsRig, token: token, issue: r.key, tree: r.key, role: role, session: kept.id, model: stepRecordsModel,
		bootToken: launch.BootToken, sh: dialShim(t, address, launch.BootToken), prompts: make(chan shimwire.Prompt, 8)}
	status, body := r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: kept.id, OmpSessionFile: kept.file, AgentID: kept.id, PluginContract: api.DaemonAPIVersion(),
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register %s in its session %s = %d %s, want 200", token, kept.id, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the registration of %s: %v", token, err)
	}
	c.secret = registered.Secret
	sh, d := c.sh, r.d
	sh.serve(func() { c.serve(sh, d) })
	if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: kept.id, Secret: c.secret, Generation: launch.Generation,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready %s = %d %s, want 204", token, status, body)
	}
	r.claims[token] = c
}

// taskPlace is where the walk's issue's task a stands, as its plan_tasks row holds it: the kind of
// the act it runs and its position.
func (r *earlierReleaseRig) taskPlace() (string, int) {
	r.t.Helper()
	var kind string
	var position int
	if err := r.st.Pool().QueryRow(context.Background(), `select running_kind, position from plan_tasks
		where issue = $1 and generation = 1 and task = 'a'`, r.key).Scan(&kind, &position); err != nil {
		r.t.Fatalf("read where %s's task a stands: %v", r.key, err)
	}
	return kind, position
}

// wayBackStart opens the way back in a migration's header and wayBackEnd ends it: what stands
// between them is what psql reads on its standard input.
const (
	wayBackStart = "psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1 <<'SQL'"
	wayBackEnd   = "SQL"
)

// wayBackOf is the way back in the header of the embedded migration whose name ends suffix, as an
// operator copies it out of the file: the lines between the psql line and the heredoc's end, each
// without the comment marker and the header's own indent. A header with no such block, or a block
// that never ends, stops the test, since running nothing would pass.
func wayBackOf(t *testing.T, suffix string) string {
	t.Helper()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	for _, migration := range all {
		if !strings.HasSuffix(migration.Name, suffix) {
			continue
		}
		var block []string
		opened := false
		for _, line := range strings.Split(migration.SQL, "\n") {
			comment, isComment := strings.CutPrefix(line, "--")
			if !isComment {
				break
			}
			switch text := strings.TrimSpace(comment); {
			case !opened && text == wayBackStart:
				opened = true
			case opened && text == wayBackEnd:
				if len(block) == 0 {
					t.Fatalf("the way back in the header of %s holds no statement", migration.Name)
				}
				return strings.Join(block, "\n")
			case opened:
				block = append(block, strings.TrimPrefix(comment, "   "))
			}
		}
		t.Fatalf("the header of %s holds no way back that opens with %q and ends with %q", migration.Name, wayBackStart, wayBackEnd)
	}
	t.Fatalf("no embedded migration ends %s", suffix)
	return ""
}

// runSQL runs sql on a connection of its own to dsn, as psql runs it under ON_ERROR_STOP: a
// statement that fails ends the run with its error, and the transaction it stood in is rolled
// back. dsn is a URL a pgx pool reads (testdb.Database), which holds pgxpool's keys, and
// pgx.Connect would send them to the server as settings it does not know, so the connection is
// made to the same database by the URL with those keys taken out (testdb.DatabaseURL).
func runSQL(t *testing.T, dsn, sql string) error {
	t.Helper()
	ctx := context.Background()
	plain, err := testdb.DatabaseURL(dsn, "")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, plain)
	if err != nil {
		t.Fatalf("connect to %s: %v", plain, err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, sql); err != nil {
		if _, rollback := conn.Exec(ctx, "rollback"); rollback != nil && !errors.Is(rollback, context.Canceled) {
			t.Fatalf("roll back the failed statements: %v", rollback)
		}
		return err
	}
	return nil
}
