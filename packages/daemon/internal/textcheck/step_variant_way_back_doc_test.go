package textcheck

// internal/store's step_variant_way_back_test.go reads only the copy in its own directory, so it
// has no line; this file holds that copy to the guide, in the text-check job.
//ci:reads docs/kubernetes.md wayBackBlockOfTheGuide reads it under ../../../.. (shippedText) for TestTheStepVariantWayBackExtractIsTheGuidesBlock
//ci:reads packages/daemon/internal/store/step_variant_way_back_extract.txt quotedBody reads it as ../store/step_variant_way_back_extract.txt (readExtract) for TestTheStepVariantWayBackExtractIsTheGuidesBlock, the committed copy it holds the guide's block to

import "testing"

// The way back of the release that names the variant on each step record stands in the guide,
// under "Rolling back the release that names the variant on each step record", which is where an
// operator reads it at a rollback and its one source. internal/store's
// TestTheStepVariantWayBackLeavesWhatTheReleaseBeforeReads runs the copy beside it,
// step_variant_way_back_extract.txt, over queued rows and kept starts; this test holds that copy
// to the guide's block, so the block an operator runs is the block that test proves. The split
// keeps the guide out of the daemon Go lane's filter, as way_back_doc_test.go does.
const (
	// stepVariantWayBackExtract is the copy, read from the package whose test runs it.
	stepVariantWayBackExtract = "../store/step_variant_way_back_extract.txt"
	// stepVariantWayBackSection is the heading of the guide's section that gives the way back.
	stepVariantWayBackSection = "Rolling back the release that names the variant on each step record"
	// stepVariantWayBackMark is the line that strips the two members, which no other rollback's
	// block carries.
	stepVariantWayBackMark = "update outbox set payload = payload - 'experiment' - 'variant'"
)

// The copy internal/store runs is the guide's block, line for line. An edit to either side alone
// fails here: a line changed in the guide and not in the copy would otherwise never reach the test
// that runs the copy.
func TestTheStepVariantWayBackExtractIsTheGuidesBlock(t *testing.T) {
	copied := quotedBody(t, stepVariantWayBackExtract)
	if copied == "" {
		t.Fatalf("%s carries nothing under its `#` header: internal/store's test of the way back has no "+
			"copy of the guide's block to run", stepVariantWayBackExtract)
	}
	guide := wayBackBlockOfTheGuide(t, stepVariantWayBackSection, stepVariantWayBackMark, stepVariantWayBackExtract)
	if copied == guide {
		return
	}
	t.Errorf("%s and %s's way back disagree:\n%s"+
		"internal/store's test runs the copy and an operator the guide's block; change them in one commit",
		stepVariantWayBackExtract, wayBackGuide, wayBackBlockDifference("the extract", copied, guide))
}
