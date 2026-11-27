package textcheck

// This test reads the store's migrations through migrations.All: its import puts their embed
// in the go list closure of the text-check run, which needs no row, so the guide is its one row.
//ci:reads docs/kubernetes.md `packages/daemon/internal/textcheck/waiting_line_texts_test.go#shippedText` reads it under ../../../.. for phaseEntryList

import (
	"errors"
	"fmt"
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
// migrations in both directions. The sentence that opens the list carries no count and no
// contract number: every change that adds a migration would have to edit the one, and every
// change of the daemon API contract the other, and the list it opens is held to the migrations
// of the tree it ships in, which is all the sentence says.
//
// The migrations are read through migrations.All, the set the daemon's binary embeds and its
// Migrate applies, rather than through a copy. That package is the migrations themselves and the
// rule of their names, so importing it puts in this job's filter the files whose change these
// tests must see, and nothing else of the daemon.
//
// The list is what a reader sees of it (visibleMarkdown): a name inside an HTML comment counts
// for nothing, since the rendered guide does not show it, and a list whose opening sentence or
// ending line a comment hides is no list a reader finds. A migration the list describes is one an
// entry says what the block does to: one the entry opens on, alone or joined by "and" to the
// others it speaks for (entrySubjects). A name anywhere else in an entry, a later clause or a
// parenthesis or another's reference, is a mention in passing, which tells the operator nothing
// of what the block does to that migration, so it describes nothing; the list's every name is
// still held to the embedded set.
const (
	// phaseEntryMigration is the migration whose way back the list follows, by the name the guide
	// (wayBackGuide, way_back_doc_test.go) writes. The list's subject is every embedded migration
	// with a higher version.
	phaseEntryMigration = "20261111225552_phase_entry"
	// phaseEntryListEnd opens the line after the list, the one that names the identifier of the
	// phase entry's block.
	phaseEntryListEnd = "The `20261111225552` is the identifier of "
	// phaseEntryListOpening is the sentence that opens the list, the whole of its line.
	phaseEntryListOpening = "The migrations recorded above this one are these."
	// migrationName is a migration's name as the guide writes it, the file's name without
	// `.up.sql`: the one rule both patterns below find a name by.
	migrationName = `\d{14}_\w+`
)

var (
	// migrationNameInGuide finds a migration's name anywhere in a text.
	migrationNameInGuide = regexp.MustCompile(`\b` + migrationName)
	// entrySubject is one migration an entry opens on, its group holding the name: the first opens
	// the entry and each after it follows " and ", each perhaps followed by the ", of <reference>,"
	// of the change it came with, which holds no subject of its own.
	entrySubject = regexp.MustCompile(`(?:^| and )(` + migrationName + `)(?:, of \S+,)?`)
)

// Every migration the daemon embeds above the phase entry is described in the list after the
// phase entry's way back. An operator who rolls back by the list is told nothing of a migration
// it leaves out, or names only in passing: neither that the block holds over it nor that it must
// be reversed first.
func TestTheGuideNamesEveryMigrationAboveThePhaseEntry(t *testing.T) {
	above, _ := embeddedMigrationsAboveThePhaseEntry(t)
	if missing := phaseEntryList(t).undescribed(above); len(missing) > 0 {
		t.Errorf("%s's list of the migrations recorded above %s describes no %s: give each an entry "+
			"beside the one it belongs with, opening on its name, alone or joined by \"and\" to the others "+
			"the entry speaks for, saying what it adds or changes and what the phase entry's block does to "+
			"it; a name inside an HTML comment, or in passing elsewhere in an entry, does not count",
			wayBackGuide, phaseEntryMigration, strings.Join(missing, ", "))
	}
}

// The list names no migration but the ones the daemon embeds above the phase entry. A name that
// no migration has, or one at or below the phase entry, tells the operator of a migration the
// block has nothing to do with, or misspells one it has.
func TestTheGuidesListAboveThePhaseEntryNamesOnlyThoseMigrations(t *testing.T) {
	above, all := embeddedMigrationsAboveThePhaseEntry(t)
	for _, name := range phaseEntryList(t).strays(above) {
		if slices.Contains(all, name) {
			t.Errorf("%s's list of the migrations recorded above %s names %s, which is not above it",
				wayBackGuide, phaseEntryMigration, name)
			continue
		}
		t.Errorf("%s's list of the migrations recorded above %s names %s, and the daemon embeds no "+
			"migration of that name", wayBackGuide, phaseEntryMigration, name)
	}
}

// A migration the list names only inside an HTML comment is one the operator never reads: the
// rendered guide shows nothing of it. So the list counts no name a comment holds, whether the
// comment fills a line, follows an entry or spans lines, and still counts every name of an entry
// the reader sees. A comment that hides the sentence opening the list, opened before it or around
// it alone, or one never closed, which hides the line that ends the list, leaves no list a reader
// sees, so the list is refused as hidden rather than read. A comment closed before the sentence,
// and a "<!--" inside a fenced block, hide nothing of it. Each case is a list as the guide writes
// one (phaseEntryListText), after the text before it.
func TestTheListCountsNoMigrationNamedOnlyInAnHTMLComment(t *testing.T) {
	for _, list := range []struct {
		shape, before, entries string
		want                   []string
		err                    error
	}{
		{"a comment on a line of its own", "",
			"20990101000001_seen adds a column.\n<!-- 20990101000002_hidden adds a table. -->",
			[]string{"20990101000001_seen"}, nil},
		{"a comment after an entry", "",
			"20990101000001_seen adds a column. <!-- 20990101000002_hidden adds a table. -->",
			[]string{"20990101000001_seen"}, nil},
		{"a comment across lines", "",
			"20990101000001_seen adds a column.\n<!--\n20990101000002_hidden adds a table.\n-->",
			[]string{"20990101000001_seen"}, nil},
		{"a comment never closed", "",
			"20990101000001_seen adds a column.\n<!-- 20990101000002_hidden adds a table.",
			nil, errHidden},
		{"one visible entry for two migrations", "",
			"20990101000001_seen and 20990101000003_seen_with_it touch none of the six.",
			[]string{"20990101000001_seen", "20990101000003_seen_with_it"}, nil},
		{"a comment opened before the opening sentence and closed inside the list", "<!--\n",
			"20990101000002_hidden adds a table.\n-->\n20990101000001_seen adds a column.",
			nil, errHidden},
		{"the whole list inside a comment", "<!--\n",
			"20990101000001_seen adds a column.\n-->",
			nil, errHidden},
		{"the opening sentence alone inside a comment", "<!--\n",
			"-->\n20990101000001_seen adds a column.",
			nil, errHidden},
		{"a comment closed before the opening sentence", "<!--\n20990101000002_hidden adds a table.\n-->\n",
			"20990101000001_seen adds a column.",
			[]string{"20990101000001_seen"}, nil},
		{"a comment opened inside a fenced block before the list", "```\n<!--\n```\n",
			"20990101000001_seen adds a column.",
			[]string{"20990101000001_seen"}, nil},
	} {
		listing, err := readPhaseEntryList(list.shape, phaseEntryListText(list.before, list.entries))
		if !errors.Is(err, list.err) {
			t.Errorf("%s: reading the list answers the error %v, want %v: %q", list.shape, err, list.err,
				list.before+list.entries)
			continue
		}
		if !slices.Equal(listing.described, list.want) || !slices.Equal(listing.named, list.want) {
			t.Errorf("%s: the list describes %v and names %v, want %v for both: %q", list.shape,
				listing.described, listing.named, list.want, list.entries)
		}
	}
}

// A migration counts as described only where an entry says what the block does to it: in the
// subjects the entry opens on, one migration or several joined by "and", each perhaps followed by
// the ", of <reference>," of the change it came with. A name anywhere else in an entry is a
// mention in passing, which tells the operator nothing of what the block does to that migration,
// so it describes nothing, while the list still names it and the other test still holds that name
// to the embedded set: a name after the entry's predicate, one after ", and" in a later clause or
// in parentheses, and a name that stands as another's reference. The cases are the shapes the
// guide's entries take, and those mentions in passing. Each case also runs the guide tests' two
// checks over its list: where a list names a migration only in passing, the forward check finds
// that migration undescribed among every name the list carries, and the reverse check finds it
// outside the described ones, so a check that read the other's names fails here.
func TestTheListDescribesOnlyTheMigrationsThatOpenAnEntry(t *testing.T) {
	for _, list := range []struct {
		shape, entry        string
		described, mentions []string
	}{
		{"one migration opening its entry",
			"20990101000001_alone adds a column and touches none of the six.",
			[]string{"20990101000001_alone"}, nil},
		{"one migration and the change it came with",
			"20990101000001_alone, of owner/repo#1, adds a column and touches none of the six.",
			[]string{"20990101000001_alone"}, nil},
		{"several migrations joined by and",
			"20990101000001_first and 20990101000002_second, of owner/repo#2, and 20990101000003_third, " +
				"of owner/repo#3, touch none of the six either.",
			[]string{"20990101000001_first", "20990101000002_second", "20990101000003_third"}, nil},
		{"a mention opening a later clause after \", and\"",
			"20990101000001_first writes into three of the six, and 20990101000002_passing adds the table " +
				"it marks, so the block above reverses both first.",
			[]string{"20990101000001_first"}, []string{"20990101000002_passing"}},
		{"a mention in parentheses",
			"20990101000001_alone adds a column. (The same release also includes 20990101000002_passing.)",
			[]string{"20990101000001_alone"}, []string{"20990101000002_passing"}},
		{"a mention after \", and\" in parentheses",
			"20990101000001_alone adds a column. (The same release also includes an outbox check, and " +
				"20990101000002_passing.)",
			[]string{"20990101000001_alone"}, []string{"20990101000002_passing"}},
		{"a mention after and inside a clause",
			"20990101000001_alone adds a column and 20990101000002_passing reads it.",
			[]string{"20990101000001_alone"}, []string{"20990101000002_passing"}},
		{"a mention as the reference of another's change",
			"20990101000001_alone, of 20990101000002_passing, adds a column.",
			[]string{"20990101000001_alone"}, []string{"20990101000002_passing"}},
	} {
		listing, err := readPhaseEntryList(list.shape, phaseEntryListText("", list.entry))
		if err != nil {
			t.Errorf("%s: %v", list.shape, err)
			continue
		}
		want := append(slices.Clone(list.described), list.mentions...)
		if !slices.Equal(listing.described, list.described) || !slices.Equal(listing.named, want) {
			t.Errorf("%s: the list describes %v and names %v, want %v described and %v named: %q", list.shape,
				listing.described, listing.named, list.described, want, list.entry)
		}
		if missing := listing.undescribed(want); !slices.Equal(missing, list.mentions) {
			t.Errorf("%s: the forward check finds %v of %v undescribed, want %v: %q", list.shape, missing, want,
				list.mentions, list.entry)
		}
		if strays := listing.strays(list.described); !slices.Equal(strays, list.mentions) {
			t.Errorf("%s: the reverse check finds %v outside %v, want %v: %q", list.shape, strays,
				list.described, list.mentions, list.entry)
		}
	}
}

// phaseEntryListText is a text that holds a list as the guide writes one: before, then the
// sentence that opens the list, its entries (one or several lines, no newline after the last),
// and the line that ends it.
func phaseEntryListText(before, entries string) string {
	return before + phaseEntryListOpening + "\n" + entries + "\n" + phaseEntryListEnd +
		"`20261111225552_phase_entry.up.sql`.\n"
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
			wayBackGuide)
	}
	return all[at+1:], all
}

// phaseEntryList is the list in the guide (readPhaseEntryList). A guide it cannot read the list of
// stops the test.
func phaseEntryList(t *testing.T) phaseEntryListing {
	t.Helper()
	listing, err := readPhaseEntryList(wayBackGuide, shippedText(t, wayBackGuide))
	if err != nil {
		t.Fatal(err)
	}
	return listing
}

// phaseEntryListing is the list as a reader sees it. It holds the migrations its entries describe,
// every migration name it carries, described or not, and its entries, one to a line. Each guide
// test reads it only through the method that is its check, so no check reads another's fields.
type phaseEntryListing struct{ described, named, entries []string }

// undescribed is every migration of above that no entry of the list describes, in above's order:
// what TestTheGuideNamesEveryMigrationAboveThePhaseEntry reports.
func (l phaseEntryListing) undescribed(above []string) []string {
	var missing []string
	for _, name := range above {
		if !slices.Contains(l.described, name) {
			missing = append(missing, name)
		}
	}
	return missing
}

// strays is every name the list carries that above does not hold, sorted, each once: what
// TestTheGuidesListAboveThePhaseEntryNamesOnlyThoseMigrations reports.
func (l phaseEntryListing) strays(above []string) []string {
	var strays []string
	for _, name := range l.named {
		if !slices.Contains(above, name) {
			strays = append(strays, name)
		}
	}
	slices.Sort(strays)
	return slices.Compact(strays)
}

// errHidden is why a check refuses a line it reads the guide by: only an HTML comment holds it.
var errHidden = errors.New("only an HTML comment holds it, and the rendered guide shows none of it")

// readPhaseEntryList reads the list in text, the guide called name, where a reader sees it
// (visibleMarkdown): the lines after its opening sentence, up to the line phaseEntryListEnd opens.
// It answers the migrations the list describes, every migration name it carries, and its visible
// lines, one entry to a line. A text with no such sentence or with two, with no such line after
// it, or whose list names no migration is refused: each would otherwise read as a list that names
// nothing, which
// TestTheGuidesListAboveThePhaseEntryNamesOnlyThoseMigrations passes. A sentence or line that only
// an HTML comment holds is refused as hidden (errHidden), a comment opened before the sentence
// among them: that comment hides the sentence and whatever of the list it runs over.
func readPhaseEntryList(name, text string) (phaseEntryListing, error) {
	written, lines := strings.Split(text, "\n"), strings.Split(visibleMarkdown(text), "\n")
	start := -1
	for i, line := range lines {
		if line != phaseEntryListOpening {
			continue
		}
		if start >= 0 {
			return phaseEntryListing{}, fmt.Errorf("%s opens the list of the migrations above %s twice, at "+
				"lines %d and %d", name, phaseEntryMigration, start+1, i+1)
		}
		start = i
	}
	if start < 0 && slices.Contains(written, phaseEntryListOpening) {
		return phaseEntryListing{}, fmt.Errorf("%s's line %q, which opens the list of the migrations "+
			"recorded above %s: %w", name, phaseEntryListOpening, phaseEntryMigration, errHidden)
	}
	if start < 0 {
		return phaseEntryListing{}, fmt.Errorf("%s carries no line %q: that sentence, which carries no "+
			"count and no contract number, opens the list of the migrations recorded above %s after that "+
			"migration's way back", name, phaseEntryListOpening, phaseEntryMigration)
	}
	isEnd := func(line string) bool { return strings.HasPrefix(line, phaseEntryListEnd) }
	end := slices.IndexFunc(lines[start+1:], isEnd)
	if end < 0 && slices.ContainsFunc(written[start+1:], isEnd) {
		return phaseEntryListing{}, fmt.Errorf("%s's line opening %q after line %d, which ends the list of "+
			"the migrations above %s: %w", name, phaseEntryListEnd, start+1, phaseEntryMigration, errHidden)
	}
	if end < 0 {
		return phaseEntryListing{}, fmt.Errorf("%s carries no line opening %q after line %d, which ends the "+
			"list of the migrations above %s", name, phaseEntryListEnd, start+1, phaseEntryMigration)
	}
	var listing phaseEntryListing
	visible := strings.Join(lines[start+1:start+1+end], "\n")
	listing.named = migrationNameInGuide.FindAllString(visible, -1)
	if len(listing.named) == 0 {
		return phaseEntryListing{}, fmt.Errorf("%s's list after line %d names no migration", name, start+1)
	}
	listing.entries = strings.Split(visible, "\n")
	for _, entry := range listing.entries {
		listing.described = append(listing.described, entrySubjects(strings.TrimLeft(entry, " \t"))...)
	}
	return listing, nil
}

// entrySubjects is the migrations entry opens on, in order (entrySubject): the first opens the
// entry and each after it joins the one before. A name that does neither is past the subjects,
// and so is every name after it.
func entrySubjects(entry string) []string {
	var subjects []string
	at := 0
	for _, match := range entrySubject.FindAllStringSubmatchIndex(entry, -1) {
		if match[0] != at {
			break
		}
		subjects, at = append(subjects, entry[match[2]:match[3]]), match[1]
	}
	return subjects
}
