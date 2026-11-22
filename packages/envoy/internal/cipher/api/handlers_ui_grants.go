// handlers_ui_grants.go: GET /v1/grants, POST /v1/grants/{id}/revoke-by-approver — a person's own
// live-grant list and human revocation route. Part of the routes a listed service calls with its
// own ServiceAccount token (accountAuth), relaying a person's own page: the token proves which
// service is calling, and the person is the one the service names.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/wire"
)

// approverGrantsResponse is GET /v1/grants's answer: wire.ApproverGrantsResponse itself, named
// here because the broker's generated HTTP reference documents a route's answer from a type this
// package declares, as introspectionResponse names oauthwire.Introspection
// (handlers_introspect.go).
type approverGrantsResponse = wire.ApproverGrantsResponse

// listGrantsForApprover lists the live grants of the person ?approver= names (writeApproverGrants).
func (s *server) listGrantsForApprover(w http.ResponseWriter, r *http.Request, _ string) {
	approver := r.URL.Query().Get("approver")
	if !requireApprover(w, approver) {
		return
	}
	s.writeApproverGrants(w, r, approver)
}

// writeApproverGrants answers every live grant of a session approver operates, automatic or
// approved by anyone, and every grant approver approved on anyone's session
// (requests.Machine.GrantsForApprover). Dispatch's Live grants page and the operator's own grant
// list both answer through it, so the two agree.
func (s *server) writeApproverGrants(w http.ResponseWriter, r *http.Request, approver string) {
	rows, err := s.deps.Machine.GrantsForApprover(r.Context(), approver)
	if err != nil {
		writeInternal(w, "list grants for approver", err)
		return
	}
	out := make([]wire.ApproverGrantResp, len(rows))
	for i, g := range rows {
		names := g.Names
		if names == nil {
			names = []string{}
		}
		out[i] = wire.ApproverGrantResp{
			GrantID: g.GrantID, Granted: g.Granted, RecordID: g.RecordID, Enrollment: enrollmentResp(g.Enrollment),
			Names: names, Approver: strPtr(g.Approver), DecidedBy: strPtr(g.DecidedBy),
			ExpiresAt: g.ExpiresAt, CreatedAt: g.CreatedAt,
		}
	}
	writeJSON(w, http.StatusOK, approverGrantsResponse{Grants: out})
}

// revokeByApproverBody is {"approver"}, the body of both revoke-by-approver routes (a grant's and a
// launcher credential's): the revoking person's login, which Dispatch's server sets from its own
// session.
type revokeByApproverBody struct {
	// The login of the person revoking: for a grant, its approver or its session's operator; for a
	// person's machine login, the person who approved it; for a person's pod login, the person
	// BROKER_POD_OPERATORS maps the pod's account to; for a service's machine login, anyone signed
	// in.
	Approver string `json:"approver"`
}

// revokeByApprover ends a grant as the person the caller names (revokeGrantAs), logging the caller
// that relayed it beside that person. The caller is a named service identity, while the person's
// name is still taken on the caller's word: a revoke only takes power away, unlike a secret
// request's approval, which only the broker's own page makes.
func (s *server) revokeByApprover(w http.ResponseWriter, r *http.Request, callerName string) {
	id, ok := pathUUID(w, r, "id", "GRANT_ID_INPUT", "grant")
	if !ok {
		return
	}
	var body revokeByApproverBody
	if !readJSON(w, r, &body, "INVALID_REVOKE") {
		return
	}
	if !requireApprover(w, body.Approver) {
		return
	}
	// The approver is the caller's word, of any length requireApprover takes, so the line cuts it
	// (answer.ClipFields).
	slog.Info("grant revocation relayed", answer.ClipFields([]any{"caller", callerName, "approver", body.Approver, "grant", id})...)
	s.revokeGrantAs(w, r, id, body.Approver, record.HumanActor(body.Approver))
}

// revokeGrantAs ends grant id as login, recording actor on every row it writes
// (requests.Machine.RevokeByApprover): login must be the grant's approver, the person its
// enrollment acts for (operatorOf), or, for a grant the model judge approved in anyone's stead,
// anyone whom the grant's private project, if it has one, lets see it (mayRevoke). The approver
// and that person end it whatever Dispatch answers of its project. Anyone else whom the project
// hides the grant from is answered 404 exactly as for an id no grant has, and a grant Dispatch
// could not answer for is 503 PROJECT_ACCESS_UNAVAILABLE to them; a grant they may see that names
// them nowhere is 403 NOT_APPROVER, and one that moved to another box each time the revoke read it
// is 409 GRANT_MOVING; each revokes nothing. A person Dispatch calls a restricted member is no
// approver of a secret, and ends a grant only as the person its enrollment acts for: one the grant
// admits only as an approver is 403 NOT_APPROVER, saying why, revoking nothing. While Dispatch does
// not answer what kind of member the person is, only the person the grant names as its approver
// ends it as an approver, since a revoke only takes access away; every revoke through a group or
// as anyone then, whatever session holds the grant, and every revoke while Dispatch answers
// anything else the broker cannot read, is 503 MEMBER_KIND_UNCHECKABLE, revoking nothing. A grant
// on a person's session can still be ended then by that person, its operator, who asks Dispatch
// nothing; a grant on a session that acts for no person (a service's pod, a person's pod the
// broker no longer maps, or a machine's own session) is ended through the broker by nobody but its
// named approver until Dispatch answers. A revoke whose caller hung up after the member-kind read
// is 503 REQUEST_ENDED, writes no line and is counted cancelled, as one that hung up during the
// read is. When the operator revokes a grant the session got without asking, its secrets are
// withheld from that session, even when the grant had already ended: its other grants that got
// them without asking end too, and its later requests for them ask their owner.
func (s *server) revokeGrantAs(w http.ResponseWriter, r *http.Request, id, login, actor string) {
	if err := s.deps.Machine.RevokeByApprover(r.Context(), id, login, actor); err != nil {
		writeRevokeRefusal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, stateResponse{State: "revoked"})
}

// grantMoving is the one sentence a 409 GRANT_MOVING answers with.
const grantMoving = "the grant moved to another box each time this revoke read it, so nothing was revoked; run the same revoke again"

// writeRevokeRefusal answers the error RevokeByApprover refused a revoke with, err not nil.
func writeRevokeRefusal(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such grant")
	case errors.Is(err, requests.ErrProjectAccessUnavailable):
		// Dispatch could not say whether the person may see the grant's project, so nothing was
		// revoked; the same revoke succeeds once Dispatch answers.
		writeProjectAccessUnavailable(w, "revoke grant", err)
	case errors.Is(err, requests.ErrNotApprover):
		writeError(w, http.StatusForbidden, "NOT_APPROVER", err.Error())
	case writeMemberKindUncheckable(w, "the revoke", err):
		// The grant admits the person only as an approver, and the broker could not read what kind
		// of member they are: Dispatch answered what the broker cannot read, the caller went away,
		// or Dispatch did not answer and the grant does not name the person as its approver.
		// Nothing was revoked.
	case errors.Is(err, requests.ErrGrantMoving):
		// Adoptions kept moving the grant between the person rule and the transaction, so the
		// revoke decided nothing; it succeeds once the grant stays on one session.
		slog.Warn("broker: revoke grant: the grant kept moving", "error", err)
		writeError(w, http.StatusConflict, "GRANT_MOVING", grantMoving)
	case errors.Is(err, context.Canceled):
		// The caller hung up, and a read or the transaction after the member-kind read failed on
		// the request's own cancellation: that says nothing of the broker or of Dispatch, so it
		// writes no line and is counted cancelled, as a hang-up during that read is
		// (writeMemberKindUncheckable).
		answer.FromWriter(w).NoteCallerEnded()
		writeError(w, http.StatusServiceUnavailable, "REQUEST_ENDED", "the request ended before the revoke finished; send it again")
	default:
		writeInternal(w, "revoke grant", err)
	}
}
