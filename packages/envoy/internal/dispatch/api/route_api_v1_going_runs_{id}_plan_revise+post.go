package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/going/runs/{id}/plan/revise",
	Auth:        authAny,
	Description: fmt.Sprintf("Ask, in words, for another plan in place of the caller's own run's waiting plan {plan, words}: the plan is rejected and nothing of it runs, and the words, at most %d characters (400 %s), are saved as a step of the run beside the plan's rows, which the run's model reads when it proposes the next plan. Like an approval it moves the run's grant to the caller's credential, and makes the run ready. Answers the rejected plan. 409 %s, nothing written, for the plan of a run another writer's delete withdrew (GET /api/v1/going/runs/{id}), 409 %s for a plan that is not the run's latest, 409 %s for a run or a plan that does not wait, a run its own deletes alone withdrew among them.", goingReviseMostRunes, codeGoingPlanInput, codeGoingRunWithdrawn, codeGoingPlanNotCurrent, codeGoingPlanNotWaiting),
	Handler:     (*server).reviseGoingPlan,
	WriteClass:  scopeSession,
	ID:          &idRoute{Reason: "a going run's id, whose plan the route answers for its own author alone and answers for itself"},
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape: routeShape{
		Status: http.StatusOK, Response: goingPlanAnswer{}, Request: goingPlanReviseInput{},
	},
})
