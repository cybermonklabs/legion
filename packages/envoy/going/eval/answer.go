package eval

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
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
// block it drew holds the label and the count, and no line of its prose holds the label with the
// count beside it.
func MissingChartRows(rows []ChartRow, answer string, blocks []json.RawMessage) []string {
	var drawn []map[string]any
	for _, block := range blocks {
		var data struct {
			Rows []map[string]any `json:"rows"`
		}
		if json.Unmarshal(block, &data) == nil {
			drawn = append(drawn, data.Rows...)
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

// inBlocks reports whether a drawn row holds the label as one of its values (sameLabel) and the
// count as another. A priority label, P0 to P3, is also held by the row's priority field as the
// number the issue reads answer it as (priorityLabel), and the count is then one of its other
// values.
func inBlocks(drawn []map[string]any, row ChartRow) bool {
	for _, values := range drawn {
		labelAt, count := "", false
		for field, value := range values {
			switch v := value.(type) {
			case string:
				if labelAt == "" && sameLabel(v, row.Label) {
					labelAt = field
				}
			case float64:
				if labelAt == "" && field == "priority" && strings.EqualFold(priorityLabel(v), row.Label) && row.Label != "" {
					labelAt = field
				}
			}
		}
		for field, value := range values {
			if v, isNumber := value.(float64); isNumber && field != labelAt && v == float64(row.Count) {
				count = true
			}
		}
		if labelAt != "" && count {
			return true
		}
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
// the count as a number of its own.
func inLines(lines []string, row ChartRow) bool {
	label := strings.ToLower(row.Label)
	count := regexp.MustCompile(`(^|[^0-9.])` + strconv.Itoa(row.Count) + `($|[^0-9.]|\.[^0-9])`)
	for _, line := range lines {
		i := strings.Index(line, label)
		if i < 0 {
			continue
		}
		if count.MatchString(line[:i] + " " + line[i+len(label):]) {
			return true
		}
	}
	return false
}
