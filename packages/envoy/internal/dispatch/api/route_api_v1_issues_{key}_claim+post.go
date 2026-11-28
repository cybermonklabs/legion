package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/issues/{key}/claim",
	Auth:        authAny,
	Description: "Claim the issue for the calling session (or human): records who is working it. The status does not move. The answer is the issue with legion_managed_status, claim_changed and previous_claim beside it: legion_managed_status is true when the issue carries the legion label in any case, or sits under one of its own project that does, which means the Legion daemon owns the status; claim_changed is false where the claim already stood as asked (the caller already holds it, and keeps the time work started), so the request changed nothing; previous_claim is the claim that stood when the claim took the issue's lock, null for none. With DISPATCH_LEGION_STATUS_SCOPE on, Dispatch refuses a status write from a credential that holds neither the person scope nor legion-status (403 LEGION_STATUS_SCOPE_REQUIRED), but for one: an architect of an issue above, once its claim is ready, writes done, backlog, icebox or triage with its own broker token, whose session the daemon has published as that issue's architect. Otherwise the daemon undoes a status another writer sets. 409 ISSUE_CLAIMED names a live holder (or a human one, whose claim has no session to end); a caller whose credential holds the person scope may pass {force: true} to take it anyway, and one that does not is 403 OAUTH_SCOPE_REQUIRED. The command bar's undo of a release makes the claim conditional by an in-process condition on the request's context, never a body member: nobody - the caller included - may hold the issue when it takes the lock, else 409 CLAIM_HOLDER_CHANGED carrying the claim, nothing written (500 GOING_CONDITION_MISSING for such a write back carrying no condition). 409 CLAIM_CONTENDED means the holder changed twice while the request ran, so nothing was applied and nobody's liveness was checked: read the issue and retry.",
	Handler:     (*server).claimIssue,
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusOK, Response: claimAnswer{}, Request: claimIssueInput{},
		Refusals: []routeAnswer{{Status: http.StatusConflict, Body: issueClaimedError{}}},
	},
})
