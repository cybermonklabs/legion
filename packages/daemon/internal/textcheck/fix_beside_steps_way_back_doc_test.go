package textcheck

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/store/migrations"
)

// The way back of the release that starts a fix beside the other steps stands in two files: the
// header of store migration 20261123134547_fix_beside_steps, which ships the block with the
// migration, and a fenced block of docs/kubernetes.md, under "Rolling back the release that starts
// a fix beside the other steps", which is where an operator reads it at a rollback. internal/store's
// TestMigrationFixBesideStepsKeepsEachRowAndItsWayBack runs the header's block over queued rows of
// two projects and reads what it leaves; this test holds the guide's block to that one, as the
// owner's steps' way back is held (owner_steps_way_back_doc_test.go).
const (
	// fixBesideStepsWayBackSection is the heading of the guide's section that gives the way back.
	fixBesideStepsWayBackSection = "Rolling back the release that starts a fix beside the other steps"
	// fixBesideStepsWayBackMark is the line that deletes the migration's version row, which no
	// other rollback's block carries.
	fixBesideStepsWayBackMark = "delete from schema_version where version = 20261123134547;"
)

// The guide's way back of the release that starts a fix beside the other steps is the block the
// migration's header carries, line for line. An edit to either side alone fails here: a line
// changed in the guide and not in the header would otherwise never reach the test that runs the
// header's block.
func TestTheFixBesideStepsWayBackIsOneTextInTheHeaderAndTheGuide(t *testing.T) {
	name := "the header of " + migrationNames(t, migrations.FixBesideSteps)[0]
	assertIsTheGuidesBlock(t, heldBlock{Text: headersWayBack(t, migrations.FixBesideSteps, name), Name: name, Label: "the header",
		Why: "internal/store's test runs the header's block and an operator the guide's"},
		fixBesideStepsWayBackSection, fixBesideStepsWayBackMark)
}
