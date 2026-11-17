package daemon

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// launchesSince is every launch the runtime was asked for of token's process, in order, among the
// calls rt recorded after its first after: each a Spawn, which starts a fresh session, or a
// Resume, which continues the session its spec names (launchesOf reads the two apart).
func launchesSince(rt *fake.Runtime, token claim.Token, after int) []fake.Call {
	var launches []fake.Call
	for _, call := range rt.Calls()[after:] {
		if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == token {
			launches = append(launches, call)
		}
	}
	return launches
}

// A child's architect leaves the workflow with its child and comes back with it. The park_child
// of the architect above the child (its own write of backlog) takes the child out of the workflow,
// which suspends every claim of the child (the workflow's leave), the child's architect's among
// them: the claim stays the child's, suspended by the daemon with the session its agent
// registered, and its process is stopped once. That architect's rerun_child (its own write of todo
// on the parked child) starts the child's first phase again, and that start resumes the child's
// architect's claim in its session: the runtime is asked to resume the process from the session
// file the claim recorded, and no fresh one is spawned. Its ready tells it the child at the
// generation the rerun opened.
func TestAParkedChildsArchitectIsSuspendedAndItsRerunResumesItsSession(t *testing.T) {
	rig := newChildArchitectRig(t)
	rig.written("release", childAt("todo", 2))
	rig.drain()
	machine := rig.boot(doorChild, claim.RoleArchitect, "ses-child-architect")
	rig.drain()
	token := mustClaimToken(t, doorChild, claim.RoleArchitect)
	before, started := machine.Claim(), readIssue(t, rig.pool, doorChild)
	if before.State != supervise.StateReady || before.Session != "ses-child-architect" || before.SessionFile != doorSessionFile("ses-child-architect") {
		t.Fatalf("the child's architect before the park = %+v, want it ready on the session it registered", before)
	}

	rig.written("park", childAt("backlog", 3))
	rig.drain()
	if got := readIssue(t, rig.pool, doorChild); got.Phase != phase.Done || got.Status != "backlog" {
		t.Fatalf("the parked child = %s, %s; want it out of the workflow, done and in backlog", got.Phase, got.Status)
	}
	parked := rig.machine(doorChild, claim.RoleArchitect).Claim()
	if parked.State != supervise.StateSuspended || parked.SuspendedBy != supervise.SuspendedByDaemon ||
		parked.Session != before.Session || parked.SessionFile != before.SessionFile || parked.Issue != doorChild {
		t.Fatalf("the child's architect after the park = %+v, want it suspended by the daemon with its session %s kept", parked, before.Session)
	}
	suspends := 0
	for _, call := range rig.rt.Calls() {
		if call.Method == "Suspend" && call.Locator.Claim == token {
			suspends++
		}
	}
	if suspends != 1 {
		t.Fatalf("the park suspended the child's architect's process %d times, want once", suspends)
	}

	called := len(rig.rt.Calls())
	rig.written("rerun", childAt("todo", 4))
	rig.drain()
	child := readIssue(t, rig.pool, doorChild)
	if child.Phase != phase.Planning || child.Generation <= started.Generation {
		t.Fatalf("the rerun child = %s at generation %d, want planning at a generation past %d", child.Phase, child.Generation, started.Generation)
	}
	if again := rig.machine(doorChild, claim.RoleArchitect); again != machine {
		t.Fatal("the rerun made the child's architect a new claim, want the claim the park suspended")
	}
	launches := launchesSince(rig.rt, token, called)
	if len(launches) != 1 || launches[0].Method != "Resume" || launches[0].Spec.ResumeSessionFile != before.SessionFile {
		t.Fatalf("the rerun launched the child's architect %+v, want one resume of the session file %s", launches, before.SessionFile)
	}
	resumed := machine.Claim()
	if resumed.Session != before.Session || resumed.SessionFile != before.SessionFile || resumed.State == supervise.StateSuspended {
		t.Fatalf("the child's architect after the rerun = %+v, want it resumed on its session %s", resumed, before.Session)
	}

	rig.boot(doorChild, claim.RoleArchitect, before.Session)
	rig.drain()
	told := rig.noticesTo(doorChild)
	if len(told) < 2 || told[len(told)-1].Kind != "catch-up" || told[len(told)-1].CatchUp.Generation != child.Generation {
		t.Fatalf("the child's architect was published %+v, want its resumed launch's catch-up at generation %d last", told, child.Generation)
	}
}
