package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/me/going-plans",
	Auth:        authAny,
	Description: "The caller's own command-bar runs whose latest plan waits for them, while History lists their conversation, newest plan first, and nobody else's, which the Inbox shows: each row is {run, conversation, words, plan, summary, waiting (how many of its rows wait), created_at, version}, version the plan's as GET /api/v1/going/runs/{id}/plan answers it, which an approval of the plan names (409 GOING_PLAN_CHANGED once the plan has moved on from it); the plan itself is GET /api/v1/going/runs/{id}/plan, where the person answers it. A withdrawn run (GET /api/v1/going/runs/{id}) is not listed, whoever withdrew it: nobody may approve its plan, and its summary may quote the deleted post; a run its own deletes alone withdrew reads stopped, and no ended run is listed. Each change of what it lists appends going_plan.updated {login}, which wakes nobody. Pages with limit (1-200, default 50) and offset; {rows, total, limit, offset}.",
	Handler:     (*server).listMyGoingPlans,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: goingWaitingPlansPage{}, Params: myAnswersQuery{}},
})
