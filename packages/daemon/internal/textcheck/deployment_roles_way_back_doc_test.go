package textcheck

// internal/store's deployment_roles_way_back_test.go reads only the copy in its own directory, so
// it has no line; this file holds that copy to the guide, in the text-check job.
//ci:reads docs/kubernetes.md wayBackBlockOfTheGuide reads it under ../../../.. (shippedText) for TestTheDeploymentRolesWayBackExtractIsTheGuidesBlock
//ci:reads packages/daemon/internal/store/deployment_roles_way_back_extract.txt quotedBody reads it as ../store/deployment_roles_way_back_extract.txt (readExtract) for TestTheDeploymentRolesWayBackExtractIsTheGuidesBlock, the committed copy it holds the guide's block to

import "testing"

// The outbox's way back of the release whose deployment defines roles stands in the guide, under
// "Rolling back the release whose deployment defines roles", which is where an operator reads it
// at a rollback and its one source. internal/store's
// TestTheDeploymentRolesWayBackLeavesWhatTheReleaseBeforeReads runs the copy beside it,
// deployment_roles_way_back_extract.txt, over queued rows of two projects; this test holds that
// copy to the guide's block, so the block an operator runs is the block that test proves. The
// split keeps the guide out of the daemon Go lane's filter, as way_back_doc_test.go does.
const (
	// deploymentRolesWayBackExtract is the copy, read from the package whose test runs it.
	deploymentRolesWayBackExtract = "../store/deployment_roles_way_back_extract.txt"
	// deploymentRolesWayBackSection is the heading of the guide's section that gives the way back.
	deploymentRolesWayBackSection = "Rolling back the release whose deployment defines roles"
	// deploymentRolesWayBackMark is the line that takes the worker off a merge-forward review,
	// which no other fenced block of the guide carries.
	deploymentRolesWayBackMark = "update outbox set payload = payload - 'worker'"
)

// The copy internal/store runs is the guide's block, line for line. An edit to either side alone
// fails here: a line changed in the guide and not in the copy would otherwise never reach the test
// that runs the copy.
func TestTheDeploymentRolesWayBackExtractIsTheGuidesBlock(t *testing.T) {
	copied := quotedBody(t, deploymentRolesWayBackExtract)
	if copied == "" {
		t.Fatalf("%s carries nothing under its `#` header: internal/store's test of the way back has no "+
			"copy of the guide's block to run", deploymentRolesWayBackExtract)
	}
	guide := wayBackBlockOfTheGuide(t, deploymentRolesWayBackSection, deploymentRolesWayBackMark, deploymentRolesWayBackExtract)
	if copied == guide {
		return
	}
	t.Errorf("%s and %s's way back disagree:\n%s"+
		"internal/store's test runs the copy and an operator the guide's block; change them in one commit",
		deploymentRolesWayBackExtract, wayBackGuide, wayBackBlockDifference("the extract", copied, guide))
}
