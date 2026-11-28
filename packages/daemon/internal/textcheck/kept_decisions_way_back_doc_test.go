package textcheck

import "testing"

// The way back of the release that renames the kept decisions is in the header of store migration
// 20261124002047_kept_decisions, which ships the block with the migration, and in a fenced block of
// docs/kubernetes.md, under "Rolling back the release that renames the kept decisions", where an
// operator reads it at a rollback. internal/store's
// TestTheKeptDecisionsTakeTheirNamesAndTheirWayBackGivesTheOldOnesAgain runs the header's block
// over the names and values it moves, and internal/daemon's
// TestKeptDecisionsTheReleaseBeforeLeftAreToldOnceAfterTheRenameAndComeBackOnTheWayBack runs it
// beside the daemon's own reads. This test holds the guide's block to that one, so the block an
// operator runs is the block those tests prove. The split keeps the guide out of the store's CI
// filter, as the set-back sequence's way back does (set_back_sequence_way_back_doc_test.go).
const (
	// keptDecisionsVersion is the version of the migration whose header carries the block.
	keptDecisionsVersion = 20261124002047
	// keptDecisionsWayBackSection is the heading of the guide's section that gives the way back.
	keptDecisionsWayBackSection = "Rolling back the release that renames the kept decisions"
	// keptDecisionsWayBackMark is the line that deletes the migration's own version row, which no
	// other rollback's block carries.
	keptDecisionsWayBackMark = "delete from schema_version where version = 20261124002047;"
)

// The guide's way back of the release that renames the kept decisions is the block the
// migration's header carries, line for line. An edit to either side alone fails here: a line
// changed in the guide and not in the header would otherwise never reach the tests that run the
// header's block.
func TestTheKeptDecisionsWayBackIsOneTextInTheHeaderAndTheGuide(t *testing.T) {
	name := "the header of " + migrationNames(t, keptDecisionsVersion)[0]
	assertIsTheGuidesBlock(t, heldBlock{Text: headersWayBack(t, keptDecisionsVersion, name), Name: name, Label: "the header",
		Why: "internal/store's and internal/daemon's tests run the header's block and an operator the guide's"},
		keptDecisionsWayBackSection, keptDecisionsWayBackMark)
}

// The guide's list of the migrations recorded above the phase entry sends an operator from the
// migration of the release that renames the kept decisions to that release's own way back: the
// migration's entry links the section that gives the block. The list's own checks hold its names
// alone (phase_entry_migrations_doc_test.go), so an entry that kept the name and lost the link,
// or linked another rollback's section, would pass them. An operator who rolls back below this
// release would then have the phase entry's block alone, and the release before would read none
// of the renamed table.
func TestTheKeptDecisionsMigrationsEntryLinksItsWayBack(t *testing.T) {
	migration := migrationNames(t, keptDecisionsVersion)[0]
	if err := entryLinkIn(shippedText(t, wayBackGuide), migration, keptDecisionsWayBackSection); err != nil {
		t.Errorf("%s's list of the migrations recorded above %s: %v: give the entry the link [%s](#%s), since "+
			"a rollback to a release before %s runs that section's block with the phase entry's",
			wayBackGuide, phaseEntryMigration, err, keptDecisionsWayBackSection,
			sectionAnchor(keptDecisionsWayBackSection), migration)
	}
}
