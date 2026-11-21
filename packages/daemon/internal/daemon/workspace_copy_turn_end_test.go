package daemon

// The copy each turn's end owes (R1 of dispatch://INFRA-1117) as the daemon takes it: the copy is
// handed the claim whose turn ended and asks that role, the copy owed behind one in flight asks
// that role too, whichever other asks it stands for came first, and a stop held for the turn's end
// waits for whichever copy holds that end. These cases drive the copy rig of
// workspace_copies_test.go.

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The copy a turn's end takes asks the role whose turn ended, as the machine handed it the claim
// at that end, and reads no claims for a role of its own. The issue's other live role, first by
// token, is what a read of the claims would choose, and nothing of that role waits for the copy:
// its own stop, a pause's of an idle role above all, lands whenever it comes and cuts off a copy
// asked of it, and the turn's end is then in no copy. The stop held for the turn's end does wait:
// while the bundler is still building the copy, the role whose turn ended is idle and running,
// and once the copy has ended the stop lands.
//
// The control is the copy reading the claims itself again, as it did before it was handed the
// claim: it then asks the architect, and the test fails at the role the bundle was asked of.
func TestATurnsEndCopyAsksTheRoleWhoseTurnEndedAndTheStopHeldForThatEndWaitsForIt(t *testing.T) {
	rig := newCopyRig(t)
	rig.copiesTurnEnds()
	rig.liveArchitect(t)
	release := rig.heldBundler()
	rig.turnStart(t)
	rig.holdStop(t)

	rig.turnEnd(t)

	rig.awaitAsks(t, 1)
	if asked := rig.bundler.reads(); asked[0] != claim.RoleImplementer {
		t.Fatalf("the copy of the implementer's turn's end was asked of %v, want the implementer whose turn ended", asked)
	}
	rig.waitsForTheCopy(t)

	release()
	rig.awaitCopy(t, len(copyRigBundle.Bundle))
	rig.stopsOnceCopied(t)
	if asked := rig.bundler.reads(); len(asked) != 1 {
		t.Fatalf("the bundler was asked %v, want the one copy of the turn's end", asked)
	}
}

// A turn that ends while the cadence's copy of its issue is still being built is owed a copy
// behind it, and that owed copy asks the role whose turn ended, as the turn's own copy would have:
// it is handed the claim the turn's end handed its copy, and reads no claims to find a role. The
// stop held for that turn's end waits for the owed copy, which is the one that holds the end: the
// copy in flight read the workspace before the turn's last work was written. So the role whose
// turn ended runs on while the copy in flight is built, and its stop lands once the owed copy has
// ended. The cadence asked the issue's first live role by token, the architect.
//
// The control is the owed copy reading the claims itself again, as it did before it carried the
// role it was owed with: it then asks the architect a second time, and the test fails at the role
// the owed copy was asked of.
func TestTheCopyOwedToATurnsEndAsksTheRoleWhoseTurnEndedAndItsHeldStopWaitsForIt(t *testing.T) {
	rig := newCopyRig(t)
	rig.copiesTurnEnds()
	rig.liveArchitect(t)
	ctx := context.Background()
	release := rig.heldBundler()
	rig.turnStart(t)
	rig.w.copyWorkingWorkspaces(ctx)
	rig.awaitAsks(t, 1)
	rig.holdStop(t)

	rig.turnEnd(t)

	rig.awaitCopyOwed(t, "the turn's end", "the copy in flight")
	rig.waitsForTheCopy(t)

	release()
	rig.awaitAsks(t, 2)
	if asked := rig.bundler.reads(); asked[0] != claim.RoleArchitect || asked[1] != claim.RoleImplementer {
		t.Fatalf("the bundler was asked %v, want the cadence's copy of the architect and the owed copy of the implementer whose turn ended", asked)
	}
	rig.stopsOnceCopied(t)
	rig.awaitQuiet(t)
	if asked := rig.bundler.reads(); len(asked) != 2 {
		t.Fatalf("the bundler was asked %v, want the copy in flight and the one the turn's end owed after it", asked)
	}
}

// Two asks behind one copy in flight owe one copy between them, and it asks the role of the turn's
// end among them whichever came first, since a stop of that role waits for the copy and the role
// is still there to ask: a turn's end that comes after a push takes the owed copy over, and a push
// that comes after a turn's end leaves it to that turn's role. The push asks the issue's first
// live role by token, the architect, as the cadence's copy in flight did. The turn's end is handed
// no callback here, and is a turn's end all the same: the ask says so itself (copyAsk.turnEnd).
//
// The controls: the owed copy never taken over by a later ask fails the first case at the role
// the owed copy asked, and the turn's end read from whether its ask brought a callback, as it was,
// fails the second the same way.
func TestTheCopyOwedBehindOneInFlightAsksTheTurnsEndsRoleWhicheverAskCameFirst(t *testing.T) {
	const owedLine = "running copy: a copy of this issue's workspace is already being taken, so this one is owed and will be taken when that one ends"
	for _, tc := range []struct {
		name string
		// turnEndFirst is whether the turn's end asks before the push.
		turnEndFirst bool
	}{
		{name: "a turn's end after a push takes the owed copy over"},
		{name: "a push after a turn's end leaves the owed copy to the turn's role", turnEndFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newCopyRig(t)
			rig.liveArchitect(t)
			apply := rig.pushEngine(t)
			release := rig.heldBundler()
			rig.turnStart(t)
			rig.w.copyWorkingWorkspaces(context.Background())
			rig.awaitAsks(t, 1)
			// ask makes one ask behind the copy in flight, through asking, and waits for it to be
			// owed, so the two asks reach the cadence in the order the case names.
			owed := 0
			ask := func(asking func()) {
				t.Helper()
				asking()
				owed++
				if !testwait.Holds(testwait.Bounded(t, 30*time.Second, time.Second), testwait.Poll, func() bool {
					return len(rig.lines(t, owedLine)) >= owed
				}) {
					t.Fatalf("ask %d was never owed behind the copy in flight; the daemon logged:\n%s", owed, rig.logs.String())
				}
			}
			turnEnd := func() { rig.w.copyWorkspaceAtTurnEnd(rig.machine.Claim(), nil) }
			push := func() { apply("c1", "c2") }
			if tc.turnEndFirst {
				ask(turnEnd)
				ask(push)
			} else {
				ask(push)
				ask(turnEnd)
			}

			release()

			rig.awaitAsks(t, 2)
			rig.awaitQuiet(t)
			if asked := rig.bundler.reads(); len(asked) != 2 || asked[0] != claim.RoleArchitect || asked[1] != claim.RoleImplementer {
				t.Fatalf("the bundler was asked %v, want the cadence's copy of the architect and one owed copy of the implementer whose turn ended", asked)
			}
		})
	}
}
