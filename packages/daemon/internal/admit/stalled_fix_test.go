package admit

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A task held at its end, before its retro, for its CI or for its fix silences no tree. A root
// held for its CI stands in held, which the rule reads as stopped before it reads any word
// (lineHolds.waitsByDesign), so a quiet tree of it is told. A root held for its fix is not quiet
// while its fix's claim works, so its tree is told to nobody; with that claim idle it is told as
// any held tree is. The measure reads each root under the word of its hold, ci_at_head or
// fix_running, and the premise below reads those words from the walk the measure reads.
//
// The control is a tree whose root waits on a person: a wait by design, so it is silent, and a
// pass that names the held trees is a pass that still leaves a waiting tree alone.
func TestAStalledSweepNamesTheCIHold(t *testing.T) {
	bound := deploymentBound(t, "")
	pool := migratedPool(t)
	admission := quiet(t, 4)
	admission.UseStalledTreeNotice(bound)
	registerController(t, pool)
	records := claimStore(t, pool)
	approved := 1
	gateOf := func(root string) record.DesignGate {
		return record.DesignGate{Issue: root, ArtifactID: "artifact-" + root, LatestVersion: 1, ApprovedVersion: &approved, EverOpened: true}
	}
	heldAtTheEnd := func(root, rank string, reason record.HoldReason, implementer supervise.ClaimState) {
		t.Helper()
		issue := quietRootRecord(root, rank, phase.Admitted)
		issue.Phase, issue.Hold, issue.Status, issue.DispatchStatus = phase.Held, &record.Hold{From: phase.Retro, Reason: reason}, "needs_review", "needs_review"
		putIssue(t, pool, issue)
		seedLifecycle(t, pool, root)
		seedGate(t, pool, gateOf(root))
		seedMirror(t, pool, root, issue.Status)
		seedClaim(t, records, root, root, claim.RoleArchitect, supervise.StateIdle)
		seedClaim(t, records, root, root, claim.RoleImplementer, implementer)
	}

	const ciHeld, fixWorking, fixIdle, control = "LEGION-630", "LEGION-640", "LEGION-650", "LEGION-660"
	heldAtTheEnd(ciHeld, "A", record.HoldForCI, supervise.StateIdle)
	heldAtTheEnd(fixWorking, "B", record.HoldForFix, supervise.StateWorking)
	heldAtTheEnd(fixIdle, "C", record.HoldForFix, supervise.StateIdle)
	seedQuietRoot(t, pool, control, "D", phase.AwaitingPerson)
	seedGate(t, pool, gateOf(control))
	seedMirror(t, pool, control, "in_progress")
	seedClaim(t, records, control, control, claim.RoleArchitect, supervise.StateIdle)

	// The premise, read from the one walk the measure reads (record.Waiting's second answer): each
	// held root reads the word of its hold.
	facts, slots := passFacts(t, pool)
	words := passWords(t, pool, facts, slots)
	for key, want := range map[string]record.HeldReason{
		ciHeld: record.HeldCIAtHead, fixWorking: record.HeldFixRunning, fixIdle: record.HeldFixRunning,
	} {
		if got := words[key]; got != want {
			t.Fatalf("the line leaves %s behind for %q, want %q", key, got, want)
		}
	}

	settled := settleMovements(t, pool, ciHeld, fixWorking, fixIdle, control)
	admission.now = func() time.Time { return settled.Add(bound + time.Hour) }
	tick(t, pool, admission, "end holds")

	if got, want := stalledRoots(t, pool), []string{ciHeld, fixIdle}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the controller was told of %v, want %v: a tree held for CI, and one held for its fix whose claim is idle, are no wait by design", got, want)
	}
}
