package api

import (
	"fmt"
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/priorities", Auth: authAny, Versions: onlyV2,
		Description: fmt.Sprintf("Every live priority, in rank order: id, title, description (markdown of any "+
			"length), owner, rank (its place in the one order), state live, and when and by whom it was made, each "+
			"beside the work that advances it (work): by_status, the open issues in each lifecycle status, claimed, "+
			"open_asks, closed_last_7_days and newest_event_at. The work under a priority is the issues that state the "+
			"relation and every issue below one of them. Beside the envelope it answers the two faults: "+
			"work_under_no_priority {count, by_project}, the open issues in flight (in_progress, testing, needs_review, "+
			"retro) that advance no live priority, and priority_work_parked {count, by_priority}, the open issues in "+
			"backlog or icebox whose own row names a live priority no issue above them carries (a child that only "+
			"inherits a priority, or restates one an issue above it carries, is no such fault), by_priority counting "+
			"each under every priority it so names; include=fault_issues adds to each its issues, the newest %d rows, each "+
			"carrying advances as the issue read does. empty is the sentence saying why the list is empty where no "+
			"priority is live. A priority belongs to no project, so any authenticated caller reads this; project=<key>, "+
			"repeatable, narrows the work counted and both faults to those projects and never the priorities "+
			"themselves. limit 0 to 200 (default 50) and offset; {items, total, limit, offset}, total every live "+
			"priority. limit=0 answers no priority and, without include=fault_issues, the two fault counts alone, with "+
			"no by_project and no by_priority, which is what the navigation rail reads.", priorityFaultRows),
		Handler:  s.listPrioritiesV2,
		Resource: "priorities", Kind: kindCollection, Key: "{id}",
		CrossProject: true,
		Query:        prioritiesV2Query{},
		Shape:        routeShape{Status: http.StatusOK, Response: prioritiesV2{}},
		List:         listBounds{Default: defaultListLimit, Max: maxListLimit, Counts: true},
		Refusals:     listRefusals(),
	}
})
