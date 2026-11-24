package daemon

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
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

// A child's architect leaves the workflow with its child and comes back with it. The architect
// above the child writes backlog on it in Dispatch, and the daemon takes that write's event, whose
// actor is that architect's session, as it takes a person's move: the child leaves the workflow,
// which suspends every claim of the child (the workflow's leave), the child's architect's among
// them. The claim stays the child's, suspended by the daemon with the session its agent
// registered, and its process is stopped once. The rerun_child of the architect above (its own
// write of todo on the child in backlog) starts the child's first phase again, and that start
// resumes the child's architect's claim in its session: the runtime is asked to resume the
// process from the session file the claim recorded, and no fresh one is spawned. Its ready tells
// it the child at the generation the rerun opened.
func TestAChildsArchitectIsSuspendedByTheOwnersBacklogAndItsRerunResumesItsSession(t *testing.T) {
	t.Parallel()
	rig := newChildArchitectRig(t)
	rig.written("release", childAt("todo", 2))
	rig.drain()
	machine := rig.boot(doorChild, claim.RoleArchitect, "ses-child-architect")
	rig.drain()
	token := mustClaimToken(t, doorChild, claim.RoleArchitect)
	before, started := machine.Claim(), readIssue(t, rig.pool, doorChild)
	if before.State != supervise.StateReady || before.Session != "ses-child-architect" || before.SessionFile != doorSessionFile("ses-child-architect") {
		t.Fatalf("the child's architect before the backlog = %+v, want it ready on the session it registered", before)
	}

	// The root's architect, whose claim the rig seeds on session ses-root-architect, writes backlog
	// on the child in Dispatch, and the consumer delivers that write's event.
	rig.dispatch.issue = childAt("backlog", 3)
	rig.apply("owner-backlog", intake.DispatchIssue{Key: doorChild, ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated", Status: "backlog",
		Title: doorChild, Parent: doorRoot, Rank: "V", ActorSession: "ses-root-architect"})
	rig.drain()
	if got := readIssue(t, rig.pool, doorChild); got.Phase != phase.Done || got.Status != "backlog" {
		t.Fatalf("the child after its owning architect's backlog = %s, %s; want it out of the workflow, done and in backlog", got.Phase, got.Status)
	}
	suspended := rig.machine(doorChild, claim.RoleArchitect).Claim()
	if suspended.State != supervise.StateSuspended || suspended.SuspendedBy != supervise.SuspendedByDaemon ||
		suspended.Session != before.Session || suspended.SessionFile != before.SessionFile || suspended.Issue != doorChild {
		t.Fatalf("the child's architect after the backlog = %+v, want it suspended by the daemon with its session %s kept", suspended, before.Session)
	}
	suspends := 0
	for _, call := range rig.rt.Calls() {
		if call.Method == "Suspend" && call.Locator.Claim == token {
			suspends++
		}
	}
	if suspends != 1 {
		t.Fatalf("the backlog suspended the child's architect's process %d times, want once", suspends)
	}

	called := len(rig.rt.Calls())
	rig.written("rerun", childAt("todo", 4))
	rig.drain()
	child := readIssue(t, rig.pool, doorChild)
	if child.Phase != phase.Planning || child.Generation <= started.Generation {
		t.Fatalf("the rerun child = %s at generation %d, want planning at a generation past %d", child.Phase, child.Generation, started.Generation)
	}
	if again := rig.machine(doorChild, claim.RoleArchitect); again != machine {
		t.Fatal("the rerun made the child's architect a new claim, want the claim the backlog suspended")
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
