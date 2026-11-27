package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/going/runs/{id}/plan/reject",
	Auth:        authAny,
	Description: fmt.Sprintf("Reject the caller's own run's waiting plan {plan}: nothing of it runs, and the run ends done with %s. Answers the rejected plan. 409 %s for a plan that is not the run's latest, 409 %s for a run or a plan that does not wait.", goingPlanRejected, codeGoingPlanNotCurrent, codeGoingPlanNotWaiting),
	Handler:     (*server).rejectGoingPlan,
	WriteClass:  scopeSession,
	ID:          &idRoute{Reason: "a going run's id, whose plan the route answers for its own author alone and answers for itself"},
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape: routeShape{
		Status: http.StatusOK, Response: goingPlanAnswer{}, Request: goingPlanRejectInput{},
	},
})
