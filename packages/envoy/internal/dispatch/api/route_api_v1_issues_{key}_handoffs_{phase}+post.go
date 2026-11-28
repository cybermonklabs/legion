package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/issues/{key}/handoffs/{phase}",
	Auth:        authBearer,
	Description: fmt.Sprintf("Record a session's handoff for one phase of an issue {actor, body}: the session names itself in actor (400 ACTOR_KIND otherwise; a cookie caller is 403 HANDOFF_FORBIDDEN), phase is a lowercase slug (%s, 400 HANDOFF_INPUT otherwise), and body is one JSON object as a string (400 HANDOFF_INPUT otherwise) of at most %d bytes (413 CAP_EXCEEDED), stored exactly as sent. The record is the phase's next version, from 1; a closed issue is 409 ISSUE_CLOSED. Answers 201 {id, issue_key, phase, version, author, created_at} and appends handoff.recorded {handoff_id, phase, version}, which never carries the body.", contracts.HandoffPhasePattern, contracts.DispatchHandoffBodyMax),
	Handler:     (*server).createHandoff,
	Successor:   successorIs(http.MethodPost, "/issues/{key}/handoffs"),
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusCreated, Response: model.HandoffSummary{}, Request: createHandoffInput{},
	},
})
