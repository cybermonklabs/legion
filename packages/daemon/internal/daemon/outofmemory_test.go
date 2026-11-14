package daemon

// The note a role gets when its pod ran out of memory, end to end over the real supervision: a
// real Postgres, real machines and a faked runtime, as the update-round tests run (the rig is
// podupdate_test.go's). The note is written on the delivery the end took back, so the daemon that
// answers the task's next take may be another one entirely — which is what the delivery's own
// column is for, and why this test restarts the daemon before its worker takes the task again.

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// outOfMemoryTools is where the rule about the work an issue pod does not run is written, which
// is what the note sends the role to read.
const outOfMemoryTools = "skill://legion-worker/references/tools.md"

// A role whose turn ended with its pod running out of memory is told so with the task it takes
// again: the agent is given no reason by the kubelet that killed its container, so the take's
// text carries the note and the pointer to the reference that says what an issue pod does not
// run. The note belongs to the task the end cut off and to nothing else, so the role that held no
// task is given nothing. It survives a restart of the daemon, because the end is written on the
// delivery in the store and the daemon that relaunches the role may not be the one that saw the
// end: this test posts the end to one daemon and reads the text the next one answers the take with.
func TestAnOutOfMemoryEndTellsTheRoleWhoseTurnItCutAcrossARestart(t *testing.T) {
	const task = "Continue Workflow. Issue: LEGION-484. Phase: implementing."
	worked, idle := claim.RoleImplementer, claim.RoleTester
	rig := newPodUpdateRig(t, readyWorker(worked), readyWorker(idle))

	rig.handle(worked, supervise.RequestDeliver{Claim: rig.tokens[worked], Task: task, ID: "outbox:1229",
		Phase: phase.Implementing, Generation: 1})
	if answer := rig.take(worked); answer.Outcome != supervise.TakeOutcomeAccepted {
		t.Fatalf("the %s's take of its task = %+v, want it accepted", worked, answer)
	}
	rig.handle(worked, supervise.StreamTurnStart{Claim: rig.tokens[worked]})
	if c := rig.claimOf(worked); c.Pending == nil || c.Pending.ConfirmedAt.IsZero() {
		t.Fatalf("the %s holds %+v in state %s, want a confirmed delivery it is working", worked, c.Pending, c.State)
	}
	if c := rig.claimOf(idle); c.Pending != nil {
		t.Fatalf("the %s holds %+v, want no pending delivery at all", idle, c.Pending)
	}

	// The pod's one container runs every role of the issue, so its out-of-memory end is every
	// role's process gone, each observed Gone with the mark the runtime read off the kubelet's
	// reason. Each relaunch comes up in the pod that replaced it.
	for _, role := range []claim.Role{worked, idle} {
		token := rig.tokens[role]
		rig.rt.ScriptResume(fake.SpawnResult{Locator: *podUpdateLocator(token, podUpdateIssue, podUpdateReplacementPod)})
		rig.handle(role, supervise.RuntimeObservation{Observation: runtime.Observation{
			Locator: *rig.claimOf(role).Locator, Kind: runtime.Gone, At: rig.clock.Now(), OutOfMemory: true,
			Detail: "pod legion-legion-484 (uid " + podUpdatePod + ") container roles terminated (OOMKilled, exit code 128)",
		}})
		rig.awaitClaim(role, "its relaunch into the pod that replaced the one that ran out of memory",
			func(c supervise.Claim) bool { return podOf(c.Locator) == podUpdateReplacementPod })
	}

	next := rig.restart()

	if p := next.claimOf(worked).Pending; p == nil || !p.Interrupted || !p.OutOfMemory {
		t.Fatalf("the daemon that restarted read back %+v, want the task waiting, interrupted by an out-of-memory end", p)
	}
	for _, role := range []claim.Role{worked, idle} {
		next.registerInto(role, podUpdateReplacementPod)
		next.settle(role)
	}

	answer := next.take(worked)
	if answer.Outcome != supervise.TakeOutcomeAccepted {
		t.Fatalf("the relaunched %s's take = %+v, want its task accepted", worked, answer)
	}
	sent := answer.Text
	switch {
	case !strings.Contains(sent, task):
		t.Errorf("the take's text does not carry the task %q: %q", task, sent)
	case !strings.HasPrefix(sent, "Your previous turn on this task was interrupted"):
		t.Errorf("the take's text does not begin with the interrupted sentence: %q", sent)
	case !strings.Contains(sent, "ran out of memory"):
		t.Errorf("the take's text does not say the pod ran out of memory: %q", sent)
	case !strings.Contains(sent, outOfMemoryTools):
		t.Errorf("the take's text does not point at %s: %q", outOfMemoryTools, sent)
	}
	if held := next.claimOf(idle).Pending; held != nil {
		t.Fatalf("the %s, which held no task, holds %+v; the note belongs to the turn the end cut off", idle, held)
	}
}
