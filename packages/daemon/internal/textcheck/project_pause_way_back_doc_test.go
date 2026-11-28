package textcheck

// This test reads the pause's migration through migrations.All, as owner_steps_way_back_doc_test.go
// reads the plan rows' migration. internal/store's project_pause_way_back_test.go, which runs that
// migration's way back, reads no text outside its package, so the guide is this one's one row.
//ci:reads docs/kubernetes.md `packages/daemon/internal/textcheck/waiting_line_texts_test.go#shippedText` reads it under ../../../.. for TestTheProjectPauseWayBackIsOneTextInTheHeaderAndTheGuide (through `packages/daemon/internal/textcheck/way_back_doc_test.go#wayBackBlockOfTheGuide`)

import "testing"

// The way back of the release that pauses a project stands in two files. The header of store
// migration 20261115194357_project_pause ships the block with the migration. A fenced block of
// docs/kubernetes.md, under "Rolling back the project pause", is where an operator reads it at a
// rollback. internal/store's TestTheProjectPauseWayBackLeavesWhatTheReleaseBeforeReads runs the
// header's block over queued rows and reads what it leaves. This test holds the guide's block to
// that one.
const (
	// projectPauseVersion is the migration whose header carries the block.
	projectPauseVersion = 20261115194357
	// projectPauseWayBackSection is the heading of the guide's section that gives the way back.
	projectPauseWayBackSection = "Rolling back the project pause"
	// projectPauseWayBackMark is the line that deletes each pause's stop, which no other rollback's
	// block carries.
	projectPauseWayBackMark = "delete from outbox where kind = 'supervise' and payload->>'op' = 'pause_stop';"
)

// The guide's way back of the release that pauses a project is the block the migration's header
// carries, line for line. An edit to either side alone fails here. A line changed in the guide and
// not in the header would otherwise never reach the test that runs the header's block.
func TestTheProjectPauseWayBackIsOneTextInTheHeaderAndTheGuide(t *testing.T) {
	name := "the header of " + migrationNames(t, projectPauseVersion)[0]
	assertIsTheGuidesBlock(t, heldBlock{Text: headersWayBack(t, projectPauseVersion, name), Name: name, Label: "the header",
		Why: "internal/store's test runs the header's block and an operator the guide's"},
		projectPauseWayBackSection, projectPauseWayBackMark)
}
