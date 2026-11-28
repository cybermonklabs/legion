package api

// envelope_v2.go is the one list envelope of /api/v2 (R1.4 of dispatch://INFRA-2946/spec): every
// list answers {items, total, limit, offset}, and takes limit and offset (pageQuery), which the
// decoder reads against the row's List bounds. A member that describes the page beside items -
// the route map's "beside" columns, such as the route index's docs - is declared on the row's
// answer type, which embeds the page:
//
//	type routeIndexV2 struct {
//		Docs string `json:"docs"`
//		page[routeIndexV2Entry]
//	}

import (
	"fmt"
	"strings"
)

// page is one page of a list (R1.4): its items, how many the whole list holds, and the limit and
// offset the page was read at. Items is never null: an empty page answers [].
type page[T any] struct {
	Items  []T `json:"items"`
	Total  int `json:"total"`
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

// logPage is one page of a list whose total is null (R5 of dispatch://INFRA-4071/spec):
// the global event log, GET /api/v2/events, which a log read forward by id would count whole on
// every page, about ten times the page for a reader near the oldest event, and the caller's
// dismissed threads, GET /api/v2/me/threads?state=dismissed, every thread they have ever
// dismissed (listMyThreadsV2), whose other states answer their exact total in it. Every other
// list, every other log among them, answers page's exact total.
type logPage[T any] struct {
	Items  []T  `json:"items"`
	Total  *int `json:"total"`
	Limit  int  `json:"limit"`
	Offset int  `json:"offset"`
}

// newPage is the page of a list a query read in the store: items the rows the query's limit and
// offset selected, total the count of the whole list, and query the page the request asked for.
func newPage[T any](items []T, total int, query pageQuery) page[T] {
	if items == nil {
		items = []T{}
	}
	return page[T]{Items: items, Total: total, Limit: query.Limit, Offset: query.Offset}
}

// pageOf is the page query asks for of items, a list held whole in memory, such as the route
// index. A query whose Limit is 0 - a catalogue read whole, whose row's List is Whole, that named
// no limit - is answered every item from its offset, at the limit of the whole list.
func pageOf[T any](items []T, query pageQuery) page[T] {
	total := len(items)
	limit := query.Limit
	if limit == 0 {
		limit = total
	}
	start := min(query.Offset, total)
	end := start + min(limit, total-start)
	return newPage(items[start:end:end], total, pageQuery{Limit: limit, Offset: query.Offset})
}

// includedPageMax is the largest page a list answers with an include member that renders each
// listed row whole, which the list's query names in its include field's `renders` tag and the
// decoder holds (holdIncludedPage): include=text and include=blocks on a list of an owner's
// artifacts, each document's text up to pmdoc.MaxDocumentBytes and its tree of blocks,
// include=body on an issue's handoffs, each body up to contracts.DispatchHandoffBodyMax, and
// include=thread on a list of asks, each thread's replies, rewordings, answers and followers. A
// page of maxListLimit could hold 200 of them at once.
const includedPageMax = 20

// includedPageDefault is the page a list with bounds reads where a request names a member that
// renders each listed row and no limit: the list's own default, held to includedPageMax, and
// includedPageMax for a catalogue read whole, whose default is every item.
func includedPageDefault(bounds listBounds) int {
	_, fallback, _ := bounds.limits()
	if fallback == 0 || fallback > includedPageMax {
		return includedPageMax
	}
	return fallback
}

// includeMembersText words members as include= values, the last two joined by conjunction:
// "include=body", "include=text or include=blocks".
func includeMembersText(members []string, conjunction string) string {
	words := make([]string, len(members))
	for index, member := range members {
		words[index] = "include=" + member
	}
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " " + conjunction + " " + words[len(words)-1]
}

// rendersEachRowText is what a refusal and the document say after members, include members that
// render each listed row: ", which renders each listed row", or of several ", which render each
// listed row".
func rendersEachRowText(members []string) string {
	if len(members) > 1 {
		return ", which render each listed row"
	}
	return ", which renders each listed row"
}

// includedPageText words the page a list with bounds takes with any of members, the members that
// render each listed row: "1 to 20 (default 20) with include=text or include=blocks".
func includedPageText(bounds listBounds, members []string) string {
	return fmt.Sprintf("1 to %d (default %d) with %s", includedPageMax, includedPageDefault(bounds), includeMembersText(members, "or"))
}

// includedPageLimitText is what the description of a list says of its page, from bounds, the row's
// List, and query, the row's Query, whose include names members that render each listed row
// (`renders`), as the decoder reads them: the list's own limit and the page with those members,
// "limit 1 to 200 (default 50), and 1 to 20 (default 20) with include=text or include=blocks, and
// offset;" (TestEveryListThatRendersItsRowsSaysItsPage). It panics on a query the decoder cannot
// read, and on one whose include renders no row: such a list has no page with those members to word.
func includedPageLimitText(bounds listBounds, query any) string {
	schema, err := compileV2Query(query)
	if err != nil {
		panic(fmt.Sprintf("includedPageLimitText(%T): %v", query, err))
	}
	if schema.rendering == nil {
		panic(fmt.Sprintf("includedPageLimitText(%T): its query names no include member that renders each listed row (renders)", query))
	}
	least, fallback, most := bounds.limits()
	return fmt.Sprintf("limit %d to %d (default %d), and %s, and offset;", least, most, fallback,
		includedPageText(bounds, schema.rendering.renders))
}
