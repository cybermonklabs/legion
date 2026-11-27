package textcheck

import "testing"

// The way back of the release that credits a holder's news at its take's turn is in the header of
// store migration 20261122160000_design_news_credit_at_turn_start, which ships the block with the
// migration, and in a fenced block of docs/kubernetes.md, under "Rolling back the release that
// credits a holder's news at its take's turn", where an operator reads it at a rollback.
// internal/store's TestADeliverysCopyCreditsOnlyOnceItsTakesTurnStartedHoweverTheStoreWasMigrated
// runs the header's block and reads what the function then counts. This test holds the guide's
// block to that one, so the block an operator runs is the block that test proves. The split keeps
// the guide out of the store's CI filter, as the set-back sequence's way back does
// (set_back_sequence_way_back_doc_test.go).
const (
	// creditAtTurnStartVersion is the version of the migration whose header carries the block.
	creditAtTurnStartVersion = 20261122160000
	// creditAtTurnStartWayBackSection is the heading of the guide's section that gives the way back.
	creditAtTurnStartWayBackSection = "Rolling back the release that credits a holder's news at its take's turn"
	// creditAtTurnStartWayBackMark is the line that deletes the migration's version row, which no
	// other rollback's block carries.
	creditAtTurnStartWayBackMark = "delete from schema_version where version = 20261122160000;"
)

// The guide's way back of the release that credits a holder's news at its take's turn is the block
// the migration's header carries, line for line. An edit to either side alone fails here: a line
// changed in the guide and not in the header would otherwise never reach the test that runs the
// header's block.
func TestTheCreditAtTurnStartWayBackIsOneTextInTheHeaderAndTheGuide(t *testing.T) {
	name := "the header of " + migrationNames(t, creditAtTurnStartVersion)[0]
	assertIsTheGuidesBlock(t, heldBlock{Text: headersWayBack(t, creditAtTurnStartVersion, name), Name: name, Label: "the header",
		Why: "internal/store's test runs the header's block and an operator the guide's"},
		creditAtTurnStartWayBackSection, creditAtTurnStartWayBackMark)
}

// The guide's list of the migrations recorded above the phase entry sends an operator from the
// migration of the release that credits a holder's news at its take's turn to that release's own
// way back: the migration's entry links the section that gives the block. An entry that kept the
// name and lost the link would pass the list's own checks, and an operator who rolls back below
// this release would then run the task numbers' block with this function left reading the column
// that block drops.
func TestTheCreditAtTurnStartMigrationsEntryLinksItsWayBack(t *testing.T) {
	migration := migrationNames(t, creditAtTurnStartVersion)[0]
	if err := entryLinkIn(shippedText(t, wayBackGuide), migration, creditAtTurnStartWayBackSection); err != nil {
		t.Errorf("%s's list of the migrations recorded above %s: %v: give the entry the link [%s](#%s), since "+
			"a rollback to a release before %s takes that section's block before the task numbers' block",
			wayBackGuide, phaseEntryMigration, err, creditAtTurnStartWayBackSection,
			sectionAnchor(creditAtTurnStartWayBackSection), migration)
	}
}
