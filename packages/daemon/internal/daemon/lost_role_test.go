// A restart of the whole daemon under settings that no longer define a role of the deployment: the
// daemon boots, and each task that still has a step of that role to run stops, while an issue of
// the default steps goes on.
//
// The test boots the whole daemon over the package's broker, the real store, the real outbox and
// the real Dispatch of this package's tests, with the fake runtime and the fake GitHub, and walks
// each issue as the whole-daemon tests of a deployment's roles walk it (deploymentRolesRig,
// deployment_roles_test.go). The restart is the rig's: the same store, the same Dispatch and the
// same ports, each pane outliving the daemon as a tmux pane does, with the settings' roles changed.
package daemon

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A task whose role the deployment no longer defines stops. The daemon boots with checker defined;
// issue A stands at step 2 of `implementer checker` with its checker at work, issue B at step 1 of
// the same plan with checker next, and issue C of the default steps has its planner at work. The
// daemon restarts with checker taken out
// of the settings. It boots; A's task and B's are stopped with `step 2 names the role checker,
// which the deployment no longer defines`, each issue held task-stopped from the phase its task
// stood in, its owner told once; A's checker claim, in its turn, holds the daemon's suspension for
// that turn's end and is suspended with its session kept once the turn ends; and C goes on to
// done, its retro rating the plan 4 with its reason, which the retro's step record names.
//
// The fault: a restart under settings that lack a role leaves the role's tasks running. The
// control is a mutation of the release's code, run by hand, since the read tree cannot be the
// control (it refuses a file with `roles:` at its load, and takes no claim of checker): booted
// without its call of stopLostRoles (workflow/resume.go, after sweepMerged). At the restart A's
// task is then not stopped and holds no task-stopped hold, and the assertion of A's stop fails.
func TestARestartWithoutARoleStopsItsTasksAndBoots(t *testing.T) {
	t.Parallel()
	roles := deploymentRoles(t)
	r := newDeploymentRolesRig(t, newLiveProofOn(t, workflowNATS(t)), startScratchDispatchOn(t, scratchDispatchOptions{}), roles,
		[]string{"Running checker loses its role", "Implementer waits before dropped verifier", "Default walk finishes after restart"})
	running, waiting, byDefault := r.issues[0].key, r.issues[1].key, r.issues[2].key

	r.on(running)
	r.givePlan("implementer", "checker")
	if first := r.openGate(); first != phase.Implementing {
		t.Fatalf("%s, given `implementer checker`, opened its gate at %s, want implementing", running, first)
	}
	r.work(claim.RoleImplementer, "implement", api.HandoffCompleteRequest{Summary: "implemented", Commit: stepRecordsHead}, phase.Step)
	r.worker(deploymentChecker).take()

	r.on(waiting)
	r.givePlan("implementer", "checker")
	if first := r.openGate(); first != phase.Implementing {
		t.Fatalf("%s, given `implementer checker`, opened its gate at %s, want implementing", waiting, first)
	}
	r.worker(claim.RoleImplementer).take()

	r.on(byDefault)
	if first := r.openGate(); first != phase.Planning {
		t.Fatalf("%s, given no plan, opened its gate at %s, want planning", byDefault, first)
	}
	planner := r.worker(claim.RolePlanner)
	planner.take()

	withoutChecker := slices.DeleteFunc(slices.Clone(roles), func(role config.Role) bool { return role.Name == deploymentChecker })
	r.restartWith(withoutChecker, running, waiting, byDefault)

	const why = "step 2 names the role checker, which the deployment no longer defines"
	for _, stopped := range []struct {
		key  string
		from phase.Phase
	}{{running, phase.Step}, {waiting, phase.Implementing}} {
		r.on(stopped.key)
		if err := r.taskStoppedHolds(stopped.from, why); err != nil {
			t.Fatal(err)
		}
		said := fmt.Sprintf("task a of %s was stopped: %s", stopped.key, why)
		if notices := r.waitArchitectNotices(stopped.key, "task-stopped", 1); notices[0].Payload != said {
			t.Fatalf("%s's owner was told %q, want %q", stopped.key, notices[0].Payload, said)
		}
		// The notice rides the outbox: a second one would land after the first.
		time.Sleep(liveSettle)
		if notices := r.architectNotices(stopped.key, "task-stopped"); len(notices) != 1 {
			t.Fatalf("%s's owner was told %d task-stopped notices, want one", stopped.key, len(notices))
		}
	}
	// The checker was in its turn when its task stopped, and a suspension of a claim in a turn is
	// held for that turn's end (supervise's holdSuspension): the daemon's suspension is held, and
	// once the checker's agent ends its turn the claim is suspended, its session kept.
	r.on(running)
	checker := r.worker(deploymentChecker)
	var got api.OperatorClaim
	if !testwait.Holds(stepRecordsWait, 100*time.Millisecond, func() bool {
		got = r.d.claim(checker.token)
		return got.State == string(supervise.StateWorking) && got.SuspensionHeld && got.SuspensionHeldBy == string(supervise.SuspendedByDaemon)
	}) {
		t.Fatalf("%s is %s with a suspension held %v by %q after its task stopped, want working with the daemon's suspension held for its turn",
			checker.token, got.State, got.SuspensionHeld, got.SuspensionHeldBy)
	}
	checker.endTurn()
	if !testwait.Holds(stepRecordsWait, 100*time.Millisecond, func() bool {
		got = r.d.claim(checker.token)
		return got.State == string(supervise.StateSuspended) && got.SuspendedBy == string(supervise.SuspendedByDaemon)
	}) {
		t.Fatalf("%s is %s by %q once its turn ended, want suspended by the daemon", checker.token, got.State, got.SuspendedBy)
	}
	if got.Session != checker.session {
		t.Fatalf("%s holds the session %q once suspended, want its own %s kept", checker.token, got.Session, checker.session)
	}

	r.on(byDefault)
	// A restarted daemon knows no model until the agent answers again (supervise.StreamAnswered).
	planner.ownTurn()
	r.finish(planner, "plan", api.HandoffCompleteRequest{Summary: "planned"}, phase.Implementing)
	r.implement()
	r.test()
	r.review("APPROVED", phase.Accepting)
	r.accept()
	r.retro()
	r.merge()
	waitDeploymentSteps(r.stepRecordsRig, "default", true, []deploymentStepWant{
		{kind: "step", step: 1, role: "planner", typ: "planning", model: stepRecordsModel},
		{kind: "step", step: 2, role: "implementer", typ: "implementation", model: stepRecordsModel},
		{kind: "step", step: 3, role: "tester", typ: "testing", model: stepRecordsModel},
		{kind: "step", step: 4, role: "reviewer", typ: "review", model: stepRecordsModel},
		{kind: "step", step: 5, role: "acceptance", typ: "review", model: stepRecordsModel},
		{kind: "retro", role: "implementer"},
		{kind: "merge", role: "merger"},
	})
}

// taskStoppedHolds waits for the walk's issue to stand held task-stopped from from, its task a
// stopped with why in the daemon's store, and answers what does not hold once the wait runs out.
func (r *deploymentRolesRig) taskStoppedHolds(from phase.Phase, why string) error {
	r.t.Helper()
	var last error
	if testwait.Holds(stepRecordsWait, 100*time.Millisecond, func() bool {
		issue := r.d.state().Issues[r.key]
		var state, reason string
		if err := r.st.Pool().QueryRow(context.Background(), `select state, stopped_reason from plan_tasks
			where issue = $1 and generation = $2 and task = 'a'`, r.key, int64(issue.Generation)).Scan(&state, &reason); err != nil {
			last = fmt.Errorf("read %s's task a: %w", r.key, err)
			return false
		}
		switch {
		case issue.Phase != phase.Held || issue.HoldReason != string(record.HoldTaskStopped) || issue.HeldFrom != string(from):
			last = fmt.Errorf("%s stands in %s held %q from %q, want held %s from %s", r.key, issue.Phase, issue.HoldReason, issue.HeldFrom, record.HoldTaskStopped, from)
		case state != "stopped" || reason != why:
			last = fmt.Errorf("%s's task a is %s with the reason %q, want stopped with %q", r.key, state, reason, why)
		default:
			last = nil
		}
		return last == nil
	}) {
		return nil
	}
	return last
}
