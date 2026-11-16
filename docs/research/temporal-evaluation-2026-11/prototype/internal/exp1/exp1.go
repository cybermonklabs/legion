// Package exp1 is experiment 1 of the Temporal evaluation: Legion's own issue lifecycle
// as a single workflow driven by signals.
//
// Legion today spreads that lifecycle over a declarative transition table, an engine
// that interprets it, a durable outbox and a set of records in SQLite. The question this
// experiment answers is a narrow one: can the same lifecycle be a workflow function, and
// does the run's own history then hold the evidence that the issue went backwards and
// entered a phase twice? Nothing here is a claim that the rest of the engine's work --
// guards over record state, effects, statuses -- comes for free.
//
// The workflow is also experiment 6's subject: it is the thing whose history is
// recorded, changed and replayed.
package exp1

import (
	"context"
	"fmt"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// The registered names of this experiment's pieces. They are strings because experiment
// 6 registers a changed workflow under the same name to replay this one's history.
const (
	WorkflowName           = "exp1PhaseWorkflow"
	RolePromptActivityName = "exp1RolePrompt"
	// TriggerSignal carries one TriggerKind of Legion's table, by name.
	TriggerSignal = "trigger"
)

// The phases, named exactly as packages/daemon/internal/phase names them.
const (
	PhaseAdmitted      = "admitted"
	PhasePlanning      = "planning"
	PhaseImplementing  = "implementing"
	PhaseTesting       = "testing"
	PhaseReviewing     = "reviewing"
	PhaseAccepting     = "accepting"
	PhaseRetro         = "retro"
	PhaseMerging       = "merging"
	PhaseAwaitingMerge = "awaiting_merge"
	PhaseDone          = "done"
)

// Edge is one row's key: the phase an issue stands in and the trigger it observes.
type Edge struct {
	From    string
	Trigger string
}

// Table is the subset of packages/daemon/internal/workflow.Table this experiment drives:
// the forward path from admitted to done, plus the backward edge from reviewing to
// implementing that a rejected review takes. It is data, as Legion's own table is; the
// workflow function below is the whole interpreter.
var Table = map[Edge]string{
	{From: PhaseAdmitted, Trigger: "gate_opened"}:              PhasePlanning,
	{From: PhasePlanning, Trigger: "planner_completed"}:        PhaseImplementing,
	{From: PhaseImplementing, Trigger: "implementation_ready"}: PhaseTesting,
	{From: PhaseTesting, Trigger: "tester_passed"}:             PhaseReviewing,
	{From: PhaseTesting, Trigger: "tester_failed"}:             PhaseImplementing,
	{From: PhaseReviewing, Trigger: "review_approved"}:         PhaseAccepting,
	{From: PhaseReviewing, Trigger: "review_rejected"}:         PhaseImplementing,
	{From: PhaseAccepting, Trigger: "sign_off"}:                PhaseRetro,
	{From: PhaseRetro, Trigger: "retro_completed"}:             PhaseMerging,
	{From: PhaseMerging, Trigger: "ready"}:                     PhaseAwaitingMerge,
	{From: PhaseAwaitingMerge, Trigger: "pull_request_merged"}: PhaseDone,
}

// Role is the agent role a phase prompts when an issue enters it. The phases where the
// issue only waits -- awaiting_merge, done -- prompt nobody, so they have no entry.
var Role = map[string]string{
	PhasePlanning:     "planner",
	PhaseImplementing: "implementer",
	PhaseTesting:      "tester",
	PhaseReviewing:    "reviewer",
	PhaseAccepting:    "architect",
	PhaseRetro:        "implementer",
	PhaseMerging:      "merger",
}

// Script is the triggers the driver sends, in order: the forward path with one rejected
// review, which sends the issue from reviewing back to implementing and so through
// testing a second time.
var Script = []string{
	"gate_opened",
	"planner_completed",
	"implementation_ready",
	"tester_passed",
	"review_rejected",
	"implementation_ready",
	"tester_passed",
	"review_approved",
	"sign_off",
	"retro_completed",
	"ready",
	"pull_request_merged",
}

// Params is one issue's run.
type Params struct {
	// IssueID is the issue the workflow stands for, which the role prompts carry.
	IssueID string
}

// Result is what the run did: where it ended, every phase it stood in, and how much it
// took to get there.
type Result struct {
	// Phase is the phase the workflow ended in.
	Phase string
	// Path is every phase the issue stood in, admitted first, in order.
	Path []string
	// Triggers is how many trigger signals the workflow consumed.
	Triggers int
	// Prompts is how many role prompts ran: one per entry into a phase that has a role.
	Prompts int
	// Ignored is the triggers no row of the table accepted in the phase they arrived in.
	Ignored int
}

// PhaseWorkflow is Legion's issue lifecycle as one workflow: it stands in a phase, waits
// for the trigger that moves it, prompts the role the new phase belongs to, and stops
// when it reaches done. The whole interpreter is this function; the rows are data.
func PhaseWorkflow(ctx workflow.Context, p Params) (*Result, error) {
	res := &Result{Phase: PhaseAdmitted, Path: []string{PhaseAdmitted}}
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
	triggers := workflow.GetSignalChannel(ctx, TriggerSignal)
	for res.Phase != PhaseDone {
		var trigger string
		triggers.Receive(ctx, &trigger)
		res.Triggers++
		to, ok := Table[Edge{From: res.Phase, Trigger: trigger}]
		if !ok {
			res.Ignored++
			continue
		}
		res.Phase = to
		res.Path = append(res.Path, to)
		role, prompts := Role[to]
		if !prompts {
			continue
		}
		var reply string
		if err := workflow.ExecuteActivity(ctx, RolePromptActivityName, p.IssueID, to, role).Get(ctx, &reply); err != nil {
			return res, err
		}
		res.Prompts++
	}
	return res, nil
}

// RolePrompt stands for what Legion does on entering a phase: one prompt into an agent
// session. The experiment measures how many of them a lifecycle costs, not what they do,
// so the activity only reports who was prompted about what.
func RolePrompt(ctx context.Context, issueID, phase, role string) (string, error) {
	return fmt.Sprintf("issue=%s phase=%s role=%s attempt=%d", issueID, phase, role, activity.GetInfo(ctx).Attempt), nil
}

// Register puts the workflow and its activity on a worker under the names history
// records them by.
func Register(w worker.Worker) {
	w.RegisterWorkflowWithOptions(PhaseWorkflow, workflow.RegisterOptions{Name: WorkflowName})
	w.RegisterActivityWithOptions(RolePrompt, activity.RegisterOptions{Name: RolePromptActivityName})
}
