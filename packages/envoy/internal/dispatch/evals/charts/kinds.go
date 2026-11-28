package charts

import (
	"fmt"
	"math/big"
	"strings"
)

// The kinds that draw no reading axes of their own: a run view, a text section, a table of text
// and numbers, a lane chart's own fields, a network and an architecture diagram.

const (
	textColumnsMax, textRowsMax = 8, 40
	textRowLinesMax             = 2
	proseTitleWords             = 8
	proseParagraphsMax          = 4
	proseParagraphMaxChars      = 600
	laneHeaderMaxChars          = 40
	architectureBinsMax         = 4
	architectureNumberedMax     = 10
	architectureColumnsMax      = 12
)

// runColumnFields are where a run view's column reads its value: a run's settings, its tags or its
// final numbers.
var runColumnFields = []string{"setting", "tag", "final"}

// runsFields checks a run view: the columns it shows at first, the name it draws over time and
// the title of its x axis.
func runsFields(chart *object, path string) []string {
	var messages []string
	columns, ok := chart.get("columns").([]any)
	if !ok {
		messages = append(messages, path+".columns must be a list of objects, each {setting: <key>}, {tag: <key>} or {final: <name>}")
	}
	seen := map[[2]string]bool{}
	for index, item := range columns {
		at := fmt.Sprintf("%s.columns[%d]", path, index)
		column, ok := asObject(item)
		if !ok {
			messages = append(messages, at+" must be an object: {setting: <key>}, {tag: <key>} or {final: <name>}")
			continue
		}
		for _, field := range column.unknown(runColumnFields...) {
			messages = append(messages, at+"."+field+" is unknown")
		}
		var named []string
		for _, field := range runColumnFields {
			if column.has(field) {
				named = append(named, field)
			}
		}
		if len(named) != 1 {
			messages = append(messages, at+" must hold exactly one of setting, tag or final")
			continue
		}
		field := named[0]
		switch value := column.get(field); {
		case !plainText(value):
			messages = append(messages, at+"."+field+" must be nonempty single-line plain text")
		case seen[[2]string{field, value.(string)}]:
			messages = append(messages, at+" must not repeat an earlier column")
		default:
			seen[[2]string{field, value.(string)}] = true
		}
	}
	for _, field := range []string{"name", "step_label"} {
		if !plainText(chart.get(field)) {
			messages = append(messages, path+"."+field+" must be nonempty single-line plain text")
		}
	}
	return messages
}

// proseFields checks a text section after the charts: its title is the section's title, and its
// paragraphs draw in the page's body text.
func proseFields(chart *object, path string) []string {
	var messages []string
	if title, ok := chart.get("title").(string); ok && !blank(title) && words(title) > proseTitleWords {
		messages = append(messages, fmt.Sprintf("%s.title must be at most %d words: it is the section's title", path, proseTitleWords))
	}
	paragraphs, ok := chart.get("paragraphs").([]any)
	if !ok || len(paragraphs) < 1 || len(paragraphs) > proseParagraphsMax {
		return append(messages, fmt.Sprintf("%s.paragraphs must list 1 to %d paragraphs of plain text", path, proseParagraphsMax))
	}
	for index, paragraph := range paragraphs {
		if !plainTextOf(paragraph, proseParagraphMaxChars) {
			messages = append(messages, fmt.Sprintf("%s.paragraphs[%d] must be one nonempty line of plain text of at most %d characters",
				path, index, proseParagraphMaxChars))
		}
	}
	return messages
}

// textFields checks a table of text and numbers: one grid series, its declared columns in order,
// and an optional order of its rows. A column is {col, label}, with bar, field and on a bar mark
// dots; or a spark column {spark, label}, a small line of another series over time for each row.
// Either takes width, its share of the table's width, and the widths sum to less than 1 so the
// column of the rows' names keeps some of it.
func textFields(chart *object, path string) []string {
	var messages []string
	series := chart.get("series")
	if !matches(seriesName, series) {
		messages = append(messages, path+".series must be a series name matching "+seriesPattern)
	}
	columns, ok := chart.get("columns").([]any)
	if !ok || len(columns) < 1 || len(columns) > textColumnsMax {
		messages = append(messages, fmt.Sprintf("%s.columns must list 1 to %d columns, each {col, label}", path, textColumnsMax))
		columns = nil
	}
	seen := map[[2]string]bool{}
	widths := new(big.Rat)
	for index, item := range columns {
		at := fmt.Sprintf("%s.columns[%d]", path, index)
		column, ok := asObject(item)
		if !ok {
			messages = append(messages, at+" must be an object: {col, label}, with bar or field if it needs them")
			continue
		}
		if !shortLabel(column.get("label")) {
			messages = append(messages, fmt.Sprintf("%s.label must be nonempty single-line plain text of at most %d characters",
				at, shortLabelMaxChars))
		}
		if column.has("width") {
			width := column.get("width")
			if !finite(width) || !above(width, int64Zero) || compare(width, 1.0) >= 0 {
				messages = append(messages, at+".width must be a number above 0 and below 1: the column's share of the table's "+
					"width, such as 0.3")
			} else {
				widths.Add(widths, exact(width))
			}
		}
		if column.has("spark") {
			for _, field := range []string{"col", "bar", "field", "mark", "value_states", "lead"} {
				if column.has(field) {
					messages = append(messages, fmt.Sprintf("%s.%s must not be set with spark: a spark column draws its own series "+
						"in each row", at, field))
				}
			}
			for _, field := range column.unknown("spark", "label", "col", "bar", "field", "mark", "width", "value_states", "lead") {
				messages = append(messages, at+"."+field+" is unknown")
			}
			spark := column.get("spark")
			switch {
			case !matches(seriesName, spark):
				messages = append(messages, at+".spark must be a series name matching "+seriesPattern)
			case equal(spark, series):
				messages = append(messages, at+".spark must be another series than the table's own: it draws that series over "+
					"time for each row")
			case seen[[2]string{"spark", spark.(string)}]:
				messages = append(messages, at+" must not repeat an earlier spark column's series")
			default:
				seen[[2]string{"spark", spark.(string)}] = true
			}
			continue
		}
		for _, field := range column.unknown("col", "label", "bar", "field", "mark", "width", "value_states", "lead") {
			messages = append(messages, at+"."+field+" is unknown")
		}
		if column.has("value_states") {
			messages = append(messages, valueStatesFields(column.get("value_states"), at+".value_states")...)
			if truthy(column.get("bar")) {
				messages = append(messages, at+".value_states must not be set with bar: a bar cell draws its bar and number in the "+
					"table's blue")
			}
		}
		if !shortLabel(column.get("col")) {
			messages = append(messages, fmt.Sprintf("%s.col must be nonempty single-line plain text of at most %d characters",
				at, shortLabelMaxChars))
		}
		field := column.getOr("field", "value")
		if !isOneOf(field, "value", "note") {
			messages = append(messages, at+".field must be value or note: the part of the cell's reading that it prints")
		}
		if _, ok := column.get("bar").(bool); column.has("bar") && !ok {
			messages = append(messages, at+".bar must be a boolean")
		} else if truthy(column.get("bar")) && !is(field, "value") {
			messages = append(messages, at+".bar needs field value: a bar draws the cell's number")
		}
		if _, ok := column.get("lead").(bool); column.has("lead") && !ok {
			messages = append(messages, at+".lead must be a boolean")
		} else if truthy(column.get("lead")) && !is(field, "note") {
			messages = append(messages, at+".lead needs field note: it draws the cell's note as its lead, large and bold in its "+
				"value's state")
		}
		// mark dots draws a bar cell's total, its reading's n, as one dot for each unit.
		if column.has("mark") {
			if bar, _ := column.get("bar").(bool); !is(column.get("mark"), "dots") {
				messages = append(messages, at+".mark must be dots: it draws a bar cell's total as dots")
			} else if !bar {
				messages = append(messages, at+".mark dots needs bar true: it draws the bar's total, the reading's n, as dots")
			}
		}
		// A col and field that are refused above are named there, and are no identity to repeat.
		col, isText := column.get("col").(string)
		if !isText || !isOneOf(field, "value", "note") {
			continue
		}
		identity := [2]string{col, field.(string)}
		if seen[identity] {
			messages = append(messages, at+" must not repeat an earlier column's col and field")
		}
		seen[identity] = true
	}
	// The widths' sum as math.fsum rounds it: the exact sum, to the nearest float.
	if sum, _ := widths.Float64(); sum >= 1 {
		messages = append(messages, path+".columns' widths must sum to less than 1: the column of the rows' names takes a share "+
			"of the table's width too")
	}
	if chart.has("row_label") && !shortLabel(chart.get("row_label")) {
		messages = append(messages, fmt.Sprintf("%s.row_label must be nonempty single-line plain text of at most %d characters: "+
			"the header of the rows' names", path, shortLabelMaxChars))
	}
	if chart.has("order") && !plainList(chart.get("order"), 1, textRowsMax) {
		messages = append(messages, fmt.Sprintf("%s.order must list 1 to %d distinct single-line row names", path, textRowsMax))
	}
	if chart.has("row_subtitles") {
		messages = append(messages, textRowSubtitlesFields(chart, path)...)
	}
	return messages
}

// textRowSubtitlesFields checks the lines under each row's name of a text table: a map of 1 to 40
// row names, each to one line of plain text or to a list of one or two such lines.
func textRowSubtitlesFields(chart *object, path string) []string {
	lines, ok := asObject(chart.get("row_subtitles"))
	if !ok || lines.size() < 1 || lines.size() > textRowsMax || !allPlain(lines.keys) {
		return []string{fmt.Sprintf("%s.row_subtitles must map 1 to %d single-line row names to a line of text, or to a list "+
			"of 1 to %d lines", path, textRowsMax, textRowLinesMax)}
	}
	var messages []string
	for _, name := range lines.keys {
		each, ok := lines.get(name).([]any)
		if !ok {
			each = []any{lines.get(name)}
		}
		valid := len(each) >= 1 && len(each) <= textRowLinesMax
		for _, line := range each {
			valid = valid && shortLabel(line)
		}
		if !valid {
			messages = append(messages, fmt.Sprintf("%s.row_subtitles[%s] must be nonempty single-line plain text of at most %d "+
				"characters, or a list of 1 to %d such lines", path, dumps(name), shortLabelMaxChars, textRowLinesMax))
		}
	}
	return messages
}

// lanesFields checks a lane chart's own fields: one lane for each row of y.series over measured
// time, value_states that map each reading's number to a state, and marks, a second series whose
// readings in a lane are dated marks. Its x axis is measured time, so x holds only its label and
// ticks week. group_label, row_label and end_heading head the columns of the groups' names, the
// lanes' names and the lanes' end texts.
func lanesFields(chart *object, path string) []string {
	var messages []string
	axis, ok := asObject(chart.get("y"))
	if !ok {
		messages = append(messages, path+".y must be an object")
		axis = newObject()
	}
	for _, field := range axis.unknown("series", "label") {
		messages = append(messages, path+".y."+field+" is unknown")
	}
	series := axis.get("series")
	if !matches(seriesName, series) {
		messages = append(messages, path+".y.series must be a series name matching "+seriesPattern)
	}
	across, ok := asObject(chart.getOr("x", newObject()))
	if !ok {
		messages = append(messages, path+".x must be an object")
		across = newObject()
	}
	for _, field := range across.unknown("series", "label", "ticks") {
		messages = append(messages, path+".x."+field+" is unknown")
	}
	if across.has("ticks") && !is(across.get("ticks"), "week") {
		messages = append(messages, path+".x.ticks must be week on lanes: one tick each Monday")
	}
	if across.get("series") != nil {
		messages = append(messages, path+".x.series must be null for lanes: a lane chart's x axis is measured time")
	}
	for _, given := range []struct {
		name string
		axis *object
	}{{"x", across}, {"y", axis}} {
		label := given.axis.get("label")
		if text, ok := label.(string); label != nil && (!ok || text != "" && !printable(text) || chars(text) > definitionTextMaxChars) {
			messages = append(messages, path+"."+given.name+".label must be single-line plain text of at most 2000 characters")
		}
	}
	for _, field := range []string{"group_label", "row_label", "end_heading"} {
		if chart.has(field) && !plainTextOf(chart.get(field), laneHeaderMaxChars) {
			messages = append(messages, fmt.Sprintf("%s.%s must be nonempty single-line plain text of at most %d characters: the "+
				"header of its column", path, field, laneHeaderMaxChars))
		}
	}
	if chart.has("group_label") && !chart.has("groups") {
		messages = append(messages, path+".group_label needs groups: it heads the column of the groups' names")
	}
	if truthy(axis.get("label")) && (chart.has("groups") || chart.has("row_label") || chart.has("row_columns")) {
		messages = append(messages, path+".y.label must not be set with groups, row_label or row_columns: the lanes' names then "+
			"stand in a column of their own, under row_label")
	}
	if !chart.has("value_states") {
		messages = append(messages, path+".value_states is required for lanes: it maps each reading's number to the state that "+
			"colours its lane's end")
	}
	if chart.has("marks") {
		marks := chart.get("marks")
		if !matches(seriesName, marks) {
			messages = append(messages, path+".marks must be a series name matching "+seriesPattern)
		} else if equal(marks, series) {
			messages = append(messages, path+".marks must be another series than y.series: its readings are the dated marks")
		}
	}
	return messages
}

// networkFields checks a network chart: the node attributes it sizes, colours, outlines, labels
// and selects by, and the words it draws. The platform also requires its graph; Dispatch draws
// the network from readings, so a graph is checked only where one is given.
func networkFields(chart *object, path string) []string {
	var messages []string
	for _, field := range []string{"size", "color", "outline", "label"} {
		value := chart.get(field)
		if value == nil && (field == "outline" || field == "label") {
			continue
		}
		if !matches(attribute, value) {
			messages = append(messages, path+"."+field+" must name a node attribute")
		}
	}
	for _, field := range []string{"selector", "not_reported"} {
		values, ok := chart.getOr(field, []any{}).([]any)
		valid := ok
		for _, value := range values {
			valid = valid && matches(attribute, value)
		}
		if !valid {
			messages = append(messages, path+"."+field+" must be a list of node attributes")
		} else if !distinctNames(values) {
			messages = append(messages, path+"."+field+" must not repeat an attribute")
		}
	}
	labels, ok := asObject(chart.getOr("labels", newObject()))
	valid := ok
	if ok {
		for _, key := range labels.keys {
			valid = valid && attribute.MatchString(key) && plainText(labels.get(key))
		}
	}
	if !valid {
		messages = append(messages, path+".labels must map node attributes to single-line plain text")
	}
	if chart.has("empty_label") && !plainText(chart.get("empty_label")) {
		messages = append(messages, path+".empty_label must be single-line plain text")
	}
	words, ok := asObject(chart.getOr("value_labels", newObject()))
	valid = ok && len(words.unknown("yes", "no", "partial")) == 0
	if ok {
		for _, key := range words.keys {
			valid = valid && plainText(words.get(key))
		}
	}
	if !valid {
		messages = append(messages, path+".value_labels must map yes, no or partial to single-line plain text")
	}
	if ratio := chart.get("ratio"); ratio != nil {
		if ratio, ok := asObject(ratio); !ok {
			messages = append(messages, path+".ratio must be an object")
		} else {
			for _, field := range ratio.unknown("of", "out_of", "noun") {
				messages = append(messages, path+".ratio."+field+" is unknown")
			}
			for _, field := range []string{"of", "out_of"} {
				if !matches(attribute, ratio.get(field)) {
					messages = append(messages, path+".ratio."+field+" must name a node attribute")
				}
			}
			if !plainText(ratio.get("noun")) {
				messages = append(messages, path+".ratio.noun must be single-line plain text")
			}
		}
	}
	if chart.has("graph") {
		messages = append(messages, graphFields(chart, path)...)
	}
	return messages
}

// graphFields checks the platform's graph of a network or an architecture chart: {team,
// source_prefix}, the folder of a team's delivery whose graph files the chart draws.
func graphFields(chart *object, path string) []string {
	graph, ok := asObject(chart.get("graph"))
	if !ok {
		return []string{path + ".graph must be an object"}
	}
	var messages []string
	for _, field := range graph.unknown("team", "source_prefix") {
		messages = append(messages, path+".graph."+field+" is unknown")
	}
	if !matches(team, graph.get("team")) {
		messages = append(messages, path+".graph.team must name a team")
	}
	prefix, ok := graph.get("source_prefix").(string)
	valid := ok && strings.HasSuffix(prefix, "/")
	if valid {
		for _, part := range strings.Split(strings.TrimSuffix(prefix, "/"), "/") {
			valid = valid && part != "." && part != ".." && folder.MatchString(part)
		}
	}
	if !valid {
		messages = append(messages, path+".graph.source_prefix must be a relative folder prefix ending in /")
	}
	return messages
}

// architectureFields checks an architecture diagram: each part coloured by its reading of
// value_series against bins, the numbered heaviest parts badged, each value printed with decimals
// decimals, and columns, bands and key titling the diagram; second_reading is a second series of
// the same rows, which the diagram does not draw. The platform also requires its graph, the
// ledger folder of its parts; Dispatch draws the parts from readings, so a graph is checked only
// where one is given.
func architectureFields(chart *object, path string) []string {
	var messages []string
	if chart.has("graph") {
		messages = graphFields(chart, path)
	}
	value := chart.get("value_series")
	if !matches(seriesName, value) {
		messages = append(messages, path+".value_series must be a series name matching "+seriesPattern)
	}
	if chart.has("second_reading") {
		second := chart.get("second_reading")
		if !matches(seriesName, second) {
			messages = append(messages, path+".second_reading must be a series name matching "+seriesPattern)
		} else if equal(second, value) {
			messages = append(messages, path+".second_reading must be another series than value_series")
		}
	}
	bins, ok := chart.get("bins").([]any)
	valid := ok && len(bins) >= 1 && len(bins) <= architectureBinsMax
	for _, edge := range bins {
		valid = valid && finite(edge) && above(edge, int64Zero)
	}
	for index := 1; valid && index < len(bins); index++ {
		valid = above(bins[index], bins[index-1])
	}
	if !valid {
		messages = append(messages, fmt.Sprintf("%s.bins must list 1 to %d increasing numbers above 0: the limits above which a "+
			"part draws darker", path, architectureBinsMax))
	}
	if chart.has("numbered") && !intBetween(chart.get("numbered"), 0, architectureNumberedMax) {
		messages = append(messages, fmt.Sprintf("%s.numbered must be a whole number from 0 to %d: how many of the heaviest parts "+
			"draw a numbered badge", path, architectureNumberedMax))
	}
	if chart.has("decimals") && !wholeDecimals(chart.get("decimals")) {
		messages = append(messages, fmt.Sprintf("%s.decimals must be a whole number from 0 to %d: the decimals each value of the "+
			"diagram prints with", path, decimalsMax))
	}
	if chart.has("columns") {
		columns, ok := chart.get("columns").([]any)
		valid := ok && len(columns) <= architectureColumnsMax
		for _, column := range columns {
			valid = valid && (is(column, "") || shortLabel(column))
		}
		if !valid {
			messages = append(messages, fmt.Sprintf("%s.columns must list at most %d column titles, each empty or single-line plain "+
				"text of at most %d characters", path, architectureColumnsMax, shortLabelMaxChars))
		}
	}
	for _, titles := range [][3]string{{"bands", "above", "below"}, {"key", "fault_shown", "kernel"}} {
		field, names := titles[0], titles[1:]
		if !chart.has(field) {
			continue
		}
		given, ok := asObject(chart.get(field))
		if !ok {
			messages = append(messages, fmt.Sprintf("%s.%s must be an object with %s or %s", path, field, names[0], names[1]))
			continue
		}
		for _, name := range given.unknown(names...) {
			messages = append(messages, path+"."+field+"."+name+" is unknown")
		}
		for _, name := range names {
			if given.has(name) && !shortLabel(given.get(name)) {
				messages = append(messages, fmt.Sprintf("%s.%s.%s must be nonempty single-line plain text of at most %d characters",
					path, field, name, shortLabelMaxChars))
			}
		}
	}
	return messages
}
