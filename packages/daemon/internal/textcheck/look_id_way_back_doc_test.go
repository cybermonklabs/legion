package textcheck

// internal/store's look_id_way_back_test.go reads only the copy in its own directory, so it has no
// line; this file holds that copy to the guide, in the text-check job.
//ci:reads docs/kubernetes.md wayBackBlockOfTheGuide reads it under ../../../.. (shippedText) for TestTheLookIDWayBackExtractIsTheGuidesBlock
//ci:reads packages/daemon/internal/store/look_id_way_back_extract.txt quotedBody reads it as ../store/look_id_way_back_extract.txt (readExtract) for TestTheLookIDWayBackExtractIsTheGuidesBlock, the committed copy it holds the guide's block to

import "testing"

// The way back of the release whose look report gives its look id stands in the guide, under
// "Rolling back the release whose look report gives its look id", which is where an operator reads
// it at a rollback and its one source; the header of
// 20261127171352_production_check_look_id.up.sql names that section as the release's whole way
// back. internal/store's TestTheWayBackStripsTheLookIDRowsAndClosesTheChecksOfChildrenThatRunAgain
// runs the copy beside it, look_id_way_back_extract.txt, over queued rows and production checks of
// two projects; this test holds that copy to the guide's block, so the block an operator runs is
// the block that test proves. The split keeps the guide out of the daemon Go lane's filter, as
// way_back_doc_test.go does.
const (
	// lookIDWayBackExtract is the copy, read from the package whose test runs it.
	lookIDWayBackExtract = "../store/look_id_way_back_extract.txt"
	// lookIDWayBackSection is the heading of the guide's section that gives the way back.
	lookIDWayBackSection = "Rolling back the release whose look report gives its look id"
	// lookIDWayBackMark is the line that deletes the retire_look rows, which no other rollback's
	// block carries.
	lookIDWayBackMark = "delete from outbox where kind = 'supervise' and payload->>'op' = 'retire_look'"
)

// The copy internal/store runs is the guide's block, line for line. An edit to either side alone
// fails here: a line changed in the guide and not in the copy would otherwise never reach the test
// that runs the copy.
func TestTheLookIDWayBackExtractIsTheGuidesBlock(t *testing.T) {
	copied := quotedBody(t, lookIDWayBackExtract)
	if copied == "" {
		t.Fatalf("%s carries nothing under its `#` header: internal/store's test of the way back has no "+
			"copy of the guide's block to run", lookIDWayBackExtract)
	}
	guide := wayBackBlockOfTheGuide(t, lookIDWayBackSection, lookIDWayBackMark, lookIDWayBackExtract)
	if copied == guide {
		return
	}
	t.Errorf("%s and %s's way back disagree:\n%s"+
		"internal/store's test runs the copy and an operator the guide's block; change them in one commit",
		lookIDWayBackExtract, wayBackGuide, wayBackBlockDifference("the extract", copied, guide))
}
