package textcheck

import "testing"

// The way back of the release that stores no tree (dispatch://INFRA-4730, R22 of
// dispatch://INFRA-3320) is in the header of store migration 20261122154645_issue_facts, which
// ships the block with the migration, and in a fenced block of docs/kubernetes.md, under "Rolling
// back the release that stores no tree", where an operator reads it at a rollback, and which
// scripts/e2e/tree-rollback.sh reads and runs. internal/store's
// TestTheTreeConversionWayBackLeavesWhatTheReleaseBeforeReads runs the header's block over the rows
// this release leaves and reads what it leaves. This test holds the guide's block to that one, so
// the block an operator runs is the block that test proves. The guide's second block of that
// section, the relabel of each Sandbox, is kubectl and no SQL, so the header carries none of it,
// and it holds no line of the mark below (wayBackBlockIn refuses two blocks that hold one mark).
// The split keeps the guide out of the store's CI filter, as the set-back sequence's way back does
// (set_back_sequence_way_back_doc_test.go).
const (
	// treeConversionVersion is the version of the migration whose header carries the block.
	treeConversionVersion = 20261122154645
	// treeConversionWayBackSection is the heading of the guide's section that gives the way back.
	treeConversionWayBackSection = "Rolling back the release that stores no tree"
	// treeConversionWayBackMark is the line that deletes the project's record of the conversion,
	// which no other rollback's block carries.
	treeConversionWayBackMark = "delete from tree_conversions where project = '<PROJECT>';"
)

// The guide's way back of the release that stores no tree is the block the migration's header
// carries, line for line. An edit to either side alone fails here: a line changed in the guide and
// not in the header would otherwise never reach the test that runs the header's block.
func TestTheTreeConversionWayBackIsOneTextInTheHeaderAndTheGuide(t *testing.T) {
	name := "the header of " + migrationNames(t, treeConversionVersion)[0]
	assertIsTheGuidesBlock(t, heldBlock{Text: headersWayBack(t, treeConversionVersion, name), Name: name, Label: "the header",
		Why: "internal/store's test runs the header's block and an operator the guide's"},
		treeConversionWayBackSection, treeConversionWayBackMark)
}

// The guide's list of the migrations recorded above the phase entry sends an operator from the
// migration of the release that stores no tree to that release's own way back: the migration's
// entry links the section that gives the block. The list's own checks hold its names alone
// (phase_entry_migrations_doc_test.go), so an entry that kept the name and lost the link, or linked
// another rollback's section, would pass them. An operator who rolls back below this release
// would then have the phase entry's block alone, and the release before would refuse the rows this
// release left.
func TestTheTreeConversionMigrationsEntryLinksItsWayBack(t *testing.T) {
	migration := migrationNames(t, treeConversionVersion)[0]
	if err := entryLinkIn(shippedText(t, wayBackGuide), migration, treeConversionWayBackSection); err != nil {
		t.Errorf("%s's list of the migrations recorded above %s: %v: give the entry the link [%s](#%s), since "+
			"a rollback to a release before %s runs that section's step first",
			wayBackGuide, phaseEntryMigration, err, treeConversionWayBackSection,
			sectionAnchor(treeConversionWayBackSection), migration)
	}
}
