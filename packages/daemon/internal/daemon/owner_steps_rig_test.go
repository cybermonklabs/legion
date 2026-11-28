// The rig of the whole-daemon tests of an owner's plan, and of its live driver
// (owner_steps_live_test.go): the walk's rig of the step records (stepRecordsRig) over issues that
// stand at admitted with no design gate and no pull request, so nothing of an issue starts before
// its owner, the architect of its tree, has given its plan (givePlan) and registered its spec
// (openGate). Each fact of GitHub reaches the booted daemon through its own reads and its own
// intake, never through a write to its store: the pull request is listed by GitHub's API from the
// implementer's completion on (the daemon's read of the branch applies the opened), a review is a
// person's on the stream, and the merge is GitHub's envelope on the stream.
package daemon

import (
	"context"
	"fmt"
	"net/http"
	"os"
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
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// ownerStepsRig is the walk's rig over issues whose owner gives their plan before their work
// starts, with the id of each issue's spec, its primary document, by issue.
type ownerStepsRig struct {
	*stepRecordsRig
	specs map[string]string
}

// stepAnswer is how the agent of one step of a walk answers: pass, or fail, a tester's verdict
// fail or a review that requests changes, which returns the task to its implementer.
type stepAnswer string

const (
	answerPass stepAnswer = "pass"
	answerFail stepAnswer = "fail"
)

// newOwnerStepsRig sets up dispatch, a running Dispatch, with the run's project, which runs as
// project says (its production check, its default steps), its repository mapping and issues issues,
// each created with a spec, seeds the daemon's store of proof with each issue at admitted with no
// design gate and no pull request (seedAdmitted), boots the daemon at Dispatch and registers each
// tree's architect. Nothing of an issue starts until its gate opens (firstStep). The walk's steps act
// on the first issue until on moves them.
func newOwnerStepsRig(t *testing.T, proof liveProof, dispatch *scratchDispatch, issues int, project config.Project) *ownerStepsRig {
	t.Helper()
	// A Dispatch project key is at most ten characters, and the daemon's project is its Dispatch
	// project, so the run's project is a short one of its own.
	proof.cfg.Project = "OS" + randomSuffix(t)[:6]
	proof.cfg.Projects = map[string]config.Project{proof.cfg.Project: project}
	proof.cfg.AdmissionCap = 8
	var err error
	if proof.project, err = claim.ProjectToken(proof.cfg.Project); err != nil {
		t.Fatal(err)
	}
	number, err := strconv.ParseInt(randomSuffix(t)[:6], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	r := &ownerStepsRig{stepRecordsRig: &stepRecordsRig{liveProof: proof, dispatch: dispatch, github: newStepRecordsGitHub(t),
		claims: map[claim.Token]*stepClaimant{}, waits: make(chan claim.Token, 16)}, specs: map[string]string{}}
	if err := os.WriteFile(r.cfg.DispatchTokenFile, []byte(r.dispatch.token+"\n"), 0o600); err != nil {
		t.Fatalf("write the Dispatch token file: %v", err)
	}
	r.cfg.DispatchURL = r.dispatch.base
	r.person(http.MethodPost, "/api/v1/projects", map[string]string{"key": r.cfg.Project, "name": "Owner steps"}, http.StatusCreated, nil)
	r.person(http.MethodPut, "/api/v1/projects/"+r.cfg.Project+"/default-repository", map[string]any{"repo": liveRepo}, http.StatusOK, nil)
	for place := range issues {
		var spec struct {
			createdIssue
			PrimaryArtifactID string `json:"primary_artifact_id"`
		}
		r.person(http.MethodPost, "/api/v1/issues", map[string]string{"project": r.cfg.Project, "title": walkTitles[place],
			"spec": "The issue's change, as its owner states it.\n"}, http.StatusCreated, &spec)
		if spec.PrimaryArtifactID == "" {
			t.Fatalf("Dispatch created %s with no primary document", spec.Key)
		}
		created := spec.createdIssue
		r.person(http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{"status": "in_progress", "labels": []string{"legion"}}, http.StatusOK, &created)
		r.specs[created.Key] = spec.PrimaryArtifactID
		r.issues = append(r.issues, walkIssue{key: created.Key, title: walkTitles[place], number: int(number) + 1 + place})
		r.on(created.Key)
		r.seedAdmitted(created.Rank, created.LastSeq)
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
// handed to Legion at admitted, its lifecycle under the workflow, no design gate registered, no
// pull request, and its architect's claim a launch a previous daemon began and never recorded,
// which the boot relaunches.
func (r *ownerStepsRig) seedAdmitted(rank string, seq int64) {
	t := r.t
	t.Helper()
	ctx := context.Background()
	if err := store.Transact(ctx, r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return r.records.PutIssue(ctx, tx, record.Issue{
			Key: r.key, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: r.key, Project: r.cfg.Project,
			Title: r.title(), Phase: phase.Admitted, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
			Rank: rank, LastDispatchSeq: seq, HandedOver: true,
		})
	}); err != nil {
		t.Fatalf("seed %s: %v", r.key, err)
	}
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

// title is the walk's issue's title.
func (r *ownerStepsRig) title() string {
	for _, issue := range r.issues {
		if issue.key == r.key {
			return issue.title
		}
	}
	return ""
}

// planOf is the text of a plan.md with one task a whose steps attribute names steps, or names none
// for no steps.
func planOf(steps ...string) string {
	return ":::plan_task{#a steps=\"" + strings.Join(steps, " ") + "\"}\nMake the change the issue asks for.\n:::\n"
}

// document uploads markdown as the document name onto the walk's issue (documentOf).
func (r *ownerStepsRig) document(name, markdown string) string {
	r.t.Helper()
	return r.documentOf(r.key, name, markdown)
}

// documentOf uploads markdown as the document name onto issue, as a person through Dispatch's
// route, and answers its id. Dispatch stores it at version 1.
func (r *ownerStepsRig) documentOf(issue, name, markdown string) string {
	r.t.Helper()
	var created struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}
	r.person(http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": name, "content": markdown}, http.StatusCreated, &created)
	if created.Artifact.ID == "" {
		r.t.Fatalf("Dispatch answered the upload of %s on %s with no artifact id", name, issue)
	}
	return created.Artifact.ID
}

// registerPlan is the architect of the walk's issue giving Legion version of the document artifact
// as the issue's plan (registerPlanOf).
func (r *ownerStepsRig) registerPlan(artifact string, version int) (int, string) {
	r.t.Helper()
	return r.registerPlanOf(r.key, artifact, version)
}

// registerPlanOf is the architect of the walk's issue giving Legion version of the document
// artifact as the plan of issue, an issue of its tree (register_plan), answered as the daemon's
// status and body.
func (r *ownerStepsRig) registerPlanOf(issue, artifact string, version int) (int, string) {
	r.t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/plans/register", api.PlanRegisterRequest{
		GrantID: r.worker(claim.RoleArchitect).grant(), Issue: issue, ArtifactID: artifact, Version: version}, false)
	return status, string(body)
}

// givePlan writes plan.md on the walk's issue with one task a naming steps (planOf) and has its
// architect give version 1 of it, which the daemon must take.
func (r *ownerStepsRig) givePlan(steps ...string) string {
	r.t.Helper()
	artifact := r.document("plan.md", planOf(steps...))
	if status, body := r.registerPlan(artifact, 1); status != http.StatusOK {
		r.t.Fatalf("register_plan of %s's plan %q = %d %s, want 200", r.key, strings.Join(steps, " "), status, body)
	}
	return artifact
}

// openGate is the opening of the walk's issue's gate (firstStep), which must start first.
func (r *ownerStepsRig) openGate(first phase.Phase) {
	r.t.Helper()
	if got := r.firstStep(); got != first {
		r.t.Fatalf("%s's gate opened and its first step is %s, want %s", r.key, got, first)
	}
}

// firstStep is a person approving the walk's issue's spec at version 1 and its architect
// registering that version (register_gate), which carries the approval and so opens the issue's
// gate; the waiting line then starts the issue's first step. It answers the phase the issue stands
// in once it has left admitted.
func (r *ownerStepsRig) firstStep() phase.Phase {
	r.t.Helper()
	spec := r.specs[r.key]
	r.person(http.MethodPost, "/api/v1/artifacts/"+spec+"/reviews", map[string]any{"state": "approved"}, http.StatusCreated, nil)
	status, body := r.d.request(http.MethodPost, "/legion/v1/gates/register", map[string]any{
		"grantId": r.worker(claim.RoleArchitect).grant(), "issue": r.key, "artifactId": spec, "version": 1}, false)
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

// stepPhase is the phase a step of a plan runs as, which a step word names by its role.
func stepPhase(t *testing.T, step string) phase.Phase {
	t.Helper()
	switch step {
	case "planner":
		return phase.Planning
	case "implementer":
		return phase.Implementing
	case "tester":
		return phase.Testing
	case "reviewer":
		return phase.Reviewing
	case "acceptance:owner":
		return phase.Accepting
	}
	t.Fatalf("the step %q runs as no phase of this release", step)
	return ""
}

// walkSteps walks the task of the walk's issue through steps from its first, the issue standing in
// the first already, each step's agent answering as answers says in the order the steps run
// (answerPass where answers names nothing), then its retro and its merge (merge). A failed step
// returns the task to its implementer, as the walk returns it.
func (r *ownerStepsRig) walkSteps(steps []string, answers ...stepAnswer) {
	r.t.Helper()
	for place, run := 0, 0; place < len(steps); run++ {
		answer := answerPass
		if run < len(answers) {
			answer = answers[run]
		}
		step := steps[place]
		next := phase.Retro
		if answer == answerFail {
			place = slices.Index(steps, "implementer")
			next = phase.Implementing
		} else if place++; place < len(steps) {
			next = stepPhase(r.t, steps[place])
		}
		r.runStep(step, answer, next)
	}
	r.retro()
	r.merge()
}

// runStep is one run of step on the walk's issue, its agent answering answer, and the wait for the
// issue to stand in next. The implementer's completion is the first moment GitHub lists the issue's
// pull request, so the daemon reads it on its own.
func (r *ownerStepsRig) runStep(step string, answer stepAnswer, next phase.Phase) {
	t := r.t
	t.Helper()
	switch {
	case step == "planner" && answer == answerPass:
		r.work(claim.RolePlanner, "plan", api.HandoffCompleteRequest{Summary: "planned"}, next)
	case step == "implementer" && answer == answerPass:
		if !r.github.opened(r.number) {
			r.github.open("legion/"+r.key, r.number)
		}
		r.work(claim.RoleImplementer, "implement", api.HandoffCompleteRequest{Summary: "implemented", Commit: stepRecordsHead}, next)
	case step == "tester":
		r.work(claim.RoleTester, "test", api.HandoffCompleteRequest{Summary: "the criteria read " + string(answer), Verdict: string(answer)}, next)
	case step == "reviewer":
		state := "APPROVED"
		if answer == answerFail {
			state = "CHANGES_REQUESTED"
		}
		r.review(state, next)
	case step == "acceptance:owner" && answer == answerPass:
		r.signOff(r.key)
	default:
		t.Fatalf("the walk has no answer %s for the step %s", answer, step)
	}
}

// launchesOfRole is every launch of role's claim on the walk's issue the daemon running now has
// made, in order.
func (r *ownerStepsRig) launchesOfRole(role claim.Role) []string {
	token := r.token(role)
	var methods []string
	for _, call := range r.rt.Calls() {
		if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == token {
			methods = append(methods, call.Method)
		}
	}
	return methods
}

// stepStarts is each row of step_starts of the walk's issue, `<task> <kind> <place> starts <n>
// run <run>`, in key order.
func (r *ownerStepsRig) stepStarts() []string {
	r.t.Helper()
	rows, err := r.st.Pool().Query(context.Background(), `select task, kind, place, starts, run from step_starts where issue = $1
		order by task, kind, place`, r.key)
	if err != nil {
		r.t.Fatalf("read the step starts of %s: %v", r.key, err)
	}
	defer rows.Close()
	var all []string
	for rows.Next() {
		var task, kind, run string
		var place int
		var starts int64
		if err := rows.Scan(&task, &kind, &place, &starts, &run); err != nil {
			r.t.Fatal(err)
		}
		all = append(all, fmt.Sprintf("%s %s %d starts %d run %s", task, kind, place, starts, run))
	}
	if err := rows.Err(); err != nil {
		r.t.Fatal(err)
	}
	return all
}

// deletePlanRows deletes the walk's issue's rows from plan_steps, plan_tasks and issue_plans, at
// every generation, from the daemon's store.
func (r *ownerStepsRig) deletePlanRows() {
	r.t.Helper()
	for _, table := range []string{"plan_steps", "plan_tasks", "issue_plans"} {
		r.exec("delete from "+table+" where issue = $1", r.key)
	}
}

// planRows is how many rows the walk's issue holds in plan_steps, plan_tasks and issue_plans.
func (r *ownerStepsRig) planRows() int {
	r.t.Helper()
	var count int
	if err := r.st.Pool().QueryRow(context.Background(), `select (select count(*) from plan_steps where issue = $1)
		+ (select count(*) from plan_tasks where issue = $1) + (select count(*) from issue_plans where issue = $1)`, r.key).Scan(&count); err != nil {
		r.t.Fatalf("count the plan rows of %s: %v", r.key, err)
	}
	return count
}
