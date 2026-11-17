package textcheck

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/store/migrations"
)

// The way back of store migration 20261111225552_phase_entry in docs/kubernetes.md drops six
// columns and deletes that migration's own version row, and the lines after its block name each
// migration recorded above it and say what the block does to it: it holds over the migration, or
// the migration is reversed first. An operator rolls a store back by that list. A migration the
// list leaves out is one nobody told them about, and a name it carries that no migration has
// sends them looking for a way back that does not exist. Every change that adds a migration of the
// daemon puts one more above the phase entry, so these tests hold the list to the embedded
// migrations in both directions. The sentence that opens the list carries no count: every such
// change would have to edit it, and nothing here checks a count.
//
// The migrations are read through migrations.All, the set the daemon's binary embeds and its
// Migrate applies, rather than through a copy. That package is the migrations themselves and the
// rule of their names, so importing it puts in this job's filter the files whose change these
// tests must see, and nothing else of the daemon.
//
// A name counts wherever it stands in the list, because one entry may speak for several
// migrations. So a migration named only in passing inside another's entry would pass for one the
// list describes: name a migration in the list only in the entry that says what the block does
// to it.
const (
	// phaseEntryMigration is the migration whose way back the list follows, by the name the guide
	// writes. The list's subject is every embedded migration with a higher version.
	phaseEntryMigration = "20261111225552_phase_entry"
	// phaseEntryListGuide is the guide that carries the list, read under ../../../.. by
	// shippedText.
	phaseEntryListGuide = "docs/kubernetes.md"
	// phaseEntryListEnd opens the line after the list, the one that names the identifier of the
	// phase entry's block.
	phaseEntryListEnd = "The `20261111225552` is the identifier of "
)

var (
	// phaseEntryListOpening is the sentence that opens the list. It names the contract of the
	// release it speaks of and no count.
	phaseEntryListOpening = regexp.MustCompile(`^The migrations recorded above this one at contract \d+ are these\.$`)
	// migrationNameInGuide is a migration's name as the guide writes it: the file's name without
	// `.up.sql`.
	migrationNameInGuide = regexp.MustCompile(`\b\d{14}_\w+`)
)

// Every migration the daemon embeds above the phase entry is named in the list after the phase
// entry's way back. An operator who rolls back by the list is told nothing of a migration it
// leaves out: neither that the block holds over it nor that it must be reversed first.
func TestTheGuideNamesEveryMigrationAboveThePhaseEntry(t *testing.T) {
	above, _ := embeddedMigrationsAboveThePhaseEntry(t)
	named := phaseEntryListNames(t)
	var missing []string
	for _, name := range above {
		if !slices.Contains(named, name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%s's list of the migrations recorded above %s does not name %s: give each an entry "+
			"beside the one it belongs with, saying what it adds or changes and what the phase entry's "+
			"block does to it", phaseEntryListGuide, phaseEntryMigration, strings.Join(missing, ", "))
	}
}

// The list names no migration but the ones the daemon embeds above the phase entry. A name that
// no migration has, or one at or below the phase entry, tells the operator of a migration the
// block has nothing to do with, or misspells one it has.
func TestTheGuidesListAboveThePhaseEntryNamesOnlyThoseMigrations(t *testing.T) {
	above, all := embeddedMigrationsAboveThePhaseEntry(t)
	named := phaseEntryListNames(t)
	slices.Sort(named)
	for _, name := range slices.Compact(named) {
		switch {
		case slices.Contains(above, name):
		case slices.Contains(all, name):
			t.Errorf("%s's list of the migrations recorded above %s names %s, which is not above it",
				phaseEntryListGuide, phaseEntryMigration, name)
		default:
			t.Errorf("%s's list of the migrations recorded above %s names %s, and the daemon embeds no "+
				"migration of that name", phaseEntryListGuide, phaseEntryMigration, name)
		}
	}
}

// embeddedMigrationsAboveThePhaseEntry is every migration the daemon embeds above the phase entry,
// and every migration it embeds, each by the name the guide writes, in version order. A set
// without the phase entry stops the test, since the list it is held to follows that
// migration's way back.
func embeddedMigrationsAboveThePhaseEntry(t *testing.T) (above, all []string) {
	t.Helper()
	embedded, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	for _, migration := range embedded {
		all = append(all, strings.TrimSuffix(migration.Name, ".up.sql"))
	}
	// migrations.All answers in ascending version order, one migration to a version, so the
	// migrations above the phase entry are the ones after it.
	at := slices.Index(all, phaseEntryMigration)
	if at < 0 {
		t.Fatalf("the daemon embeds no %s, whose way back the list in %s follows", phaseEntryMigration,
			phaseEntryListGuide)
	}
	return all[at+1:], all
}

// phaseEntryListNames is every migration name the list carries: the lines after its opening
// sentence, up to the line phaseEntryListEnd opens. A guide with no such sentence or with two,
// with no such line after it, or whose list names no migration stops the test: each would
// otherwise read as a list that names nothing, which
// TestTheGuidesListAboveThePhaseEntryNamesOnlyThoseMigrations passes.
func phaseEntryListNames(t *testing.T) []string {
	t.Helper()
	lines := strings.Split(shippedText(t, phaseEntryListGuide), "\n")
	start := -1
	for i, line := range lines {
		if !phaseEntryListOpening.MatchString(line) {
			continue
		}
		if start >= 0 {
			t.Fatalf("%s opens the list of the migrations above %s twice, at lines %d and %d",
				phaseEntryListGuide, phaseEntryMigration, start+1, i+1)
		}
		start = i
	}
	if start < 0 {
		t.Fatalf("%s carries no line matching %s: that sentence, which carries no count, opens the list "+
			"of the migrations recorded above %s after that migration's way back", phaseEntryListGuide,
			phaseEntryListOpening, phaseEntryMigration)
	}
	end := slices.IndexFunc(lines[start+1:], func(line string) bool { return strings.HasPrefix(line, phaseEntryListEnd) })
	if end < 0 {
		t.Fatalf("%s carries no line opening %q after line %d, which ends the list of the migrations above %s",
			phaseEntryListGuide, phaseEntryListEnd, start+1, phaseEntryMigration)
	}
	var names []string
	for _, line := range lines[start+1 : start+1+end] {
		names = append(names, migrationNameInGuide.FindAllString(line, -1)...)
	}
	if len(names) == 0 {
		t.Fatalf("%s's list after line %d names no migration", phaseEntryListGuide, start+1)
	}
	return names
}
