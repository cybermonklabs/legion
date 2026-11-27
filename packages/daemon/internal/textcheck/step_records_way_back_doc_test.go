package textcheck

// This test reads the roll forward's header through migrations.WayBack, whose embed is in the
// go list closure of the text-check run, as the row above says, so the guide is its one row.
//ci:reads docs/kubernetes.md `packages/daemon/internal/textcheck/waiting_line_texts_test.go#shippedText` reads it under ../../../.. for TestTheStepRecordsMigrationsEntriesLinkTheirWayBack and TestTheStepRecordsWayBackIsOneTextInTheRollForwardAndTheGuide (through `packages/daemon/internal/textcheck/way_back_doc_test.go#wayBackBlockOfTheGuide`)

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/store/migrations"
)

// The way back of the release that records each step stands in the header of its roll forward,
// 20261117064152_step_starts_roll_forward, which ships it with the migration it re-arms, and in a
// fenced block of docs/kubernetes.md, which is where an operator reads it at a rollback.
// internal/store's TestTheRollForwardMarksUncountedEachIssueTheEarlierReleaseMoved runs the
// header's block; this holds the guide's block to it, byte for byte. The header is read from the
// embedded migrations (internal/store/migrations, as the guide's migration list is), so neither
// half reads a file of another package.
const (
	// stepRecordsWayBackFirst opens the block in the header and in the guide.
	stepRecordsWayBackFirst = "insert into step_starts_rollback (issue, tree, witness, taken_at)"
	// stepRecordsWayBackSection is the heading of the guide's section that gives the block, which
	// the entry of each of the release's migrations in the guide's list of the migrations above the
	// phase entry links.
	stepRecordsWayBackSection = "Rolling back the release that records each step"
)

// stepRecordsMigrations are the versions of the release's migrations: the outbox kind's, the
// tables' and the roll forward's.
var stepRecordsMigrations = []int{20261117000528, 20261117004117, migrations.StepRecordsRollForward}

// stepRecordsWayBackMark is the line that deletes the roll forward's version row, which closes the
// block and which no other rollback's block carries.
var stepRecordsWayBackMark = fmt.Sprintf("delete from schema_version where version = %d;", migrations.StepRecordsRollForward)

// The guide's way back of the release that records each step is the block its roll forward's
// header carries, line for line: an edit to either alone fails here, since the store's test runs
// the header's and an operator runs the guide's.
func TestTheStepRecordsWayBackIsOneTextInTheRollForwardAndTheGuide(t *testing.T) {
	name := "the header of " + migrationNames(t, migrations.StepRecordsRollForward)[0]
	header := headersWayBack(t, migrations.StepRecordsRollForward, name)
	if first, _, _ := strings.Cut(header, "\n"); first != stepRecordsWayBackFirst {
		t.Fatalf("the way back of migration %d opens %q, want %q", migrations.StepRecordsRollForward, first, stepRecordsWayBackFirst)
	}
	assertIsTheGuidesBlock(t, heldBlock{Text: header, Name: name, Label: "the header",
		Why: "the store's test runs the header's block and an operator the guide's"},
		stepRecordsWayBackSection, stepRecordsWayBackMark)
}

// The guide's list of the migrations recorded above the phase entry sends an operator from each
// migration of the release that records each step to that release's own way back: each entry links
// the section that gives the block, which the page carries under that one heading. The list's own
// checks hold its names alone (phase_entry_migrations_doc_test.go), so an entry that kept its name
// and lost the link, or linked another rollback's section, and a heading renamed under its links,
// would pass them and leave an operator who rolls back below this release with the phase entry's
// block alone. Each is held where a reader follows it (entryLinkIn, sectionHeadingIn).
func TestTheStepRecordsMigrationsEntriesLinkTheirWayBack(t *testing.T) {
	guide := shippedText(t, wayBackGuide)
	if err := sectionHeadingIn(guide, stepRecordsWayBackSection); err != nil {
		t.Errorf("%s's section %v: the entries of the release's migrations link it by its anchor, #%s, "+
			"so the heading and those links change together", wayBackGuide, err, sectionAnchor(stepRecordsWayBackSection))
	}
	for _, migration := range migrationNames(t, stepRecordsMigrations...) {
		if err := entryLinkIn(guide, migration, stepRecordsWayBackSection); err != nil {
			t.Errorf("%s's list of the migrations recorded above %s: %v: give the entry the link [%s](#%s), since "+
				"a rollback to a release before %s takes that section's steps with the phase entry's block",
				wayBackGuide, phaseEntryMigration, err, stepRecordsWayBackSection,
				sectionAnchor(stepRecordsWayBackSection), migration)
		}
	}
}

// migrationNames is the name the guide writes of each version given, read from the embedded
// migrations: the migration's file name without its `.up.sql`. A version the set does not hold
// stops the test by name, since the entry that names it is what the caller holds the guide to.
func migrationNames(t *testing.T, versions ...int) []string {
	t.Helper()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	var names []string
	for _, migration := range all {
		if slices.Contains(versions, migration.Version) {
			names = append(names, strings.TrimSuffix(migration.Name, ".up.sql"))
		}
	}
	if len(names) != len(versions) {
		t.Fatalf("the daemon embeds %v of the migrations %v", names, versions)
	}
	return names
}
