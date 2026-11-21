// handlers_ui_records.go: POST /v1/credential-requests/{record}/approve and .../deny, the two
// routes that decide a machine login; .../dismiss, which takes an expired record of either kind
// off every list; and the record shape POST /v1/machine-logins/lookup answers. A machine login
// is decided on the caller's word by a listed service calling with its own Kubernetes
// ServiceAccount token: the token proves which service is calling, and the service names the
// deciding person in the body's approver field, who must be one the record's rule admits
// (record.MayDecide) and must type the login's code.
// Each such decision, and each dismissal, is logged with the caller that relayed it and the
// approver it named. A secret request is never decided here, by any caller: it is decided only
// on the broker's own approval page (internal/cipher/approval), by a person signed in to the
// broker, and the two decide routes refuse it naming that page once the caller is authenticated,
// to a person the record's project lets see it. No record is read for a caller the broker has not
// authenticated, so such a caller learns nothing of which record ids exist, and a person a private
// project hides a record from reads of it what they read of an id no record has. A service's
// machine login, which only a broker before token logins opened, may only be denied: approving it
// is 400 SERVICE_LOGS_IN_WITH_TOKEN.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/machine"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/wire"
)

func sessionResp(s requests.Session) wire.RecordSessionResp {
	return wire.RecordSessionResp{Request: strPtr(s.Request), Enrollment: strPtr(s.Enrollment)}
}

func enrollmentResp(e record.Enrollment) wire.RecordEnrollmentResp {
	return wire.RecordEnrollmentResp{Kind: e.Kind, RuntimeID: e.RuntimeID, Operator: e.Operator, Slot: strPtr(e.Slot)}
}

// recordResponse is a credential-request record as POST /v1/machine-logins/lookup answers it:
// wire.RecordResponse itself, named here because the broker's generated HTTP reference documents
// a route's answer from a type this package declares, as introspectionResponse names
// oauthwire.Introspection (handlers_introspect.go).
type recordResponse = wire.RecordResponse

// buildRecordResponse builds the record shape POST /v1/machine-logins/lookup answers.
func buildRecordResponse(detail requests.RecordDetail) recordResponse {
	resp := recordResponse{
		RecordID: detail.RecordID, Kind: detail.Kind, State: detail.State, Approver: detail.Approver,
		Identifiers: detail.Identifiers, Reason: detail.Reason, LifetimeSeconds: detail.LifetimeSeconds,
		RulesVersion: detail.RulesVersion, ExpiresAt: detail.ExpiresAt, RequestedAt: detail.RequestedAt,
		Session: sessionResp(detail.Session),
	}
	if detail.Enrollment != nil {
		enr := enrollmentResp(*detail.Enrollment)
		resp.Enrollment = &enr
	}
	resp.Service = strPtr(detail.Service)
	if detail.Decided != nil {
		resp.Decided = &wire.RecordDecisionResp{Event: detail.Decided.Event, At: detail.Decided.At, CredentialID: strPtr(detail.Decided.CredentialID)}
	}
	return resp
}

// decideBody is both approve's and deny's request body for a machine login, {"approver", "code"}.
// approver is the deciding person's login, which Dispatch's server sets from its own session, and
// code the login's confirmation code: machine.Service.ApplyDecision checks it on both approve and
// deny.
type decideBody struct {
	// The login of the person deciding, which the record's rule must admit: its approver, or
	// anyone signed in for the denial of a service's machine login.
	Approver string `json:"approver"`
	// The machine login's confirmation code, typed again.
	Code *string `json:"code"`
}

// decisionResponse is what a machine login's approval answers: wire.DecisionResponse itself,
// named here for the same reason recordResponse is.
type decisionResponse = wire.DecisionResponse

func (s *server) approveRecord(w http.ResponseWriter, r *http.Request, callerName string) {
	s.decideRecord(w, r, true, callerName)
}

func (s *server) denyRecord(w http.ResponseWriter, r *http.Request, callerName string) {
	s.decideRecord(w, r, false, callerName)
}

// decideRecord refuses a secret request and decides a machine login, for a listed service only.
// Its two rows are accountAuth rows, so before it runs authenticate has spent the source
// address's bucket (DefaultAccountLimit), verified the caller's ServiceAccount token against
// BROKER_API_ACCOUNTS and checked that the caller's name carries relaysDecisions: a caller with no
// token, or with one the broker does not accept, gets that refusal before any record is read, the
// same refusal for an id that names a record as for one no record has, so the decide routes tell
// no such caller which record ids exist. Only then is the record's kind read. A record id no
// record has answers 404. A secret request is refused (refuseSecretRequestDecision): 404, as an id
// no record has, to a person its private project hides it from, then 403 JUDGE_DECIDES while the
// model judge holds it, and otherwise 403 DECIDE_ON_BROKER_PAGE naming its page, since no token
// makes one decidable here. Nothing is written for any of these refusals. A machine login's
// decision is logged with the caller that relayed it and the approver it named, the record of who
// acted, since that name is the caller's to state.
func (s *server) decideRecord(w http.ResponseWriter, r *http.Request, approve bool, callerName string) {
	recordID, ok := pathRecordID(w, r, "record", "RECORD_ID_INPUT")
	if !ok {
		return
	}
	kind, err := s.deps.Machine.RecordKind(r.Context(), recordID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeNoSuchRecord(w)
		return
	case err != nil:
		writeInternal(w, "read credential request kind", err)
		return
	case kind != record.KindLauncherCredential:
		s.refuseSecretRequestDecision(w, r, recordID)
		return
	}
	var body decideBody
	if !readJSON(w, r, &body, "INVALID_DECISION") {
		return
	}
	if !requireApprover(w, body.Approver) {
		return
	}
	// The approver is the caller's word, of any length requireApprover takes, so the line cuts it
	// (answer.ClipFields).
	slog.Info("decision relayed", answer.ClipFields([]any{"caller", callerName, "approver", body.Approver, "record", recordID, "approve", approve})...)
	s.decideMachineLogin(w, r, recordID, approve, body)
}

// refuseSecretRequestDecision answers a listed caller's approve or deny of the secret request
// recordID (decideRecord). It first reads the person rule for the person the decision's body
// names (decidingPerson; requests.Machine.HideUnseen): a person the record's private project
// hides it from is answered 404 exactly as for an id no record has, and a record Dispatch could
// not answer for 503 PROJECT_ACCESS_UNAVAILABLE, so neither learns whether the id names a record,
// or whether the model judge holds it, or reads its page. Only then is it 403 JUDGE_DECIDES while
// the model judge holds it, else 403 DECIDE_ON_BROKER_PAGE naming the page where a person signed
// in to the broker decides it, or naming BROKER_SIGNIN=off on a broker that serves no page. The
// rule reads the record's rows, not its signed request, so a record signed for another
// BROKER_PUBLIC_URL, which no read of this broker verifies, is refused a person who may see it
// with 403 as before.
func (s *server) refuseSecretRequestDecision(w http.ResponseWriter, r *http.Request, recordID string) {
	err := s.deps.Machine.HideUnseen(r.Context(), recordID, decidingPerson(w, r))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeNoSuchRecord(w)
		return
	case errors.Is(err, requests.ErrProjectAccessUnavailable):
		writeProjectAccessUnavailable(w, "decide credential request", err)
		return
	case err != nil:
		writeInternal(w, "read credential request", err)
		return
	}
	// Only the hold is read, not the record: a record signed for another BROKER_PUBLIC_URL
	// verifies for no broker but its own, and is refused DECIDE_ON_BROKER_PAGE as before. A read
	// of the hold that failed is refused the same way, since no secret request is decided here
	// either way.
	if held, err := s.deps.Machine.JudgeHolds(r.Context(), recordID); err == nil && held {
		writeError(w, http.StatusForbidden, "JUDGE_DECIDES",
			"the model judge is deciding this secret request; no person approves or denies it unless the judge cannot decide it")
		return
	}
	// A broker with BROKER_SIGNIN=off serves no page, so approveURL names none, since every
	// /approve path is a 404 there, and the refusal says why nobody can decide it.
	page := s.approveURL(recordID)
	if page == "" {
		writePageRefusal(w, http.StatusForbidden, "DECIDE_ON_BROKER_PAGE",
			"a secret request is decided only on the secrets broker's own page, and this broker serves none: it signs nobody in (BROKER_SIGNIN=off), so this request cannot be decided here", "")
		return
	}
	writePageRefusal(w, http.StatusForbidden, "DECIDE_ON_BROKER_PAGE",
		"a secret request is decided only on the secrets broker's own page, by a person signed in to the broker: "+page, page)
}

// decidingPerson is the login the body of a decision of a secret request names as its approver,
// read for the person rule alone, since nothing else of the body decides a secret request here.
// A body that names none, or that is no decision, names no person, who is shown no private
// project, so its answer still cannot tell a record the rule hides from an id no record has.
func decidingPerson(w http.ResponseWriter, r *http.Request) string {
	var body decideBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)).Decode(&body); err != nil {
		return ""
	}
	return body.Approver
}

// writeNoSuchRecord is the 404 of a record id no record has. Every refusal of a record the person
// rule hides from the person a call names answers through it too, so the two answers are one by
// construction rather than by two copies of the same text.
func writeNoSuchRecord(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "NOT_FOUND", "no such credential request")
}

func (s *server) decideMachineLogin(w http.ResponseWriter, r *http.Request, recordID string, approve bool, body decideBody) {
	if body.Code == nil || *body.Code == "" {
		writeError(w, http.StatusBadRequest, "CODE_REQUIRED", "code is required to decide a machine login")
		return
	}
	_, credentialID, err := s.deps.MachineLogin.ApplyDecision(r.Context(), recordID, approve, body.Approver, *body.Code)
	switch {
	case errors.Is(err, machine.ErrNotFound):
		writeNoSuchRecord(w)
		return
	case errors.Is(err, machine.ErrCodeMismatch):
		writeError(w, http.StatusForbidden, "CODE_MISMATCH", err.Error())
		return
	case errors.Is(err, record.ErrNotApprover):
		writeError(w, http.StatusForbidden, "NOT_APPROVER", err.Error())
		return
	case errors.Is(err, machine.ErrAlreadyDecided), errors.Is(err, machine.ErrLoginExpired):
		writeError(w, http.StatusConflict, "RECORD_TERMINAL", err.Error())
		return
	case errors.Is(err, machine.ErrKeyHoldsLiveCredential):
		writeError(w, http.StatusConflict, "KEY_HOLDS_LIVE_CREDENTIAL", err.Error())
		return
	case errors.Is(err, machine.ErrKeyBindsReadToken):
		writeError(w, http.StatusConflict, "KEY_BINDS_READ_TOKEN", err.Error())
		return
	case errors.Is(err, machine.ErrServiceLogsInWithToken):
		writeError(w, http.StatusBadRequest, "SERVICE_LOGS_IN_WITH_TOKEN", err.Error())
		return
	case err != nil:
		writeInternal(w, "decide machine login", err)
		return
	}
	if !approve {
		writeJSON(w, http.StatusOK, stateResponse{State: "denied"})
		return
	}
	writeJSON(w, http.StatusOK, decisionResponse{CredentialID: strPtr(credentialID), State: "approved"})
}

// dismissBody is dismiss's request body, {"approver"}: the dismissing person's login, which
// Dispatch's server sets from its own session.
type dismissBody struct {
	// The login of the person dismissing the record: its approver, a member of the group that is
	// its approver, or anyone signed in for a record whose approver is anyone, which is whose list
	// holds it.
	Approver string `json:"approver"`
}

// dismissResponse is what a dismissal answers: wire.DismissResponse itself, named here for the
// same reason recordResponse is.
type dismissResponse = wire.DismissResponse

// dismissRecord takes an expired credential-request record off every approver's list for good,
// as the person the caller names (requests.Machine.Dismiss): a secret request or a machine login
// whose deadline passed before anybody decided it, which every decision refuses. It is an
// accountAuth row, as the decide routes are, so authenticate has spent the source address's
// bucket and checked the caller's token and its power before it runs; unlike the decide routes
// it does not refuse a secret request: a dismissal decides nothing and releases nothing, so a
// secret request's is relayed from Dispatch as a revoke is, with the person taken on the caller's
// word. That person must be one whose list holds the record: its approver, a member the group
// reader admits to the group that is its approver, or, for a record whose approver is anyone,
// anybody signed in. A second dismissal answers the first one's values and writes nothing, so a
// person who clicks twice, or two people who dismiss one shared request, read one answer. It is
// logged with the caller that relayed it and the approver it named, as a
// machine login's decision is.
func (s *server) dismissRecord(w http.ResponseWriter, r *http.Request, callerName string) {
	recordID, ok := pathRecordID(w, r, "record", "RECORD_ID_INPUT")
	if !ok {
		return
	}
	var body dismissBody
	if !readJSON(w, r, &body, "INVALID_DISMISSAL") {
		return
	}
	if !requireApprover(w, body.Approver) {
		return
	}
	// The approver is the caller's word, of any length requireApprover takes, so the line cuts it
	// (answer.ClipFields).
	slog.Info("dismissal relayed", answer.ClipFields([]any{"caller", callerName, "approver", body.Approver, "record", recordID})...)
	dismissal, err := s.deps.Machine.Dismiss(r.Context(), recordID, body.Approver)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No record has the id, or it is a secret request of a private project the person may
		// not see, which every read answers the same way.
		writeNoSuchRecord(w)
		return
	case errors.Is(err, requests.ErrProjectAccessUnavailable):
		// Dispatch could not say whether the person may see the record's project, so nothing was
		// dismissed; the same dismissal succeeds once Dispatch answers.
		writeProjectAccessUnavailable(w, "dismiss credential request", err)
		return
	case errors.Is(err, record.ErrNotApprover):
		writeError(w, http.StatusForbidden, "NOT_APPROVER", "only the record's approver, a member of the group that is its approver, or anyone signed in for a request whose approver is anyone, may dismiss it")
		return
	case errors.Is(err, requests.ErrNotExpired):
		writeError(w, http.StatusConflict, "RECORD_PENDING", err.Error())
		return
	case errors.Is(err, requests.ErrTerminal):
		writeError(w, http.StatusConflict, "RECORD_TERMINAL", "the credential request was decided, or cancelled before its deadline, so it did not expire and there is nothing to dismiss")
		return
	case err != nil:
		writeInternal(w, "dismiss credential request", err)
		return
	}
	writeJSON(w, http.StatusOK, dismissResponse{RecordID: dismissal.RecordID, DismissedBy: dismissal.DismissedBy, DismissedAt: dismissal.DismissedAt})
}
