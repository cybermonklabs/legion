package textcheck

// This test reads the done-by-checks migration through migrations.WayBack, as the other ways back
// of this package do, and the guide under ../../../.., so the guide is its one row.
//ci:reads docs/kubernetes.md `packages/daemon/internal/textcheck/waiting_line_texts_test.go#shippedText` reads it under ../../../.. for TestTheDoneByChecksWayBackIsOneTextInTheHeaderAndTheGuide (through `packages/daemon/internal/textcheck/way_back_doc_test.go#wayBackBlockOfTheGuide`)

import "testing"

// The way back of the release that closes an issue when its checks pass stands in two files: the
// header of store migration 20261125004932_done_by_checks, which ships the two blocks with the
// migration, and a fenced block of docs/kubernetes.md, under "Rolling back the release that closes
// an issue when its checks pass", which is where an operator reads them at a rollback. This test
// holds the guide's block to the header's, as the owner's plan's way back is held
// (owner_steps_way_back_doc_test.go).
const (
	// doneByChecksVersion is the migration whose header carries the block.
	doneByChecksVersion = 20261125004932
	// doneByChecksWayBackSection is the heading of the guide's section that gives the way back.
	doneByChecksWayBackSection = "Rolling back the release that closes an issue when its checks pass"
	// doneByChecksWayBackMark is the line that deletes the migration's version row, which no other
	// rollback's block carries.
	doneByChecksWayBackMark = "delete from schema_version where version = 20261125004932;"
)

// The guide's way back of the release that closes an issue when its checks pass is the block the
// migration's header carries, line for line. An edit to either side alone fails here: a line
// changed in the guide and not in the header would otherwise never reach the test that runs the
// header's block.
func TestTheDoneByChecksWayBackIsOneTextInTheHeaderAndTheGuide(t *testing.T) {
	name := "the header of " + migrationNames(t, doneByChecksVersion)[0]
	assertIsTheGuidesBlock(t, heldBlock{Text: headersWayBack(t, doneByChecksVersion, name), Name: name, Label: "the header",
		Why: "the store's tests run the header's block and an operator the guide's"},
		doneByChecksWayBackSection, doneByChecksWayBackMark)
}
