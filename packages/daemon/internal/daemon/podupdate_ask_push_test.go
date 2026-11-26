package daemon

// An update round's ask is published only while the role's push is still the one the ask names
// (record.UpdateAsk's Branch, OwnFix and FixReads). The sweep reads each role's push once, when it
// queues the ask (askRound), and the row waits in the outbox until the role-message lane runs it.
// Between the two, the start the push was read from can end and a later start of the same step
// begin with no binding: the old ask would then tell that later start's agent to push its commits
// to a branch beside a fix that its own start does not name, and that no later recovery reads. So
// the outbox's updateAsk reads the role's push again under the claim's lock, inside the publish
// (supervise.Machine.PublishUpdateAsk), and publishes nothing where it changed. These cases drive
// the pod-update rig of podupdate_test.go, with the lane reading the pushes as the workflow's
// attach wires the outbox.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// updateAskOf is the ask an update-ask row carries.
func updateAskOf(t *testing.T, row record.OutboxRow) record.UpdateAsk {
	t.Helper()
	var ask record.UpdateAsk
	if err := json.Unmarshal(row.Payload, &ask); err != nil {
		t.Fatalf("decode update ask %d: %v", row.ID, err)
	}
	return ask
}

// The tester's first start (entry A) runs beside fix 1 and is bound to its branch B, and the
// round's ask to the tester is queued naming B. Before the lane runs that row, fix 1 ends, the run
// of entry A ends, and a later start of the tester's step (entry B) begins beside no fix, with no
// binding: the tester's push is now the zero push. The row then publishes nothing to the tester's
// topic, marks the claim asked at no time, and finishes. The next sweep finds the tester unasked
// with no row queued and asks it again from its push now: a row with no branch, which is
// published once and marks the claim.
//
// Both starts are the engine's own (workflow.(*Engine).StartPromotedWorker), which raises the
// tester's entry and writes the start of its step's run as a transition's start does (enter):
// the first binds the start to B, since fix 1 runs, and the second binds none. The end of fix 1
// and the end of entry A's run are written with the store's writers (record.Store.UpdateFix,
// record.Store.EndStepStart). In production the tester's completion ends that run and the way
// after the fix starts the next one, but that completion goes through the testing move, which
// waits for Dispatch's check states at the tested head, and these tests seed past the testing
// step, as every test of a step beside a fix does.
//
// It was run red on the executor that published the row's text after checking only the claim and
// its round: the tester's topic was published the ask naming B, and the claim was marked asked.
// A read of the push that fails publishes nothing and marks nothing, and the row answers the
// read's error, so it keeps its retry.
//
// The control, kept: a row whose push is the role's push now, the bound branch B while entry A
// still runs, is published once and marks the claim, so a lane that read no push, or read the
// zero push, fails it.
func TestAnUpdateAskIsPublishedOnlyWhileTheRolesPushIsTheOneItNames(t *testing.T) {
	t.Parallel()
	bound := besideBranch(t, claim.RoleTester)
	// queued is the rig with entry A of the tester started beside running fix 1 and bound to B,
	// and the round's ask to the tester queued, naming B, and not yet run.
	queued := func(t *testing.T) (*podUpdateRig, record.OutboxRow) {
		t.Helper()
		rig := newBesideRig(t)
		rig.lane.updatePushes = rig.s.updatePushes
		rig.seedBesideTask(2, phase.Testing)
		rig.openFix(record.FixRunning)
		rig.startStanding()
		if start := rig.startAt(2); start.Starts != 1 || start.Run != record.RunOpen || start.PushBranch != bound {
			t.Fatalf("the tester's first start = %+v, want start 1 open and bound to %s", start, bound)
		}
		if ask := rig.askedPushes(besideRoles...)[claim.RoleTester]; pushOf(ask) != (supervise.UpdatePush{Branch: bound}) {
			t.Fatalf("the ask queued for the tester carries %+v, want the branch %s", pushOf(ask), bound)
		}
		return rig, rig.awaitAskQueued(claim.RoleTester, podUpdateRound)
	}

	t.Run("the start the ask names ended and a later start has no binding", func(t *testing.T) {
		t.Parallel()
		rig, row := queued(t)
		rig.endFix()
		rig.endStart(2)
		rig.startStanding()
		if start := rig.startAt(2); start.Starts != 2 || start.Run != record.RunOpen || start.PushBranch != "" {
			t.Fatalf("the tester's later start = %+v, want start 2 open and bound to no branch", start)
		}

		// runAsk returns only once the lane finished the row: an error fails the test.
		rig.runAsk(row)
		if n := rig.deliveredTo(claim.RoleTester); n != 0 {
			t.Fatalf("the ask naming %s was published %d times after the start it was read from ended, want none", bound, n)
		}
		if c := rig.claimOf(claim.RoleTester); c.UpdateTaskedAt != nil {
			t.Fatalf("the tester's claim records the ask at %v, want it unasked, so the next sweep asks it again", c.UpdateTaskedAt)
		}

		next := rig.awaitAskQueued(claim.RoleTester, podUpdateRound)
		if next.ID == row.ID {
			t.Fatalf("the next sweep's ask is row %d, the finished row, want a new row", next.ID)
		}
		if push := pushOf(updateAskOf(t, next)); push != (supervise.UpdatePush{}) {
			t.Fatalf("the next ask to the tester carries %+v, want the zero push of its later start", push)
		}
		rig.runAsk(next)
		if n := rig.deliveredTo(claim.RoleTester); n != 1 {
			t.Fatalf("the next ask was published %d times, want once", n)
		}
		if c := rig.claimOf(claim.RoleTester); c.UpdateTaskedAt == nil {
			t.Fatal("the tester's claim records no ask after the next ask was published, want it marked")
		}
	})
	t.Run("control: an ask whose push is the role's push now is published once and marks the claim", func(t *testing.T) {
		t.Parallel()
		rig, row := queued(t)
		rig.runAsk(row)
		if n := rig.deliveredTo(claim.RoleTester); n != 1 {
			t.Fatalf("the ask naming %s while entry A still runs bound to it was published %d times, want once", bound, n)
		}
		if c := rig.claimOf(claim.RoleTester); c.UpdateTaskedAt == nil {
			t.Fatal("the tester's claim records no ask after the ask was published, want it marked")
		}
	})
	t.Run("a read of the push that fails publishes nothing and answers the read's error", func(t *testing.T) {
		t.Parallel()
		rig, row := queued(t)
		down := errors.New("the store did not answer")
		rig.lane.updatePushes = func(context.Context, string) (map[claim.Role]supervise.UpdatePush, error) { return nil, down }
		var err error
		testwait.Eventually(t, "the ask's attempt to meet its claim's lock free", func() bool {
			rig.settleAll()
			err = rig.lane.execute(rig.ctx, row)
			return !errors.Is(err, wait.ErrWaiting)
		})
		if !errors.Is(err, down) {
			t.Fatalf("the ask whose read of the push failed answers %v, want the read's error, so the row keeps its retry", err)
		}
		if n := rig.deliveredTo(claim.RoleTester); n != 0 {
			t.Fatalf("the ask whose read of the push failed was published %d times, want none", n)
		}
		if c := rig.claimOf(claim.RoleTester); c.UpdateTaskedAt != nil {
			t.Fatalf("the tester's claim records the ask at %v, want it unasked", c.UpdateTaskedAt)
		}
	})
}
