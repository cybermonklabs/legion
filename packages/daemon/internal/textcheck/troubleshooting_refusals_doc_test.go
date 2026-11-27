package textcheck

// cmd/legion/troubleshooting_refusals_test.go reads no text outside its own package and has
// no row: troubleshooting_refusals_extract.txt, the copy of the troubleshooting page's refusal
// table it reads its rows from, stands in its own directory, and this row is what holds that
// copy to the page, in the text-check job (dispatch://INFRA-1413).
//ci:reads docs/site/src/content/docs/legion/troubleshooting.md `packages/daemon/internal/textcheck/waiting_line_texts_test.go#shippedText` reads it under ../../../.. for TestTheTroubleshootingRefusalsExtractIsTheGuidesRefusalTable
//ci:reads packages/daemon/cmd/legion/troubleshooting_refusals_extract.txt `packages/daemon/internal/textcheck/waiting_line_texts_test.go#readExtract` reads it as ../../cmd/legion/troubleshooting_refusals_extract.txt for TestTheTroubleshootingRefusalsExtractIsTheGuidesRefusalTable, the committed copy it holds the page's refusal table to (quotedBody)

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The refusal table of the troubleshooting page - the table under its `## The daemon will not
// start` heading - is held to the daemon by cmd/legion's
// TestTheTroubleshootingTableHoldsEveryRefusalToAMessageTheLoaderProduces, which drives a fixture
// per row and holds the row's `It says` text to the refusal `legion config check` prints.
// That comparison is in two halves, because a test that reads the guide puts the guide in its
// job's filter — a declared read applies to every compiled source file of a run — and a
// documentation-only edit would then start the whole daemon Go lane. The product half reads
// cmd/legion/troubleshooting_refusals_extract.txt, a copy of the heading and its table committed
// beside it; this half holds that copy to the page. Neither half alone is the guarantee: together
// they are the one the single test used to give. (INFRA-1413.)
//
// What it proves: the page has the copy's heading as a line of its own, and the section under it,
// to the next `## ` heading, holds the copy's table as one passage of whole lines and no table
// row outside it - which is every row the product half's parse of the page used to take - both as
// the page is written and as it renders (refusalTableOfThePage), so that a heading or a row an
// HTML comment holds, which the rendered page does not show, fails rather than passing as the
// table. What it does not: that a row's text is a refusal the daemon prints, which is the product
// half's.
const (
	// The committed copy, read from the package whose test reads it, and the guide, read under
	// ../../../.. by shippedText.
	troubleshootingRefusalsExtract = "../../cmd/legion/troubleshooting_refusals_extract.txt"
	troubleshootingGuide           = "docs/site/src/content/docs/legion/troubleshooting.md"
)

// The committed copy of the troubleshooting page's refusal table is the page's own heading and
// table, line for line. An edit to the page alone fails here: a heading renamed, or a row
// reworded, added or deleted, would otherwise never reach cmd/legion's check, which reads the copy
// and not the page. The failure prints what the page carries now, so the fix is a paste.
func TestTheTroubleshootingRefusalsExtractIsTheGuidesRefusalTable(t *testing.T) {
	heading, table, _ := strings.Cut(quotedBody(t, troubleshootingRefusalsExtract), "\n")
	rows := strings.Split(strings.TrimLeft(table, "\n"), "\n")
	if !strings.HasPrefix(heading, "## ") || slices.ContainsFunc(rows, func(row string) bool { return !strings.HasPrefix(row, "|") }) {
		t.Fatalf("%s carries no heading line and table under its `#` header: what stands there is the "+
			"page's `## ` heading, a blank line and the table's rows, which cmd/legion's "+
			"TestTheTroubleshootingTableHoldsEveryRefusalToAMessageTheLoaderProduces reads", troubleshootingRefusalsExtract)
	}
	err := refusalTableOfThePage(shippedText(t, troubleshootingGuide), heading, rows)
	if err != nil {
		t.Errorf("%s is no longer the table under %q in %s, which cmd/legion's "+
			"TestTheTroubleshootingTableHoldsEveryRefusalToAMessageTheLoaderProduces reads it for: %v\n"+
			"repaste the copy from the page as it reads now, or take out the comment the failure names, in "+
			"the same commit as the edit that changed it",
			troubleshootingRefusalsExtract, heading, troubleshootingGuide, err)
	}
}

// A refusal table a reader of the rendered page cannot see is none: the page shows nothing of a
// heading or a row inside an HTML comment, and a row the copy lacks inside a comment is still a
// row of the page as an editor reads it. So the table is held in both readings of the page, and a
// comment around the table, around one of its rows or around its heading, or one opened on a line
// of its own and never closed before it, fails for the comment. A comment that holds nothing of the
// heading or the table, one around the section's fenced command, a "<!--" inside that fenced block,
// and a "<!--" inside a line that its paragraph never closes, which the page shows as text, hide
// nothing of it.
// Each case is a section as the page writes one: the heading, the fenced command and the table.
func TestTheRefusalTableIsHeldWhereAReaderSeesIt(t *testing.T) {
	const heading = "## The daemon will not start"
	rows := []string{"| It says | What to do |", "| --- | --- |", "| `a refusal` | Fix what it names. |"}
	table := strings.Join(rows, "\n")
	const command = "```sh\nlegion config check --config legion.yaml\n```"
	for _, page := range []struct {
		shape, text string
		err         error
	}{
		{"a table a reader sees",
			heading + "\n\n" + command + "\n\n" + table + "\n\n## The next section\n", nil},
		{"the table inside a comment",
			heading + "\n\n" + command + "\n\n<!--\n" + table + "\n-->\n\n## The next section\n", errCommented},
		{"one row inside a comment",
			heading + "\n\n" + command + "\n\n" + rows[0] + "\n" + rows[1] + "\n<!--\n" + rows[2] + "\n-->\n", errCommented},
		{"a row the copy lacks inside a comment",
			heading + "\n\n" + command + "\n\n" + table + "\n<!--\n| `another refusal` | Fix it too. |\n-->\n", errCommented},
		{"the heading inside a comment",
			"<!--\n" + heading + "\n-->\n\n" + command + "\n\n" + table + "\n", errCommented},
		{"a comment never closed before the table",
			heading + "\n\nRun the check first.\n<!-- a note\n\n" + command + "\n\n" + table + "\n", errCommented},
		{"a \"<!--\" never closed inside a line before the table",
			heading + "\n\nRun the check first. <!-- a note\n\n" + command + "\n\n" + table + "\n", nil},
		{"the section's fenced command inside a comment",
			heading + "\n\n<!--\n" + command + "\n-->\n\n" + table + "\n", nil},
		{"a comment opened inside the section's fenced block",
			heading + "\n\n```html\n<!--\n```\n\n" + table + "\n", nil},
		{"a table that is not the copy's",
			heading + "\n\n" + command + "\n\n" + rows[0] + "\n" + rows[1] + "\n| `another refusal` | Fix it too. |\n",
			errNotTheRefusalTable},
	} {
		err := refusalTableOfThePage(page.text, heading, rows)
		if !errors.Is(err, page.err) {
			t.Errorf("%s: holding the table answers the error %v, want %v: %q", page.shape, err, page.err, page.text)
		}
	}
}

// errNotTheRefusalTable is why refusalTableIn refuses a text: the copy's heading and table do not
// stand in it.
var errNotTheRefusalTable = errors.New("the copy's heading and table do not stand in it")

// refusalTableOfThePage holds rows, the copy's table, to page, a Markdown page, in both its
// readings (bothReadings): as written, and as it renders (visibleMarkdown).
func refusalTableOfThePage(page, heading string, rows []string) error {
	return bothReadings(refusalTableIn(page, heading, rows), refusalTableIn(visibleMarkdown(page), heading, rows))
}

// refusalTableIn holds rows, the copy's table, to text, one reading of a page: it has heading as a
// line of its own, and the section under it, to the next `## ` heading, holds rows as one passage
// of whole lines and no table row outside it. A failure prints the heading and the rows the
// section carries, so the fix is a paste.
func refusalTableIn(text, heading string, rows []string) error {
	lines := strings.Split(text, "\n")
	at := slices.Index(lines, heading)
	if at < 0 {
		return fmt.Errorf("no line is the %q heading: %w", heading, errNotTheRefusalTable)
	}
	section := lines[at+1:]
	if next := slices.IndexFunc(section, func(line string) bool { return strings.HasPrefix(line, "## ") }); next >= 0 {
		section = section[:next]
	}
	var pageRows []string
	for _, line := range section {
		if strings.HasPrefix(line, "|") {
			pageRows = append(pageRows, line)
		}
	}
	passage := "\n" + strings.Join(rows, "\n") + "\n"
	if slices.Equal(pageRows, rows) && strings.Contains("\n"+strings.Join(section, "\n")+"\n", passage) {
		return nil
	}
	return fmt.Errorf("%w; the page carries:\n%s\n\n%s\n", errNotTheRefusalTable, heading, strings.Join(pageRows, "\n"))
}
