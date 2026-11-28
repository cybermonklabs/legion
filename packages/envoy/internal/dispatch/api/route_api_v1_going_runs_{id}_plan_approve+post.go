package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/going/runs/{id}/plan/approve",
	Auth:        authAny,
	Description: fmt.Sprintf("Approve the caller's own run's waiting plan {plan, rows?: [{row, tick, values?}], version?}: each named row's tick, where false leaves the row out and every row that needs it, and values, arguments of the row's tool put over the arguments the plan shows; a waiting row the body does not name is approved as shown. version is the plan's version as the approving page read it: 409 %s, nothing written, where the plan's version moved since, as when another page read the plan again and its rows show another value. Each ticked row is read again as the caller with its values: one whose item changed after the plan showed - another value now, or another holder of its issue's claim - shows the new value and waits for approval again, changed; every other is approved over that read. The approval moves the run's grant to the caller's own sign-in or personal token (403 %s for any other credential), is saved as a step of the run, and makes the run ready, whose agent runs the approved rows in order, each read again before its write: a row whose item changed since waits again, and one whose needed row failed is skipped. Answers the plan as GET answers it. 409 %s, nothing written, for the plan of a run another writer's delete withdrew (GET /api/v1/going/runs/{id}), 409 %s for a plan that is not the run's latest, 409 %s for a run or a plan that does not wait - a run its own deletes alone withdrew reads stopped, so its plan among them - 409 %s, nothing written, for a ticked row whose comment or issue message its author deleted after the plan read it, which the person unticks to approve the rest, 400 %s for a row that is no waiting row of the plan, values its tool's schema refuses, or a value of \"$N\" the row may not hold - one that names no new issue of the plan the row waits for, one in the argument that names the row's own item, or one inside an object - in words that name the rows as the plan does and say which values the field takes, 400 %s for a row with no tick, and for an approval that leaves out every waiting row, with nothing written: an approval approves at least one row, and a reject ends a plan with nothing done.", codeGoingPlanChanged, codeRunNeedsPersonCredential, codeGoingRunWithdrawn, codeGoingPlanNotCurrent, codeGoingPlanNotWaiting, codePostDeleted, codeGoingPlanValues, codeGoingPlanInput),
	Handler:     (*server).approveGoingPlan,
	WriteClass:  scopeSession,
	ID:          &idRoute{Reason: "a going run's id, whose plan the route answers for its own author alone and answers for itself"},
	OAuthScopes: routeScopeSet{model.ScopePerson, model.ScopeApprove},
	Shape: routeShape{
		Status: http.StatusOK, Response: goingPlanAnswer{}, Request: goingPlanApproveInput{},
	},
})
