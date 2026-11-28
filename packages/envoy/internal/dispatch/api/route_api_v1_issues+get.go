package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues",
	Auth:        authAny,
	Description: fmt.Sprintf("List issues; filters project, status (repeatable: the issues standing in any of those statuses; a blank value names none, and a value is matched as written, so a status the lifecycle does not hold matches no issue: alone it answers none, and beside another status it adds nothing), parent, label, priority (repeatable: 0-3, or none for unset), advances (repeatable: a priority id, none for the work that advances no live priority or any for the work that advances one, own or inherited; several are a union), own (true keeps, of the issues the advances values answer, those whose own row names a priority a value matches and no issue above them carries it, so a child that only inherits a priority or restates one an issue above it carries is left out and none keeps nothing; without an advances value it filters nothing; advances=any&own=true over backlog and icebox is the second fault of GET /api/v1/priorities), open, updated_since, closed_since and closed_before (RFC3339; the issues closed at or after closed_since and before closed_before, so an open issue matches neither), route_status (live, no_holder or unknown; open issues only); ?pinned=true needs the person scope, since the pins are one person's. A malformed updated_since, closed_since or closed_before is 400 INVALID_UPDATED_SINCE, INVALID_CLOSED_SINCE or INVALID_CLOSED_BEFORE, and a closed_before at or before closed_since 400 INVALID_CLOSED_WINDOW. Each issue carries closed_at, when it was closed or null while it is open, and progress: {tasks, children}, its spec's task-list items and its direct children as {done, total}, each null when it has none. ?moved=true adds moved_at and moved_by to each issue standing in backlog or icebox: when it was moved there and who moved it, read from its newest issue.updated event that set one of the two statuses, as the second fault of GET /api/v1/priorities reads them; an issue standing elsewhere, or whose events do not reach back to the move, carries neither, and a listing without moved=true carries neither on any issue. Pages with limit (1-%d) and offset (0 or more; alone it pages %d): a paged answer is {issues, total, limit, offset}, total counting every issue the filters match; without either it is every matching issue as an array. cursor is 400 INVALID_QUERY.", contracts.MaxIssuePageLimit, contracts.DefaultIssuePageLimit),
	Handler:     (*server).listIssues,
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route lists the issues that existed then, each as it stood, under the same filters, order and pages as now. At a cut, from as_of or from a token, route_status and pinned are 400 " + contracts.DispatchAsOfNotServed + " whatever their value, because the route roster and a person's pins keep no history."},
	Shape: routeShape{
		Status: http.StatusOK, Response: []model.IssueSummary{}, Params: issueListQuery{},
		// A request naming limit or offset is answered the page wrapper instead
		// (issue_page.go).
		Also: []routeAnswer{{Status: http.StatusOK, Body: model.IssueSummaryPage{}}},
	},
})
