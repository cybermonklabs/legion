package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodDelete, Pattern: "/issues/{key}/claim", Auth: authAny, Versions: onlyV2,
		Description: "Release the issue's claim: its holder, a caller whose credential holds the person scope, or " +
			"anyone when the holding session is no longer live; a bearer names its session in actor. An issue nobody " +
			"holds answers the issue and writes nothing. " + issueClaimText + " 409 ISSUE_CLAIMED names the holder this " +
			"caller may not release; 404 ISSUE_NOT_FOUND.",
		Handler:  s.releaseIssueClaimV2,
		Resource: "claim", Kind: kindSingleton,
		Body:  requestBody{Kind: bodyJSON, Type: actorInput{}, Optional: true},
		Shape: routeShape{Status: http.StatusOK, Response: claimAnswer{}, Refusals: v2ClaimRefusals},
		Scope: scopeProject, ProjectOf: projectPathIssue,
		Refusals: claimCycleRefusals(),
	}
})
