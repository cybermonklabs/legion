package charts

import (
	"fmt"
	"math"
	"math/big"
	"slices"
	"strings"
)

// The optional display fields of a reading chart change how the page draws it, never what it
// reads; each is taken by the kinds displayKinds lists.

var (
	plotKinds = []string{"xy", "band", "bar", "interval", "stacked-bar"}
	// intervalKinds draw a reading's own interval, whose interval_label names an interval with no
	// level; a number takes interval_label too.
	intervalKinds = []string{"xy", "band", "bar", "interval", "grid"}
	displayKinds  = map[string][]string{
		"height": plotKinds, "states": plotKinds, "highlight": plotKinds,
		// A lane chart takes dated rules only: x and label over its measured time.
		"annotations": append(slices.Clone(plotKinds), "lanes"),
		// On an xy and a band, order gives each group its place, and so its colour.
		"legend": plotKinds, "order": {"xy", "band", "bar", "interval", "stacked-bar", "grid", "lanes"},
		"layout": {"bar", "interval", "stacked-bar"}, "orientation": {"bar"}, "of": {"bar", "grid"}, "mark": {"bar"},
		"row_notes": {"bar"}, "curve": {"xy", "band"}, "width": append(slices.Clone(plotKinds), "grid", "lanes"),
		// A chart zooms only where it asks, on a time or numeric x axis; a stacked bar by row draws
		// one frame per week.
		"zoom": {"xy", "band"}, "frames": {"stacked-bar"},
		// Colour by value: each cell, bar or part takes the state that covers its value, or on a bar
		// the value of state_of in its place.
		"value_states": {"bar", "stacked-bar", "grid", "lanes"}, "state_of": {"bar", "stacked-bar"},
		"stack_order":    {"stacked-bar", "bar"},
		"interval_label": intervalKinds, "interval_level_in": intervalKinds, "labels": {"interval"},
		"facet": {"bar"}, "groups": {"bar", "lanes"}, "row_subtitles": {"bar", "text"},
		"color": {"xy"}, "points": {"xy"}, "rings": {"xy"}, "medians": {"xy"}, "join": {"xy"},
		// A switch of the value the chart draws on y, among 2 to 4 series.
		"y_options": {"xy", "band"},
		"items":     {"xy"},
		// A second value axis right of the plot: the y axis's numbers times a factor in another unit.
		"y2":          {"xy"},
		"row_columns": {"bar", "lanes"}, "plot_header": {"bar"},
		"not_started_outline": append(slices.Clone(plotKinds), "grid", "lanes"),
	}
	displayFieldNames = sortedKeys(displayKinds)
	states            = []string{"proved", "partial", "attention", "context", "not-started"}
	statesText        = strings.Join(states[:len(states)-1], ", ") + " or " + states[len(states)-1]
	// facetFields are the display fields a facet table takes.
	facetFieldNames = []string{"facet", "order", "legend", "states", "stack_order", "row_subtitles", "width", "not_started_outline"}
	rowColumnFields = []string{"label", "series", "field", "decimals", "empty_label", "unit_in_label"}
	// What a chart with y_options, y2 or row_columns does not take, and why.
	yOptionConflicts = [][2]string{{"y", "each option names its own y"},
		{"panels", "panels draw each y side by side, a switch one at a time"},
		{"reference", "each option holds its own reference"}, {"mockup", "a mock-up draws one y"}}
	y2Conflicts = [][2]string{{"panels", "panels draw one value axis across two or three plots, and the second axis " +
		"stands right of one plot"},
		{"y_options", "each option draws another series, which one factor does not turn into the second unit"},
		{"missing_x", "its strip of readings with no x reading stands where the second axis draws"}}
	rowColumnsConflicts = [][2]string{{"row_notes", "the columns hold what a row's note would"},
		{"annotations", "an annotation names a row too, and would share its line"},
		{"reference", "a chart with row columns draws no reference, so put its words in a column"},
		{"legend", "each row's name names its dot, and its value stands beside it"}}
	intervalLevelPlaces = []string{"subtitle", "footnote", "note", "method"}
)

const (
	heightMin, heightMax = 160, 720
	// order and stack_order name up to 200 bars or parts, and on a grid up to 1,000 rows.
	orderMax, gridOrderMax   = 200, 1000
	valueStatesMax           = 5
	statesMax                = 16
	annotationsMax           = 8
	groupsMax                = 8
	groupLabelMaxChars       = 60
	rowSubtitleMaxChars      = 60
	rowColumnsMax            = 3
	rowColumnLabelMaxChars   = 40
	decimalsMax              = 3
	intervalLabelMaxChars    = 40
	yOptionsMin, yOptionsMax = 2, 4
	y2UnitMaxChars           = 40
)

var (
	y2ScaleMin, y2ScaleMax = 1e-12, 1e12
	y2RangeMin, y2RangeMax = 1e-12, 1e15
)

func sortedKeys(fields map[string][]string) []string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// wholeDecimals is a count of decimals a declaration may give its numbers: an int from 0 to 3.
func wholeDecimals(value any) bool {
	whole, ok := value.(*big.Int)
	return ok && whole.Sign() >= 0 && whole.Cmp(big.NewInt(decimalsMax)) <= 0
}

// intBetween is Python's type(value) is int and low <= value <= high.
func intBetween(value any, low, high int64) bool {
	whole, ok := value.(*big.Int)
	return ok && whole.Cmp(big.NewInt(low)) >= 0 && whole.Cmp(big.NewInt(high)) <= 0
}

// displayFields checks the optional fields that change how a reading chart is drawn.
func displayFields(chart *object, path, kind string, form xform) []string {
	var messages []string
	present := map[string]bool{}
	for _, field := range displayFieldNames {
		if !chart.has(field) {
			continue
		}
		if slices.Contains(displayKinds[field], kind) {
			present[field] = true
		} else {
			messages = append(messages, fmt.Sprintf("%s.%s is only supported for %s", path, field, listed(displayKinds[field])))
		}
	}
	add := func(message string) { messages = append(messages, message) }
	layout, orientation := chart.get("layout"), chart.get("orientation")
	horizontal := is(orientation, "horizontal")
	if present["height"] && !intBetween(chart.get("height"), heightMin, heightMax) {
		add(fmt.Sprintf("%s.height must be a whole number of pixels from %d to %d", path, heightMin, heightMax))
	}
	if present["order"] {
		// On a stacked bar by row order names its rows; by time, its groups; on a grid, its rows;
		// on an xy and a band, its groups. total draws the bars by their totals, largest first.
		order, most := chart.get("order"), orderMax
		if kind == "grid" {
			most = gridOrderMax
		}
		if is(order, "total") {
			if kind != "bar" && !(kind == "stacked-bar" && is(chart.get("stack"), "row")) {
				add(path + ".order total is only supported for bar and stacked-bar by row: it draws the bars by their totals")
			}
		} else if !plainList(order, 1, most) {
			noun := "category"
			if kind == "grid" {
				noun = "row"
			} else if kind == "xy" || kind == "band" {
				noun = "group"
			}
			add(fmt.Sprintf("%s.order must list 1 to %d distinct single-line %s names", path, most, noun))
		}
	}
	if present["stack_order"] {
		if !plainList(chart.get("stack_order"), 1, orderMax) {
			add(fmt.Sprintf("%s.stack_order must list 1 to %d distinct single-line group names", path, orderMax))
		} else if kind == "bar" && !chart.has("facet") {
			add(path + ".stack_order needs facet on a bar: it names the parts of each row of a facet table")
		} else if kind == "stacked-bar" && !is(chart.get("stack"), "row") && chart.has("order") {
			add(path + ".stack_order must not be set with order on a stacked-bar over time: both order the parts of each bar")
		}
	}
	switch {
	case present["legend"] && !is(chart.get("legend"), "direct"):
		add(path + ".legend must be direct")
	case present["legend"] && kind == "bar" && truthy(chart.get("group")) && !is(layout, "grouped") && !chart.has("facet"):
		// A grouped bar names each group over its first cluster, and a facet table each col over
		// its column; any other bar would colour its groups with nothing to name them.
		add(path + ".legend direct on a bar with group needs layout grouped or facet: nothing else names each group's colour")
	case present["legend"] && kind == "interval" && !is(layout, "grouped"):
		add(path + ".legend direct on an interval needs layout grouped: it names each col above its first cluster")
	}
	if present["states"] {
		groups, ok := asObject(chart.get("states"))
		named := ok && groups.size() >= 1 && groups.size() <= statesMax
		for _, name := range objectOr(chart.get("states")).keys {
			named = named && plainText(name)
		}
		if !named {
			add(fmt.Sprintf("%s.states must map 1 to %d single-line group names to a state", path, statesMax))
		} else {
			for _, name := range groups.keys {
				if !isOneOf(groups.get(name), states...) {
					add(fmt.Sprintf("%s.states[%s] must be %s", path, dumps(name), statesText))
				}
			}
		}
	}
	if present["highlight"] {
		if chart.has("states") {
			add(path + ".highlight must not be set with states: a chart takes one or the other")
		} else if !plainText(chart.get("highlight")) {
			add(path + ".highlight must be one single-line group name")
		}
	}
	if present["annotations"] {
		spans := (kind == "xy" || kind == "band") && (form == formTime || form == formNumber)
		points := !(kind == "bar" && horizontal || (kind == "bar" || kind == "interval") && is(layout, "grouped"))
		messages = append(messages, annotationFields(chart.get("annotations"), path+".annotations", form, spans, points,
			kind == "xy", kind == "lanes")...)
	}
	if present["layout"] && (kind == "bar" || kind == "interval") {
		if !is(layout, "grouped") {
			add(fmt.Sprintf("%s.layout must be grouped on %s", path, kind))
		} else if !is(chart.get("group"), "col") {
			mark := "point"
			if kind == "bar" {
				mark = "bar"
			}
			add(fmt.Sprintf("%s.layout grouped needs group col: one cluster per row, one %s per col", path, mark))
		}
		if chart.has("orientation") {
			add(path + ".layout must not be set with orientation")
		}
	} else if present["layout"] {
		if chart.has("stack") {
			add(path + ".layout is only supported for stacked-bar by time: area needs a time axis, and stack row has none")
		} else if !is(layout, "area") {
			add(path + ".layout must be area on stacked-bar")
		}
	}
	if present["orientation"] && !horizontal {
		add(path + ".orientation must be horizontal")
	}
	ySeries, yIsObject := any(nil), false
	if axis, ok := asObject(chart.get("y")); ok {
		ySeries, yIsObject = axis.get("series"), true
	}
	if present["of"] {
		// of names the series of each row's total (on a grid, each cell's; on a grouped bar, each
		// bar's), or is n: each reading's own n.
		of := chart.get("of")
		if kind == "bar" && !horizontal && !is(layout, "grouped") {
			add(path + ".of needs orientation horizontal or layout grouped: it names each bar's total")
		}
		if !matches(seriesName, of) {
			add(path + ".of must be n or a series name matching " + seriesPattern)
		} else if yIsObject && is(ySeries, of.(string)) {
			add(path + ".of must be another series than y.series: it holds each total")
		}
	}
	if present["value_states"] {
		messages = append(messages, valueStatesFields(chart.get("value_states"), path+".value_states")...)
		// States and a highlight set the colours of the bars too; an area has one colour a group.
		for _, other := range []string{"states", "highlight"} {
			if chart.has(other) {
				add(fmt.Sprintf("%s.value_states must not be set with %s: both set the colours of the bars", path, other))
			}
		}
		if is(layout, "area") {
			add(path + ".value_states must not be set with layout area: an area draws one colour for each group")
		}
	}
	if present["state_of"] {
		stateOf := chart.get("state_of")
		if !chart.has("value_states") {
			add(path + ".state_of needs value_states: it picks each bar's state by that series")
		}
		if !matches(seriesName, stateOf) {
			add(path + ".state_of must be a series name matching " + seriesPattern)
		} else if yIsObject && is(ySeries, stateOf.(string)) {
			add(path + ".state_of must be another series than y.series: without it, each bar takes the state of its own value")
		}
	}
	if present["mark"] {
		// dot draws a dot at each value; dots one dot for each unit of each row's total.
		mark := chart.get("mark")
		switch {
		case !horizontal || !isOneOf(mark, "dot", "dots"):
			add(path + ".mark must be dot or dots, on a bar with orientation horizontal")
		case is(mark, "dot") && chart.has("of"):
			add(path + ".mark dot must not be set with of: a dot has no length to draw against a total; mark dots " +
				"draws each total as dots")
		case is(mark, "dots") && !chart.has("of"):
			add(path + ".mark dots needs of: it draws one dot for each unit of each row's total")
		}
	}
	if present["groups"] {
		messages = append(messages, groupsFields(chart, path)...)
	}
	if present["facet"] {
		messages = append(messages, facetFields(chart, path)...)
	}
	if present["row_subtitles"] {
		messages = append(messages, rowSubtitlesFields(chart, path)...)
	}
	if present["row_notes"] {
		// Each row's newest reading's note, drawn in that row.
		switch {
		case !horizontal:
			add(path + ".row_notes needs orientation horizontal: it writes a note in each row")
		case !is(chart.get("row_notes"), "note"):
			add(path + ".row_notes must be note: the field of each row's reading that it prints")
		case chart.has("annotations"):
			// On a horizontal bar an annotation names a row too, and the two would share its line.
			add(path + ".row_notes must not be set with annotations: both write in a row, so put the annotation's " +
				"words in that row's note")
		}
	}
	if present["row_columns"] || present["plot_header"] {
		messages = append(messages, rowColumnsFields(chart, path, kind)...)
	}
	if present["not_started_outline"] {
		messages = append(messages, notStartedOutlineFields(chart, path)...)
	}
	if present["curve"] {
		// step over time; against a numeric x series, linear on an xy or step on an xy or a band.
		curve := chart.get("curve")
		switch {
		case !isOneOf(curve, "step", "linear"):
			add(path + ".curve must be step or linear")
		case form == formTime:
			if !is(curve, "step") {
				add(path + ".curve must be step on a time x axis: a line over time already joins its readings")
			}
		case form != formNumber:
			add(path + ".curve is only supported for an xy or band over time or against an x series: steps are categories")
		case kind == "band" && is(curve, "linear"):
			add(path + ".curve linear is not supported for band: a band always joins its points")
		}
		if chart.has("summary") {
			add(path + ".curve must not be set with summary: a summary draws its own median line")
		}
	}
	if present["zoom"] {
		if zoom, ok := chart.get("zoom").(bool); !ok || !zoom {
			add(path + ".zoom must be true")
		} else if form != formTime && form != formNumber {
			add(path + ".zoom is only supported for an xy or band over time or against an x series: a category axis never zooms")
		}
	}
	if present["frames"] {
		if !is(chart.get("frames"), "week") {
			add(path + ".frames must be week")
		} else if !is(chart.get("stack"), "row") {
			add(path + ".frames needs stack row: it draws one frame of a stacked bar by row for each week")
		}
	}
	if present["width"] && !is(chart.get("width"), "half") {
		add(path + ".width must be half")
	}
	if present["interval_label"] {
		messages = append(messages, intervalLabelFields(chart.get("interval_label"), path)...)
	}
	if present["interval_level_in"] {
		messages = append(messages, intervalLevelInFields(chart, path)...)
	}
	if present["labels"] && !is(chart.get("labels"), "value") {
		add(path + ".labels must be value: it prints each point's value beside it")
	}
	// An xy with steps draws one line for each row, through the newest reading at each step; with
	// points only it draws each reading as a dot at its step, and no line.
	const steps = ": each row draws one line through its steps, with each point's interval"
	strip := form == formCategory && is(chart.get("points"), "only")
	if present["color"] {
		// With one line for each row, each line takes the colour of its row group.
		switch {
		case !is(chart.get("color"), "row_group"):
			add(path + ".color must be row_group: each row's line takes the colour of its row group")
		case form == formCategory:
			add(path + ".color is not supported with x.steps" + steps)
		case !is(chart.get("group"), "row"):
			add(path + ".color needs group row: it colours each row's line by the row group of its readings")
		}
	}
	if present["points"] {
		// A dot at each line's last reading only, as a running total ends; a scatter has no line to end.
		points := chart.get("points")
		switch {
		case !isOneOf(points, "end", "only"):
			add(path + ".points must be end or only: a dot at the last point of each line only, or every point and no line")
		case form == formCategory && is(points, "end"):
			add(path + ".points end is not supported with x.steps" + steps + ", or with points only a dot for each " +
				"reading and no line")
		case is(points, "end") && form == formNumber && !chart.has("curve"):
			add(path + ".points end needs curve against an x series: with no curve the points join no line, so each point draws")
		case is(points, "only") && chart.has("curve"):
			add(path + ".points only must not be set with curve: a curve joins the points into lines")
		}
	}
	if present["join"] {
		// From each step to the next, a thin grey line through the median of each row group's dots.
		if !is(chart.get("join"), "row_group") {
			add(path + ".join must be row_group: a thin line joins the dots of each row group from step to step")
		} else if !strip {
			add(path + ".join needs x.steps and points only: it joins each row group's dots from step to step")
		}
	}
	if present["legend"] && strip && is(chart.get("legend"), "direct") {
		add(path + ".legend must not be set with points only and x.steps: the x axis names each step, and each " +
			"median's value stands beside its bar")
	}
	if reference, ok := asObject(chart.get("reference")); strip && ok && reference.has("from_start") {
		add(path + ".reference.from_start must not be set with points only: it draws a target on each line, and " +
			"no line joins the dots")
	}
	if present["y_options"] {
		messages = append(messages, yOptionsFields(chart, path, kind, form)...)
	}
	if present["y2"] {
		messages = append(messages, y2Fields(chart, path)...)
	}
	if present["rings"] {
		if form == formCategory {
			add(path + ".rings is not supported with x.steps" + steps)
		}
		messages = append(messages, ringsFields(chart, path)...)
	}
	if present["medians"] {
		messages = append(messages, mediansFields(chart, path, form == formCategory)...)
	}
	if axis, ok := asObject(chart.get("y")); (kind == "bar" || kind == "interval") && ok && truthy(axis.get("log")) {
		if present["layout"] && is(layout, "grouped") {
			marks := "points"
			if kind == "bar" {
				marks = "bars"
			}
			add(path + ".y.log must be false with layout grouped: the page draws grouped " + marks + " on a linear axis")
		}
		if present["of"] {
			add(path + ".y.log must be false with of: a bar drawn against its total runs on a 0 to 100% axis")
		} else if kind == "bar" && horizontal && !is(chart.get("mark"), "dot") {
			add(path + ".y.log needs mark dot on a horizontal bar: a bar's length on a log axis means nothing")
		}
	}
	return messages
}

// annotationFields checks labelled marks at an x value: with y a dot at (x, y), with x_end a
// shaded span, else a dashed rule. spans says whether the x axis is one a span can shade (an xy or
// band over time or against an x series); points whether x is the chart's x axis, so that (x, y)
// is a place on it; reach whether a note takes y_end, dot and bracket (an xy); rules whether each
// note is a dated rule across a lane chart's lanes, with x, label and detail only.
func annotationFields(value any, path string, form xform, spans, points, reach, rules bool) []string {
	annotations, ok := value.([]any)
	if !ok || len(annotations) < 1 || len(annotations) > annotationsMax {
		return []string{fmt.Sprintf("%s must list 1 to %d objects with x and label", path, annotationsMax)}
	}
	var messages []string
	add := func(message string) { messages = append(messages, message) }
	known := []string{"x", "x_end", "y", "y_end", "label", "detail", "state", "dot", "bracket"}
	unknown := " is unknown"
	if rules {
		known = []string{"x", "label", "detail"}
		unknown = " is unknown on lanes: a lane chart's annotation is a dated rule, with x, label and detail"
	}
	for index, item := range annotations {
		at := fmt.Sprintf("%s[%d]", path, index)
		given, ok := asObject(item)
		if !ok {
			add(at + " must be an object with x and label")
			continue
		}
		for _, field := range given.unknown(known...) {
			add(at + "." + field + unknown)
		}
		note := given.only(known...)
		start, startOK := xValue(form, note.get("x"))
		if !startOK {
			add(at + ".x must be " + xValues[form])
		}
		if note.has("x_end") {
			end, endOK := xValue(form, note.get("x_end"))
			switch {
			case !spans:
				add(at + ".x_end is only supported for an xy or band over time or against an x series")
			case note.has("y"):
				add(at + ".x_end must not be set with y: an annotation marks a point or a span")
			case !endOK:
				add(at + ".x_end must be " + xValues[form])
			case startOK && !later(end, start):
				add(at + ".x_end must be later than x")
			}
		}
		if note.has("y") && !points {
			add(at + ".y is only supported where x is the chart's x axis: on a horizontal or grouped bar, x names a row or a cluster")
		} else if note.has("y") && !finite(note.get("y")) {
			add(at + ".y must be a finite number")
		}
		if note.has("y_end") {
			switch {
			case !reach:
				add(at + ".y_end is only supported for xy: it draws a dotted line from y to y_end")
			case !note.has("y"):
				add(at + ".y_end needs y: the dotted line runs from y to y_end")
			case note.has("x_end"):
				add(at + ".y_end must not be set with x_end: a note marks a span or a point")
			case !finite(note.get("y_end")):
				add(at + ".y_end must be a finite number")
			}
		}
		// dot false (an xy) draws a point note's label with no dot: a free label, which a reader
		// does not take for a reading.
		if note.has("dot") {
			switch {
			case !reach:
				add(at + ".dot is only supported for xy: it leaves out the dot of a note at a point")
			case !note.has("y"):
				add(at + ".dot needs y: only a note at a point draws a dot")
			default:
				if _, ok := note.get("dot").(bool); !ok {
					add(at + ".dot must be true or false")
				}
			}
		}
		// bracket true (an xy) draws a bracket from y to y_end at x in place of the dotted line,
		// with its label at its foot, and no dot.
		if note.has("bracket") {
			bracket, isBool := note.get("bracket").(bool)
			switch {
			case !reach:
				add(at + ".bracket is only supported for xy: it draws a bracket from y to y_end")
			case !isBool:
				add(at + ".bracket must be true or false")
			case bracket && !(note.has("y") && note.has("y_end")):
				add(at + ".bracket needs y and y_end: the bracket runs from y to y_end")
			case bracket && note.has("dot"):
				add(at + ".dot must not be set with bracket: a bracket draws no dot")
			case bracket && finite(note.get("y")) && finite(note.get("y_end")) && compare(note.get("y"), note.get("y_end")) == 0:
				add(at + ".y_end must differ from y on a bracket: the bracket runs from y to y_end")
			}
		}
		if note.has("state") && !isOneOf(note.get("state"), states...) {
			add(at + ".state must be " + statesText)
		}
		if !shortLabel(note.get("label")) {
			add(fmt.Sprintf("%s.label must be nonempty single-line plain text of at most %d characters", at, shortLabelMaxChars))
		}
		if note.has("detail") && !shortLabel(note.get("detail")) {
			add(fmt.Sprintf("%s.detail must be nonempty single-line plain text of at most %d characters: the note's "+
				"second line, under its label", at, shortLabelMaxChars))
		}
	}
	return messages
}

// valueStatesFields checks colour by value: 1 to 5 states, each with a label; the first covers
// every value below the second's from, and each later from rises.
func valueStatesFields(value any, path string) []string {
	entries, ok := value.([]any)
	if !ok || len(entries) < 1 || len(entries) > valueStatesMax {
		return []string{fmt.Sprintf("%s must list 1 to %d objects with state and label", path, valueStatesMax)}
	}
	var messages []string
	var last any
	for index, item := range entries {
		at := fmt.Sprintf("%s[%d]", path, index)
		entry, ok := asObject(item)
		if !ok {
			messages = append(messages, at+" must be an object with state and label")
			continue
		}
		for _, field := range entry.unknown("state", "label", "from") {
			messages = append(messages, at+"."+field+" is unknown")
		}
		if !isOneOf(entry.get("state"), states...) {
			messages = append(messages, at+".state must be "+statesText)
		}
		if !shortLabel(entry.get("label")) {
			messages = append(messages, fmt.Sprintf("%s.label must be nonempty single-line plain text of at most %d characters",
				at, shortLabelMaxChars))
		}
		switch {
		case index == 0:
			if entry.has("from") {
				messages = append(messages, at+".from must not be set on the first state: it covers every value below the next")
			}
		case !finite(entry.get("from")):
			messages = append(messages, at+".from must be a finite number: the lowest value this state covers")
		default:
			if last != nil && !above(entry.get("from"), last) {
				messages = append(messages, at+".from must be larger than the from before it")
			}
			last = entry.get("from")
		}
	}
	return messages
}

// groupsFields checks row groups on a horizontal bar, each a heading above its rows, or on a lane
// chart, each a name in the column left of its lanes' names; a row is in one group at most.
func groupsFields(chart *object, path string) []string {
	if is(chart.get("kind"), "bar") && !is(chart.get("orientation"), "horizontal") {
		return []string{path + ".groups needs orientation horizontal: each group's label draws as a heading above its rows"}
	}
	if chart.has("order") {
		return []string{path + ".groups must not be set with order: both set the order of the rows"}
	}
	groups, ok := chart.get("groups").([]any)
	if !ok || len(groups) < 1 || len(groups) > groupsMax {
		return []string{fmt.Sprintf("%s.groups must list 1 to %d objects with label and rows", path, groupsMax)}
	}
	var messages []string
	seen := map[string]bool{}
	for index, item := range groups {
		at := fmt.Sprintf("%s.groups[%d]", path, index)
		group, ok := asObject(item)
		if !ok {
			messages = append(messages, at+" must be an object with label and rows")
			continue
		}
		for _, field := range group.unknown("label", "rows") {
			messages = append(messages, at+"."+field+" is unknown")
		}
		if !plainTextOf(group.get("label"), groupLabelMaxChars) {
			messages = append(messages, fmt.Sprintf("%s.label must be nonempty single-line plain text of at most %d characters",
				at, groupLabelMaxChars))
		}
		if !plainList(group.get("rows"), 1, orderMax) {
			messages = append(messages, fmt.Sprintf("%s.rows must list 1 to %d distinct single-line row names", at, orderMax))
			continue
		}
		rows := group.get("rows").([]any)
		for _, row := range rows {
			if seen[row.(string)] {
				messages = append(messages, fmt.Sprintf("%s.rows names %s, which an earlier group names: a row is in one "+
					"group at most", at, dumps(row)))
				break
			}
		}
		for _, row := range rows {
			seen[row.(string)] = true
		}
	}
	return messages
}

// rowColumnsFields checks the columns of a horizontal dot chart, right of its plot, or of a lane
// chart, between its lanes' names and its lanes: each the newest reading of a series in each row,
// headed by its label, and plot_header heading the plot the same way.
func rowColumnsFields(chart *object, path, kind string) []string {
	if !chart.has("row_columns") {
		return []string{path + ".plot_header needs row_columns: it heads the plot as each column's label heads its column"}
	}
	var messages []string
	if chart.has("plot_header") && !plainTextOf(chart.get("plot_header"), rowColumnLabelMaxChars) {
		messages = append(messages, fmt.Sprintf("%s.plot_header must be nonempty single-line plain text of at most "+
			"%d characters: the header over the plot", path, rowColumnLabelMaxChars))
	}
	if kind == "bar" && (!is(chart.get("orientation"), "horizontal") || !is(chart.get("mark"), "dot")) {
		return append(messages, path+".row_columns needs orientation horizontal and mark dot: its cells stand on each dot's row")
	}
	if kind == "bar" {
		for _, conflict := range rowColumnsConflicts {
			if chart.has(conflict[0]) {
				messages = append(messages, fmt.Sprintf("%s.row_columns must not be set with %s: %s", path, conflict[0], conflict[1]))
			}
		}
	}
	columns, ok := chart.get("row_columns").([]any)
	if !ok || len(columns) < 1 || len(columns) > rowColumnsMax {
		return append(messages, fmt.Sprintf("%s.row_columns must list 1 to %d columns, each {label, series}", path, rowColumnsMax))
	}
	var own, marks any
	if axis, ok := asObject(chart.get("y")); ok {
		own = axis.get("series")
	}
	if kind == "lanes" {
		marks = chart.get("marks")
	}
	seen := map[[2]string]bool{}
	for index, item := range columns {
		at := fmt.Sprintf("%s.row_columns[%d]", path, index)
		column, ok := asObject(item)
		if !ok {
			messages = append(messages, at+" must be an object: {label, series}, with field if it needs one")
			continue
		}
		for _, field := range column.unknown(rowColumnFields...) {
			messages = append(messages, at+"."+field+" is unknown")
		}
		if !plainTextOf(column.get("label"), rowColumnLabelMaxChars) {
			messages = append(messages, fmt.Sprintf("%s.label must be nonempty single-line plain text of at most %d "+
				"characters: the column's header", at, rowColumnLabelMaxChars))
		}
		series, field := column.get("series"), column.getOr("field", "value")
		switch {
		case !matches(seriesName, series):
			messages = append(messages, at+".series must be a series name matching "+seriesPattern)
		case equal(series, own):
			lane := "dot"
			if kind == "lanes" {
				lane = "lane"
			}
			messages = append(messages, at+".series must be another series than y.series: each "+lane+" already draws that series")
		case equal(series, marks):
			messages = append(messages, at+".series must be another series than marks: each lane already draws its marks")
		}
		if !isOneOf(field, "value", "note") {
			messages = append(messages, at+".field must be value or note: the part of the row's reading that its cell prints")
		} else if series, ok := series.(string); ok {
			identity := [2]string{series, field.(string)}
			if seen[identity] {
				messages = append(messages, at+" must not repeat an earlier column's series and field")
			}
			seen[identity] = true
		}
		// A number column prints its numbers with its decimals; a note has none.
		if column.has("decimals") {
			if !wholeDecimals(column.get("decimals")) {
				messages = append(messages, fmt.Sprintf("%s.decimals must be a whole number from 0 to %d: the decimals each "+
					"of its cells prints with", at, decimalsMax))
			} else if is(field, "note") {
				messages = append(messages, at+".decimals needs field value: a note prints no number")
			}
		}
		// A cell with no reading prints empty_label; with none it is empty.
		if column.has("empty_label") && !plainText(column.get("empty_label")) {
			messages = append(messages, at+".empty_label must be single-line plain text")
		}
		// unit_in_label true: the label names the unit, and a cell prints its number alone.
		if _, ok := column.get("unit_in_label").(bool); column.has("unit_in_label") && !ok {
			messages = append(messages, at+".unit_in_label must be a boolean: true where the label names the unit its cells "+
				"then leave out")
		}
	}
	return messages
}

// notStartedOutlineFields checks not_started_outline grey, which draws each not-started mark with
// a grey outline in place of the state scale's blue; it needs a mark that can draw that state.
func notStartedOutlineFields(chart *object, path string) []string {
	var messages []string
	if !is(chart.get("not_started_outline"), "grey") {
		messages = append(messages, path+".not_started_outline must be grey: with no not_started_outline, a not-started "+
			"mark's outline is the state scale's blue")
	}
	drawn := false
	if groups, ok := asObject(chart.get("states")); ok {
		for _, name := range groups.keys {
			drawn = drawn || is(groups.get(name), "not-started")
		}
	}
	for _, field := range []string{"value_states", "annotations"} {
		for _, item := range listOr(chart.get(field)) {
			if entry, ok := asObject(item); ok {
				drawn = drawn || is(entry.get("state"), "not-started")
			}
		}
	}
	if !drawn {
		messages = append(messages, path+".not_started_outline needs a not-started state in states, value_states or an "+
			"annotation: it sets the outline of a not-started mark, and the chart draws none")
	}
	return messages
}

// facetFields checks a facet table on a bar: one row of the table for each row of the readings,
// one column for each col, and in each cell a short bar scaled within its row, so it draws no
// value axis.
func facetFields(chart *object, path string) []string {
	if !is(chart.get("facet"), "row") {
		return []string{path + ".facet must be row: each row of the readings draws as one row of the facet table"}
	}
	if !is(chart.get("group"), "col") {
		return []string{path + ".facet needs group col: each row of the facet table draws one bar for each col"}
	}
	var messages []string
	for _, field := range displayFieldNames {
		if chart.has(field) && !slices.Contains(facetFieldNames, field) {
			messages = append(messages, fmt.Sprintf("%s.%s must not be set with facet: a facet table takes only order, legend, "+
				"states, stack_order, row_subtitles, width and not_started_outline", path, field))
		}
	}
	if chart.has("reference") {
		messages = append(messages, path+".reference must not be set with facet: a facet table draws no value axis")
	}
	// log false is the default that a declaration may write out.
	axis := objectOr(chart.get("y"))
	for _, field := range []string{"log", "min", "max", "ticks"} {
		if axis.has(field) && (field != "log" || truthy(axis.get(field))) {
			messages = append(messages, fmt.Sprintf("%s.y.%s must not be set with facet: each row's bars are scaled within "+
				"the row, on no value axis", path, field))
		}
	}
	return messages
}

// rowSubtitlesFields checks the line under each row's name of a facet table.
func rowSubtitlesFields(chart *object, path string) []string {
	if !chart.has("facet") {
		return []string{path + ".row_subtitles needs facet: it writes a line under each row's name of a facet table"}
	}
	lines, ok := asObject(chart.get("row_subtitles"))
	if !ok || lines.size() < 1 || lines.size() > orderMax || !allPlain(lines.keys) {
		return []string{fmt.Sprintf("%s.row_subtitles must map 1 to %d single-line row names to a line of text", path, orderMax)}
	}
	var messages []string
	for _, name := range lines.keys {
		if !plainTextOf(lines.get(name), rowSubtitleMaxChars) {
			messages = append(messages, fmt.Sprintf("%s.row_subtitles[%s] must be nonempty single-line plain text of at most "+
				"%d characters", path, dumps(name), rowSubtitleMaxChars))
		}
	}
	return messages
}

func allPlain(names []string) bool {
	for _, name := range names {
		if !plainText(name) {
			return false
		}
	}
	return true
}

// intervalLabelFields checks the words that name a range with no level, such as "lowest to
// highest task", in place of "recorded interval".
func intervalLabelFields(label any, path string) []string {
	if plainTextOf(label, intervalLabelMaxChars) {
		return nil
	}
	return []string{fmt.Sprintf("%s.interval_label must be nonempty single-line plain text of at most %d characters",
		path, intervalLabelMaxChars)}
}

// intervalLevelInFields checks the declared text that states the level of the chart's intervals.
// A field of the chart's own is one the declaration gives; the platform checks the eval's method
// apart from the charts.
func intervalLevelInFields(chart *object, path string) []string {
	place := chart.get("interval_level_in")
	if !isOneOf(place, intervalLevelPlaces...) {
		return []string{path + ".interval_level_in must be subtitle, footnote, note or method: the declared text that " +
			"states the level of the chart's intervals"}
	}
	if !is(place, "method") && !truthy(chart.get(place.(string))) {
		return []string{fmt.Sprintf("%s.interval_level_in names %s, which this declaration does not give", path, place)}
	}
	return nil
}

// yOptionsFields checks y_options on an xy or a band: 2 to 4 y axes, each the y of one view of
// the chart, which buttons named by each label switch between; two options differ in their label
// and in their series.
func yOptionsFields(chart *object, path, kind string, form xform) []string {
	options, ok := chart.get("y_options").([]any)
	if !ok || len(options) < yOptionsMin || len(options) > yOptionsMax {
		return []string{fmt.Sprintf("%s.y_options must list %d to %d objects, each with series and label", path, yOptionsMin, yOptionsMax)}
	}
	var messages []string
	for _, conflict := range yOptionConflicts {
		if chart.has(conflict[0]) {
			messages = append(messages, fmt.Sprintf("%s.%s must not be set with y_options: %s", path, conflict[0], conflict[1]))
		}
	}
	if form == formCategory {
		messages = append(messages, path+".y_options is not supported with x.steps: each step draws one y")
	}
	labels, series := map[string]bool{}, map[string]bool{}
	for index, item := range options {
		at := fmt.Sprintf("%s.y_options[%d]", path, index)
		option, ok := asObject(item)
		if !ok {
			messages = append(messages, at+" must be an object with series and label")
			continue
		}
		for _, field := range option.unknown("series", "label", "log", "min", "max", "reference") {
			messages = append(messages, at+"."+field+" is unknown")
		}
		messages = append(messages, axisFields(option.only("series", "label", "log", "min", "max"), at, "y", kind, form, chart, true)...)
		label := option.get("label")
		switch {
		case !shortLabel(label):
			messages = append(messages, fmt.Sprintf("%s.label must be nonempty single-line plain text of at most %d "+
				"characters: it names the option's button", at, shortLabelMaxChars))
		case labels[label.(string)]:
			messages = append(messages, at+".label must differ from the label of each other option")
		default:
			labels[label.(string)] = true
		}
		if name, ok := option.get("series").(string); ok {
			if series[name] {
				messages = append(messages, at+".series must differ from the series of each other option")
			}
			series[name] = true
		}
		if option.has("reference") {
			messages = append(messages, referenceFields(option.get("reference"), at, kind, false)...)
		}
	}
	return messages
}

// y2Fields checks y2 on an xy: a second value axis right of the plot, whose numbers are the y
// axis's times scale, with label naming the axis and unit its unit, as a reading logs one.
func y2Fields(chart *object, path string) []string {
	second, ok := asObject(chart.get("y2"))
	if !ok {
		return []string{path + ".y2 must be an object with scale, label and unit"}
	}
	var messages []string
	for _, field := range second.unknown("scale", "label", "unit") {
		messages = append(messages, path+".y2."+field+" is unknown")
	}
	for _, conflict := range y2Conflicts {
		if chart.has(conflict[0]) {
			messages = append(messages, fmt.Sprintf("%s.y2 must not be set with %s: %s", path, conflict[0], conflict[1]))
		}
	}
	scale := second.get("scale")
	if !isNumber(scale) || compare(scale, y2ScaleMin) < 0 || compare(scale, y2ScaleMax) > 0 {
		messages = append(messages, path+".y2.scale must be a number from 1e-12 to 1e12: each y value times it is the second axis's")
	} else {
		messages = append(messages, y2Range(chart, path, scale)...)
	}
	if !shortLabel(second.get("label")) {
		messages = append(messages, fmt.Sprintf("%s.y2.label must be nonempty single-line plain text of at most %d "+
			"characters: it names the second axis", path, shortLabelMaxChars))
	}
	if !plainTextOf(second.get("unit"), y2UnitMaxChars) {
		messages = append(messages, fmt.Sprintf("%s.y2.unit must be nonempty single-line plain text of at most %d "+
			"characters: the second axis's unit, as a reading logs one", path, y2UnitMaxChars))
	}
	return messages
}

// y2Range checks that each declared end of the y axis times scale is a float from 1e-12 to 1e15 in
// size, or 0 on a linear y axis: the second axis draws only those. An end that is no finite
// number, or not above 0 on a log axis, is rangeFields' to refuse.
func y2Range(chart *object, path string, scale any) []string {
	axis := objectOr(chart.get("y"))
	logged, _ := axis.get("log").(bool)
	var messages []string
	for _, end := range []string{"min", "max"} {
		value := axis.get(end)
		if !finite(value) || logged && !above(value, int64Zero) || !logged && compare(value, int64Zero) == 0 {
			continue
		}
		size := math.Abs(asFloat(value) * asFloat(scale))
		if !(y2RangeMin <= size && size <= y2RangeMax) {
			messages = append(messages, fmt.Sprintf("%s.y.%s times %s.y2.scale must be from 1e-12 to 1e15 in size, or 0 on a "+
				"linear y axis: the second axis draws only numbers in that range", path, end, path))
		}
	}
	return messages
}

// int64Zero is the int 0, against which a number is compared exactly.
var int64Zero = big.NewInt(0)

// ringsFields checks rings on an xy: the groups whose points draw hollow, and the words of the
// key's ring, such as "target not finished".
func ringsFields(chart *object, path string) []string {
	rings, ok := asObject(chart.get("rings"))
	if !ok {
		return []string{path + ".rings must be an object with groups and label"}
	}
	var messages []string
	for _, field := range rings.unknown("groups", "label") {
		messages = append(messages, path+".rings."+field+" is unknown")
	}
	if !isOneOf(chart.get("group"), "col", "row_group", "row") {
		messages = append(messages, path+".rings needs group: it names the groups whose points draw hollow")
	}
	if !plainList(rings.get("groups"), 1, orderMax) {
		messages = append(messages, fmt.Sprintf("%s.rings.groups must list 1 to %d distinct single-line group names", path, orderMax))
	}
	if !shortLabel(rings.get("label")) {
		messages = append(messages, fmt.Sprintf("%s.rings.label must be nonempty single-line plain text of at most %d "+
			"characters: the key names the ring with it", path, shortLabelMaxChars))
	}
	return messages
}

// mediansFields checks medians on an xy of one line for each row: the rows that are medians over
// the runs, each drawn larger than a run's with its own interval as its range. On an xy with
// steps (stepped) and points only, each median draws as a short bar across its step's dots.
func mediansFields(chart *object, path string, stepped bool) []string {
	medians, ok := asObject(chart.get("medians"))
	if !ok {
		return []string{path + ".medians must be an object with rows"}
	}
	var messages []string
	for _, field := range medians.unknown("rows", "label", "overall", "join", "band") {
		messages = append(messages, path+".medians."+field+" is unknown")
	}
	// join: a thin line through each median row's marks across x, for every row (true) or the
	// rows it lists; band: the overall row's range shaded across x (true), or each listed row's.
	rowsNamed := listOr(medians.get("rows"))
	for _, field := range []string{"join", "band"} {
		if _, isBool := medians.get(field).(bool); !medians.has(field) || isBool {
			continue
		}
		named, ok := medians.get(field).([]any)
		valid := ok && len(named) > 0
		for _, name := range named {
			_, isText := name.(string)
			valid = valid && isText
		}
		if !valid || !distinctNames(named) {
			messages = append(messages, fmt.Sprintf("%s.medians.%s must be true, false or a list of distinct names of medians.rows", path, field))
			continue
		}
		for _, name := range named {
			if !contains(rowsNamed, name) {
				messages = append(messages, fmt.Sprintf("%s.medians.%s names %s, which is not one of medians.rows", path, field, name))
			}
		}
	}
	if band, _ := medians.get("band").(bool); band && !medians.has("overall") {
		messages = append(messages, path+".medians.band needs overall: it shades the range of the median over every run; a "+
			"list of rows shades theirs")
	}
	if stepped {
		if !is(chart.get("points"), "only") {
			messages = append(messages, path+".medians needs points only with x.steps: each median draws as a bar across its "+
				"step's dots")
		}
		for _, field := range []string{"join", "band"} {
			if medians.has(field) {
				messages = append(messages, fmt.Sprintf("%s.medians.%s is not supported with x.steps: each median draws as a "+
					"labelled bar at its step", path, field))
			}
		}
	} else if !is(chart.get("group"), "row") {
		messages = append(messages, path+".medians needs group row: a median is a row of its own, one line over the sets")
	}
	if chart.has("summary") {
		messages = append(messages, path+".medians must not be set with summary: the page computes no median of readings "+
			"that the team logs as medians")
	}
	if !plainList(medians.get("rows"), 1, orderMax) {
		messages = append(messages, fmt.Sprintf("%s.medians.rows must list 1 to %d distinct single-line row names", path, orderMax))
	}
	// overall: the one row of rows that is the median over every run.
	if overall, isText := medians.get("overall").(string); medians.has("overall") {
		if rows, isList := medians.get("rows").([]any); !isText || !isList || !contains(rows, overall) {
			messages = append(messages, path+".medians.overall must be one of medians.rows: the median over every run")
		}
	}
	if medians.has("label") && !shortLabel(medians.get("label")) {
		messages = append(messages, fmt.Sprintf("%s.medians.label must be nonempty single-line plain text of at most %d "+
			"characters: the key names the medians with it", path, shortLabelMaxChars))
	}
	return messages
}
