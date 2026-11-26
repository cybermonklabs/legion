package textcheck

// This test reads the plan rows' migration through migrations.All, as
// plan_ratings_way_back_doc_test.go reads the plan record's, and internal/store's
// owner_steps_way_back_test.go, which runs that migration's way back and the statements of the
// rollback below the release whose owner does a step, reads no text outside its package:
// owner_does_a_step_way_back_extract.txt, its copy of the guide's block of those statements,
// stands in its own directory, and the second row below is what holds that copy to the guide.
//ci:reads docs/kubernetes.md `packages/daemon/internal/textcheck/waiting_line_texts_test.go#shippedText` reads it under ../../../.. for TestTheOwnerStepsWayBackIsOneTextInTheHeaderAndTheGuide (through `packages/daemon/internal/textcheck/way_back_doc_test.go#wayBackBlockOfTheGuide`)
//ci:reads packages/daemon/internal/store/owner_does_a_step_way_back_extract.txt TestTheOwnerDoesAStepWayBackExtractIsTheGuidesBlock reads it as ../store/owner_does_a_step_way_back_extract.txt (through `packages/daemon/internal/textcheck/waiting_line_texts_test.go#quotedBody`)

import "testing"

// The way back of the release whose owner's plan picks the steps stands in two files: the header
// of store migration 20261117034837_issue_plans, which ships the block with the migration, and a
// fenced block of docs/kubernetes.md, under "Rolling back the release whose owner's plan picks the
// steps", which is where an operator reads it at a rollback. internal/store's
// TestTheOwnerStepsWayBackDeletesOnlyItsProjectsRowsTheReleaseBeforeCannotRead runs the header's
// block over queued rows of two projects and reads what it leaves; this test holds the guide's
// block to that one, as the plan ratings' way back is held (plan_ratings_way_back_doc_test.go).
const (
	// ownerStepsVersion is the migration whose header carries the block.
	ownerStepsVersion = 20261117034837
	// ownerStepsWayBackSection is the heading of the guide's section that gives the way back.
	ownerStepsWayBackSection = "Rolling back the release whose owner's plan picks the steps"
	// ownerStepsWayBackMark is the line that deletes the migration's version row, which no other
	// rollback's block carries.
	ownerStepsWayBackMark = "delete from schema_version where version = 20261117034837;"
)

// The guide's way back of the release whose owner's plan picks the steps is the block the
// migration's header carries, line for line. An edit to either side alone fails here: a line
// changed in the guide and not in the header would otherwise never reach the test that runs the
// header's block.
func TestTheOwnerStepsWayBackIsOneTextInTheHeaderAndTheGuide(t *testing.T) {
	name := "the header of " + migrationNames(t, ownerStepsVersion)[0]
	assertIsTheGuidesBlock(t, heldBlock{Text: headersWayBack(t, ownerStepsVersion, name), Name: name, Label: "the header",
		Why: "internal/store's test runs the header's block and an operator the guide's"},
		ownerStepsWayBackSection, ownerStepsWayBackMark)
}

// No migration header carries the statements of the rollback below the release whose owner does a
// step, so they stand in two files: a fenced block of docs/kubernetes.md, under "Rolling back below
// the release whose owner does a step", which is where an operator reads them, and internal/store's
// owner_does_a_step_way_back_extract.txt, a copy of that block committed beside the test that runs
// it (TestTheOwnerDoesAStepWayBackLeavesItsProjectOnlyRowsTheReleaseBeforeReads). That test reads
// the copy and not the guide, so an edit of the guide alone starts this run and not the daemon's
// Go lane, and this test holds the copy to the guide's block.
const (
	// ownerDoesAStepExtract is the committed copy, read from the package whose test runs it.
	ownerDoesAStepExtract = "../store/owner_does_a_step_way_back_extract.txt"
	// ownerDoesAStepSection is the heading of the guide's section that gives the block.
	ownerDoesAStepSection = "Rolling back below the release whose owner does a step"
	// ownerDoesAStepMark is the line that deletes the owner-step notices, which no other block of
	// the guide carries.
	ownerDoesAStepMark = "delete from outbox where kind = 'notice' and payload->>'kind' = 'owner-step' and split_part(issue, '-', 1) = '<PROJECT>';"
)

// The committed copy of the statements of the rollback below the release whose owner does a step
// is the guide's own block, line for line. An edit to either side alone fails here: a line changed
// in the guide and not in the copy would otherwise never reach internal/store's test, which runs
// the copy and not the guide. A copy with nothing under its header fails too, since a check of it
// would compare nothing.
func TestTheOwnerDoesAStepWayBackExtractIsTheGuidesBlock(t *testing.T) {
	copied := quotedBody(t, ownerDoesAStepExtract)
	if copied == "" {
		t.Fatalf("%s carries nothing under its `#` header: internal/store's test has no copy of the guide's block to run", ownerDoesAStepExtract)
	}
	assertIsTheGuidesBlock(t, heldBlock{Text: copied, Name: ownerDoesAStepExtract, Label: "the extract",
		Why: "internal/store's test runs the extract's block and an operator the guide's"},
		ownerDoesAStepSection, ownerDoesAStepMark)
}
