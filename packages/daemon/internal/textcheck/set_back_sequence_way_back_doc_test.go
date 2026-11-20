package textcheck

import "testing"

// The way back of the release that lets the owning architect's status stand is in two files: the
// header of store migration 20261120012547_set_back_observed_seq, which ships the block with the
// migration, and a fenced block of docs/kubernetes.md, under "Rolling back the release that lets
// the owning architect's status stand", where an operator reads it at a rollback.
// internal/store's TestTheSetBackSequenceWayBackLeavesWhatTheReleaseBeforeReads runs the header's
// block over pending rows and reads what it leaves. This test holds the guide's block to that one,
// so the block an operator runs is the block that test proves. The split keeps the guide out of
// the store's CI filter, as the plan ratings' way back does (plan_ratings_way_back_doc_test.go).
const (
	// setBackSequenceVersion is the version of the migration whose header carries the block.
	setBackSequenceVersion = 20261120012547
	// setBackSequenceWayBackSection is the heading of the guide's section that gives the way back.
	setBackSequenceWayBackSection = "Rolling back the release that lets the owning architect's status stand"
	// setBackSequenceWayBackMark is the line that takes the sequence off the set-backs of the
	// project that rolls back, which no other rollback's block carries.
	setBackSequenceWayBackMark = "update outbox set payload = payload - 'observedSeq' where kind = 'dispatch_status' and payload ? 'observedSeq' and split_part(issue, '-', 1) = '<PROJECT>';"
)

// The guide's way back of the release that lets the owning architect's status stand is the block
// the migration's header carries, line for line. An edit to either side alone fails here: a line
// changed in the guide and not in the header would otherwise never reach the test that runs the
// header's block.
func TestTheSetBackSequenceWayBackIsOneTextInTheHeaderAndTheGuide(t *testing.T) {
	name := "the header of " + migrationNames(t, setBackSequenceVersion)[0]
	assertIsTheGuidesBlock(t, heldBlock{Text: headersWayBack(t, setBackSequenceVersion, name), Name: name, Label: "the header",
		Why: "internal/store's test runs the header's block and an operator the guide's"},
		setBackSequenceWayBackSection, setBackSequenceWayBackMark)
}

// The guide's list of the migrations recorded above the phase entry sends an operator from the
// migration of the release that lets the owning architect's status stand to that release's own
// way back: the migration's entry links the section that gives the block. The list's own checks
// hold its names alone (phase_entry_migrations_doc_test.go), so an entry that kept the name and
// lost the link, or linked another rollback's section, would pass them. An operator who rolls back
// below this release would then have the phase entry's block alone.
func TestTheSetBackSequenceMigrationsEntryLinksItsWayBack(t *testing.T) {
	migration := migrationNames(t, setBackSequenceVersion)[0]
	if err := entryLinkIn(shippedText(t, wayBackGuide), migration, setBackSequenceWayBackSection); err != nil {
		t.Errorf("%s's list of the migrations recorded above %s: %v: give the entry the link [%s](#%s), since "+
			"a rollback to a release before %s takes that section's steps with the phase entry's block",
			wayBackGuide, phaseEntryMigration, err, setBackSequenceWayBackSection,
			sectionAnchor(setBackSequenceWayBackSection), migration)
	}
}
