package textcheck

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
	header := stepRecordsWayBackOfTheHeader(t)
	guide := wayBackBlockOfTheGuide(t, stepRecordsWayBackMark,
		fmt.Sprintf("the header of migration %d", migrations.StepRecordsRollForward))
	if header == guide {
		return
	}
	t.Errorf("the header of migration %d and %s's way back disagree:\n%s"+
		"the store's test runs the header's block and an operator the guide's; change them in one commit",
		migrations.StepRecordsRollForward, wayBackGuide, wayBackBlockDifference("the header", header, guide))
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
	for _, migration := range stepRecordsMigrationNames(t) {
		if err := entryLinkIn(guide, migration, stepRecordsWayBackSection); err != nil {
			t.Errorf("%s's list of the migrations recorded above %s: %v: give the entry the link [%s](#%s), since "+
				"a rollback to a release before %s takes that section's steps with the phase entry's block",
				wayBackGuide, phaseEntryMigration, err, stepRecordsWayBackSection,
				sectionAnchor(stepRecordsWayBackSection), migration)
		}
	}
}

// stepRecordsMigrationNames is the name the guide writes of each of the release's migrations, read
// from the embedded migrations by version. A set without one of them stops the test, since the
// entry it would hold is the one that names it.
func stepRecordsMigrationNames(t *testing.T) []string {
	t.Helper()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	var names []string
	for _, migration := range all {
		if slices.Contains(stepRecordsMigrations, migration.Version) {
			names = append(names, strings.TrimSuffix(migration.Name, ".up.sql"))
		}
	}
	if len(names) != len(stepRecordsMigrations) {
		t.Fatalf("the daemon embeds %v of the release that records each step's migrations %v", names,
			stepRecordsMigrations)
	}
	return names
}

// stepRecordsWayBackOfTheHeader is the block the roll forward's header carries, read by the one
// reader of it (migrations.WayBack) that internal/store's test runs, so the text held to the guide
// is the text that test runs. A header with no such block stops the test, and so does a block that
// does not open with stepRecordsWayBackFirst: the guide's block is read from that line on.
func stepRecordsWayBackOfTheHeader(t *testing.T) string {
	t.Helper()
	block, err := migrations.WayBack(migrations.StepRecordsRollForward)
	if err != nil {
		t.Fatalf("read the way back of the step records' roll forward: %v", err)
	}
	if first, _, _ := strings.Cut(block, "\n"); first != stepRecordsWayBackFirst {
		t.Fatalf("the way back of migration %d opens %q, want %q", migrations.StepRecordsRollForward, first, stepRecordsWayBackFirst)
	}
	return block
}
