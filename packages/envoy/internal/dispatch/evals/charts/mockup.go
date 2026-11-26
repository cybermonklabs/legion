package charts

import "fmt"

// An owner's mock-up sketches a graph that has no reading yet, and the chart's own drawing draws
// it. On a reading kind with no shape its points are in the form of readings; else, and on a
// pending chart, it has the older form: shape line or bar, labelled axes, and points.

const (
	mockupMaxSeries = 8
	mockupMaxPoints = 50
)

func mockupFields(value any, path string, chart *object) []string {
	mockup, ok := asObject(value)
	if !ok {
		return []string{path + " must be an object"}
	}
	kind := chart.get("kind")
	readings := !mockup.has("shape") && isOneOf(kind, readingKinds...)
	if !readings && isOneOf(kind, "grid", "lanes") {
		return []string{fmt.Sprintf("%s.shape is not supported for %s: its mock-up gives each point as a reading, with no shape",
			path, kind)}
	}
	fields := []string{"submitted_by", "submitted_at", "points", "shape", "x", "y"}
	if readings {
		fields = []string{"submitted_by", "submitted_at", "points", "ci_level"}
	}
	var messages []string
	for _, field := range mockup.unknown(fields...) {
		messages = append(messages, path+"."+field+" is unknown")
	}
	if !plainText(mockup.get("submitted_by")) {
		messages = append(messages, path+".submitted_by must be nonempty single-line plain text")
	}
	if _, ok := utcTime(mockup.get("submitted_at")); !ok {
		messages = append(messages, path+".submitted_at must be a UTC ISO time, for example 2026-11-11T09:00:00Z")
	}
	shape := mockup.get("shape")
	if !readings {
		if !isOneOf(shape, "line", "bar") {
			messages = append(messages, path+".shape must be line or bar")
		}
		for _, name := range []string{"x", "y"} {
			at := path + "." + name
			axis, ok := asObject(mockup.get(name))
			if !ok {
				messages = append(messages, at+" must be an object with label and unit")
				continue
			}
			for _, field := range axis.unknown("label", "unit") {
				messages = append(messages, at+"."+field+" is unknown")
			}
			if !plainText(axis.get("label")) {
				messages = append(messages, at+".label must be nonempty single-line plain text")
			}
			if unit, ok := axis.get("unit").(string); !ok || unit != "" && !printable(unit) || chars(unit) > definitionTextMaxChars {
				messages = append(messages, at+".unit must be single-line plain text (it may be empty)")
			}
		}
	}
	points, ok := mockup.get("points").([]any)
	if !ok || len(points) == 0 {
		return append(messages, path+".points must be a nonempty list")
	}
	// Each series' count of points, in the order the points first name it.
	var series []string
	counts := map[string]int{}
	intervals := 0
	for index, item := range points {
		at := fmt.Sprintf("%s.points[%d]", path, index)
		point, ok := asObject(item)
		if !ok {
			messages = append(messages, at+" must be an object")
			continue
		}
		if name := point.get("series"); plainText(name) {
			if counts[name.(string)] == 0 {
				series = append(series, name.(string))
			}
			counts[name.(string)]++
		} else {
			messages = append(messages, at+".series must be nonempty single-line plain text")
		}
		if readings {
			messages = append(messages, mockupPointFields(point, at, chart)...)
			if point.has("ci_low") || point.has("ci_high") {
				intervals++
			}
			continue
		}
		for _, field := range point.unknown("series", "x", "y") {
			messages = append(messages, at+"."+field+" is unknown")
		}
		if is(shape, "line") && !finite(point.get("x")) {
			messages = append(messages, at+".x must be a finite number for shape line")
		}
		if is(shape, "bar") && !plainText(point.get("x")) {
			messages = append(messages, at+".x must be nonempty single-line plain text for shape bar")
		}
		if !finite(point.get("y")) {
			messages = append(messages, at+".y must be a finite number")
		}
	}
	if readings {
		// The level of the points' intervals, exactly when they carry intervals: the page names it
		// under the drawing.
		level := mockup.get("ci_level")
		switch {
		case intervals > 0 && !mockup.has("ci_level"):
			messages = append(messages, path+".ci_level is required: the points carry intervals, and the drawing names their level")
		case mockup.has("ci_level") && intervals == 0:
			messages = append(messages, path+".ci_level must not be set: no point carries an interval")
		case mockup.has("ci_level") && !(finite(level) && above(level, int64Zero) && compare(level, 1.0) < 0):
			messages = append(messages, path+".ci_level must be a number above 0 and below 1, such as 0.8 for an 80% interval")
		}
	}
	if len(series) > mockupMaxSeries {
		messages = append(messages, fmt.Sprintf("%s.points holds %d series; a mock-up holds at most %d", path, len(series), mockupMaxSeries))
	}
	for _, name := range series {
		if counts[name] > mockupMaxPoints {
			messages = append(messages, fmt.Sprintf("%s.points holds %d points of series %s; a series holds at most %d",
				path, counts[name], dumps(name), mockupMaxPoints))
		}
	}
	return messages
}

// mockupPointFields checks one mock-up point in the form of a reading: series (its line, bar
// group, part or lane), x as the chart's x axis takes it (on a grid row and col in its place), y,
// and optionally n (above 0 and at least y) and ci_low with ci_high (both or neither, around y).
func mockupPointFields(point *object, at string, chart *object) []string {
	kind := chart.get("kind")
	across := objectOr(chart.get("x"))
	place := []string{"x"}
	if is(kind, "grid") {
		place = []string{"row", "col"}
	}
	var messages []string
	for _, field := range point.unknown(append([]string{"series", "y", "n", "ci_low", "ci_high"}, place...)...) {
		messages = append(messages, at+"."+field+" is unknown")
	}
	if is(kind, "grid") {
		for _, field := range place {
			if !plainText(point.get(field)) {
				messages = append(messages, at+"."+field+" must be nonempty single-line plain text: a grid point names its row and col")
			}
		}
	} else {
		// A lane chart's x is measured time, as an xy's over time is.
		form := formTime
		if !is(kind, "lanes") {
			form = xForm(kind.(string), across, chart.get("stack"))
		}
		x := point.get("x")
		steps, stepped := across.get("steps").([]any)
		_, isDate := utcDate(x)
		_, isTime := utcTime(x)
		_, isValue := xValue(form, x)
		switch {
		case form != formTime && (isDate || isTime):
			messages = append(messages, at+".x is a UTC date, but this chart's x is not time: it must be "+xValues[form])
		case !isValue:
			messages = append(messages, at+".x must be "+xValues[form])
		case stepped && !contains(steps, x):
			messages = append(messages, at+".x must be one of x.steps: each point stands at one step")
		}
	}
	y := point.get("y")
	if !finite(y) {
		messages = append(messages, at+".y must be a finite number")
	}
	if n := point.get("n"); point.has("n") && !(finite(n) && above(n, int64Zero) && (!finite(y) || compare(n, y) >= 0)) {
		messages = append(messages, at+".n must be a number above 0 and at least y: it is the total that y counts out of")
	}
	low, high := point.get("ci_low"), point.get("ci_high")
	switch {
	case point.has("ci_low") != point.has("ci_high"):
		given, missing := "ci_high", "ci_low"
		if point.has("ci_low") {
			given, missing = "ci_low", "ci_high"
		}
		messages = append(messages, fmt.Sprintf("%s.%s must be set with %s: an interval has both ends", at, missing, given))
	case point.has("ci_low"):
		if !finite(low) || !finite(high) {
			messages = append(messages, at+".ci_low and ci_high must be finite numbers")
		} else if finite(y) && !(compare(low, y) <= 0 && compare(y, high) <= 0) {
			messages = append(messages, at+".ci_low must be at or below y, and ci_high at or above it")
		}
	}
	return messages
}
