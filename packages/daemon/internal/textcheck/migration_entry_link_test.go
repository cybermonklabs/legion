package textcheck

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// The guide's list of the migrations recorded above the phase entry sends an operator from a
// migration to its release's own way back by a link in the migration's entry. These two checks
// hold that link where a reader follows it, in both readings of the page: the entry links the
// section's anchor (entryLinkIn), and the page has the one heading that anchor names
// (sectionHeadingIn).

var (
	// errNoEntry is why linkOfTheEntry refuses a reading: no line of it opens on the migration's
	// name.
	errNoEntry = errors.New("no entry opens on it")
	// errEntries is why linkOfTheEntry refuses a reading: more than one line of it opens on the
	// migration's name, where the list gives a migration one entry.
	errEntries = errors.New("more than one entry opens on it, where the list gives a migration one")
	// errNoLink is why linkOfTheEntry refuses a reading: the migration's entry links no section by
	// the section's anchor.
	errNoLink = errors.New("its entry links no section")
	// errNotOneSection is why headingOf refuses a reading: no heading of the section's text stands
	// outside a fenced block, or more than one does, so the anchor names no one section.
	errNotOneSection = errors.New("no one heading of it stands outside a fenced block, so its anchor names no one section")
)

// The entry check holds an entry's link in both readings of a page: an entry that links its section
// passes, and one that lost the link or links another rollback's section, one written twice and a
// page with no entry are refused for it. An entry, a link or a second entry that only an HTML
// comment holds, and a comment that splits the link, make the two readings disagree, and are
// refused for the comment. A "<!--" inside a fenced block hides nothing. Each case is a list as the
// guide writes one, the entry among others.
func TestTheEntryCheckReadsTheLinkOfTheEntryAReaderSees(t *testing.T) {
	const (
		migration = "20990101000001_recorded"
		heading   = "Rolling back the release that records each step"
		other     = "20990101000002_other touches none of the six, so the block holds over it.\n"
		entry     = migration + " touches none of the six, so the block holds over it, and a rollback to a " +
			"release before it takes the steps of its own section, "
		link   = "[Rolling back the release that records each step](#rolling-back-the-release-that-records-each-step).\n"
		linked = entry + link
		// redirected is the entry linking the role-holders publication's section, which another
		// entry of the list links.
		redirected = entry + "[Rolling back the release that publishes each issue's role sessions to Dispatch]" +
			"(#rolling-back-the-release-that-publishes-each-issues-role-sessions-to-dispatch).\n"
	)
	for _, list := range []struct {
		shape, text string
		err         error
	}{
		{"the entry linking its section", other + linked + other, nil},
		{"the entry without its link", other + migration + " touches none of the six.\n", errNoLink},
		{"the entry linking another rollback's section", other + redirected, errNoLink},
		{"the entry written twice", linked + other + linked, errEntries},
		{"no entry of the migration", other + other, errNoEntry},
		{"the link inside an HTML comment", other + entry + "<!-- " + strings.TrimSuffix(link, "\n") + " -->\n",
			errCommented},
		{"the entry inside an HTML comment", other + "<!--\n" + linked + "-->\n", errCommented},
		{"a second entry inside an HTML comment", other + linked + "<!--\n" + redirected + "-->\n", errCommented},
		{"an HTML comment splitting the link", other + entry +
			"[Rolling back the release that records each step](#rolling-back-the-release-<!-- each -->that-records-each-step).\n",
			errCommented},
		{"a comment opened inside a fenced block before the list", "```html\n<!--\n```\n" + other + linked, nil},
	} {
		err := entryLinkIn(list.text, migration, heading)
		if !errors.Is(err, list.err) {
			t.Errorf("%s: holding the entry's link answers the error %v, want %v: %q", list.shape, err, list.err,
				list.text)
		}
	}
}

// The heading check holds a section's heading in both readings of a page: the one heading of its
// text, at any level, passes; a page without it, with it twice, or with it only as a line of a fenced
// block, a shell comment of a command, is refused for it; and a heading only an HTML comment holds
// makes the two readings disagree, and is refused for the comment.
func TestTheSectionHeadingCheckReadsTheHeadingAReaderSees(t *testing.T) {
	const (
		heading = "Rolling back the release that records each step"
		section = "This release adds one outbox kind.\n"
	)
	for _, page := range []struct {
		shape, text string
		err         error
	}{
		{"the heading", "#### " + heading + "\n\n" + section, nil},
		{"the heading at another level", "## " + heading + "\n\n" + section, nil},
		{"the heading renamed", "#### Rolling back the release that records each run\n\n" + section,
			errNotOneSection},
		{"the heading twice", "#### " + heading + "\n\n" + section + "#### " + heading + "\n\n" + section,
			errNotOneSection},
		{"the heading only as a line of a fenced block", "```sh\n# " + heading + "\n```\n\n" + section,
			errNotOneSection},
		{"the heading inside an HTML comment", "<!--\n#### " + heading + "\n-->\n\n" + section, errCommented},
	} {
		err := sectionHeadingIn(page.text, heading)
		if !errors.Is(err, page.err) {
			t.Errorf("%s: holding the heading answers the error %v, want %v: %q", page.shape, err, page.err, page.text)
		}
	}
}

// entryLinkIn holds the entry of migration in text, a Markdown page carrying the guide's list of
// migrations, to its link to the section heading opens, in both readings of the page
// (bothReadings): as written, and as it renders (visibleMarkdown). So an entry or a link that only
// an HTML comment holds, which the rendered page does not show, fails, and so does a second entry
// only the text as written carries or a link a comment splits.
func entryLinkIn(text, migration, heading string) error {
	return bothReadings(linkOfTheEntry(text, migration, heading),
		linkOfTheEntry(visibleMarkdown(text), migration, heading))
}

// linkOfTheEntry holds one reading of a page: the one line opening on migration's name, its entry in
// the list, links the section heading opens by that heading's anchor (sectionAnchor). A reading with
// no such line (errNoEntry) or with two (errEntries), and one whose line carries no such link
// (errNoLink), is refused.
func linkOfTheEntry(text, migration, heading string) error {
	opens := regexp.MustCompile(`^` + regexp.QuoteMeta(migration) + `\b`)
	entries := 0
	linked := false
	for _, line := range strings.Split(text, "\n") {
		if opens.MatchString(line) {
			entries++
			linked = strings.Contains(line, "](#"+sectionAnchor(heading)+")")
		}
	}
	switch {
	case entries == 0:
		return fmt.Errorf("%s: %w", migration, errNoEntry)
	case entries > 1:
		return fmt.Errorf("%s: %d lines open on it: %w", migration, entries, errEntries)
	case !linked:
		return fmt.Errorf("%s: %w %q, whose anchor is #%s", migration, errNoLink, heading, sectionAnchor(heading))
	}
	return nil
}

// sectionHeadingIn holds text, a Markdown page, to the one heading of heading's text that a link to
// its anchor (sectionAnchor) sends a reader to, in both readings of the page (bothReadings): a link
// sends a reader nowhere where the page has no such heading, or shows it only inside an HTML comment.
func sectionHeadingIn(text, heading string) error {
	return bothReadings(headingOf(text, heading), headingOf(visibleMarkdown(text), heading))
}

// headingOf holds one reading of a page: it has one line of `#` marks, a space and heading outside a
// fenced block, from a line opening "```" to the next, where a `#` line is a shell comment and no
// heading. A reading with none or with more than one is refused (errNotOneSection).
func headingOf(text, heading string) error {
	fenced := false
	headings := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
			continue
		}
		if !fenced && strings.HasPrefix(line, "#") && strings.TrimLeft(line, "#") == " "+heading {
			headings++
		}
	}
	if headings != 1 {
		return fmt.Errorf("%q: %d headings of it: %w", heading, headings, errNotOneSection)
	}
	return nil
}

// sectionAnchor is the anchor a link of the guide names a section by: the heading in lower case,
// each space a hyphen, and every character but a letter, a digit, a hyphen and an underscore
// dropped.
func sectionAnchor(heading string) string {
	var anchor strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case r == ' ':
			anchor.WriteByte('-')
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			anchor.WriteRune(r)
		}
	}
	return anchor.String()
}
