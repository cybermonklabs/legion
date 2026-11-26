package charts

import (
	"fmt"
	"strings"
)

// A number declaration is the page's headline and its small numbers: tiles in a row at the top,
// and sections of small numbers among the charts. Each small number is the newest reading of one
// of its series, and a series draws once.

const (
	tilesMax             = 8
	tileLabelMaxChars    = 140
	sectionsMax          = 4
	sectionTitleWords    = 8
	sectionSubtitleWords = 60
)

// numberFields checks a number's series and adapter. The platform requires its approved too.
func numberFields(chart *object, path string) []string {
	var messages []string
	series, ok := chart.get("series").([]any)
	valid := ok && len(series) >= 1 && len(series) <= 8
	for _, name := range series {
		valid = valid && matches(seriesName, name)
	}
	if !valid || !distinctNames(series) {
		messages = append(messages, path+".series must list 1 to 8 distinct series names matching "+seriesPattern)
	}
	if chart.has("adapter") && !isOneOf(chart.get("adapter"), adapters...) {
		messages = append(messages, fmt.Sprintf("%s.adapter must name an adapter of loaders/adapters.json (%s); %s is not one",
			path, strings.Join(adapters, ", "), dumps(chart.get("adapter"))))
	}
	return messages
}

// tilesFields checks a row of small numbers, a number's or a section's: each the newest reading of
// one of the declaration's series (declared), with a label and, as of, its whole. seen holds the
// series the declaration's tiles and counts drew before.
func tilesFields(value any, at string, declared, seen map[string]bool) []string {
	tiles, ok := value.([]any)
	if !ok || len(tiles) < 1 || len(tiles) > tilesMax {
		return []string{fmt.Sprintf("%s must list 1 to %d objects with series and label", at, tilesMax)}
	}
	var messages []string
	for index, item := range tiles {
		place := fmt.Sprintf("%s[%d]", at, index)
		tile, ok := asObject(item)
		if !ok {
			messages = append(messages, place+" must be an object with series and label")
			continue
		}
		for _, field := range tile.unknown("series", "label", "of", "state", "signed", "count") {
			messages = append(messages, place+"."+field+" is unknown")
		}
		series, isText := tile.get("series").(string)
		switch {
		case !isText || !declared[series]:
			messages = append(messages, place+".series must be a series that this declaration's series lists")
		case seen[series]:
			messages = append(messages, place+".series must not repeat: a series has at most one tile")
		default:
			seen[series] = true
		}
		if tile.has("of") {
			// A tile's of is n (its reading's own n) or another series of this declaration.
			of, ofText := tile.get("of").(string)
			if of != "n" && (!ofText || !declared[of]) {
				messages = append(messages, place+".of must be n or a series that this declaration's series lists")
			} else if equal(tile.get("of"), tile.get("series")) {
				messages = append(messages, place+".of must be another series than the tile's own")
			}
		}
		if tile.has("state") && !isOneOf(tile.get("state"), states...) {
			messages = append(messages, place+".state must be "+statesText)
		}
		// signed true prints a leading + before a positive value of the small number.
		if _, ok := tile.get("signed").(bool); tile.has("signed") && !ok {
			messages = append(messages, place+".signed must be a boolean")
		}
		// count true prints the reading's n after the small number: "from 90 tasks".
		if tile.has("count") {
			if count, ok := tile.get("count").(bool); !ok {
				messages = append(messages, place+".count must be a boolean")
			} else if count && tile.has("of") {
				messages = append(messages, place+".count must not be set with of: of prints the count as a total")
			}
		}
		if !plainTextOf(tile.get("label"), tileLabelMaxChars) {
			messages = append(messages, fmt.Sprintf("%s.label must be nonempty single-line plain text of at most %d characters",
				place, tileLabelMaxChars))
		}
	}
	return messages
}

// countsFields checks a section's counts: one count for each row of one series of the
// declaration, in the rows' order, each with its reading's own n as its total with of n.
func countsFields(value any, at string, declared, seen map[string]bool) []string {
	counts, ok := asObject(value)
	if !ok {
		return []string{at + " must be an object with series"}
	}
	var messages []string
	for _, field := range counts.unknown("series", "of", "order") {
		messages = append(messages, at+"."+field+" is unknown")
	}
	series, isText := counts.get("series").(string)
	switch {
	case !isText || !declared[series]:
		messages = append(messages, at+".series must be a series that this declaration's series lists")
	case seen[series]:
		messages = append(messages, at+".series must not repeat: a series draws in one tile or in one section's counts")
	default:
		seen[series] = true
	}
	if counts.has("of") && !is(counts.get("of"), "n") {
		messages = append(messages, at+".of must be n: each count's total is its reading's own n")
	}
	// The rows that order names draw first, in its order.
	if counts.has("order") && !plainList(counts.get("order"), 1, orderMax) {
		messages = append(messages, fmt.Sprintf("%s.order must list 1 to %d distinct single-line row names", at, orderMax))
	}
	return messages
}

// sectionsFields checks a number's sections: each a titled group of small numbers that stands
// among the charts, after the approved chart its after names (charted holds the ids of the
// list's approved declarations that draw a chart). A section owns its tiles' series, their
// totals and its counts' series, and a series a section owns never draws at the page's top: it
// is never the declaration's first series, the headline's, nor a series of its top tiles or
// their totals.
func sectionsFields(chart *object, path string, declared, seen, charted map[string]bool) []string {
	sections, ok := chart.get("sections").([]any)
	if !ok || len(sections) < 1 || len(sections) > sectionsMax {
		return []string{fmt.Sprintf("%s.sections must list 1 to %d objects with title, after and tiles", path, sectionsMax)}
	}
	var first string
	hasFirst := false
	if names, ok := chart.get("series").([]any); ok && len(names) > 0 {
		first, hasFirst = names[0].(string)
	}
	topTiles := listOr(chart.get("tiles"))
	top := map[string]bool{}
	for _, tile := range topTiles {
		if tile, ok := asObject(tile); ok {
			if series, ok := tile.get("series").(string); ok {
				top[series] = true
			}
		}
	}
	if hasFirst {
		top[first] = true
	}
	owned := map[string]bool{}
	var messages []string
	for index, item := range sections {
		at := fmt.Sprintf("%s.sections[%d]", path, index)
		section, ok := asObject(item)
		if !ok {
			messages = append(messages, at+" must be an object with title, after and tiles")
			continue
		}
		for _, field := range section.unknown("title", "subtitle", "after", "tiles", "counts") {
			messages = append(messages, at+"."+field+" is unknown")
		}
		if !lineOf(section.get("title"), sectionTitleWords) {
			messages = append(messages, fmt.Sprintf("%s.title must be one line of plain text of at most %d words: it is the "+
				"section's title", at, sectionTitleWords))
		}
		if section.has("subtitle") && !lineOf(section.get("subtitle"), sectionSubtitleWords) {
			messages = append(messages, fmt.Sprintf("%s.subtitle must be one line of plain text of at most %d words", at, sectionSubtitleWords))
		}
		if after, ok := section.get("after").(string); !ok || !charted[after] {
			messages = append(messages, at+".after must be the id of an approved declaration of this eval that draws a chart: "+
				"the section stands after it")
		}
		tiles := section.get("tiles")
		messages = append(messages, tilesFields(tiles, at+".tiles", declared, seen)...)
		if section.has("counts") {
			messages = append(messages, countsFields(section.get("counts"), at+".counts", declared, seen)...)
		}
		type drawing struct {
			field  string
			series any
		}
		var drawn []drawing
		for place, tile := range listOr(tiles) {
			if tile, ok := asObject(tile); ok {
				drawn = append(drawn, drawing{fmt.Sprintf("%s.tiles[%d].series", at, place), tile.get("series")})
			}
		}
		counts, hasCounts := asObject(section.get("counts"))
		if hasCounts {
			drawn = append(drawn, drawing{at + ".counts.series", counts.get("series")})
		}
		for _, each := range drawn {
			if hasFirst && is(each.series, first) {
				messages = append(messages, fmt.Sprintf("%s must not be %s, this declaration's first series: it is the headline's",
					each.field, first))
			}
		}
		for place, item := range listOr(tiles) {
			var total any
			tile, isTile := asObject(item)
			if isTile {
				total = tile.get("of")
			}
			name, isText := total.(string)
			if isText && name != "n" && top[name] {
				messages = append(messages, fmt.Sprintf("%s.tiles[%d].of must not be %s: the page draws that series at its top, "+
					"and a section's total is its section's own", at, place, name))
			}
			if isTile {
				if series, ok := tile.get("series").(string); ok {
					owned[series] = true
				}
			}
			if isText && name != "n" {
				owned[name] = true
			}
		}
		if hasCounts {
			if series, ok := counts.get("series").(string); ok {
				owned[series] = true
			}
		}
	}
	// A top tile's total that a section owns would draw that series at the top as well as in its
	// section.
	for place, item := range topTiles {
		if tile, ok := asObject(item); ok {
			if total, ok := tile.get("of").(string); ok && total != "n" && owned[total] {
				messages = append(messages, fmt.Sprintf("%s.tiles[%d].of must not be %s: a section of this declaration draws that "+
					"series, and a series that a section owns never draws at the page's top", path, place, total))
			}
		}
	}
	return messages
}
