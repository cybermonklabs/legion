// The experiment's second group through the whole daemon over the real Dispatch of this
// package's tests (ownerStepsRig, owner_steps_rig_test.go): an issue whose group runs the
// project's default steps takes a later version that changes only the text of its running task
// default, and Legion answers every other plan its owner gives with PLAN_NOT_TAKEN.
package daemon

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/experiment"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// secondGroup is the row of the experiment planning that an issue holds when it is drawn into
// the arm b, whose issues run the project's default steps.
var secondGroup = record.Assignment{Experiment: "planning", Variant: "b", Plan: string(config.PlanGroupDefault)}

// drawIssue records the draw of the walk's issue over the experiments its project declares, with
// the function admission draws a root with when it records it (experiment.DrawAndRecord in
// putNewRoot). The rig records each issue at admitted itself (seedAdmitted), as admission records
// a root, and so no draw of admission's reaches it.
func (r *ownerStepsRig) drawIssue() {
	r.t.Helper()
	if err := store.Transact(context.Background(), r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return experiment.DrawAndRecord(ctx, tx, r.records, r.key, r.cfg.ExperimentsDeclared(), config.ExperimentIssue, experiment.CryptoSource, time.Now())
	}); err != nil {
		r.t.Fatalf("draw %s: %v", r.key, err)
	}
}

// planningRow is the walk's issue's row of the experiment planning in the daemon's store: its
// arm and the arm's plan.
func (r *ownerStepsRig) planningRow() record.Assignment {
	r.t.Helper()
	row := record.Assignment{Experiment: "planning"}
	if err := r.st.Pool().QueryRow(context.Background(), `select variant, plan from experiment_assignments
		where issue_key = $1 and experiment = 'planning'`, r.key).Scan(&row.Variant, &row.Plan); err != nil {
		r.t.Fatalf("read the row of the experiment planning of %s: %v", r.key, err)
	}
	return row
}

// The project declares the experiment planning at mode on at its arm b, so the experiment's whole
// share is on the second group, whose issues run the project's default steps. The issue drawn
// there starts at planning. While its implementer works, its owner gives plan.md with the one
// block default and a new text. register_plan answers 200 and names step 2, the implementer. The
// issue keeps its group, the version keeps the source default, and the tester's and the
// reviewer's start tasks hold the new text and the project's default steps. While the reviewer
// works, the owner gives a version whose block
// default names steps. Legion refuses it PLAN_NOT_TAKEN, not PLAN_REFUSED, and keeps the version
// in force. The control takes out the exception for a change of the text alone: Legion then
// refuses the first version PLAN_NOT_TAKEN, and the test fails there.
func TestTheSecondGroupTakesAChangeOfTheTextOfDefault(t *testing.T) {
	t.Parallel()
	r := newOwnerStepsRig(t, newLiveProofOn(t, workflowNATS(t)), startScratchDispatchOn(t, scratchDispatchOptions{}), 1,
		config.Project{Experiments: []config.Experiment{planningExperiment(config.ExperimentOn, "b")}})
	const was, now = "The project's default steps.", "Start the review with the changelog entry."
	r.drawIssue()
	if got := r.planningRow(); got != secondGroup {
		t.Fatalf("%s was drawn into %+v, want %+v: the experiment's whole share is on its arm b", r.key, got, secondGroup)
	}
	r.openGate(phase.Planning)
	r.plan()
	implementer := r.worker(claim.RoleImplementer)
	holdsText(t, "implementer", takeText(implementer), was, now)

	document := r.document("plan.md", textPlan("default", "", now))
	if got, want := r.registeredRunning(document, 1)["default"], []runningAct{{Kind: "step", Place: 2, Role: string(claim.RoleImplementer)}}; !slices.Equal(got, want) {
		t.Fatalf("register_plan of %s's version 1 names default running at %+v, want %+v: step 2, the implementer", r.key, got, want)
	}
	if got := r.planningRow(); got != secondGroup {
		t.Fatalf("%s holds %+v after its owner's version 1, want it still in %+v", r.key, got, secondGroup)
	}
	var source string
	if err := r.st.Pool().QueryRow(context.Background(), `select source from issue_plans where issue = $1 and generation = 1 and version = 1`, r.key).
		Scan(&source); err != nil || source != record.PlanSourceDefault {
		t.Fatalf("version 1 of %s's plan is stored with the source %q (%v), want %q: the issue still runs the project's default steps", r.key, source, err, record.PlanSourceDefault)
	}

	if !r.github.opened(r.number) {
		r.github.open("legion/"+r.key, r.number)
	}
	r.finish(implementer, "implement", api.HandoffCompleteRequest{Summary: "implemented", Commit: stepRecordsHead}, phase.Testing)
	tester := r.worker(claim.RoleTester)
	holdsText(t, "tester", takeText(tester), now, was)
	r.finish(tester, "test", testCompletion, phase.Reviewing)
	task := takeText(r.worker(claim.RoleReviewer))
	holdsText(t, "reviewer", task, now, was)
	if steps := " Steps: 1 planner, 2 implementer, 3 tester, 4 reviewer, 5 acceptance:owner then"; !strings.Contains(task, "Task: default. Step 4 of 5: reviewer.") ||
		!strings.Contains(task, steps) {
		t.Fatalf("the reviewer's start task = %q, want step 4 of the task default on the project's default steps", task)
	}

	r.editPlan(document, map[string]any{"op": "retype", "block": "default", "type": "plan_task",
		"attributes": map[string]any{"steps": "implementer reviewer"}}, 2)
	status, body := r.registerPlan(document, 2)
	var refusal api.Failure
	want := intake.PlanNotTaken(r.key, secondGroup)
	if err := json.Unmarshal([]byte(body), &refusal); err != nil || status != want.Status || refusal != (api.Failure{Code: want.Code, Error: want.Message}) {
		t.Fatalf("register_plan of version 2, whose block default names steps, = %d %s, want %d %s %q", status, body, want.Status, want.Code, want.Message)
	}
	var newest int
	if err := r.st.Pool().QueryRow(context.Background(), `select max(version) from issue_plans where issue = $1 and generation = 1`, r.key).Scan(&newest); err != nil || newest != 1 {
		t.Fatalf("the newest version of %s's plan after the refusal = %d (%v), want version 1 in force", r.key, newest, err)
	}
}
