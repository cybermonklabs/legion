package textcheck

// This test reads the plan rows' migration through migrations.All, as
// plan_ratings_way_back_doc_test.go reads the plan record's, and internal/store's
// owner_steps_way_back_test.go, which runs that migration's way back, reads no text outside its
// package, so the guide is this one's one row.
//ci:reads docs/kubernetes.md `packages/daemon/internal/textcheck/waiting_line_texts_test.go#shippedText` reads it under ../../../.. for TestTheOwnerStepsWayBackIsOneTextInTheHeaderAndTheGuide (through `packages/daemon/internal/textcheck/way_back_doc_test.go#wayBackBlockOfTheGuide`)

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
