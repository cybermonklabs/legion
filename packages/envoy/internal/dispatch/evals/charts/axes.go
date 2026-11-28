package charts

import (
	"fmt"
	"slices"
	"time"
)

// xform is what an x value is on a chart: a time, a number or a category; none on a grid.
type xform string

const (
	formNone     xform = ""
	formTime     xform = "time"
	formNumber   xform = "number"
	formCategory xform = "category"
)

// xValues is what an annotation's or a mock-up point's x must be, by the chart's x form.
var xValues = map[xform]string{
	formTime:     "a UTC date (2026-11-06) or UTC time (2026-11-06T09:00:00Z) on this time axis",
	formNumber:   "a finite number on this numeric axis",
	formCategory: "a single-line category name on this axis",
}

const (
	panelsMin, panelsMax = 2, 3
	panelTitleWords      = 6
	summaryMaxGroups     = 8
)

var (
	axisFieldNames  = []string{"series", "label", "log", "ticks", "min", "max"}
	panelYFields    = []string{"series", "label", "log", "min", "max"}
	itemsFieldNames = []string{"series", "outlines", "parts_label"}
	panelConflicts  = [][2]string{{"x", "each panel names its own x axis"}, {"annotations", "each panel holds its own annotations"},
		{"zoom", "a panel's x axis does not zoom"}, {"summary", "a summary is drawn against one x axis"},
		{"missing_x", "each panel lists the readings that have no x reading of its own"},
		{"legend", "the chart's one key names each group once"},
		{"mockup", "a mock-up's points stand on one x axis"}}
)

// xForm is what an x value is on this chart. A stacked bar by time draws one bar per measured
// time, so its x values are times.
func xForm(kind string, across *object, stack any) xform {
	if kind == "bar" || kind == "interval" || kind == "xy" && across.has("steps") || kind == "stacked-bar" && is(stack, "row") {
		return formCategory
	}
	if (kind == "xy" || kind == "band") && across.get("series") != nil {
		return formNumber
	}
	if slices.Contains(plotKinds, kind) {
		return formTime
	}
	return formNone
}

// xValue reads an x as its axis orders it, a date as its first moment; false if it is none.
func xValue(form xform, value any) (any, bool) {
	switch form {
	case formTime:
		if moment, ok := utcDate(value); ok {
			return moment, true
		}
		if moment, ok := utcTime(value); ok {
			return moment, true
		}
		return nil, false
	case formNumber:
		return value, finite(value)
	}
	return value, plainText(value)
}

// later is end > start of two x values of one time or numeric axis.
func later(end, start any) bool {
	if end, ok := end.(time.Time); ok {
		return end.After(start.(time.Time))
	}
	return above(end, start)
}

// referenceFields checks a reference: {y, label}, a rule at y on every kind with axes, and with
// y_end the band from y to y_end; {line: "y=x", label}, the diagonal of an xy; {from_start,
// label}, on an xy with steps, a target on each line at its last step. A reference holds one of
// y, line and from_start; a rule at y may name a state, whose colours its rule and label take.
func referenceFields(value any, path, kind string, steps bool) []string {
	if !slices.Contains(plotKinds, kind) {
		return []string{path + ".reference is only supported for " + listed(plotKinds)}
	}
	reference, ok := asObject(value)
	if !ok {
		return []string{path + ".reference must be an object"}
	}
	diagonal, targets := kind == "xy" && !steps, kind == "xy" && steps
	allowed := []string{"y", "y_end", "label", "state"}
	if diagonal {
		allowed = append(allowed, "line")
	}
	if targets {
		allowed = append(allowed, "from_start")
	}
	var messages []string
	for _, field := range reference.unknown(allowed...) {
		switch field {
		case "line":
			messages = append(messages, path+".reference.line is only supported for xy without x.steps; this chart takes reference.y")
		case "from_start":
			messages = append(messages, path+".reference.from_start is only supported for xy with x.steps: it draws a target on "+
				"each line at its last step")
		default:
			messages = append(messages, path+".reference."+field+" is unknown")
		}
	}
	var rules []string
	for _, field := range []string{"y", "line", "from_start"} {
		if reference.has(field) && slices.Contains(allowed, field) {
			rules = append(rules, field)
		}
	}
	onlyY := len(rules) == 1 && rules[0] == "y"
	switch {
	case len(rules) > 1:
		messages = append(messages, fmt.Sprintf("%s.reference.%s must not be set with reference.%s: a reference draws one rule",
			path, rules[len(rules)-1], rules[0]))
	case len(rules) == 1 && rules[0] == "line":
		if !is(reference.get("line"), "y=x") {
			messages = append(messages, path+".reference.line must be y=x")
		}
	case len(rules) == 1 && rules[0] == "from_start":
		if !finite(reference.get("from_start")) {
			messages = append(messages, path+".reference.from_start must be a finite number")
		}
	case !finite(reference.get("y")):
		messages = append(messages, path+".reference.y must be a finite number")
	}
	if reference.has("y_end") {
		// The band from y to y_end.
		switch {
		case !onlyY:
			messages = append(messages, path+".reference.y_end needs reference.y, and no line or from_start: it shades the "+
				"band from y to y_end")
		case !finite(reference.get("y_end")):
			messages = append(messages, path+".reference.y_end must be a finite number")
		case finite(reference.get("y")) && !above(reference.get("y_end"), reference.get("y")):
			messages = append(messages, path+".reference.y_end must be larger than reference.y: the band runs from y up to y_end")
		}
	}
	if reference.has("state") {
		if !isOneOf(reference.get("state"), states...) {
			messages = append(messages, path+".reference.state must be "+statesText)
		} else if !onlyY || reference.has("y_end") {
			messages = append(messages, path+".reference.state needs reference.y, and no y_end, line or from_start: it colours "+
				"the rule at y and its label")
		}
	}
	if !plainText(reference.get("label")) {
		messages = append(messages, path+".reference.label must be nonempty single-line plain text")
	}
	return messages
}

// rangeFields checks min and max of an axis: the y axis of each kind with a numeric value axis,
// and the x axis of an xy or a band against an x series, spans them. The time x axis of an xy or
// a band takes UTC dates or times as its ends.
func rangeFields(axis *object, at, name, kind string, form xform, chart *object) []string {
	var ends []string
	for _, end := range []string{"min", "max"} {
		if axis.has(end) {
			ends = append(ends, end)
		}
	}
	if len(ends) == 0 {
		return nil
	}
	xy := kind == "xy" || kind == "band"
	if name == "y" && !slices.Contains(plotKinds, kind) {
		return []string{fmt.Sprintf("%s.%s is only supported for %s: a grid draws cell values, not an axis", at, ends[0], listed(plotKinds))}
	}
	if name == "x" && xy && form == formTime {
		var messages []string
		moments := map[string]any{}
		for _, end := range ends {
			moment, ok := xValue(formTime, axis.get(end))
			if !ok {
				messages = append(messages, fmt.Sprintf("%s.%s must be a UTC date (2026-11-20) or UTC time (2026-11-20T09:00:00Z) on a "+
					"time axis", at, end))
			}
			moments[end] = moment
		}
		if len(messages) == 0 && len(ends) == 2 && !later(moments["max"], moments["min"]) {
			messages = append(messages, fmt.Sprintf("%s.min must be before %s.max", at, name))
		}
		return messages
	}
	if name == "x" && !(xy && form == formNumber) {
		return []string{fmt.Sprintf("%s.%s is only supported for an xy or band against an x series or over time", at, ends[0])}
	}
	if name == "y" && kind == "bar" && is(chart.get("orientation"), "horizontal") && chart.has("of") {
		return []string{fmt.Sprintf("%s.%s is not supported with of: a bar drawn against its total runs from 0 to 100%%", at, ends[0])}
	}
	var messages []string
	for _, end := range ends {
		if !finite(axis.get(end)) {
			messages = append(messages, fmt.Sprintf("%s.%s must be a finite number", at, end))
		}
	}
	if len(messages) > 0 {
		return messages
	}
	low, high := axis.get("min"), axis.get("max")
	if low != nil && high != nil && !above(high, low) {
		messages = append(messages, fmt.Sprintf("%s.min must be below %s.max", at, name))
	}
	if logged, _ := axis.get("log").(bool); logged {
		for _, end := range ends {
			if !above(axis.get(end), int64Zero) {
				messages = append(messages, fmt.Sprintf("%s.%s must be above 0 on a log axis", at, end))
			}
		}
	} else if name == "y" && (kind == "bar" || kind == "stacked-bar") && !is(chart.get("mark"), "dot") && low != nil && above(low, int64Zero) {
		messages = append(messages, fmt.Sprintf("%s.min must be 0 or below on %s: a bar starts at 0", at, kind))
	}
	return messages
}

// ticksFields checks y.ticks whole on a plot, and x.ticks week on a time axis (an xy or band over
// time, or a stacked area) or whole against an x series.
func ticksFields(ticks any, at, name, kind string, form xform, layout any) []string {
	if name == "y" {
		if kind == "grid" {
			return []string{at + ".ticks is not supported for grid: a grid draws cell values, not an axis"}
		}
		if is(ticks, "whole") {
			return nil
		}
		return []string{at + ".ticks must be whole"}
	}
	xy := kind == "xy" || kind == "band"
	if (xy || kind == "stacked-bar" && is(layout, "area")) && form == formTime {
		if is(ticks, "week") {
			return nil
		}
		return []string{at + ".ticks must be week on a time x axis"}
	}
	if xy && form == formNumber {
		if is(ticks, "whole") {
			return nil
		}
		return []string{at + ".ticks must be whole on a numeric x axis"}
	}
	return []string{at + ".ticks is only supported for the x axis of an xy or band, or of a stacked-bar with layout " +
		"area: week over time, whole against an x series"}
}

// stepsFields checks an xy's categorical x: two to eight step names, each matched against a
// reading's col.
func stepsFields(axis *object, path, kind string) []string {
	if kind != "xy" {
		return []string{path + ".x.steps is only supported for xy"}
	}
	var messages []string
	if !plainList(axis.get("steps"), 2, 8) {
		messages = append(messages, path+".x.steps must list 2 to 8 distinct single-line step names")
	}
	if axis.get("series") != nil {
		messages = append(messages, path+".x.series must be null with x.steps: each step is a reading's col")
	}
	if truthy(axis.get("log")) {
		messages = append(messages, path+".x.log must be false with x.steps: the steps are not numeric")
	}
	return messages
}

// summaryFields checks {cols} on a numeric xy: each named group's median and 95% interval per x
// value. A name is a col value, or a population: the part of a col before " · ".
func summaryFields(chart *object, path, kind string) []string {
	across := objectOr(chart.get("x"))
	if kind != "xy" || across.get("series") == nil || across.has("steps") {
		return []string{path + ".summary is only supported for xy with a numeric x.series: a group is summarised per x value"}
	}
	summary, ok := asObject(chart.get("summary"))
	if !ok {
		return []string{path + ".summary must be an object"}
	}
	var messages []string
	for _, field := range summary.unknown("cols") {
		messages = append(messages, path+".summary."+field+" is unknown")
	}
	if !plainList(summary.get("cols"), 1, summaryMaxGroups) {
		messages = append(messages, fmt.Sprintf("%s.summary.cols must list 1 to %d distinct single-line col values or populations",
			path, summaryMaxGroups))
	}
	return messages
}

// axisFields checks one axis object, x or y (at is its path): its series (null on x for measured
// time), label, log, ticks, min and max, and on x its steps where steps allows them.
func axisFields(axis *object, at, name, kind string, form xform, chart *object, steps bool) []string {
	known := axisFieldNames
	if name == "x" && steps {
		known = append(slices.Clone(axisFieldNames), "steps")
	}
	var messages []string
	for _, field := range axis.unknown(known...) {
		messages = append(messages, at+"."+field+" is unknown")
	}
	series := axis.get("series")
	if !(name == "x" && series == nil) && !matches(seriesName, series) {
		orNull := ""
		if name == "x" {
			orNull = " or null"
		}
		messages = append(messages, at+".series must be a series name matching "+seriesPattern+orNull)
	}
	if _, ok := axis.get("log").(bool); axis.has("log") && !ok {
		messages = append(messages, at+".log must be a boolean")
	}
	if label, ok := axis.get("label").(string); axis.has("label") && (!ok || label != "" && !printable(label) ||
		chars(label) > definitionTextMaxChars) {
		messages = append(messages, at+".label must be single-line plain text of at most 2000 characters")
	}
	if axis.has("ticks") {
		messages = append(messages, ticksFields(axis.get("ticks"), at, name, kind, form, chart.get("layout"))...)
	}
	return append(messages, rangeFields(axis, at, name, kind, form, chart)...)
}

// panelsFields checks panels on an xy: two or three panels side by side, each against its own x
// series. With no panel y the panels draw the chart's y on one value axis; with a y on every
// panel each draws its own, and two panels differ in their x or y series.
func panelsFields(chart *object, path, kind string) []string {
	if kind != "xy" {
		return []string{path + ".panels is only supported for xy: each panel draws its y against another x series"}
	}
	panels, ok := chart.get("panels").([]any)
	if !ok || len(panels) < panelsMin || len(panels) > panelsMax {
		return []string{fmt.Sprintf("%s.panels must list %d to %d objects, each with x", path, panelsMin, panelsMax)}
	}
	var messages []string
	for _, conflict := range panelConflicts {
		if chart.has(conflict[0]) {
			messages = append(messages, fmt.Sprintf("%s.%s must not be set with panels: %s", path, conflict[0], conflict[1]))
		}
	}
	if reference, ok := asObject(chart.get("reference")); ok && reference.has("line") {
		messages = append(messages, path+".reference.line must not be set with panels: each panel's x is another quantity than y")
	}
	own := make([]bool, len(panels))
	anyOwn, allOwn := false, true
	for index, item := range panels {
		panel, ok := asObject(item)
		own[index] = ok && panel.has("y")
		anyOwn, allOwn = anyOwn || own[index], allOwn && own[index]
	}
	if anyOwn {
		if !allOwn {
			messages = append(messages, fmt.Sprintf("%s.panels[%d].y must be set, as on each other panel: every panel names its "+
				"own y or none does", path, slices.Index(own, false)))
		}
		if chart.has("y") {
			messages = append(messages, path+".y must not be set with a y on each panel: each panel names its own y")
		}
		if chart.has("reference") {
			messages = append(messages, path+".reference must not be set with a y on each panel: each panel's y is another quantity")
		}
	}
	type pair struct {
		x, y string
		ownY bool
	}
	seen := map[pair]bool{}
	for index, item := range panels {
		at := fmt.Sprintf("%s.panels[%d]", path, index)
		panel, ok := asObject(item)
		if !ok {
			messages = append(messages, at+" must be an object with x")
			continue
		}
		for _, field := range panel.unknown("x", "y", "title", "annotations") {
			messages = append(messages, at+"."+field+" is unknown")
		}
		if panel.has("title") && !lineOf(panel.get("title"), panelTitleWords) {
			messages = append(messages, fmt.Sprintf("%s.title must be one line of plain text of at most %d words", at, panelTitleWords))
		}
		var ySeries any
		if panel.has("y") {
			values, ok := asObject(panel.get("y"))
			if !ok {
				messages = append(messages, at+".y must be an object with series")
			} else {
				for _, field := range values.unknown(panelYFields...) {
					messages = append(messages, at+".y."+field+" is unknown")
				}
				messages = append(messages, axisFields(values.only(panelYFields...), at+".y", "y", kind, formNumber, chart, true)...)
				ySeries = values.get("series")
			}
		}
		axis, ok := asObject(panel.get("x"))
		if !ok {
			messages = append(messages, at+".x must be an object with series and label")
			continue
		}
		messages = append(messages, axisFields(axis, at+".x", "x", kind, formNumber, chart, false)...)
		switch series := axis.get("series").(type) {
		case nil:
			messages = append(messages, at+".x.series must be a series name matching "+seriesPattern+
				": each panel draws y against an x series")
		case string:
			key := pair{x: series}
			if name, ok := ySeries.(string); ok {
				key.y, key.ownY = name, true
			}
			if seen[key] {
				if key.ownY {
					messages = append(messages, at+" must differ from each other panel in its x or its y series")
				} else {
					messages = append(messages, at+".x.series must differ from the x series of each other panel")
				}
			}
			seen[key] = true
		}
		if !shortLabel(axis.get("label")) {
			messages = append(messages, fmt.Sprintf("%s.x.label must be nonempty single-line plain text of at most %d characters: "+
				"it names the panel", at, shortLabelMaxChars))
		}
		if panel.has("annotations") {
			messages = append(messages, annotationFields(panel.get("annotations"), at+".annotations", formNumber, true, true, true, false)...)
		}
	}
	return messages
}

// itemsFields checks items on an xy over time: series, whose readings are the chart's items under
// the x of their measured time; outlines, an architecture chart of this eval whose parts an item
// outlines; and parts_label, the words before the list of an item's parts.
func itemsFields(chart *object, path string, form xform, charts []any) []string {
	items, ok := asObject(chart.get("items"))
	if !ok {
		return []string{path + ".items must be an object with series"}
	}
	var messages []string
	for _, field := range items.unknown(itemsFieldNames...) {
		messages = append(messages, path+".items."+field+" is unknown")
	}
	if form != formTime || chart.has("panels") {
		messages = append(messages, path+".items is only supported for an xy over time: each item lists under the x of its "+
			"reading's measured time")
	}
	series := items.get("series")
	drawn := false
	for _, axis := range append([]any{chart.get("y")}, listOr(chart.get("y_options"))...) {
		if axis, ok := asObject(axis); ok {
			drawn = drawn || equal(axis.get("series"), series)
		}
	}
	if !matches(seriesName, series) {
		messages = append(messages, path+".items.series must be a series name matching "+seriesPattern)
	} else if drawn {
		messages = append(messages, path+".items.series must be another series than each series the chart draws on y")
	}
	if items.has("outlines") {
		// The kind of the last declaration whose id is outlines, as a Python dict of ids keeps it.
		var kind any
		outlines, isText := items.get("outlines").(string)
		for _, other := range charts {
			if other, ok := asObject(other); ok && isText && is(other.get("id"), outlines) {
				kind = other.get("kind")
			}
		}
		if !isText || !is(kind, "architecture") {
			messages = append(messages, path+".items.outlines must be the id of an architecture chart of this eval: the diagram "+
				"whose parts an item outlines")
		}
	}
	if items.has("parts_label") && !shortLabel(items.get("parts_label")) {
		messages = append(messages, fmt.Sprintf("%s.items.parts_label must be nonempty single-line plain text of at most %d "+
			"characters: the words before each item's parts", path, shortLabelMaxChars))
	}
	return messages
}
