package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPut, Pattern: "/issues/{key}/claim", Auth: authAny, Versions: onlyV2,
		Description: "Claim the issue for the calling session or person {force?}, a bearer naming its session in actor: " +
			"records who is working it. The holder's own claim answers the issue and writes nothing, the claim keeping " +
			"the time work started. " + issueClaimText + " 409 ISSUE_CLAIMED names a live holder (or a person, whose " +
			"claim has no session to end); force true takes it anyway, which needs a credential holding the person scope " +
			"(403 OAUTH_SCOPE_REQUIRED otherwise). 409 ISSUE_CLOSED on a closed issue, 404 ISSUE_NOT_FOUND.",
		Handler:  s.claimIssueV2,
		Resource: "claim", Kind: kindSingleton,
		Body:  requestBody{Kind: bodyJSON, Type: claimIssueInput{}},
		Shape: routeShape{Status: http.StatusOK, Response: claimAnswer{}, Refusals: v2ClaimRefusals},
		Scope: scopeProject, ProjectOf: projectPathIssue,
		Refusals: claimCycleRefusals(
			issueClosedRefusal,
			apicode.Refusal{Condition: apicode.ForceWithoutPersonScope, Code: apicode.OAuthScopeRequired},
		),
	}
})
