package admit

import (
	"context"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// R16 of dispatch://INFRA-3320 reads Dispatch again for every recorded issue whose chain the copy
// cannot read to its top, in Legion's work or out of it (the architect's ruling,
// dispatch://INFRA-4730/comment/000d0b0f-743a-4130-8ba2-3c95b97d1ff7). Moving LEGION-500 under an
// unread parent notes it for the daemon's fast re-read (UnknownChains) while it is in work and
// again once a stop takes it to backlog; once the missing parent lands in the copy, a pass notes
// nothing of it.
func TestAPassNotesARecordedIssueOnAnUnknownChainInAndOutOfWork(t *testing.T) {
	t.Parallel()
	pool, admission := rstFixture(t)

	rstMove(t, pool, admission, "in_progress", rstUnread, 2)
	if noted := admission.UnknownChains(); !slices.Contains(noted, rstChild) {
		t.Fatalf("UnknownChains after moving %s under unread %s in work = %v, want it to name %s",
			rstChild, rstUnread, noted, rstChild)
	}

	rstMove(t, pool, admission, "backlog", rstUnread, 3)
	if stopped := issue(t, pool, rstChild); record.InLegionsWork(stopped) {
		t.Fatalf("%s after backlog = %s/%s, want it out of Legion's work: the case's premise",
			rstChild, stopped.Phase, stopped.Status)
	}
	if noted := admission.UnknownChains(); !slices.Contains(noted, rstChild) {
		t.Fatalf("UnknownChains after stopping %s to backlog on unread %s = %v, want it to name %s",
			rstChild, rstUnread, noted, rstChild)
	}

	spMirror(t, pool, rstUnread, "backlog", rstNew, 9)
	if _, err := intake.ApplyFact(context.Background(), pool, "controller", "tick-after-reread",
		intake.ControllerTick{}, blockers.NewMirror(record.NewStore(), admission.project), admission.engine, admission); err != nil {
		t.Fatalf("apply the tick after %s landed in the copy: %v", rstUnread, err)
	}
	if noted := admission.UnknownChains(); slices.Contains(noted, rstChild) {
		t.Fatalf("UnknownChains after %s landed in the copy = %v, want %s gone", rstUnread, noted, rstChild)
	}
}

// Once the unknown chain a re-parent seed waited on becomes known, the architect above the issue
// now is told whether the issue is still stopped out of work or never left work, with the same
// words (ruling 4 of dispatch://INFRA-4730).
func TestAReparentSeedWaitsOutOfWorkAndTellsTheSameWordsOnceKnown(t *testing.T) {
	t.Parallel()

	inWorkPool, inWorkAdmission := rstFixture(t)
	rstMove(t, inWorkPool, inWorkAdmission, "in_progress", rstUnread, 2)
	inWorkLast := rstReadSeeds(t, inWorkPool, inWorkAdmission, 0)
	spMirror(t, inWorkPool, rstUnread, "backlog", rstNew, 9)
	rstReadSeeds(t, inWorkPool, inWorkAdmission, inWorkLast)
	inWorkTold := rstToldTo(t, inWorkPool, rstNew)
	if len(inWorkTold) != 1 {
		t.Fatalf("in work: %s was told %d times, want once: the case's premise", rstNew, len(inWorkTold))
	}

	stoppedPool, stoppedAdmission := rstFixture(t)
	rstMove(t, stoppedPool, stoppedAdmission, "in_progress", rstUnread, 2)
	rstMove(t, stoppedPool, stoppedAdmission, "backlog", rstUnread, 3)
	if stopped := issue(t, stoppedPool, rstChild); record.InLegionsWork(stopped) {
		t.Fatalf("%s after backlog = %s/%s, want it out of Legion's work: the case's premise",
			rstChild, stopped.Phase, stopped.Status)
	}
	stoppedLast := rstReadSeeds(t, stoppedPool, stoppedAdmission, 0)
	if told := rstToldTo(t, stoppedPool, rstNew); len(told) != 0 {
		t.Fatalf("while %s is still unread, %s was told %q, want nothing yet", rstUnread, rstNew, told)
	}
	spMirror(t, stoppedPool, rstUnread, "backlog", rstNew, 9)
	rstReadSeeds(t, stoppedPool, stoppedAdmission, stoppedLast)
	stoppedTold := rstToldTo(t, stoppedPool, rstNew)
	if !slices.Equal(stoppedTold, inWorkTold) {
		t.Fatalf("stopped out of work: %s was told %q, want the same words as in work %q", rstNew, stoppedTold, inWorkTold)
	}
}
