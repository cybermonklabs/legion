package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/issues/{key}/handoffs", Auth: authBearer, Versions: onlyV2,
		Description: fmt.Sprintf("Record a session's handoff for one phase of an issue {actor, phase, body}, under a "+
			"bearer credential (a cookie caller is 403 HANDOFF_FORBIDDEN): the session names itself in actor where its "+
			"credential proves none (400 ACTOR_KIND otherwise), and a credential that proves a session may leave actor "+
			"out, an actor naming another writer then being 403 ACTOR_SESSION_MISMATCH; phase is a lowercase slug (%s), "+
			"and body is one JSON object as a string of at most "+
			"%d bytes (413 REQUEST_TOO_LARGE), stored exactly as sent; a phase string that is not such a slug, a body "+
			"string that is not one JSON object, or no body, is 400 HANDOFF_INPUT, and a phase or a body that is "+
			"neither a JSON string nor null 400 INVALID_JSON. The record is the phase's next version, from 1; a "+
			"closed issue is 409 ISSUE_CLOSED. Answers 201 {id, "+
			"issue_key, phase, version, author, created_at} and appends handoff.recorded {handoff_id, phase, version}, "+
			"which never carries the body. 404 ISSUE_NOT_FOUND.", contracts.HandoffPhasePattern, contracts.DispatchHandoffBodyMax),
		Handler:  s.createIssueHandoffV2,
		Resource: "handoffs", Kind: kindCollection, Key: "{id}",
		Body:  requestBody{Kind: bodyJSON, Type: createHandoffV2Input{}},
		Shape: routeShape{Status: http.StatusCreated, Response: model.HandoffSummary{}},
		Scope: scopeProject, ProjectOf: projectPathIssue,
		BodyLimit: handoffBodyLimit,
		Refusals: []apicode.Refusal{
			undeclaredKeyRefusal, issueMissingRefusal, issueClosedRefusal, handoffInputRefusal,
			{Condition: apicode.HandoffFromCookie, Code: apicode.HandoffForbidden},
		},
	}
})
