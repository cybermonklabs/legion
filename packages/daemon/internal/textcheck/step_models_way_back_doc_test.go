package textcheck

// This test reads the launch totals' migration through migrations.WayBack, whose embed is in the
// go list closure of the text-check run. internal/store's migration_20261125171320_test.go, which
// runs that migration's way back, reads no text outside its package. So the guide is this one's
// one row.
//ci:reads docs/kubernetes.md `packages/daemon/internal/textcheck/waiting_line_texts_test.go#shippedText` reads it under ../../../.. for TestTheStepModelsWayBackIsTheDocsBlock (`packages/daemon/internal/textcheck/way_back_doc_test.go#assertIsTheGuidesBlock`, through `packages/daemon/internal/textcheck/way_back_doc_test.go#wayBackBlockOfTheGuide`)

import "testing"

// The way back of the release that counts each step's models stands in two files. The header of
// store migration 20261125171320_launch_model_totals ships the block with the migration. A fenced
// block of docs/kubernetes.md, under "Rolling back the release that counts each step's models", is
// where an operator reads it at a rollback. internal/store's
// TestTheLaunchTotalsWayBackLeavesWhatTheReleaseBeforeReads runs the header's block. This test
// holds the guide's block to that one, as the owner's plan's way back is held
// (owner_steps_way_back_doc_test.go).
const (
	// stepModelsVersion is the migration whose header carries the block.
	stepModelsVersion = 20261125171320
	// stepModelsWayBackSection is the heading of the guide's section that gives the way back.
	stepModelsWayBackSection = "Rolling back the release that counts each step's models"
	// stepModelsWayBackMark is the line that deletes the migration's version row, which no other
	// rollback's block carries.
	stepModelsWayBackMark = "delete from schema_version where version = 20261125171320;"
)

// The guide's way back of the release that counts each step's models is the block the
// migration's header carries, line for line. An edit to either side alone fails here. A line
// changed in the guide and not in the header would otherwise never reach the test that runs the
// header's block.
func TestTheStepModelsWayBackIsTheDocsBlock(t *testing.T) {
	name := "the header of " + migrationNames(t, stepModelsVersion)[0]
	assertIsTheGuidesBlock(t, heldBlock{Text: headersWayBack(t, stepModelsVersion, name), Name: name, Label: "the header",
		Why: "internal/store's test runs the header's block and an operator the guide's"},
		stepModelsWayBackSection, stepModelsWayBackMark)
}
