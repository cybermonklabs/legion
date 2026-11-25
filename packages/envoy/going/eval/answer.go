package eval

import (
	"cmp"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/going/chips"
)

// answer.go reads an answer task's answer for what no judge is asked: the items it links and the
// numbers of its chart's table. An answer is the prose of its replies and the blocks it drew
// (going.BlockData), each block's rows the values the server's chain filled it with.

// MissingLinks is each item of links (Seed.Refs) that the answer does not link: an issue by its
// key, a document, a spec or a session by its reference or by the dashboard's path to it, and a
// comment, an ask or a message by the id Dispatch stores it under (ItemID), in the prose or in a
// block's rows.
func MissingLinks(links []string, answer string, blocks []json.RawMessage) []string {
	var text strings.Builder
	text.WriteString(answer)
	for _, block := range blocks {
		text.WriteString("\n")
		text.Write(block)
	}
	haystack := text.String()
	var missing []string
	for _, link := range links {
		if !linked(haystack, link) {
			missing = append(missing, link)
		}
	}
	return missing
}

// linked reports whether text links the item ref names: by the reference itself
// (dispatch://DEMO-3/spec), or by the dashboard's path to the item, which is how the bar's answers
// link a document (/projects/OPS/documents/<slug>, /issues/DEMO-16/artifacts/<slug>,
// /issues/DEMO-3/spec).
func linked(text, ref string) bool {
	lower := strings.ToLower(text)
	for _, kind := range []string{"ask", "comment", "message"} {
		if owner, id, found := strings.Cut(ref, "/"+kind+"/"); found {
			stored := ItemID(kind, id)
			return strings.Contains(lower, stored) || strings.Contains(lower, strings.ToLower(owner+"/"+kind+"/"+id))
		}
	}
	if !strings.Contains(ref, "/") {
		return regexp.MustCompile(`(?i)(^|[^a-z0-9-])` + regexp.QuoteMeta(ref) + `($|[^0-9])`).MatchString(text)
	}
	forms := []string{ref}
	if owner, slug, found := strings.Cut(ref, "/artifact/"); found {
		if strings.Contains(owner, "-") {
			forms = append(forms, "/issues/"+owner+"/artifacts/"+slug)
		} else {
			forms = append(forms, "/projects/"+owner+"/documents/"+slug)
		}
	}
	if key, found := strings.CutSuffix(ref, "/spec"); found {
		forms = append(forms, "/issues/"+key+"/spec")
	}
	for _, form := range forms {
		if i := strings.Index(lower, strings.ToLower(form)); i >= 0 {
			// The form ends where the link does, so DEMO-1/spec is not found inside DEMO-12/spec's.
			if end := i + len(form); end == len(lower) || !strings.ContainsRune("abcdefghijklmnopqrstuvwxyz0123456789-", rune(lower[end])) {
				return true
			}
		}
	}
	return false
}

// MissingChartRows is each row of the chart's table that the answer does not show: no row of a
// block it drew holds the label and the count, each in its column (inBlocks), and no line of its
// prose holds the label with the count beside it.
func MissingChartRows(rows []ChartRow, answer string, blocks []json.RawMessage) []string {
	var drawn []drawnBlock
	for _, block := range blocks {
		var data drawnBlock
		if json.Unmarshal(block, &data) == nil {
			drawn = append(drawn, data)
		}
	}
	lines := strings.Split(strings.ToLower(answer), "\n")
	var missing []string
	for _, row := range rows {
		if !inBlocks(drawn, row) && !inLines(lines, row) {
			missing = append(missing, fmt.Sprintf("%s=%d", row.Label, row.Count))
		}
	}
	return missing
}

// drawnBlock is a block an answer drew, as its frame holds it (going.BlockData): the spec the
// server drew it by, with the columns it draws and its chain's steps, and its rows.
type drawnBlock struct {
	Kind string `json:"kind"`
	Spec struct {
		X       string   `json:"x"`
		Y       string   `json:"y"`
		Columns []string `json:"columns"`
		Chain   []struct {
			Op string `json:"op"`
			As string `json:"as"`
		} `json:"chain"`
	} `json:"spec"`
	Rows []map[string]any `json:"rows"`
}

// countColumns is the columns of the block that hold a count of items (counts), and every column
// its chain's count, sum, mean, min and max write (aggregates). A count is in a column a count
// writes, of a table or of a chart whose y it is, or in a chart's y where its chain writes none,
// which plots a number the read gave, such as each project's open_asks. A sum, a mean, a least or
// a greatest counts no items, and a table with no count draws the read's own values, such as each
// issue's priority: chart-15 of the eval drew a table of seven alerts, and the priority 2 beside
// an issue whose status is done was read as "done=2". A metric is one number, with no label in its
// rows.
func (b drawnBlock) countColumns() (counts, aggregates []string) {
	for _, step := range b.Spec.Chain {
		switch step.Op {
		case "count", "sum", "mean", "min", "max":
			column := cmp.Or(step.As, step.Op)
			aggregates = append(aggregates, column)
			if step.Op == "count" {
				counts = append(counts, column)
			}
		}
	}
	switch b.Kind {
	case "chart":
		if b.Spec.Y != "" && (len(aggregates) == 0 || slices.Contains(counts, b.Spec.Y)) {
			return []string{b.Spec.Y}, aggregates
		}
	case "table":
		return slices.DeleteFunc(counts, func(column string) bool { return !slices.Contains(b.Spec.Columns, column) }), aggregates
	}
	return nil, aggregates
}

// labelColumns is the columns of the block that hold a label beside its count: a chart's x, and
// each of a table's columns that no count, sum, mean, min or max of its chain writes.
func (b drawnBlock) labelColumns(aggregates []string) []string {
	if b.Kind == "chart" {
		return []string{b.Spec.X}
	}
	return slices.DeleteFunc(slices.Clone(b.Spec.Columns), func(column string) bool { return slices.Contains(aggregates, column) })
}

// inBlocks reports whether a row of a drawn block holds the label in one of its label columns
// (labelCell) and the count in one of its count columns (countColumns), as the block draws them:
// a number in any other column of the row is no count of the label.
func inBlocks(drawn []drawnBlock, row ChartRow) bool {
	for _, block := range drawn {
		counts, aggregates := block.countColumns()
		if len(counts) == 0 {
			continue
		}
		labels := block.labelColumns(aggregates)
		for _, values := range block.Rows {
			if !slices.ContainsFunc(labels, func(column string) bool { return labelCell(column, values[column], row.Label) }) {
				continue
			}
			if slices.ContainsFunc(counts, func(column string) bool {
				v, isNumber := values[column].(float64)
				return isNumber && v == float64(row.Count)
			}) {
				return true
			}
		}
	}
	return false
}

// labelCell reports whether value, a drawn row's in column, is the label of a chart's row: text
// that is the label (sameLabel), or, in a column named priority, the number the issue reads
// answer a priority as (priorityLabel).
func labelCell(column string, value any, label string) bool {
	switch v := value.(type) {
	case string:
		return sameLabel(v, label)
	case float64:
		return column == "priority" && label != "" && strings.EqualFold(priorityLabel(v), label)
	}
	return false
}

// priorityLabel is a priority's label as the tasks and Dispatch write it, P0 to P3, "" for a number
// that is no priority.
func priorityLabel(number float64) string {
	if number < 0 || number > 3 || number != float64(int(number)) {
		return ""
	}
	return fmt.Sprintf("P%d", int(number))
}

// isoWeek is a chart label of a week, as the tasks write it: 2026-W45.
var isoWeek = regexp.MustCompile(`^(\d{4})-W(\d{2})$`)

// sameLabel reports whether a drawn value is the label of a chart's row: the label itself, with
// no regard to case, or, for a label of an ISO week, a day of that week, which is how the server's
// chain names a week it groups by (the date of its first day).
func sameLabel(value, label string) bool {
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, label) {
		return true
	}
	week := isoWeek.FindStringSubmatch(label)
	if week == nil {
		return false
	}
	day, err := time.Parse("2006-01-02", value)
	if err != nil {
		if day, err = time.Parse(time.RFC3339, value); err != nil {
			return false
		}
	}
	year, number := day.UTC().ISOWeek()
	return fmt.Sprintf("%d-W%02d", year, number) == label
}

// inLines reports whether a line of the prose holds the label and, once the label is taken out,
// the count as a number of its own. A status's label is its value or the page's words for it
// (chips.StatusWords, which grade_test.go holds to the status rows of
// packages/contracts/fixtures/shown-values.json), each whole, on word bounds, and no other: the
// agent writes a status by the page's words, so chart-15 of the eval wrote "1 in progress" for
// in_progress and was read as not giving it, while "to do", which is not the page's "Todo", is
// still not todo.
func inLines(lines []string, row ChartRow) bool {
	forms := []string{strings.ToLower(row.Label)}
	words, isStatus := chips.StatusWords[row.Label]
	if isStatus && !strings.EqualFold(words, row.Label) {
		forms = append(forms, strings.ToLower(words))
	}
	count := regexp.MustCompile(`(^|[^0-9.])` + strconv.Itoa(row.Count) + `($|[^0-9.]|\.[^0-9])`)
	for _, line := range lines {
		for _, form := range forms {
			var i int
			if isStatus {
				i = wholeIndex(line, form)
			} else {
				i = strings.Index(line, form)
			}
			if i >= 0 && count.MatchString(line[:i]+" "+line[i+len(form):]) {
				return true
			}
		}
	}
	return false
}

// wholeIndex is the first place of form in line that starts and ends on word bounds
// (onWordBounds), -1 for none.
func wholeIndex(line, form string) int {
	for start := 0; start < len(line); {
		index := strings.Index(line[start:], form)
		if index < 0 {
			return -1
		}
		if at := start + index; onWordBounds(line, at, at+len(form)) {
			return at
		}
		start += index + 1
	}
	return -1
}
