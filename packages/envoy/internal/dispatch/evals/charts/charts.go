// Package charts checks an eval's declared charts before the register stores them. It is a port
// of the eval platform's validate_charts at commit 4f9223b9, so a declaration that the platform
// draws is one Dispatch takes, and a refusal names the same field in the same words, in the same
// order. testdata/corpus.json holds the platform's own answers to the cases of
// testdata/corpus-specs.json, captured by testdata/capture.py, and charts_test.go holds this port
// to every one of them.
//
// The port differs from the platform where Dispatch's spec says so (dispatch://INFRA-2918/spec,
// R20, and D10 of its plan), and nowhere else:
//
//   - approved and kept are checked as the platform shapes them, but no kind requires approved,
//     and the list may hold its approved, kept and other declarations in any order;
//   - a network and an architecture chart need no graph, since Dispatch draws their nodes, parts
//     and edges from readings; a graph they give is checked as the platform shapes it;
//   - kind module is refused, naming charts[i].kind: Dispatch draws no module, so it holds no
//     registry of modules, and the platform's carry-forward of a module the registry no longer
//     holds, the one use of the eval's previous charts, has nothing to carry;
//   - a number's adapter must be one of the platform's two adapters, as on the platform.
//
// Every other message keeps the platform's words, so a message that lists kinds still lists module.
package charts

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Refusal is one thing wrong with a declaration: the field it is about, such as
// charts[2].y.series, and the platform's message, which starts with that field.
type Refusal struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Validate checks a whole list of chart declarations and answers every refusal, in the order the
// platform finds them; none means the list is accepted. The error is for charts that Python's
// json.loads would not read: bytes that are no JSON value, which a request body that decoded never
// holds, and an integer of more than 4,300 digits.
//
// The checks read text as Python 3.13 does, with Unicode 15.1's printable and white space
// characters: a character assigned later is refused as the platform refuses it, and so is a lone
// surrogate escape such as "\ud800".
func Validate(charts json.RawMessage) ([]Refusal, error) {
	declared, err := decode(charts)
	if err != nil {
		return nil, fmt.Errorf("read the charts: %w", err)
	}
	messages := validateCharts(declared)
	refusals := make([]Refusal, len(messages))
	for index, message := range messages {
		refusals[index] = Refusal{Field: fieldOf(message), Message: message}
	}
	return refusals, nil
}

// fieldOf reads the field a message names: its text up to the first space that stands outside a
// bracket, such as charts[0].states["a b"]. A possessive's apostrophe is not part of the field
// ("charts[1].columns' widths must sum ...").
func fieldOf(message string) string {
	depth, quoted, escaped := 0, false, false
	end := len(message)
	for index, char := range message {
		switch {
		case quoted && escaped:
			escaped = false
		case quoted && char == '\\':
			escaped = true
		case quoted && char == '"':
			quoted = false
		case quoted:
		case char == '"' && depth > 0:
			quoted = true
		case char == '[':
			depth++
		case char == ']':
			depth--
		case char == ' ' && depth == 0:
			end = index
		}
		if end != len(message) {
			break
		}
	}
	return strings.TrimSuffix(message[:end], "'")
}

const chartsMax = 16

var (
	// readingKinds draw readings on axes; a lane chart draws one line for each row of its readings
	// over time.
	readingKinds = []string{"xy", "band", "bar", "interval", "stacked-bar", "grid", "lanes"}
	chartKinds   = append(slices.Clone(readingKinds),
		"network", "architecture", "module", "number", "pending", "runs", "text", "prose")
	// mockupKinds take an owner's mock-up, which the kind's own drawing draws.
	mockupKinds = append(slices.Clone(readingKinds), "pending")
	// subtitleKinds take one or two paragraphs under the chart's title, and headingKinds a small
	// heading above its section's title; a number draws no section.
	subtitleKinds = append(slices.Clone(readingKinds), "text", "network", "architecture", "module")
	headingKinds  = append(slices.Clone(subtitleKinds), "pending", "runs")
	// chartNoteKinds take a footnote under the plot and a side note beside a half-width plot: the
	// kinds that draw a plot of readings.
	chartNoteKinds  = readingKinds
	chartNoteFields = []string{"footnote", "side_note"}
	chartFields     = []string{"id", "title", "kind", "note", "approved", "kept", "mockup"}
	kindFields      = map[string][]string{
		"network": {"graph", "size", "color", "selector", "outline", "label", "not_reported",
			"labels", "empty_label", "value_labels", "ratio"},
		"architecture": {"graph", "value_series", "second_reading", "bins", "numbered", "decimals",
			"columns", "bands", "key"},
		"module":  {"module"},
		"number":  {"series"},
		"pending": {"builder"},
		"runs":    {"columns", "name", "step_label"},
		"text":    {"series", "columns", "order", "row_label", "row_subtitles"},
		"prose":   {"paragraphs"},
		// A lane chart also takes the display fields displayKinds gives it.
		"lanes": {"x", "y", "marks", "group_label", "row_label", "end_heading"},
	}
	readingFields = []string{"x", "y", "group", "reference", "stack", "summary", "missing_x", "panels"}
	// adapters are the platform's adapters, one of which a number may name as its headline's.
	adapters = []string{"graph-headline", "run-headline"}
)

const (
	chartSubtitleParagraphsMax, chartSubtitleWords = 2, 125
	chartHeadingWords                              = 8
	chartNoteParagraphsMax, chartNoteWords         = 3, 80
)

// listed is the platform's _listed: "a", "a and b", "a, b and c".
func listed(kinds []string) string {
	if len(kinds) == 1 {
		return kinds[0]
	}
	return strings.Join(kinds[:len(kinds)-1], ", ") + " and " + kinds[len(kinds)-1]
}

// pyName is Python's str() of a kind that is a string, or "None" for one that is not.
func pyName(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return "None"
}

// validateCharts is the platform's validate_charts over a decoded list, with Dispatch's differences.
func validateCharts(declared any) []string {
	charts, ok := declared.([]any)
	if !ok {
		return []string{"charts must be a JSON list"}
	}
	if len(charts) > chartsMax {
		return []string{fmt.Sprintf("charts accepts at most %d specs", chartsMax)}
	}
	var messages []string
	ids := map[string]bool{}
	// The approved declarations that draw a chart, after one of which a number's section stands.
	charted := map[string]bool{}
	for _, item := range charts {
		if chart, ok := asObject(item); ok {
			if id, ok := chart.get("id").(string); ok && chart.has("approved") && !isOneOf(chart.get("kind"), "number", "prose") {
				charted[id] = true
			}
		}
	}
	numbers := 0
	for index, item := range charts {
		path := fmt.Sprintf("charts[%d]", index)
		chart, ok := asObject(item)
		if !ok {
			messages = append(messages, path+" must be an object")
			continue
		}
		kind, _ := chart.get("kind").(string)
		messages = append(messages, fieldsOf(chart, path, kind)...)
		if !matches(slug, chart.get("id")) {
			messages = append(messages, path+".id must be a slug")
		} else if id := chart.get("id").(string); ids[id] {
			messages = append(messages, path+".id must be unique in this eval")
		} else {
			ids[id] = true
		}
		switch {
		case kind == "module":
			messages = append(messages, path+".kind must not be module: Dispatch draws no module")
		case !slices.Contains(chartKinds, kind):
			messages = append(messages, path+".kind must be "+strings.Join(chartKinds[:len(chartKinds)-1], ", ")+
				", or "+chartKinds[len(chartKinds)-1])
		}
		messages = append(messages, wordsOf(chart, path, kind)...)
		if chart.has("approved") && !link(chart.get("approved"), "dispatch", "https") {
			messages = append(messages, path+".approved must be a dispatch:// or https:// link to the approved definition")
		}
		if chart.has("kept") {
			if chart.has("approved") {
				messages = append(messages, path+".kept must not be set with approved: a declaration is approved or kept, not both")
			} else if !link(chart.get("kept"), "dispatch") {
				messages = append(messages, path+".kept must be a dispatch:// link to the owner's answer")
			}
		}
		if chart.has("mockup") && kind != "module" {
			if slices.Contains(mockupKinds, kind) {
				messages = append(messages, mockupFields(chart.get("mockup"), path+".mockup", chart)...)
			} else {
				messages = append(messages, path+fmt.Sprintf(".mockup is not allowed for %s: only %s, ", pyName(chart.get("kind")),
					strings.Join(readingKinds, ", "))+"pending, and a module that draws reading series take one")
			}
		}
		if kind == "number" {
			numbers++
			if numbers > 1 {
				messages = append(messages, path+".kind must not be number again: an eval has at most one number declaration")
			}
			declared := map[string]bool{}
			for _, name := range listOr(chart.get("series")) {
				if name, ok := name.(string); ok {
					declared[name] = true
				}
			}
			seen := map[string]bool{}
			if chart.has("tiles") {
				messages = append(messages, tilesFields(chart.get("tiles"), path+".tiles", declared, seen)...)
			}
			// Sections of small numbers among the charts.
			if chart.has("sections") {
				messages = append(messages, sectionsFields(chart, path, declared, seen, charted)...)
			}
			// The words of its headline's and its small numbers' intervals that have no level.
			if chart.has("interval_label") {
				messages = append(messages, intervalLabelFields(chart.get("interval_label"), path)...)
			}
		}
		switch kind {
		case "prose":
			messages = append(messages, proseFields(chart, path)...)
		case "network":
			messages = append(messages, networkFields(chart, path)...)
		case "architecture":
			messages = append(messages, architectureFields(chart, path)...)
		case "runs":
			messages = append(messages, runsFields(chart, path)...)
		case "text":
			messages = append(messages, textFields(chart, path)...)
		case "lanes":
			messages = append(messages, displayFields(chart, path, kind, formTime)...)
			messages = append(messages, lanesFields(chart, path)...)
		case "module":
		case "number":
			messages = append(messages, numberFields(chart, path)...)
		case "pending":
			if !link(chart.get("builder"), "dispatch") {
				messages = append(messages, path+".builder must be a dispatch:// link to the issue that builds this kind")
			}
		default:
			messages = append(messages, readingChartFields(chart, path, kind, charts)...)
		}
	}
	return messages
}

// fieldsOf refuses each field that the chart's kind does not take.
func fieldsOf(chart *object, path, kind string) []string {
	fields := slices.Clone(chartFields)
	if kind == "number" || kind == "prose" {
		// A number draws no section of its own, and a text section has no plot, so neither takes a note.
		fields = slices.DeleteFunc(fields, func(field string) bool { return field == "note" })
	}
	own, known := kindFields[kind]
	if known {
		fields = append(fields, own...)
	} else {
		fields = append(fields, readingFields...)
	}
	if kind == "number" {
		fields = append(fields, "tiles", "sections", "interval_label", "adapter")
	} else if !known || kind == "lanes" {
		fields = append(fields, displayFieldNames...)
	}
	if slices.Contains(subtitleKinds, kind) {
		fields = append(fields, "subtitle")
	}
	if slices.Contains(headingKinds, kind) {
		fields = append(fields, "heading")
	}
	if slices.Contains(chartNoteKinds, kind) {
		fields = append(fields, chartNoteFields...)
	}
	var messages []string
	for _, field := range chart.unknown(fields...) {
		switch {
		case field == "subtitle":
			messages = append(messages, path+"."+field+" is only supported for "+listed(subtitleKinds))
		case field == "heading":
			messages = append(messages, path+"."+field+" is only supported for "+listed(headingKinds))
		case slices.Contains(chartNoteFields, field):
			messages = append(messages, path+"."+field+" is only supported for "+listed(chartNoteKinds))
		default:
			messages = append(messages, path+"."+field+" is unknown")
		}
	}
	return messages
}

// wordsOf checks the chart's title, note, subtitle, heading, footnote and side note.
func wordsOf(chart *object, path, kind string) []string {
	var messages []string
	for _, field := range []string{"title", "note"} {
		var fallback any
		if field == "note" {
			fallback = ""
		}
		text, ok := chart.getOr(field, fallback).(string)
		nonempty := ""
		if field == "title" {
			nonempty = " (nonempty)"
		}
		if !ok || (text != "" && !printable(text)) || (field == "title" && blank(text)) {
			messages = append(messages, path+"."+field+" must be single-line plain text"+nonempty)
		} else if chars(text) > definitionTextMaxChars {
			messages = append(messages, path+"."+field+fmt.Sprintf(" must be at most %d characters", definitionTextMaxChars))
		}
	}
	// One or two paragraphs under the title.
	if chart.has("subtitle") && slices.Contains(subtitleKinds, kind) &&
		!paragraphsOf(chart.get("subtitle"), chartSubtitleParagraphsMax, chartSubtitleWords) {
		messages = append(messages, fmt.Sprintf("%s.subtitle must be one line of plain text, or a list of 1 to "+
			"%d such lines, of at most %d words in all", path, chartSubtitleParagraphsMax, chartSubtitleWords))
	}
	// A heading above the chart's section title starts a part of the page.
	if chart.has("heading") && slices.Contains(headingKinds, kind) && !lineOf(chart.get("heading"), chartHeadingWords) {
		messages = append(messages, fmt.Sprintf("%s.heading must be one line of plain text of at most %d words", path, chartHeadingWords))
	}
	// The words that stay with the chart: under its plot, and beside a half-width plot.
	for _, field := range chartNoteFields {
		if chart.has(field) && slices.Contains(chartNoteKinds, kind) &&
			!paragraphsOf(chart.get(field), chartNoteParagraphsMax, chartNoteWords) {
			messages = append(messages, fmt.Sprintf("%s.%s must be one line of plain text, or a list of 1 to %d "+
				"such lines, of at most %d words in all", path, field, chartNoteParagraphsMax, chartNoteWords))
		}
	}
	return messages
}

// readingChartFields checks a chart that draws readings on axes: xy, band, bar, interval,
// stacked-bar and grid, and a declaration whose kind is none Dispatch knows.
func readingChartFields(chart *object, path, kind string, charts []any) []string {
	across := objectOr(chart.get("x"))
	// Each panel of an xy draws its y against an x series.
	panelled := kind == "xy" && chart.has("panels")
	form := formNumber
	if !panelled {
		form = xForm(kind, across, chart.get("stack"))
	}
	messages := displayFields(chart, path, kind, form)
	// The outlines of an xy's items name another declaration of this list.
	if kind == "xy" && chart.has("items") {
		messages = append(messages, itemsFields(chart, path, form, charts)...)
	}
	if chart.has("reference") {
		messages = append(messages, referenceFields(chart.get("reference"), path, kind, across.has("steps"))...)
	}
	if chart.has("stack") {
		if kind != "stacked-bar" {
			messages = append(messages, path+".stack is only supported for stacked-bar")
		} else if !is(chart.get("stack"), "row") {
			messages = append(messages, path+".stack must be row")
		}
	}
	if kind == "stacked-bar" && is(chart.get("stack"), "row") && is(chart.get("group"), "row_group") {
		messages = append(messages, path+".group must be col or null with stack row: each segment is a col")
	}
	if chart.has("summary") {
		messages = append(messages, summaryFields(chart, path, kind)...)
	}
	if chart.has("missing_x") {
		if kind != "xy" || across.get("series") == nil || across.has("steps") {
			messages = append(messages, path+".missing_x is only supported for xy with a numeric x.series: "+
				"it draws readings that have no x reading")
		} else if !is(chart.get("missing_x"), "band") {
			messages = append(messages, path+".missing_x must be band")
		}
	}
	if chart.has("panels") {
		messages = append(messages, panelsFields(chart, path, kind)...)
	}
	// group row draws one line for each row of an xy.
	if group := chart.get("group"); group != nil && !isOneOf(group, "col", "row_group", "row") {
		messages = append(messages, path+".group must be col, row_group, row, or null")
	} else if is(group, "row") && kind != "xy" {
		messages = append(messages, path+".group row is only supported for xy: it draws one line for each row")
	}
	// With a y on each panel, or with y_options, the chart has no y of its own.
	panelY := false
	if panels, ok := chart.get("panels").([]any); ok && panelled {
		for _, panel := range panels {
			if panel, ok := asObject(panel); ok && panel.has("y") {
				panelY = true
			}
		}
	}
	switched := slices.Contains(displayKinds["y_options"], kind) && chart.has("y_options")
	for _, name := range []string{"x", "y"} {
		if name == "y" && (panelY || switched) {
			continue
		}
		var fallback any
		if name == "x" {
			fallback = newObject()
		}
		at := path + "." + name
		axis, ok := asObject(chart.getOr(name, fallback))
		if !ok {
			messages = append(messages, at+" must be an object")
			continue
		}
		messages = append(messages, axisFields(axis, at, name, kind, form, chart, true)...)
		if name == "x" && axis.has("steps") {
			messages = append(messages, stepsFields(axis, path, kind)...)
		}
	}
	if slices.Contains([]string{"bar", "interval", "stacked-bar", "grid"}, kind) {
		if across.get("series") != nil {
			messages = append(messages, path+".x.series must be null for this kind: its x axis is not a series")
		}
		if truthy(across.get("log")) {
			messages = append(messages, path+".x.log must be false for this kind: its x axis is not numeric")
		}
	}
	if (kind == "bar" || kind == "interval") && is(chart.get("group"), "row_group") {
		messages = append(messages, path+".group must be col or null for this kind")
	}
	if kind == "grid" {
		if chart.get("group") != nil {
			messages = append(messages, path+".group must be null for grid: a grid draws every row and column")
		}
		if axis, ok := asObject(chart.get("y")); ok && truthy(axis.get("log")) {
			messages = append(messages, path+".y.log must be false for grid: a grid draws cell values, not an axis")
		}
	}
	return messages
}
