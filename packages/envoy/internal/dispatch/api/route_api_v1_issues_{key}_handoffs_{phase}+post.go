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
	Description: fmt.Sprintf("Record a session's handoff for one phase of an issue {actor, body}: the session names itself in actor (400 ACTOR_KIND otherwise; a cookie caller is 403 HANDOFF_FORBIDDEN), phase is a lowercase slug (%s, 400 HANDOFF_INPUT otherwise), and body is one JSON object as a string (400 HANDOFF_INPUT otherwise) of at most %d bytes (413 CAP_EXCEEDED), stored exactly as sent. The record is the phase's next version, from 1; a closed issue is 409 ISSUE_CLOSED. A test handoff first waits until the check states its issue's due marks owe are published, and each former head of its pull requests not read since such a mark's change has been read, and stores the issue's check states serial then, check_states_serial, so each check.state_changed event the tester's results caused has a serial at or below it; past %s it is 503 CHECK_STATES_UNSETTLED and records nothing. A read that no retry answers (no GitHub App key, no installation, a repository GitHub refuses the installation) leaves nothing owed, so it holds no test handoff: that holds for the read of which commits are a pull request's and for the read of the base revision a row of it stores none of, which Dispatch makes and stores to read those commits. Answers 201 {id, issue_key, phase, version, author, created_at, check_states_serial?} and appends handoff.recorded {handoff_id, phase, version}, which never carries the body.", contracts.HandoffPhasePattern, contracts.DispatchHandoffBodyMax, checkStatesProduction.fenceWait),
	Handler:     (*server).createHandoff,
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusCreated, Response: model.HandoffSummary{}, Request: createHandoffInput{},
	},
})
