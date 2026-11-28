package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/issues/{key}/asks", Auth: authAny, Versions: onlyV2,
		Description: "Open a question ask on an issue {question, kind?, options?, multiple?, urgency?, anchor?}, a bearer " +
			"naming its session in actor: kind question (the default) and no other, at most 8 options with unique " +
			"labels, urgency low, med (the default), high or blocking. " +
			"Answers 201 the ask as GET /api/v2/asks/{id} reads it, with advice beside it. After the write commits, a " +
			"related-item check searches the project for the question and waits at most 5 seconds; the advice's " +
			"suggestions say whether it found related items (outcome, with the reason in missing when not made), how it " +
			"searched, and the three items most like it with each ask item's answer state, and, where the meaning " +
			"search answered, other_projects: at most 2 answered question asks of other projects. 400 INVALID_ASK for " +
			"a question it refuses, 400 ASK_KIND_INPUT for any other kind, 400 CAP_EXCEEDED past a bound, 409 ISSUE_CLOSED on a closed issue, 404 " +
			"ISSUE_NOT_FOUND.",
		Handler:  s.createIssueAskV2,
		Resource: "asks", Kind: kindCollection, Key: "{id}",
		Body: requestBody{Kind: bodyJSON, Type: createAskInput{}},
		Shape: routeShape{
			Status: http.StatusCreated, Response: askWriteV2{}, Advice: writeAdvice{},
			OtherAdvice: []any{suggestionsOnlyAdvice{}}, Refusals: anchorRefusalAnswers(),
		},
		Scope: scopeProject, ProjectOf: projectPathIssue,
		Refusals: askOpenRefusals(issueMissingRefusal, issueClosedRefusal, anchorDocumentMissingRefusal),
	}
})
