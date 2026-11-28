package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodDelete,
	Pattern:     "/api/v1/issues/{key}/claim",
	Auth:        authAny,
	Description: "Release the issue's claim: its holder, a caller whose credential holds the person scope, or anyone when the holding session is no longer live. The status does not move. The answer is the issue with legion_managed_status, claim_changed and previous_claim beside it: legion_managed_status is true when the issue carries the legion label in any case, or sits under one of its own project that does, which means the Legion daemon owns the status; claim_changed is false where nobody held the claim, so the request changed nothing; previous_claim is the claim the release found when it took the issue's lock, the one it released, null for none. With DISPATCH_LEGION_STATUS_SCOPE on, Dispatch refuses a status write from a credential that holds neither the person scope nor legion-status (403 LEGION_STATUS_SCOPE_REQUIRED), but for one: an architect of an issue above, once its claim is ready, writes done, backlog, icebox or triage with its own broker token, whose session the daemon has published as that issue's architect. Otherwise the daemon undoes a status another writer sets. The command bar's undo of a claim makes the release conditional by an in-process condition on the request's context, never a body member: the holder the turn left must hold the issue when the release takes the lock, else 409 CLAIM_HOLDER_CHANGED carrying the claim, nothing written (500 GOING_CONDITION_MISSING for such a write back carrying no condition). 409 ISSUE_CLAIMED names the holder this caller may not release (there is no force on this route); 409 CLAIM_CONTENDED means the holder changed twice while the request ran, so nothing was applied and nobody's liveness was checked.",
	Handler:     (*server).releaseIssueClaim,
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusOK, Response: claimAnswer{}, Request: actorInput{},
		Refusals: []routeAnswer{{Status: http.StatusConflict, Body: issueClaimedError{}}},
	},
})
