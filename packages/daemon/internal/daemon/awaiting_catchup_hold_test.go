package daemon

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// openTreeIssue is one issue of project CAPTURE: key in tree at phase, carrying the repository
// every record holds and the last Dispatch observation a catch-up test seeds.
func openTreeIssue(key, tree string, at phase.Phase, rank string) record.Issue {
	return record.Issue{Key: key, Tree: tree, Project: "CAPTURE", Title: key, Phase: at, Generation: 1,
		Status: "in_progress", Rank: rank, HandedOver: key == tree, Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", LastDispatchSeq: catchUpHandout}
}

// openSpecGate is the spec gate of root as a person's approval of its current version leaves it:
// the row the boot's own step list reads as open, and which PutGate therefore marks as having
// opened.
func openSpecGate(root string, version int) record.DesignGate {
	return record.DesignGate{Issue: root, Step: record.DesignStepSpec, ArtifactID: "artifact-" + root,
		LatestVersion: version, ApprovedVersion: &version}
}

// The boot holds every root its own step list opens and its pass could then move, not only the
// roots that can enter awaiting_person. A list shorter than the one the tree was admitted under
// leaves every remaining step open, and an open step has ever_opened set, so record.AtWork gives
// such a root no work and record.CanWaitOnPerson answers false for it: a hold that asked only
// that predicate would hold nothing, the pass would free the root's slot, the waiting line would
// offer its first phase that slot (record.Waiting classes it LineFirstPhase, StartFirstPhase runs
// it), and the root would start on a document version nobody approved — the version the catch-up
// has still to read off Dispatch. A held READY of the same tree is released the same way, through
// the pass's own first-phase starts, whose hold is this one.
//
// So holdForCatchUp itself must hold a root standing at admitted under an open gate. The boot that
// never ran such a root is still left alone: its gate stands at no open step, and replaying the
// event log below its own admission handout is history this release must not replay.
func TestTheBootsCatchUpHoldsEveryRootItsOwnStepListOpens(t *testing.T) {
	pool, w := catchUpRig(t, &awaitingDispatch{}, 2)
	seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
		for _, issue := range []record.Issue{
			// The root a shorter list releases: admitted, holding the slot its own work took
			// while the step that has since gone held it.
			openTreeIssue("CAPTURE-200", "CAPTURE-200", phase.Admitted, "A"),
			// The root that never ran, waiting in line for its first slot: nothing of its design
			// is registered, so no list opens its gate.
			openTreeIssue("CAPTURE-400", "CAPTURE-400", phase.Admitted, "C"),
		} {
			if err := records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
		}
		for _, root := range []string{"CAPTURE-200"} {
			if err := records.PutGate(ctx, tx, openSpecGate(root, 4)); err != nil {
				return err
			}
		}
		return records.PutSlot(ctx, tx, record.Slot{Issue: "CAPTURE-200", Index: 0, AdmittedAt: catchUpClock()})
	})

	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return w.holdForCatchUp(context.Background(), tx)
	}); err != nil {
		t.Fatalf("hold the roots to catch up: %v", err)
	}

	held := w.admission.CatchUpHeld()
	slices.Sort(held)
	want := []string{"CAPTURE-200"}
	if !reflect.DeepEqual(held, want) {
		t.Fatalf("the boot holds %v, want %v: the root its own list leaves at admitted with an open gate — and not the root that never ran",
			held, want)
	}
}
