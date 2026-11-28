package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPut, Pattern: "/issues/{key}/decisions/{block_id}/state", Auth: authAny, Versions: onlyV2,
		Description: "Record that the caller, a person, agrees with a decision as the text they read it at {state: " +
			"kept, text_sha256} (the person and approve scopes): text_sha256 is the decision's own from GET " +
			"/api/v2/issues/{key}/decisions, and a decision whose text changed since is 409 DECISION_CHANGED with " +
			"nothing written (400 INVALID_KEEP for a body without it). Answers the keep {artifact_id, block_id, " +
			"changed_version, kept_by, kept_at} and appends decision.kept, which wakes nobody; a keep of the same " +
			"text_sha256 by the person who kept it answers the keep as it stands and writes nothing, and anybody " +
			"else's moves the keep to them. 400 STATE_INVALID for any other state; {block_id} is the heading block of " +
			"a decision of the issue's decisions.md outside `## Replaced`, and any other block is 404 BLOCK_NOT_FOUND; " +
			"404 ISSUE_NOT_FOUND.",
		Handler:  s.putDecisionStateV2,
		Resource: "state", Kind: kindState,
		Body:        requestBody{Kind: bodyStateUnion, Arms: map[string]any{"kept": keepDecisionStateInput{}}},
		Shape:       routeShape{Status: http.StatusOK, Response: model.DecisionKeep{}},
		OAuthScopes: routeScopeSet{model.ScopePerson, model.ScopeApprove},
		Scope:       scopeProject, ProjectOf: projectPathIssue,
		IDs: &idRoute{Reason: "a decision's heading block id, which names a block of the issue's decisions document; " +
			"the path names no document to resolve a prefix within"},
		Refusals: documentRefusals(
			undeclaredKeyRefusal, issueMissingRefusal, blockMissingRefusal,
			apicode.Refusal{Condition: apicode.DecisionTextMoved, Code: apicode.DecisionChanged},
			apicode.Refusal{Condition: apicode.KeepWithoutDigest, Code: apicode.InvalidKeep},
		),
	}
})
