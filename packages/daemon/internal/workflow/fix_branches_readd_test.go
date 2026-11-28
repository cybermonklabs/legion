package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// besideDeletions is each deletion of branches beside a fix queued for LEGION-208, oldest first,
// as the outbox's strict decode reads it (record.DecodeOutboxPayload), so a row the executor would
// refuse fails the test here.
func (r *fixRig) besideDeletions() []record.BesideBranchesDelete {
	r.t.Helper()
	var deletions []record.BesideBranchesDelete
	for _, row := range issueOutboxOf(r.t, r.pool, "LEGION-208", string(record.OutboxKindBesideBranchesDelete)) {
		payload, err := record.DecodeOutboxPayload(record.OutboxRow{ID: row.id, Kind: record.OutboxKindBesideBranchesDelete, Issue: "LEGION-208",
			Payload: row.payload})
		if err != nil {
			r.t.Fatalf("decode the deletion %s: %v", row.payload, err)
		}
		deletions = append(deletions, payload.(record.BesideBranchesDelete))
	}
	return deletions
}

// applyTaken applies fact as the event id and fails the test unless it is taken: no refusal, spent
// and no duplicate.
func (r *fixRig) applyTaken(id string, fact intake.Fact) {
	r.t.Helper()
	if result := r.apply(id, fact); result.Refusal != nil || result.Unspent || result.Duplicate {
		r.t.Fatalf("%s = %+v, want it taken", id, result)
	}
}

// A version that removes a stopped task queues the deletion of the branches beside each fix the
// task holds then, through the newest one: through fix 1. While that row waits, as it does
// when GitHub answers its deletes 500, a later version adds the task again in the same
// generation, and a failure opens the task's next fix. That fix is fix 2, above the row's newest,
// since the task's fixes stay when its plan rows go, so the row names none of its branches. Once
// the task is stopped and removed again, the next row names fix 2 as its newest.
//
// The control is the second row: its newest fix is 2, not 1, so a row that names a fixed number
// fails here, and the first row is asserted to be the only one before the re-add, so a test that
// read no row reports UNKNOWN.
func TestADeletionOfARemovedTaskNamesTheNewestFixItHeld(t *testing.T) {
	r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
	r.stop()
	other := givenTask(t, "Build the other widget.", "implementer", "reviewer")
	other.ID = "b"
	removed := ownersPlan(t, "LEGION-208", 1, 3, other)
	removed.Plan.ArtifactID = "plan-artifact" // the document of the version in force (seedStepsPlan)
	r.applyTaken("version-3", removed)

	queued := r.besideDeletions()
	if len(queued) != 1 {
		t.Fatalf("UNKNOWN: the version that removes task a queued %+v, want one deletion: a test that reads no row checks nothing", queued)
	}
	if want := (record.BesideBranchesDelete{Generation: 1, Task: "a", Through: 1}); queued[0] != want {
		t.Errorf("the version that removes task a queued %+v, want %+v: every fix of the task through fix 1, its newest", queued[0], want)
	}

	r.applyTaken("stop-b", ownersStop(t, issueOf(t, r.pool, r.engine, "LEGION-208"), "b"))
	again := ownersPlan(t, "LEGION-208", 1, 4, givenTask(t, "Build the widget.", "implementer", "tester", "reviewer"))
	again.Plan.ArtifactID = "plan-artifact"
	r.applyTaken("version-4", again)
	r.move(TriggerImplementationReady, "", record.PhaseRow{})
	r.fails(fixHead, "k-ci2")

	opened := r.latestFix()
	if opened.Fix != 2 || opened.State != record.FixReading || opened.Check != "k-ci2" {
		t.Fatalf("the task added again holds %+v as its latest fix, want fix 2 reading its job for k-ci2", opened)
	}
	if deletions := r.besideDeletions(); len(deletions) != 1 || opened.Fix <= deletions[0].Through {
		t.Fatalf("fix %d opened beside the deletions %+v, want the one deletion of the removed task, whose newest fix is below it", opened.Fix, deletions)
	}

	r.applyTaken("stop-a-again", ownersStop(t, issueOf(t, r.pool, r.engine, "LEGION-208"), "a"))
	removedAgain := ownersPlan(t, "LEGION-208", 1, 5, other)
	removedAgain.Plan.ArtifactID = "plan-artifact"
	r.applyTaken("version-5", removedAgain)

	deletions := r.besideDeletions()
	want := []record.BesideBranchesDelete{{Generation: 1, Task: "a", Through: 1}, {Generation: 1, Task: "a", Through: 2}}
	if len(deletions) != len(want) || deletions[0] != want[0] || deletions[1] != want[1] {
		t.Errorf("after the task was removed twice the deletions are %+v, want %+v: each through the newest fix the task held then", deletions, want)
	}
}
