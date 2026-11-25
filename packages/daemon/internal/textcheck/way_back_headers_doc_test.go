package textcheck

// This file reads the doc comment of migrations.WayBack from that package's source, and each header
// through migrations.WayBack. That package is in the go list closure of the text-check run, which
// needs no row, so the guide is this file's one row.
//ci:reads docs/kubernetes.md `packages/daemon/internal/textcheck/waiting_line_texts_test.go#shippedText` reads it under ../../../.. for TestEveryRollbackSectionTheMigrationListNamesIsInTheGuide (through phaseEntryList)

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/store/migrations"
)

// wayBackSource is the file that declares migrations.WayBack. Its doc comment names each header
// whose last paragraph is a way back.
const wayBackSource = "../store/migrations/embed.go"

var (
	// headerFileName finds the file name of a migration in a text: its version, its slug and
	// `.up.sql`.
	headerFileName = regexp.MustCompile(`\b\d+_\w+\.up\.sql\b`)
	// rollbackSectionNamed finds a section that an entry of the guide's migration list names. A
	// link whose text opens "Rolling back " gives its text and its anchor (groups 1 and 2). Such
	// words in double quotes give the words alone (group 3).
	rollbackSectionNamed = regexp.MustCompile(`\[(Rolling back [^\]]+)\]\(#([^)]*)\)|"(Rolling back [^"]+)"`)
)

// The doc comment of migrations.WayBack names each header whose last paragraph is a way back.
// No other check holds that list to the headers. A migration that adds a way back passes every
// other check with the list unchanged. This test takes the set from WayBack itself, run over each
// embedded migration. It names each header the list leaves out, and each name the list carries
// that no such header has.
func TestTheWayBackDocNamesEveryHeaderThatCarriesOne(t *testing.T) {
	carry := headersThatCarryAWayBack(t)
	named := headerFileName.FindAllString(wayBackDoc(t), -1)
	for _, name := range carry {
		if !slices.Contains(named, name) {
			t.Errorf("the doc comment of migrations.WayBack (%s) names no %s. The last paragraph of its "+
				"header is a way back by WayBack's own rule, so name it in that list", wayBackSource, name)
		}
	}
	slices.Sort(named)
	for _, name := range slices.Compact(named) {
		if !slices.Contains(carry, name) {
			t.Errorf("the doc comment of migrations.WayBack (%s) names %s. It is no embedded migration "+
				"whose header carries a way back by WayBack's rule, so take it out of that list", wayBackSource, name)
		}
	}
}

// Each entry of the guide's list of the migrations recorded above the phase entry sends an operator
// to its release's rollback section. An entry names it by a link, or by its words in double quotes.
// The list's own checks hold the names of migrations alone. So a heading renamed under its entries,
// or named wrongly in one, would pass them. This test holds each section an entry names to the one
// heading of its words, and each link to that heading's anchor.
func TestEveryRollbackSectionTheMigrationListNamesIsInTheGuide(t *testing.T) {
	guide := shippedText(t, wayBackGuide)
	sections := phaseEntryList(t).rollbackSections()
	if len(sections) == 0 {
		t.Fatalf("UNKNOWN: %s's list of the migrations recorded above %s names no rollback section, so "+
			"this test holds nothing", wayBackGuide, phaseEntryMigration)
	}
	for _, section := range sections {
		if err := rollbackSectionIn(guide, section); err != nil {
			t.Errorf("%s's list of the migrations recorded above %s: the entry of %s names a section: %v. "+
				"Give the guide that heading, or name in the entry the heading it has", wayBackGuide,
				phaseEntryMigration, section.entry, err)
		}
	}
}

// The section check reads both forms in which an entry names a section, and holds each to the page.
// A link and quoted words of a heading the page has pass. A heading the page lacks, a link to
// another heading's anchor, and a heading only an HTML comment holds are refused. A link whose text
// opens no "Rolling back " names no rollback section.
func TestTheSectionCheckHoldsEachRollbackSectionAnEntryNames(t *testing.T) {
	const (
		heading = "Rolling back the release that records each step"
		link    = "[" + heading + "](#rolling-back-the-release-that-records-each-step)"
		entry   = "20990101000001_recorded touches none of the six, so the block holds over it"
		page    = "#### " + heading + "\n\nThis release adds one outbox kind.\n\n"
	)
	for _, tc := range []struct {
		shape, page, entry string
		named              int
		refused            bool
	}{
		{"a link to a heading the page has", page, entry + ", and " + link + " comes first.", 1, false},
		{"quoted words of a heading the page has", page, entry + " (\"" + heading + "\" above).", 1, false},
		{"a link to a heading the page lacks", "#### Rolling back the release that rates each plan\n",
			entry + ", and " + link + " comes first.", 1, true},
		{"quoted words of a heading the page lacks", "", entry + " (\"" + heading + "\" above).", 1, true},
		{"a link to another heading's anchor", page,
			entry + ", and [" + heading + "](#rolling-back-the-release-that-rates-each-plan) comes first.", 1, true},
		{"a link to a heading only an HTML comment holds", "<!--\n" + page + "-->\n",
			entry + ", and " + link + " comes first.", 1, true},
		{"a link of other words", page, entry + " ([The memory guard](#the-memory-guard)).", 0, false},
	} {
		sections := phaseEntryListing{entries: []string{tc.entry}}.rollbackSections()
		if len(sections) != tc.named {
			t.Errorf("%s: the entry names %d rollback sections, want %d", tc.shape, len(sections), tc.named)
			continue
		}
		for _, section := range sections {
			if section.entry != "20990101000001_recorded" {
				t.Errorf("%s: the section is read as named by the entry of %q, want 20990101000001_recorded",
					tc.shape, section.entry)
			}
			if err := rollbackSectionIn(tc.page, section); (err != nil) != tc.refused {
				t.Errorf("%s: rollbackSectionIn = %v, want refused %t", tc.shape, err, tc.refused)
			}
		}
	}
}

// headersThatCarryAWayBack is the file name of each embedded migration whose header's last
// paragraph is a way back, in version order. Those are the versions migrations.WayBack answers
// with no error. A set with none stops the test, since a list held to nothing would pass.
func headersThatCarryAWayBack(t *testing.T) []string {
	t.Helper()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	var carry []string
	for _, migration := range all {
		if _, err := migrations.WayBack(migration.Version); err == nil {
			carry = append(carry, migration.Name)
		}
	}
	if len(carry) == 0 {
		t.Fatalf("UNKNOWN: migrations.WayBack answers none of the %d embedded migrations, so the list "+
			"in its doc comment is held to nothing", len(all))
	}
	return carry
}

// wayBackDoc is the doc comment of migrations.WayBack as wayBackSource declares it. A source with
// no such func, or a func with no doc comment, stops the test. A list this cannot read would read
// as a list that names nothing.
func wayBackDoc(t *testing.T) string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), wayBackSource, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", wayBackSource, err)
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Recv == nil && function.Name.Name == "WayBack" && function.Doc != nil {
			return function.Doc.Text()
		}
	}
	t.Fatalf("%s declares no func WayBack with a doc comment", wayBackSource)
	return ""
}

// rollbackSection is a section that an entry of the guide's migration list names: the migration
// the entry opens on, and the heading's words. A link also gives the anchor it names.
type rollbackSection struct {
	entry, heading, anchor string
	linked                 bool
}

// rollbackSections is each rollback section an entry of the list names (rollbackSectionNamed), in
// the list's order.
func (l phaseEntryListing) rollbackSections() []rollbackSection {
	var sections []rollbackSection
	for _, entry := range l.entries {
		for _, match := range rollbackSectionNamed.FindAllStringSubmatch(entry, -1) {
			section := rollbackSection{entry: migrationNameInGuide.FindString(entry), heading: match[3]}
			if match[1] != "" {
				section.heading, section.anchor, section.linked = match[1], match[2], true
			}
			sections = append(sections, section)
		}
	}
	return sections
}

// rollbackSectionIn holds text, a Markdown page, to a section an entry names. The page has one
// heading of its words in both readings (sectionHeadingIn). A link names that heading's anchor
// (sectionAnchor). Each error quotes the heading.
func rollbackSectionIn(text string, section rollbackSection) error {
	if err := sectionHeadingIn(text, section.heading); err != nil {
		return err
	}
	if section.linked && section.anchor != sectionAnchor(section.heading) {
		return fmt.Errorf("%q: its link names #%s, where the heading's anchor is #%s", section.heading,
			section.anchor, sectionAnchor(section.heading))
	}
	return nil
}
