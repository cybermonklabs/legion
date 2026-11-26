package textcheck

// This test reads the slots reading's migration through migrations.WayBack, as the plan
// ratings' does, so the guide is its one read.
//ci:reads docs/kubernetes.md `packages/daemon/internal/textcheck/waiting_line_texts_test.go#shippedText` reads it under ../../../.. for TestTheSlotsReadingWayBackIsOneTextInTheHeaderAndTheGuide (`packages/daemon/internal/textcheck/way_back_doc_test.go#assertIsTheGuidesBlock`, through `packages/daemon/internal/textcheck/way_back_doc_test.go#wayBackBlockOfTheGuide`) and TestTheSlotsReadingMigrationsEntryLinksItsWayBack (`packages/daemon/internal/textcheck/migration_entry_link_test.go#entryLinkIn` of `packages/daemon/internal/textcheck/waiting_line_texts_test.go#shippedText`)

import "testing"

// The way back of the release that reads the slots stands in two files: the header of store
// migration 20261121103000_slots_reading_times, which ships the block with the migration, and a
// fenced block of docs/kubernetes.md, under "Rolling back the release that reads the slots", which
// is where an operator reads it at a rollback. internal/store's
// TestTheSlotsReadingWayBackLeavesWhatTheReleaseBeforeReads runs the header's block over a store
// this release wrote and reads what it leaves; this test holds the guide's block to that one, so
// the block an operator runs is the block that test proves. The split keeps the guide out of the
// daemon Go lane's filter, as the phase entry's way back does (way_back_doc_test.go).
const (
	// slotsReadingVersion is the version of the migration whose header carries the block, which
	// migrations.WayBack reads it by and whose name the embedded set gives (migrationNames).
	slotsReadingVersion = 20261121103000
	// slotsReadingWayBackSection is the heading of the guide's section that gives the way back.
	slotsReadingWayBackSection = "Rolling back the release that reads the slots"
	// slotsReadingWayBackMark is the line that deletes the queued tick notices carrying a slots
	// reading, which no other rollback's block carries.
	slotsReadingWayBackMark = "delete from outbox where kind = 'controller_notice' and payload->>'kind' = 'tick' and payload ? 'slots';"
)

// The guide's way back of the release that reads the slots is the block the migration's header
// carries, line for line. An edit to either side alone fails here: a line changed in the guide and
// not in the header would otherwise never reach the test that runs the header's block.
func TestTheSlotsReadingWayBackIsOneTextInTheHeaderAndTheGuide(t *testing.T) {
	name := "the header of " + migrationNames(t, slotsReadingVersion)[0]
	assertIsTheGuidesBlock(t, heldBlock{Text: headersWayBack(t, slotsReadingVersion, name), Name: name, Label: "the header",
		Why: "internal/store's test runs the header's block and an operator the guide's"},
		slotsReadingWayBackSection, slotsReadingWayBackMark)
}

// The guide's list of the migrations recorded above the phase entry sends an operator from the
// migration of the release that reads the slots to that release's own way back: the migration's
// entry links the section that gives the block, held where a reader follows it (entryLinkIn).
func TestTheSlotsReadingMigrationsEntryLinksItsWayBack(t *testing.T) {
	migration := migrationNames(t, slotsReadingVersion)[0]
	if err := entryLinkIn(shippedText(t, wayBackGuide), migration, slotsReadingWayBackSection); err != nil {
		t.Errorf("%s's list of the migrations recorded above %s: %v: give the entry the link [%s](#%s), since "+
			"a rollback to a release before %s takes that section's steps",
			wayBackGuide, phaseEntryMigration, err, slotsReadingWayBackSection,
			sectionAnchor(slotsReadingWayBackSection), migration)
	}
}
