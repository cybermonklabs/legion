package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:  http.MethodGet,
	Pattern: "/api/v1/issues/{key}/asks",
	Auth:    authAny,
	Description: "List an issue's asks, each with its newest reply; ?state= filters, and ?thread=1 adds each ask's thread (replies, rewordings, answers, followers, claimant, route, route_holder). " + threadPageDescription(
		"An ask's thread is the ask, each comment whose ask_id names it, and the answer and the resolution the ask holds now (a person's decline among them), each written by its actor at its own at; an answer or a resolution a later one replaced is no entry. Its row is the list's row with last_activity_at, and ?state= and ?thread=1 filter and shape the page as they filter and shape the list.",
		"The route takes no ?person=."),
	Handler: (*server).listIssueAsks,
	Read:    readEntry{Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route lists the asks that existed then, each at its newest state event at or before the cut, with its newest reply at the cut. ?thread=1 gives each thread as it stood at the cut. " + asOfThreadPageAtACut},
	Shape: routeShape{
		Status: http.StatusOK, Response: []issueAskRow{}, Params: issueAsksQuery{},
		Also: []routeAnswer{{Status: http.StatusOK, Body: askThreadPage{}}},
	},
})
