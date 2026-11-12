package admit

import (
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A guard on what the daemon already does, written to catch a regression rather than to prove a
// change: a person who re-ranks a waiting issue on the Dispatch board moves it in the line at the
// next admission pass, with no restart, and the slot that is free at that moment goes by the new
// order. TestApplyFactRecordsRankTitleAndParentChangesAtTheSameStatus already shows the line
// moving at a rank change under a full cap, where nothing is admitted; what this one adds is the
// free slot and the rank-only event, so the reordering is read where it decides who runs.
//
// The event carries the new rank at the status, title and parent the record already holds, so the
// rank alone moves, and it is the live fact path alone: no boot and no Reconcile runs here. The
// re-ranked root is pushed down rather than up, so an observation that stopped writing the rank
// would be caught: a fact whose rank is empty leaves that root first, since the empty rank sorts
// before every other (record.RankLess), and it would take the slot the test says its neighbour
// takes.
func TestRankOnlyEventTakesTheFreeSlot(t *testing.T) {
	pool := migratedPool(t)
	// Two slots against one running root: one slot is free when the rank event lands, and the
	// line's first candidate is the one that takes it (promoteHolds).
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	seedSlotted(t, pool, "LEGION-1", "A")
	seedWaiting(t, pool, "LEGION-2", "B")
	seedWaiting(t, pool, "LEGION-3", "C")
	if got, want := waitingLine(t, pool, testProject, nil), []string{"LEGION-2", "LEGION-3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the line before the rank event = %#v, want %#v", got, want)
	}

	apply(t, pool, admission, "rerank", intake.DispatchIssue{Key: "LEGION-2", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo", Title: "LEGION-2", Rank: "D", HandedOver: handed}, engine)

	if got := issue(t, pool, "LEGION-2"); got.Rank != "D" || got.Status != "todo" {
		t.Fatalf("the re-ranked record = %#v, want rank D at todo, the status it already held", got)
	}
	assertWaiting(t, pool, []string{"LEGION-2"})
	assertSlots(t, pool, []record.Slot{
		{Issue: "LEGION-1", Index: 0, AdmittedAt: fixedNow},
		{Issue: "LEGION-3", Index: 1, AdmittedAt: fixedNow},
	})
}
