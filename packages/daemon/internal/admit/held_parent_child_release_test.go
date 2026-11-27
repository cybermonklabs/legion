package admit

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// toldMarks counts the marks the engine takes of a retro-held notice (workflow's tellRetroHeld),
// which every read of the release of a parent a Dispatch child holds before its retro takes: every
// read and write is the real store's.
type toldMarks struct {
	record.Store
	marks int
}

func (s *toldMarks) MarkProcessed(ctx context.Context, tx pgx.Tx, source, eventID string) (bool, error) {
	if source == "retro-held-told" {
		s.marks++
	}
	return s.Store.MarkProcessed(ctx, tx, source, eventID)
}

// A parent whose plan has no acceptance step stands held before its retro while a Dispatch child of
// it is open (record.HoldForChildren, dispatch://INFRA-3320 R8): here a child its architect filed
// and never released, which the daemon keeps no record of. Its close in Dispatch changes the
// daemon's mirror alone and writes no issue of the parent's tree, so no settle of the engine reads
// it: the admission pass reads the facts of every held parent (childrenWaits, workflow's
// StartHeldRetro), and the pass after the close starts the retro, with nobody acting. While the
// child is open each pass leaves the parent held, and its architect is told once which child holds
// it, however many passes run. The hold itself, which the reviewer's pass enters, is workflow's
// TestAChildNeverReleasedHoldsAParentBeforeItsRetro.
func TestAParentHeldForAChildNeverReleasedStartsItsRetroWhenDispatchClosesIt(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	spSeedParent(t, pool, spParent, phase.Held, false, func(issue *record.Issue) {
		issue.Hold = &record.Hold{From: phase.Retro, Reason: record.HoldForChildren}
	})
	planByOwner(t, pool, spParent, record.TaskPlace{Kind: record.PlaceRetro})
	seedMirrorChild(t, pool, spChild, "backlog", spParent)

	spPass(t, pool, admission)
	spPass(t, pool, admission)

	assertHeldForChildren(t, pool, spParent, "while "+spChild+" is open in Dispatch")
	want := []record.Notice{{Kind: "retro-held", Role: claim.RoleImplementer, Phase: phase.Retro, Reason: spParent + " passed its last step, and Legion holds it before its retro " +
		"while these Dispatch children of it are open: " + spChild + " (backlog). " +
		"A parent is accepted after its children, a child that runs as a tree of its own included: run each one to done, releasing one that was never released. " +
		"A child that runs as a tree of its own is moved by its own architect or a person. " +
		"Legion starts the retro by itself once nothing holds it any longer: no issue beneath it is still in the workflow, " +
		"no root that rests on its approval is still in the workflow, and no Dispatch child of it is open. " +
		record.StatusNotTakenYet + " " + record.EndChildRule}}
	if got := gateNotices(t, pool, spParent, "retro-held"); !reflect.DeepEqual(got, want) {
		t.Fatalf("the retro-held notices after two passes = %+v, want %+v", got, want)
	}

	spMirror(t, pool, spChild, "done", spParent, 2)
	spPass(t, pool, admission)

	assertRetroStarted(t, pool, spParent, "once Dispatch closed "+spChild)
	if got := gateNotices(t, pool, spParent, "retro-held"); len(got) != 1 {
		t.Fatalf("%s was told %d retro-held notices in all, want the one", spParent, len(got))
	}
}

// The admission pass reads the release of a parent held before its retro only where its facts
// show nothing holding it or show a hold other than the one the last pass that read it found
// (readsHeldRetro): the parent here is held for a Dispatch child its architect never released. The
// first pass reads it and tells its architect the hold; a second pass over the same facts reads
// nothing of it, so the engine takes no mark of a notice. A second child filed under it changes
// the facts, and the pass after it reads the parent again, tells nothing new, since the same hold
// holds it, and leaves it held. Once Dispatch closes both, the pass starts the retro.
func TestAPassReadsAHeldParentOnlyWhenItsFactsChange(t *testing.T) {
	const secondChild = "LEGION-302"
	pool := migratedPool(t)
	marks := &toldMarks{Store: record.NewStore()}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := workflow.New(marks, workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGate("").Steps(),
		Clock: func() time.Time { return fixedNow }}, log)
	admission := New(record.NewStore(), engine, 2, testProject, log)
	admission.now = func() time.Time { return fixedNow }
	spSeedParent(t, pool, spParent, phase.Held, false, func(issue *record.Issue) {
		issue.Hold = &record.Hold{From: phase.Retro, Reason: record.HoldForChildren}
	})
	planByOwner(t, pool, spParent, record.TaskPlace{Kind: record.PlaceRetro})
	seedMirrorChild(t, pool, spChild, "backlog", spParent)

	spPass(t, pool, admission)
	if marks.marks != 1 {
		t.Fatalf("the first pass took %d marks of a retro-held notice, want one: it reads the parent", marks.marks)
	}
	spPass(t, pool, admission)
	if marks.marks != 1 {
		t.Fatalf("the second pass took %d marks of a retro-held notice in all, want the first pass's one: its facts are the same", marks.marks)
	}

	seedMirrorChild(t, pool, secondChild, "backlog", spParent)
	spPass(t, pool, admission)

	if marks.marks != 2 {
		t.Fatalf("the pass after %s was filed took %d marks in all, want two: its facts changed", secondChild, marks.marks)
	}
	assertHeldForChildren(t, pool, spParent, "while "+spChild+" and "+secondChild+" are open in Dispatch")
	if got := gateNotices(t, pool, spParent, "retro-held"); len(got) != 1 {
		t.Fatalf("%s was told %d retro-held notices, want the one: the same hold holds it", spParent, len(got))
	}

	spMirror(t, pool, spChild, "done", spParent, 2)
	spMirror(t, pool, secondChild, "done", spParent, 2)
	spPass(t, pool, admission)

	assertRetroStarted(t, pool, spParent, "once Dispatch closed both children")
}

// The memory of the reads of held parents (retroHeldReads) holds only the parents the last pass's
// facts hold before their retro: a pass drops the read of a parent its facts no longer hold so, one
// whose retro a settle started or one that left the workflow (forgetRetroHeldReads). A pass's hook
// that runs once a later pass has begun, as the commit watcher runs one late after a lost commit
// reply, writes nothing (rememberHeldRetro): it would put an older read over the later pass's.
func TestTheHeldRetroMemoryHoldsOnlyHeldParentsAndNoLateRead(t *testing.T) {
	const kept, released, late = "LEGION-310", "LEGION-311", "LEGION-312"
	pool := migratedPool(t)
	admission := quiet(t, 2)
	read := retroHeldRead{moved: fixedNow, holds: record.RetroHolds{Open: []string{"LEGION-319"}}}
	remember := func(pass uint64, key string) {
		t.Helper()
		if err := store.Transact(t.Context(), pool, func(ctx context.Context, _ pgx.Tx) error {
			admission.rememberHeldRetro(ctx, pass, key, read)
			return nil
		}); err != nil {
			t.Fatalf("commit the pass that read %s: %v", key, err)
		}
	}
	remembered := func() []string { return slices.Sorted(maps.Keys(admission.retroHeldReads)) }

	first := admission.forgetRetroHeldReads(map[string]record.RetroHolds{kept: read.holds, released: read.holds})
	remember(first, kept)
	remember(first, released)
	if got := remembered(); !slices.Equal(got, []string{kept, released}) {
		t.Fatalf("the first pass's commit left the reads of %v, want %s and %s", got, kept, released)
	}

	admission.forgetRetroHeldReads(map[string]record.RetroHolds{kept: read.holds, late: read.holds})
	remember(first, late)
	if got := remembered(); !slices.Equal(got, []string{kept}) {
		t.Fatalf("after the second pass and the first pass's late hook the memory holds the reads of %v, want %s alone: %s is held no longer, and the hook of %s ran after a later pass began",
			got, kept, released, late)
	}
}
