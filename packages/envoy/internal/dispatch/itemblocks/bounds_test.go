package itemblocks

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// OverCap is every refusal of a text over a UTF-16 cap: the number to trim in the singular for
// one unit and in the plural for more, then the length and the cap. The TypeScript twin,
// overCapMessage, is held to the same two strings (packages/contracts/src/dispatch-tools.test.ts),
// so a change to either wording fails a test.
func TestOverCapNamesTheNumberToTrimInUTF16CodeUnits(t *testing.T) {
	for _, row := range []struct {
		units int
		want  string
	}{
		{1001, "title: trim 1 UTF-16 code unit (1001/1000)"},
		{1002, "title: trim 2 UTF-16 code units (1002/1000)"},
	} {
		if got := OverCap("title", row.units, 1000); got != row.want {
			t.Errorf("OverCap(title, %d, 1000) = %q, want %q", row.units, got, row.want)
		}
	}
}

func requirePasses(t *testing.T, name, body string) {
	t.Helper()
	if err := Check("body", body); err != nil {
		t.Fatalf("Check(%s) = %v, want nil", name, err)
	}
}

func requireRefusal(t *testing.T, name, body, want string) error {
	t.Helper()
	err := Check("body", body)
	if err == nil {
		t.Fatalf("Check(%s) = nil, want %q", name, want)
	}
	if err.Error() != want {
		t.Fatalf("Check(%s) = %q, want %q", name, err.Error(), want)
	}
	return err
}

// paragraphBody is count paragraphs of width characters each, the shape a body splits into one
// block a paragraph.
func paragraphBody(count, width int) string {
	return strings.TrimSuffix(strings.Repeat(strings.Repeat("a", width)+"\n\n", count), "\n\n")
}

// renderedUnits is how long the canonical rendering of body is, in UTF-16 units: what a bound on
// the rendering rather than on the text as written would measure.
func renderedUnits(t *testing.T, body string) int {
	t.Helper()
	doc, err := pmdoc.Parse(body)
	if err != nil {
		t.Fatalf("pmdoc.Parse: %v", err)
	}
	rendered, err := pmdoc.Render(doc)
	if err != nil {
		t.Fatalf("pmdoc.Render: %v", err)
	}
	return Units16(rendered)
}

// paddedTable is a table whose rows carry one cell where its header carries two: pmdoc.Render
// pads every row to the header's cells and every cell to its column's width, so the rendering is
// longer than the text its author wrote. It is 20 + 6*rows UTF-16 units as written.
func paddedTable(rows int) string {
	var body strings.Builder
	body.WriteString("| a | bb |\n|---|---|")
	for range rows {
		body.WriteString("\n| 1 |")
	}
	return body.String()
}

// escapedListItems is how many items escapedList spreads its text over.
const escapedListItems = 45

// escapedList is a bullet list whose every item holds one "*", which pmdoc.Render writes as "\*":
// a character a line more than its author wrote. The list is exactly units UTF-16 units as
// written.
func escapedList(units int) string {
	const marker = "- "
	const star = "*"
	// Each item is the marker, its run of x's and the star; the items are joined by one newline
	// each, and the first item carries what does not divide evenly among them.
	xs := units - escapedListItems*(len(marker)+len(star)) - (escapedListItems - 1)
	items := make([]string, escapedListItems)
	for index := range items {
		width := xs / escapedListItems
		if index == 0 {
			width += xs % escapedListItems
		}
		items[index] = marker + strings.Repeat("x", width) + star
	}
	return strings.Join(items, "\n")
}

// A body whose blocks each stand inside the block bound, and which holds exactly as many blocks as
// a body may hold, passes: neither bound is a bound on the body's whole length.
func TestCheckPassesABodyInsideBothBounds(t *testing.T) {
	requirePasses(t, "four 25,000-unit paragraphs", paragraphBody(4, 25_000))
	requirePasses(t, "the most blocks a body may hold", paragraphBody(contracts.DispatchItemBlocks, 1))
}

// A block over the block bound is refused, naming the field, the block's place and type, the
// number of UTF-16 code units to trim and the measured length and bound.
func TestCheckRefusesABlockOverTheBlockBound(t *testing.T) {
	body := "a\n\n" + strings.Repeat("x", contracts.DispatchBlockUnits+1)
	err := requireRefusal(t, "a second paragraph one unit over", body,
		"body block 2 (paragraph): trim 1 UTF-16 code unit (32001/32000)")
	var capExceeded *CapExceeded
	if !errors.As(err, &capExceeded) {
		t.Fatalf("Check returned %T, want *CapExceeded", err)
	}
}

// A body holding one block more than it may is refused for its block count, before any block's
// length is measured, naming the one block to trim.
func TestCheckRefusesABodyOverTheBlockCount(t *testing.T) {
	body := paragraphBody(contracts.DispatchItemBlocks+1, 1)
	err := requireRefusal(t, "one block over the count", body,
		"body: trim 1 block (32769/32768)")
	var countExceeded *CountExceeded
	if !errors.As(err, &countExceeded) {
		t.Fatalf("Check returned %T, want *CountExceeded", err)
	}
}

// A rune outside the Basic Multilingual Plane counts as the two UTF-16 units it is sent as, so
// half as many of them fit one block as characters of the plane do.
func TestCheckCountsARuneOutsideTheBasicPlaneAsTwoUnits(t *testing.T) {
	requireRefusal(t, "16,001 astral runes", strings.Repeat("🙂", 16_001),
		"body block 1 (paragraph): trim 2 UTF-16 code units (32002/32000)")
	requirePasses(t, "16,000 astral runes", strings.Repeat("🙂", 16_000))
}

// A block is measured as its author wrote it, never as pmdoc.Render would write it: a table whose
// rows the rendering pads and a list whose lines the rendering escapes both pass at the bound,
// though each renders longer than a block may be.
func TestCheckMeasuresTheTextAsWritten(t *testing.T) {
	table := paddedTable((contracts.DispatchBlockUnits - 20) / 6)
	if units := Units16(table); units != contracts.DispatchBlockUnits {
		t.Fatalf("the table is %d units as written, want %d", units, contracts.DispatchBlockUnits)
	}
	if rendered := renderedUnits(t, table); rendered <= contracts.DispatchBlockUnits {
		t.Fatalf("the table renders to %d units, want more than %d: it no longer pads", rendered, contracts.DispatchBlockUnits)
	}
	requirePasses(t, "a table at the bound as written", table)

	const listUnits = 31_956
	list := escapedList(listUnits)
	if units := Units16(list); units != listUnits {
		t.Fatalf("the list is %d units as written, want %d", units, listUnits)
	}
	if rendered := renderedUnits(t, list); rendered <= contracts.DispatchBlockUnits {
		t.Fatalf("the list renders to %d units, want more than %d: it no longer escapes a character a line", rendered, contracts.DispatchBlockUnits)
	}
	requirePasses(t, "a list under the bound as written", list)
}
