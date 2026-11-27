package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/priorities",
	Auth:        authAny,
	Description: fmt.Sprintf("Every live priority, in rank order: id, title, description (markdown of any length), owner, rank (its place in the one order), state (live; a deleted priority is gone and is not listed), and when and by whom it was made, each beside the work that advances it (work): by_status, the open issues in each lifecycle status, claimed, open_asks, closed_last_7_days and newest_event_at. The work under a priority is the issues that state the relation and every issue below one of them. Beside the list it answers the two faults: work_under_no_priority, the open issues in flight (in_progress, testing, needs_review, retro) that advance no live priority, and priority_work_parked, the open issues in backlog or icebox whose own row names a live priority no issue above them carries (a child that only inherits a priority, or restates one an issue above it carries, is no such fault), each as a whole count, the same count by project or by each priority so named, and the newest %d rows. Each row carries advances as the issue read does, one {id, inherited} entry per live priority the issue advances, inherited true for one it takes from an issue above it; a row of work_under_no_priority carries the empty list. A priority belongs to no project, so this read is of the whole deployment and any authenticated caller makes it, the shared token every pod carries included. ?project=<KEY>, repeatable, narrows the work counted and both fault lists to those projects and never the priorities themselves; ?summary=true answers the two fault counts alone, with no priority row and no fault row, which is what the navigation rail reads.", priorityFaultRows),
	Handler:     (*server).listPriorities,
	Successor:   successorIs(http.MethodGet, "/priorities"),
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		Status: http.StatusOK, Response: model.Priorities{}, Params: prioritiesQuery{},
	},
})
