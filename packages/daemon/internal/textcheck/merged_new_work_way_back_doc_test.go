package textcheck

// This test reads the merged-new-work migration through migrations.WayBack, as the other ways back
// of this package do, and the guide under ../../../.., so the guide is its one row.
//ci:reads docs/kubernetes.md `packages/daemon/internal/textcheck/waiting_line_texts_test.go#shippedText` reads it under ../../../.. for TestTheMergedNewWorkWayBackIsOneTextInTheHeaderAndTheGuide (through `packages/daemon/internal/textcheck/way_back_doc_test.go#wayBackBlockOfTheGuide`)

import "testing"

// The way back of the release that starts a next change for a request on a merged issue stands
// in two files: the header of store migration 20261128022326_merged_new_work, which ships the block
// with the migration, and a fenced block of docs/kubernetes.md, under "Rolling back the release
// that starts a next change for a request on a merged issue", which is where an operator reads it
// at a rollback. This test holds the guide's block to the header's, as the done-by-checks way back
// is held (done_by_checks_way_back_doc_test.go).
const (
	// mergedNewWorkVersion is the migration whose header carries the block.
	mergedNewWorkVersion = 20261128022326
	// mergedNewWorkWayBackSection is the heading of the guide's section that gives the way back.
	mergedNewWorkWayBackSection = "Rolling back the release that adds the request kind, the merge read kind and the never-merged column"
	// mergedNewWorkWayBackMark is the line that deletes the migration's version row, which no other
	// rollback's block carries.
	mergedNewWorkWayBackMark = "delete from schema_version where version = 20261128022326;"
)

// The guide's way back of the release that starts a next change for a request on a merged issue is
// the block the migration's header carries, line for line. An edit to either side alone fails here:
// a line changed in the guide and not in the header would otherwise never reach the test that runs
// the header's block.
func TestTheMergedNewWorkWayBackIsOneTextInTheHeaderAndTheGuide(t *testing.T) {
	name := "the header of " + migrationNames(t, mergedNewWorkVersion)[0]
	assertIsTheGuidesBlock(t, heldBlock{Text: headersWayBack(t, mergedNewWorkVersion, name), Name: name, Label: "the header",
		Why: "the store's tests run the header's block and an operator the guide's"},
		mergedNewWorkWayBackSection, mergedNewWorkWayBackMark)
}
