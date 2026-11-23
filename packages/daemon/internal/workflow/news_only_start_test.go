package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A start queued for news of the approved design alone (record.SuperviseRequest.News) names its
// member's phase only so that it finishes once the member has left that phase. It carries no task
// of the phase, and it finishes without acting at a claim that failed (the daemon's
// newsOnlyRefusal). So no rule that asks whether a queued start brings a role its phase's work
// counts it (startsWork), and each reads a role with only such a start queued as it reads one
// with nothing queued:
//
//   - a failed claim is one a start must bring back (StartFor, which the promotion and the report
//     of the claims nothing will wake read);
//   - a worker up and between turns, holding nothing, is silent (SilentPhaseWorker, the silence
//     repair's rule);
//   - an architect that holds nothing still to take owes nothing of the work it was given (owed,
//     the root's wait on a person).
//
// A start of the phase that carries its task is counted, for each.
func TestAStartQueuedForNewsAloneIsNoStartOfItsPhase(t *testing.T) {
	silent := aSilentImplementer()
	newsOnly := record.QueuedSupervise{ID: 9, Request: record.SuperviseRequest{Op: "start", Tree: silent.root.Key,
		Role: claim.RoleImplementer, Generation: silent.issue.Generation, Phase: silent.issue.Phase, News: true}}
	phaseStart := newsOnly
	phaseStart.Request.News, phaseStart.Request.Task, phaseStart.Request.Entry = false, "implement the plan", 2

	for _, tc := range []struct {
		name   string
		queued []record.QueuedSupervise
		// starts is whether the queued rows hold a start that brings the role its phase's work.
		starts bool
	}{
		{name: "nothing queued"},
		{name: "a start queued for news alone", queued: []record.QueuedSupervise{newsOnly}},
		{name: "a start of the phase carrying its task", queued: []record.QueuedSupervise{phaseStart}, starts: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failed := record.RoleRun{Queued: tc.queued, Claim: &record.RoleClaim{State: supervise.StateFailed, Tree: silent.root.Key}}
			if got := StartFor(failed, silent.root, silent.issue.Generation, silent.issue.Phase); got == tc.starts {
				t.Errorf("StartFor of a failed claim = %t with %s, want %t: a failed claim is brought back by a start of its phase alone", got, tc.name, !tc.starts)
			}

			worker := aSilentImplementer()
			worker.run.Queued = tc.queued
			if got := worker.answer(); got == tc.starts {
				t.Errorf("SilentPhaseWorker = %t with %s, want %t: a worker between turns holding nothing is silent until a start of its phase is queued", got, tc.name, !tc.starts)
			}

			architect := record.RoleRun{Queued: tc.queued, Claim: &record.RoleClaim{State: supervise.StateIdle, Tree: silent.root.Key}}
			if why, got := owed(architect); got != tc.starts {
				t.Errorf("owed = %t (%q) with %s, want %t: an idle architect holding nothing owes the work only of a start that gives it some", got, why, tc.name, tc.starts)
			}
		})
	}
}
