package textcheck

//ci:reads docs/kubernetes.md internal/textcheck/waiting_line_texts_test.go:496 reads it under ../../../.. for TestThePlanNeedsWayBackExtractIsTheGuidesWayBackBlock (assertTheExtractIsTheGuidesBlock, way_back_doc_test.go:136, shippedText)
//ci:reads packages/daemon/internal/store/plan_needs_way_back_extract.txt internal/textcheck/waiting_line_texts_test.go:486 reads it as ../store/plan_needs_way_back_extract.txt for assertTheExtractIsTheGuidesBlock (way_back_doc_test.go:136), the committed copy it holds the guide's block of the needs' way back to (quotedBody)

import "testing"

// The way back of the release that records each task's needs runs no migration, so no migration
// header carries it. It stands in two files: a fenced block of docs/kubernetes.md, under "Rolling
// back the release that records each task's needs", which is where an operator reads it at a
// rollback, and its committed copy beside internal/store, whose
// TestThePlanNeedsWayBackLetsTheReleaseBeforeDecodeItsProjectsPlanRecords runs the copy over
// queued rows of two projects and decodes what it leaves as the release before does.
const (
	// planNeedsWayBackExtract is the committed copy, read from the package whose test runs it.
	planNeedsWayBackExtract = "../store/plan_needs_way_back_extract.txt"
	// planNeedsWayBackSection is the heading of the guide's section that gives the way back.
	planNeedsWayBackSection = "Rolling back the release that records each task's needs"
	// planNeedsWayBackMark is the first line of the statement, which no other rollback's block
	// carries.
	planNeedsWayBackMark = "update outbox set payload = jsonb_set(payload, '{tasks}',"
)

// The committed copy of the needs' way back is the guide's own block, line for line. An edit to
// either side alone fails here: a line changed in the guide and not in the copy would otherwise
// never reach the store's test, which runs the copy and not the guide.
func TestThePlanNeedsWayBackExtractIsTheGuidesWayBackBlock(t *testing.T) {
	assertTheExtractIsTheGuidesBlock(t, planNeedsWayBackExtract, planNeedsWayBackSection, planNeedsWayBackMark)
}
