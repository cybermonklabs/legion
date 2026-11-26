package textcheck

// This test reads the run retros' migration through migrations.All, as
// owner_steps_way_back_doc_test.go reads the plan rows', and internal/store's
// migration_20261125213821_test.go, which runs that migration's way back, reads no text outside its
// package, so the guide is this one's one row.
//ci:reads docs/kubernetes.md `packages/daemon/internal/textcheck/waiting_line_texts_test.go#shippedText` reads it under ../../../.. for TestTheRunRetrosWayBackIsOneTextInTheHeaderAndTheGuide (through `packages/daemon/internal/textcheck/way_back_doc_test.go#wayBackBlockOfTheGuide`)

import "testing"

// The way back of the release that runs a closed issue's retro after its close stands in two
// files: the header of store migration 20261125213821_run_retros, which ships the block with the
// migration, and a fenced block of docs/kubernetes.md, under "Rolling back the release that runs a
// closed issue's retro after its close", which is where an operator reads it at a rollback.
// internal/store's TestTheRunRetroMigrationsWayBackRemovesWhatTheReleaseBeforeCannotRead runs the
// header's block over queued rows and reads what it leaves; this test holds the guide's block to
// that one, as the owner's plan's way back is held (owner_steps_way_back_doc_test.go).
const (
	// runRetrosVersion is the migration whose header carries the block.
	runRetrosVersion = 20261125213821
	// runRetrosWayBackSection is the heading of the guide's section that gives the way back.
	runRetrosWayBackSection = "Rolling back the release that runs a closed issue's retro after its close"
	// runRetrosWayBackMark is the line that deletes the migration's version row, which no other
	// rollback's block carries.
	runRetrosWayBackMark = "delete from schema_version where version = 20261125213821;"
)

// The guide's way back of the release that runs a closed issue's retro after its close is the
// block the migration's header carries, line for line. An edit to either side alone fails here: a
// line changed in the guide and not in the header would otherwise never reach the test that runs
// the header's block.
func TestTheRunRetrosWayBackIsOneTextInTheHeaderAndTheGuide(t *testing.T) {
	name := "the header of " + migrationNames(t, runRetrosVersion)[0]
	assertIsTheGuidesBlock(t, heldBlock{Text: headersWayBack(t, runRetrosVersion, name), Name: name, Label: "the header",
		Why: "internal/store's test runs the header's block and an operator the guide's"},
		runRetrosWayBackSection, runRetrosWayBackMark)
}
