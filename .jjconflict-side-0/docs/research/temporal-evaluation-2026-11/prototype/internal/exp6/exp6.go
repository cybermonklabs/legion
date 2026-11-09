// Package exp6 is experiment 6 of the Temporal evaluation: change and replay, and what
// a run costs in Actions.
//
// Legion's phase machine changes often. Under Temporal a change to a workflow function
// meets every run already in flight, which replays its history against the new code: a
// step inserted in the middle is a non-determinism error unless it is placed behind
// workflow.GetVersion. This experiment records a real run's history, replays it against
// both versions of the changed code, and shows which one survives.
//
// It then counts the same run's Actions twice: from the server's own action metric, and
// by mapping the recorded history onto Temporal Cloud's published list of billable
// Actions. The difference between the two is the point. Every cost estimate for adopting
// Temporal rests on a mapping like the second one, so a gap between it and the server's
// own count is a gap in every such estimate.
package exp6

import (
	"time"

	"go.temporal.io/sdk/workflow"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp1"
)

// LintActivityName is the step the change adds: one more prompt per phase, of the kind
// an ordinary Legion change would add.
const LintActivityName = "exp6LintPrompt"

// VersionChangeID names the change in the workflow's own history, which is what lets an
// old run keep the old path and a new run take the new one.
const VersionChangeID = "exp6-lint-prompt"

// ChangedPhaseWorkflow is exp1.PhaseWorkflow after an ordinary change: a lint prompt
// runs before every role prompt. The change is not versioned, so replaying an older
// run's history against it must fail.
func ChangedPhaseWorkflow(ctx workflow.Context, p exp1.Params) (*exp1.Result, error) {
	res := &exp1.Result{Phase: exp1.PhaseAdmitted, Path: []string{exp1.PhaseAdmitted}}
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
	triggers := workflow.GetSignalChannel(ctx, exp1.TriggerSignal)
	for res.Phase != exp1.PhaseDone {
		var trigger string
		triggers.Receive(ctx, &trigger)
		res.Triggers++
		to, ok := exp1.Table[exp1.Edge{From: res.Phase, Trigger: trigger}]
		if !ok {
			res.Ignored++
			continue
		}
		res.Phase = to
		res.Path = append(res.Path, to)
		role, prompts := exp1.Role[to]
		if !prompts {
			continue
		}
		var linted string
		if err := workflow.ExecuteActivity(ctx, LintActivityName, p.IssueID, to).Get(ctx, &linted); err != nil {
			return res, err
		}
		var reply string
		if err := workflow.ExecuteActivity(ctx, exp1.RolePromptActivityName, p.IssueID, to, role).Get(ctx, &reply); err != nil {
			return res, err
		}
		res.Prompts++
	}
	return res, nil
}

// VersionedPhaseWorkflow is the same change behind workflow.GetVersion: a history
// recorded before the change holds no marker for it, so the old run keeps the old path
// and replays cleanly, while a run started after the change takes the new one.
func VersionedPhaseWorkflow(ctx workflow.Context, p exp1.Params) (*exp1.Result, error) {
	res := &exp1.Result{Phase: exp1.PhaseAdmitted, Path: []string{exp1.PhaseAdmitted}}
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
	triggers := workflow.GetSignalChannel(ctx, exp1.TriggerSignal)
	for res.Phase != exp1.PhaseDone {
		var trigger string
		triggers.Receive(ctx, &trigger)
		res.Triggers++
		to, ok := exp1.Table[exp1.Edge{From: res.Phase, Trigger: trigger}]
		if !ok {
			res.Ignored++
			continue
		}
		res.Phase = to
		res.Path = append(res.Path, to)
		role, prompts := exp1.Role[to]
		if !prompts {
			continue
		}
		if workflow.GetVersion(ctx, VersionChangeID, workflow.DefaultVersion, 1) >= 1 {
			var linted string
			if err := workflow.ExecuteActivity(ctx, LintActivityName, p.IssueID, to).Get(ctx, &linted); err != nil {
				return res, err
			}
		}
		var reply string
		if err := workflow.ExecuteActivity(ctx, exp1.RolePromptActivityName, p.IssueID, to, role).Get(ctx, &reply); err != nil {
			return res, err
		}
		res.Prompts++
	}
	return res, nil
}
