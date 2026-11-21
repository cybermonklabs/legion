package daemon

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/supervise/supervisetest"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// aliveObservation is the runtime's sweep finding the claim's process running, as the daemon
// posts it.
func aliveObservation(m *supervise.Machine) supervise.RuntimeObservation {
	return supervise.RuntimeObservation{Observation: runtime.Observation{Locator: *m.Claim().Locator, Kind: runtime.Alive, At: time.Now()}}
}

// A claim a daemon start restored working stands in the turn an earlier daemon saw start
// (supervise's askFirst), and it holds the issue's working copy for that turn as any working claim
// does (claimInTurn), so the tester's take of its phase task is refused issue-in-turn. The sweep's
// ask finding the agent in no turn ends that turn (supervise's askTurnOver), which reaches the
// machine on an observation rather than a turn end, and the daemon tells the issue's other claims
// of it by the count of turns the machine has ended (supervise.Machine.TurnsEnded, wakeIssue): the
// tester's value is written again at once, which is the value its worker asks to take at, and its
// take is accepted then, not a sweep later.
func TestASweepEndedTurnReleasesTheIssuesHeldTake(t *testing.T) {
	rig := newIssueRig(t)
	values := supervisetest.NewTaskValues()
	rig.sup.deps.TaskValues = values
	tester, waiting := rig.ready(claim.RoleTester, fake.NewConn())

	// The implementer as a daemon start restores it: stored working, its process alive, and its
	// agent still streaming the turn the earlier daemon saw start.
	implementer, err := claim.NewToken(rig.project, "LEGION-209", claim.RoleImplementer)
	if err != nil {
		t.Fatalf("the token of the implementer: %v", err)
	}
	locator, err := rig.rt.Spawn(rig.ctx, runtime.SpawnSpec{Claim: implementer, Role: claim.RoleImplementer})
	if err != nil {
		t.Fatalf("mint the implementer's locator: %v", err)
	}
	implementerConn := fake.NewConn()
	implementerConn.SetStreaming(true)
	rig.conns.Register(implementer, implementerConn)
	if _, err := rig.sup.restore(rig.ctx, []supervise.Claim{{Token: implementer, Project: rig.project, Tree: "LEGION-208",
		Issue: "LEGION-209", Role: claim.RoleImplementer, Generation: 1, State: supervise.StateWorking, Locator: &locator,
		Session: "ses-implementer", SessionFile: "/sessions/implementer.jsonl", PluginContract: api.DaemonAPIVersion}}); err != nil {
		t.Fatalf("restore the working implementer: %v", err)
	}
	writing, found := rig.sup.Machine(implementer)
	if !found {
		t.Fatalf("the restore left no machine for %s", implementer)
	}

	if err := waiting.Handle(rig.ctx, supervise.RequestDeliver{Claim: tester, Task: "test the branch", ID: "outbox:2",
		Phase: phase.Testing, Generation: 1}); err != nil {
		t.Fatalf("deliver the tester's task: %v", err)
	}
	adoptions := rig.adoptions()
	// The worker asks at the value the queue wrote, so the hold's end writes it again.
	current, err := waiting.CurrentTask(rig.ctx)
	if err != nil || current.Value == nil {
		t.Fatalf("the tester's value after its task was queued = %+v, %v, want one written", current, err)
	}
	held := waiting.Claim()
	answer, err := waiting.Take(rig.ctx, supervise.RequestTake{Claim: tester, Generation: held.Generation, Session: held.Session,
		Number: held.Pending.Number, Revision: supervise.TaskRevision{Epoch: current.Value.Epoch, Revision: current.Value.Revision}})
	if err != nil || answer.Outcome != supervise.TakeOutcomeRefused || answer.Hold != supervise.HoldIssueInTurn {
		t.Fatalf("the tester's take while the restored implementer's turn was open = %+v, %v, want it refused %s", answer, err, supervise.HoldIssueInTurn)
	}
	written := valueWritesOf(values, tester)

	// The agent's turn ends, and the daemon never reads its agent_end: only the sweep's ask finds
	// it over.
	implementerConn.SetStreaming(false)
	rig.sup.post(implementer, aliveObservation(writing))

	testwait.Eventually(t, "the tester's value to be written again once the sweep ended the implementer's turn", func() bool {
		return valueWritesOf(values, tester) > written
	})
	if view := writing.View(); view.State != supervise.StateIdle {
		t.Fatalf("the implementer is %s after the sweep found its turn over, want it idle", view.State)
	}
	rig.wantTakenOnceTheTurnEnded(t, waiting, adoptions)
}

// valueWritesOf is how many times values was written token's value.
func valueWritesOf(values *supervisetest.TaskValues, token claim.Token) int {
	written := 0
	for _, write := range values.Writes() {
		if write.Token == token {
			written++
		}
	}
	return written
}
