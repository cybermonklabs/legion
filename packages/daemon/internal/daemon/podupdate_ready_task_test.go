package daemon

// An update round's replacement relaunches a phase worker that finished, in its session, the
// last task written to it. The worker takes its phase's task, answers the round from inside the
// turn of that take, and the turn's end with its report finishes the task (supervise's take),
// though the phase is not complete. The round's end sends only work still pending, so it sends
// nothing. The relaunched worker's ready is a workflow fact (claimDurableState,
// intake.ClaimReady), and that ready gives nothing to a phase worker that finished its last task
// in the session it runs (workflow's readyWorkerTask and finishedInItsSession,
// dispatch://INFRA-2811 R4): its session goes on, and the session's own resume reminder wakes it
// while its phase is open. Main's #5285 gave such a worker its phase's task again at this ready;
// R4 changes that rule, since that task is the re-send the issue removes. A worker whose phase is
// over is given nothing at that ready either. An update that keeps its pod relaunches nothing, and
// so gives nobody a task.
//
// The rig's ready is a workflow fact here as the boot makes it one (readiesAsTheBootBinds).

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// readyGivesNothingLine is the line the workflow logs for a ready worker that finished its last
// task in the session it runs, and that the ready gives nothing (readyWorkerTask).
const readyGivesNothingLine = "workflow: a ready worker that finished its last task in this session is given nothing; its own reminder wakes it"

// readyTaskDelivery is the delivery of the phase's task the worker holds when the round opens.
const readyTaskDelivery = "outbox:47"

// readiesAsTheBootBinds binds the daemon's one durable-state callback to every machine of the rig,
// as the boot binds it (workflowRuntime.attach): a claim's ready and its failure are workflow
// facts, applied by the rig's engine over the rig's records.
func (r *podUpdateRig) readiesAsTheBootBinds() {
	r.sup.OnDurableState(podUpdateWorkflow(r.pool, r.records, r.clock, r.log).claimDurableState)
}

// aWorkerWithItsPhasesTask is the implementer of aSilentPhaseWorker's fixture on a rig whose ready
// is a workflow fact, sent its phase's resume task, as the start that put it back to work sends
// it, and taking it: the assignment it holds when the round opens. It answers the task's number.
func aWorkerWithItsPhasesTask(t *testing.T) (*podUpdateRig, uint64) {
	t.Helper()
	rig := aSilentPhaseWorker(t)
	rig.readiesAsTheBootBinds()
	worker := claim.RoleImplementer
	task := resumeTaskOf(t, rig.pool, rig.engine, podUpdateIssue)
	rig.handle(worker, supervise.RequestDeliver{Claim: rig.tokens[worker], ID: readyTaskDelivery, Task: task,
		Phase: phase.Implementing, Generation: 1})
	if answer := rig.take(worker); answer.Outcome != supervise.TakeOutcomeAccepted || !strings.HasSuffix(answer.Text, task) {
		t.Fatalf("the worker's take of its phase task = %+v, want it accepted with %q", answer, task)
	}
	return rig, rig.claimOf(worker).Pending.Number
}

// answersInsideItsTask is role's turn of the take of the task it holds, the round opening over its
// pod, the worker answering the round from inside that turn, and the turn's end with its report,
// which finishes the task though the phase is not complete.
func (r *podUpdateRig) answersInsideItsTask(role claim.Role) {
	r.t.Helper()
	token := r.tokens[role]
	delivery := r.claimOf(role).Pending.ID
	r.handle(role, supervise.StreamTurnStart{Claim: token})
	r.awaitClaim(role, "the turn to take the task", func(c supervise.Claim) bool {
		return c.Pending != nil && !c.Pending.ConfirmedAt.IsZero()
	})
	r.tick()
	r.awaitRound(role, podUpdateRound)
	r.signal(role, "a1b2c3d")
	r.endTurn(role, delivery)
	r.awaitClaim(role, "the turn's end and its report to finish the task", func(c supervise.Claim) bool {
		return c.Pending == nil && c.State != supervise.StateWorking
	})
}

// replacedAndBack is the round replacing role's pod once role has answered, the round's end, and
// the worker's new process in the pod that replaced it, registered and ready.
func (r *podUpdateRig) replacedAndBack(role claim.Role) {
	r.t.Helper()
	r.awaitReplacement(1, "once the worker had answered")
	r.awaitSwept(role, "its round to end with the pod", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	r.reportsNothingRunning(role)
	r.displacedInto(role, podUpdateReplacementPod)
}

// holdsNothingPast fails the test unless role's claim holds no task and the record has written it
// no task past number, the one it took before the round.
func (r *podUpdateRig) holdsNothingPast(role claim.Role, number uint64, why string) {
	r.t.Helper()
	if c := r.settledClaim(role); c.Pending != nil || c.TaskWritten != number {
		r.t.Fatalf("the %s holds %+v with task %d written, want nothing held and no task written past %d: %s",
			role, c.Pending, c.TaskWritten, number, why)
	}
}

func TestAReplacedPhaseWorkerThatFinishedItsTaskInItsSessionIsGivenNothingWhenItsNewProcessIsReady(t *testing.T) {
	t.Parallel()
	rig, number := aWorkerWithItsPhasesTask(t)
	worker := claim.RoleImplementer
	rig.drifts(podUpdateDrift)
	rig.answersInsideItsTask(worker)
	finished := rig.settledClaim(worker)
	if finished.TaskFinished != number || finished.TaskWritten != number || finished.TaskFinishedSession != finished.Session {
		t.Fatalf("the worker records task %d of %d finished in session %q while it runs %q, want task %d finished in the session it runs",
			finished.TaskFinished, finished.TaskWritten, finished.TaskFinishedSession, finished.Session, number)
	}
	rig.replacedAndBack(worker)

	// The clock has not moved: the relaunched worker's ready is decided, and gives it nothing.
	testwait.Eventually(t, "the relaunched worker's ready to be decided", func() bool {
		return len(loggedLines(t, rig.logs.String(), readyGivesNothingLine)) > 0
	})
	for _, line := range loggedLines(t, rig.logs.String(), readyGivesNothingLine) {
		if line["issue"] != podUpdateIssue || line["role"] != string(worker) {
			t.Errorf("the ready's line is %+v, want it of issue %s and role %s", line, podUpdateIssue, worker)
		}
	}
	rig.drain()
	if got := rig.starts(); len(got) != 0 {
		t.Fatalf("the supervise rows of %s = %+v, want none: the worker finished its last task in the session it runs", podUpdateIssue, got)
	}
	rig.holdsNothingPast(worker, number, "its session goes on, and its own reminder wakes it")
}

func TestAReplacedPhaseWorkerWhosePhaseIsOverIsSentNothing(t *testing.T) {
	t.Parallel()
	rig, number := aWorkerWithItsPhasesTask(t)
	worker := claim.RoleImplementer
	rig.drifts(podUpdateDrift)
	rig.answersInsideItsTask(worker)
	// The worker's phase is over: the workflow moves the issue on to testing, which the tester
	// works, while the round replaces the pod.
	rig.movesTo(phase.Testing)
	rig.replacedAndBack(worker)

	rig.drain()
	rig.settle(worker)
	if got := rig.starts(); len(got) != 0 {
		t.Fatalf("the supervise rows of %s = %+v, want none: the implementer's phase is over", podUpdateIssue, got)
	}
	rig.holdsNothingPast(worker, number, "its phase is over")
}

func TestAnUpdateThatKeepsItsPodSendsItsWorkerNothing(t *testing.T) {
	t.Parallel()
	rig, number := aWorkerWithItsPhasesTask(t)
	worker := claim.RoleImplementer
	// The read before the replacement finds a commit that no branch or tag on origin holds, over a
	// drift the pod's roles can keep working under: the round keeps the pod (roundKeeps).
	rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundUnpushed)
	rig.drifts(podUpdateDrift)
	rig.answersInsideItsTask(worker)
	rig.roundEndsKept("over the unpushed commit", worker)

	rig.drain()
	rig.settle(worker)
	if got := rig.starts(); len(got) != 0 {
		t.Fatalf("the supervise rows of %s = %+v, want none: the pod and its worker's process are kept", podUpdateIssue, got)
	}
	rig.holdsNothingPast(worker, number, "only the task it took before the round was written to it")
}
