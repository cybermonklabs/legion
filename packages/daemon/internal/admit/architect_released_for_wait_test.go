package admit

import (
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A root at admitted whose design gate has opened gives its admission slot back while an open
// Dispatch child of it goes first (blockers.waitsOf, admit's releaseSlotsForWaits), and its own
// architect is still the role at work there: the rule reaches a root at admitted whether or not
// its design gate has opened (dispatch://INFRA-2614 R5). So the architect of a root released for
// a wait is weighed as it was while the root held the row: the entry repair sends it the work of
// the entry its phases row names, once, and the stall reading reads its arms and wakes the
// controller for one that holds. The root's phase workers stay out of the repair while it waits,
// as every waiting issue's do (TestReturningASlotStopsNothing); only its architect is weighed.
func TestTheArchitectOfARootReleasedForAWaitIsStillReached(t *testing.T) {
	t.Run("the repair sends the work of its entry", func(t *testing.T) {
		const (
			root  = "LEGION-830"
			child = "LEGION-831"
		)
		pool := migratedPool(t)
		admission := quiet(t, 1)
		fixture := architectSweepRoot(root)
		fixture.gateOpened = true
		seedArchitectSweepRoot(t, pool, fixture)
		seedMirror(t, pool, root, "in_progress")
		seedMirrorChild(t, pool, child, "backlog", root)

		tick(t, pool, admission, "the-wait-begins")

		if holdsSlot(t, pool, root) {
			t.Fatalf("%s holds a row while its open child %s goes first, want none: this test is about a root released for a wait", root, child)
		}
		if marked := issue(t, pool, root).ReleasedForWaitAt; marked == nil {
			t.Fatalf("%s carries no mark of a release for a wait, want the moment its row was taken away", root)
		}
		starts := architectSweepStarts(t, pool, root)
		if len(starts) != 1 || starts[0].entry != seededEntry || !strings.Contains(starts[0].task, workflow.ArchitectGateOpenLabel) {
			t.Fatalf("the architect starts of %s = %+v, want one of entry %d whose task is labelled %q: the architect of a root at admitted is the role at work there, with the row or without it",
				root, starts, seededEntry, workflow.ArchitectGateOpenLabel)
		}

		applySweep(t, pool, admission, "the-wait-stands")

		if again := architectSweepStarts(t, pool, root); len(again) != 1 {
			t.Fatalf("the architect starts of %s after the sweep = %+v, want the one the pass wrote: a start on its way is not written twice", root, again)
		}
	})

	t.Run("the stall reading reads its arms", func(t *testing.T) {
		const (
			root  = "LEGION-832"
			child = "LEGION-833"
		)
		pool := migratedPool(t)
		registerController(t, pool)
		admission := quiet(t, 1)
		fixture := architectWakeRoot(root)
		fixture.state, fixture.task = supervise.StateFailed, false
		seedArchitectWakeRoot(t, pool, fixture)
		seedGate(t, pool, record.DesignGate{Issue: root, ArtifactID: "artifact-" + root, LatestVersion: 1,
			ApprovedVersion: new(1), EverOpened: true})
		seedMirror(t, pool, root, "in_progress")
		seedMirrorChild(t, pool, child, "backlog", root)

		tick(t, pool, admission, "the-wait-begins")

		if holdsSlot(t, pool, root) {
			t.Fatalf("%s holds a row while its open child %s goes first, want none: this test is about a root released for a wait", root, child)
		}
		want := []record.ArchitectStallArm{record.ArchitectClaimUnreachable}
		why := "a failed architect claim of a running tree with no start of its run queued is a claim no start will reach, and the root's wait for its child takes nothing of that away"
		if arms := architectStallArmsOf(t, pool, admission, root); !slices.Equal(arms, want) {
			t.Fatalf("the state's reading of %s names the arms %v, want %v: %s", root, arms, want, why)
		}
		assertArchitectWakes(t, pool, root, want, why)
	})
}
