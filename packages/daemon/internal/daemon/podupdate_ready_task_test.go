package daemon

// An update round's replacement carries a phase worker's unfinished assignment to the worker's new
// process. The worker holds its phase's task, answers the round from inside the turn that took it,
// and that turn's end retires the task, though the phase is not complete: a confirmed delivery lives
// as long as its turn (supervise's settle). The round's end sends only work still pending, so it
// sends nothing. What gives the worker its work back is its relaunch's ready, which is a workflow
// fact (claimDurableState, intake.ClaimReady): a phase worker that holds its phase and nothing
// pending is given its phase's task again when its new process is ready (workflow's
// readyWorkerTask), once, without waiting for the silence sweep, and the sweep then finds a worker
// that holds its task and sends nothing more. A worker whose phase is over, as where the issue has
// moved on to a phase its role does not work, is given nothing at that ready. An update that keeps
// its pod relaunches nothing, and so prompts nobody.
//
// The rig's ready is a workflow fact here as the boot makes it one (readiesAsTheBootBinds).

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// readiesAsTheBootBinds binds the daemon's one durable-state callback to every machine of the rig,
// as the boot binds it (workflowRuntime.attach): a claim's ready and its failure are workflow
// facts, applied by the rig's engine over the rig's records.
func (r *podUpdateRig) readiesAsTheBootBinds() {
	r.sup.OnDurableState(podUpdateWorkflow(r.pool, r.records, r.clock, r.log).claimDurableState)
}

// aWorkerWithItsPhasesTask is the implementer of aSilentPhaseWorker's fixture on a rig whose ready
// is a workflow fact, sent its phase's resume task, as the start that put it back to work sends
// it, which reaches its connection: the assignment it holds when the round opens. It answers the
// task.
func aWorkerWithItsPhasesTask(t *testing.T) (*podUpdateRig, string) {
	t.Helper()
	rig := aSilentPhaseWorker(t)
	rig.readiesAsTheBootBinds()
	worker := claim.RoleImplementer
	task := resumeTaskOf(t, rig.pool, rig.engine, podUpdateIssue)
	rig.handle(worker, supervise.RequestDeliver{Claim: rig.tokens[worker], Task: task, Phase: phase.Implementing, Generation: 1})
	conn := rig.workers[worker]
	testwait.Eventually(t, "the worker's phase task to reach its connection", func() bool { return len(conn.Prompts()) == 1 })
	rig.awaitClaim(worker, "the task to be acknowledged", func(c supervise.Claim) bool {
		return c.Pending != nil && !c.Pending.DeliveredAt.IsZero()
	})
	return rig, task
}

// answersInsideItsTask is role's turn taking the task it holds, the round opening over its pod, the
// worker answering the round from inside that turn, and the turn's end, which retires the task
// though the phase is not complete.
func (r *podUpdateRig) answersInsideItsTask(role claim.Role) {
	r.t.Helper()
	token := r.tokens[role]
	delivery := r.workers[role].Prompts()[0].DeliveryID
	r.handle(role, supervise.StreamTurnStart{Claim: token})
	r.awaitClaim(role, "the turn to take the task", func(c supervise.Claim) bool {
		return c.Pending != nil && !c.Pending.ConfirmedAt.IsZero()
	})
	r.tick()
	r.awaitRound(role, podUpdateRound)
	r.signal(role, "a1b2c3d")
	r.endTurn(role, delivery)
	r.awaitClaim(role, "the turn's end to retire the task", func(c supervise.Claim) bool {
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

// silenceNoticesOf is every notice of the silence repair the outbox holds (workflow's
// workerSilentNotice).
func (r *podUpdateRig) silenceNoticesOf() []record.Notice {
	r.t.Helper()
	var silent []record.Notice
	for _, notice := range r.silenceNotices() {
		if notice.Kind == "worker-silent" {
			silent = append(silent, notice)
		}
	}
	return silent
}

func TestAReplacedPhaseWorkerGetsItsUnfinishedTaskOnceWhenItsNewProcessIsReady(t *testing.T) {
	t.Parallel()
	rig, task := aWorkerWithItsPhasesTask(t)
	worker := claim.RoleImplementer
	rig.drifts(podUpdateDrift)
	rig.answersInsideItsTask(worker)
	rig.replacedAndBack(worker)
	conn := rig.workers[worker]

	// The clock has not moved: what reaches the new process is the ready's, not the silence sweep's.
	testwait.Eventually(t, "the ready to queue the worker's task", func() bool { return len(rig.starts()) > 0 })
	rig.drain()
	testwait.Eventually(t, "the worker's task to reach its new process", func() bool { return len(conn.Prompts()) > 0 })
	if sent := conn.Prompts(); len(sent) != 1 || sent[0].Message != task {
		t.Fatalf("the new process was sent %+v, want the one prompt of the task the worker held, %q", sent, task)
	}

	// The silence sweep a bound later finds a worker that holds its task, and sends nothing more.
	rig.clock.advance(podUpdateSilence + time.Minute)
	rig.sweepForSilence()
	rig.drain()
	rig.settle(worker)
	if sent := conn.Prompts(); len(sent) != 1 {
		t.Fatalf("the new process was sent %d prompts once the silence sweep ran, want the one task: %+v", len(sent), sent)
	}
	if silent := rig.silenceNoticesOf(); len(silent) != 0 {
		t.Fatalf("the architect was told %+v, want no silence notice: the worker was given its task when it was ready", silent)
	}
}

func TestAReplacedPhaseWorkerWhosePhaseIsOverIsSentNothing(t *testing.T) {
	t.Parallel()
	rig, _ := aWorkerWithItsPhasesTask(t)
	worker := claim.RoleImplementer
	rig.drifts(podUpdateDrift)
	rig.answersInsideItsTask(worker)
	// The worker's phase is over: the workflow moves the issue on to testing, which the tester
	// works, while the round replaces the pod.
	rig.movesTo(phase.Testing)
	rig.replacedAndBack(worker)
	conn := rig.workers[worker]

	rig.drain()
	rig.settle(worker)
	if got := rig.starts(); len(got) != 0 {
		t.Fatalf("the supervise rows of %s = %+v, want none: the implementer's phase is over", podUpdateIssue, got)
	}
	if sent := conn.Prompts(); len(sent) != 0 {
		t.Fatalf("the new process was sent %+v, want nothing: its phase is over", sent)
	}
}

func TestAnUpdateThatKeepsItsPodSendsItsWorkerNothing(t *testing.T) {
	t.Parallel()
	rig, _ := aWorkerWithItsPhasesTask(t)
	worker := claim.RoleImplementer
	conn := rig.workers[worker]
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
	if sent := conn.Prompts(); len(sent) != 1 {
		t.Fatalf("the worker was sent %d prompts, want only the task it took before the round: %+v", len(sent), sent)
	}
}
