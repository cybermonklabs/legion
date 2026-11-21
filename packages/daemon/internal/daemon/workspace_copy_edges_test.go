package daemon

// Edges of the running copy (R1 of dispatch://INFRA-1117) that the copy rig of
// workspace_copies_test.go drives one at a time: a turn's end before the daemon's run starts, a
// copy that cannot read whether its pod's release bundle was saved, an owed copy that can no
// longer start, and a turn's end owed behind a release's save.

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The boot restores each claim's machine before the daemon's run begins, so a turn can end before
// any copy can run. That turn's end is copied as the run starts, and the stop held for it waits for
// that copy as for any other: an issue idle from then on would otherwise keep the copy from before
// its last turn, and an eviction of its idle pod would lose that turn's end.
//
// The control is the old behaviour, which started no copy before the run: the turn's end then
// starts none, its held stop runs at once, and the test fails at the wait.
func TestATurnsEndBeforeTheDaemonsRunStartsIsCopiedOnceTheRunHasStarted(t *testing.T) {
	rig := newCopyRig(t)
	rig.copiesTurnEnds()
	rig.w.copies.mu.Lock()
	rig.w.copies.run = nil
	rig.w.copies.mu.Unlock()
	rig.turnStart(t)
	rig.holdStop(t)

	rig.turnEnd(t)

	rig.waitsForTheCopy(t)
	if asked := rig.bundler.reads(); len(asked) != 0 {
		t.Fatalf("the bundler was asked %v before the daemon's run started, want nothing yet", asked)
	}
	run, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	rig.w.copies.runUnder(run)
	rig.awaitCopy(t, len(copyRigBundle.Bundle))
	rig.stopsOnceCopied(t)
}

// A copy that cannot read whether an idle release saved its bundle off the pod it would be taken
// off asks no shim of that pod: the bundle may be that pod's last copy. It is logged under its own
// reason at Warn, since the read failed, owes a retry, which the cadence takes once the read
// answers, and tells the turn's end it stands for, so the stop held for that end waits no longer.
//
// The control is that branch owing no retry: the cadence then never takes the copy again, and the
// test fails at the retries the cadence owes.
func TestACopyThatCannotReadItsPodsReleaseBundleOwesARetryAndTellsTheTurnsEnd(t *testing.T) {
	rig := newCopyRig(t)
	ctx := context.Background()
	rig.setLocatorPod(t, releasedPodUID)
	asked := rig.storedImplementer(t)
	closed, err := store.Open(ctx, rig.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	rig.w.copyStore = closed
	told := 0

	rig.w.takeRunningCopyOf(ctx, copyAsk{of: asked, turnEnd: true, finished: []func(){func() { told++ }}})

	rig.w.copyStore = rig.st
	line := rig.awaitLine(t, runningCopyUnsavedLine)
	if line["reason"] != runningCopyBundleUnread || line["level"] != "WARN" || line["pod"] != releasedPodUID {
		t.Fatalf("the copy was logged as %v, want it at WARN under reason %s naming the pod %s", line, runningCopyBundleUnread, releasedPodUID)
	}
	if reads := rig.bundler.reads(); len(reads) != 0 {
		t.Fatalf("the bundler was asked %v, want no shim asked of a pod whose release bundle could not be read", reads)
	}
	if told != 1 {
		t.Fatalf("the turn's end was told %d times that its copy ended, want once", told)
	}
	if owed := rig.w.copies.retries(); !slices.Equal(owed, []string{rig.issue.Key}) {
		t.Fatalf("the cadence owes retries of %v, want %s's", owed, rig.issue.Key)
	}
}

// An owed copy that can no longer start, the daemon's run having ended, tells every turn's end it
// stands for at once: no copy is coming to tell them later, and each stop held for those ends would
// wait out its whole bound for nothing.
//
// The control is that branch telling nobody: the test fails at the count of ends told.
func TestAnOwedCopyThatCanNoLongerStartTellsEveryTurnsEndItStandsFor(t *testing.T) {
	rig := newCopyRig(t)
	run, stop := context.WithCancel(context.Background())
	rig.w.copies.runUnder(run)
	stop()
	told := 0
	tell := func() { told++ }

	rig.w.startOwedCopy(copyAsk{of: rig.machine.Claim(), turnEnd: true, finished: []func(){tell, tell}})

	if told != 2 {
		t.Fatalf("the owed copy that could not start told %d turn's ends, want both it stands for", told)
	}
	rig.awaitLine(t, runningCopyStoppedLine)
	rig.asksStay(t, 0)
}

// A turn that ends while an idle release's save holds the issue's place is owed a copy behind the
// save, and the save's give-back starts it: it asks the role whose turn ended, though a read of the
// claims would choose the architect, first by token, and it tells that turn's end once it has
// ended, so the stop held for the end waits no longer.
//
// The control is the give-back starting no owed copy: the test fails waiting for the bundler.
func TestACopyOwedToATurnsEndBehindAReleaseSaveAsksThatRoleAndTellsItsEnd(t *testing.T) {
	rig := newCopyRig(t)
	rig.liveArchitect(t)
	giveBack, err := rig.w.copies.hold(context.Background(), rig.issue.Key)
	if err != nil {
		t.Fatalf("hold the issue's place for the save: %v", err)
	}
	told := make(chan struct{}, 1)

	if _, started := rig.w.copyWorkspaceAtTurnEnd(rig.machine.Claim(), func() { told <- struct{}{} }); !started {
		t.Fatal("the turn's end started no copy")
	}
	rig.awaitCopyOwed(t, "the turn's end", "the save")
	rig.asksStay(t, 0)
	giveBack()

	rig.awaitAsks(t, 1)
	if asked := rig.bundler.reads(); asked[0] != claim.RoleImplementer {
		t.Fatalf("the owed copy was asked of %v, want the implementer whose turn ended", asked)
	}
	select {
	case <-told:
	case <-time.After(testwait.Bounded(t, 30*time.Second, time.Second)):
		t.Fatalf("the turn's end was never told its owed copy ended; the daemon logged:\n%s", rig.logs.String())
	}
	rig.awaitCopy(t, len(copyRigBundle.Bundle))
}
