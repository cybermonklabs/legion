package textcheck

import (
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
// row outside it - which is every row the product half's parse of the page used to take. What it
// does not: that a row's text is a refusal the daemon prints, which is the product half's.
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
	lines := strings.Split(shippedText(t, troubleshootingGuide), "\n")
	at := slices.Index(lines, heading)
	if at < 0 {
		t.Fatalf("%s has no %q heading, which %s copies for cmd/legion's "+
			"TestTheTroubleshootingTableHoldsEveryRefusalToAMessageTheLoaderProduces: repaste the copy "+
			"from the page as it reads now, in the same commit as the edit that changed it",
			troubleshootingGuide, heading, troubleshootingRefusalsExtract)
	}
	section := lines[at+1:]
	if next := slices.IndexFunc(section, func(line string) bool { return strings.HasPrefix(line, "## ") }); next >= 0 {
		section = section[:next]
	}
	var guideRows []string
	for _, line := range section {
		if strings.HasPrefix(line, "|") {
			guideRows = append(guideRows, line)
		}
	}
	passage := "\n" + strings.Join(rows, "\n") + "\n"
	if slices.Equal(guideRows, rows) && strings.Contains("\n"+strings.Join(section, "\n")+"\n", passage) {
		return
	}
	t.Errorf("%s is no longer the table under %q in %s, which cmd/legion's "+
		"TestTheTroubleshootingTableHoldsEveryRefusalToAMessageTheLoaderProduces reads it for; the page "+
		"carries:\n%s\n\n%s\nrepaste the copy from the page, in the same commit as the edit that changed it",
		troubleshootingRefusalsExtract, heading, troubleshootingGuide, heading, strings.Join(guideRows, "\n"))
}
