package textcheck

import "testing"

// The way back of the release that gives up on intake messages stands in two files: the header of
// store migration 20261120144241_intake_failures, which ships the block with the migration, and a
// fenced block of docs/kubernetes.md, under "Rolling back the release that gives up on intake
// messages", which is where an operator reads it at a rollback. internal/store's
// TestTheIntakeGiveUpWayBackLeavesWhatTheReleaseBeforeReads and
// TestTheIntakeGiveUpWayBackDeletesOnlyTheRowsOfTheProjectThatRollsBack run the header's block
// over a store and read what it leaves; this test holds the guide's block to that one, so the
// block an operator runs is the block those tests prove. The split keeps the guide out of the
// daemon Go lane's filter, as the phase entry's way back does (way_back_doc_test.go).
//
// The header's block is read by the one reader of a migration's way back (migrations.WayBack),
// which internal/store's tests run, so the text held to the guide is the text those tests run, and
// the guide's block is the one under that heading that holds the one line only that rollback's
// block holds (wayBackBlockOfTheGuide).
const (
	// intakeGiveUpVersion is the version of the migration whose header carries the block, which
	// migrations.WayBack reads it by and whose name the embedded set gives (migrationNames), so
	// the header this test reads and the entry it holds the guide to are one migration.
	intakeGiveUpVersion = 20261120144241
	// intakeGiveUpWayBackSection is the heading of the guide's section that gives the way back.
	intakeGiveUpWayBackSection = "Rolling back the release that gives up on intake messages"
	// intakeGiveUpWayBackMark is the line that deletes the open failures of the project that rolls
	// back, which no other rollback's block carries.
	intakeGiveUpWayBackMark = "delete from intake_failures where project = '<PROJECT>' and given_up_at is null;"
)

// The guide's way back of the release that gives up on intake messages is the block the
// migration's header carries, line for line. An edit to either side alone fails here: a line
// changed in the guide and not in the header would otherwise never reach the tests that run the
// header's block.
func TestTheIntakeGiveUpWayBackIsOneTextInTheHeaderAndTheGuide(t *testing.T) {
	name := "the header of " + migrationNames(t, intakeGiveUpVersion)[0]
	assertIsTheGuidesBlock(t, heldBlock{Text: headersWayBack(t, intakeGiveUpVersion, name), Name: name, Label: "the header",
		Why: "internal/store's tests run the header's block and an operator the guide's"},
		intakeGiveUpWayBackSection, intakeGiveUpWayBackMark)
}
