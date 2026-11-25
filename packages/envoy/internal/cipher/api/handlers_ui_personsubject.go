// handlers_ui_personsubject.go: POST /v1/person-subjects/revoke, the route Dispatch's server
// calls when another Google account takes an address it holds for a person. Part of the
// routes a listed service calls with its own ServiceAccount token (accountAuth), relaying what
// Dispatch decided: the token proves which service is calling, and the service names the address,
// the account that no longer holds it and the account that does.
package api

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/oidc"
)

// revokePersonSubjectBody is POST /v1/person-subjects/revoke's body:
// wire.RevokePersonSubjectRequest itself, named here because the broker's generated HTTP
// reference documents a route's body from a type this package declares.
type revokePersonSubjectBody = wire.RevokePersonSubjectRequest

// personSubjectRevokedResponse is POST /v1/person-subjects/revoke's answer:
// wire.PersonSubjectRevokedResponse itself, named here for the same reason.
type personSubjectRevokedResponse = wire.PersonSubjectRevokedResponse

// revokePersonSubject ends everything the account the body names as replaced holds under the
// body's address, and refuses that account anything new there, in one transaction
// (enroll.Service.RevokePersonSubject): every sign-in on a machine its person token made, every
// login of the person's own machines it approved, with the tokens and sessions each holds, every
// session on the broker's own approval page it signed in, and every row that records no account,
// which might be its. What the holder made, and a service's login, are untouched. A repeat ends
// nothing and answers zeros. A revoke whose session generation is below the greatest one a revoke
// applied at the address carried was superseded by a later replacement there: it writes and ends
// nothing, and answers state "superseded" with that greatest generation (superseded_by), which
// Dispatch leaves owed and reports, since its own generations for the address are then below the
// broker's; the line it logs names both generations.
//
// Every field is required, 400 otherwise (SESSION_GENERATION_REQUIRED for the session
// generation), and the two accounts must differ: the holder is named so that a revoke of the holder
// from an earlier move, the address coming back to it, stops refusing it. The session generation is
// recorded beside the replaced account, so an approval Dispatch checked at an earlier generation is
// refused (machine.Service.ApplyDecision), and decides whether a later replacement superseded the
// revoke. No Dispatch sends this route without it: the route and the field came in one change. The
// issuer is compared as oidc.CanonicalIssuer spells it, so Google's two spellings are one. The
// address and the accounts are the caller's word, as every relayed revoke takes its person. That
// word does more than take power away: naming the holder lifts the refusal an earlier revoke
// recorded for that account under the address, so it signs in and approves there again. It lifts
// nothing else, and only a caller that relays decisions (relaysDecisions), the power that recorded
// the refusal, can lift one.
func (s *server) revokePersonSubject(w http.ResponseWriter, r *http.Request, callerName string) {
	var body revokePersonSubjectBody
	if !readJSON(w, r, &body, "INVALID_REVOKE") {
		return
	}
	if !requirePerson(w, body.Person) {
		return
	}
	if strings.TrimSpace(body.Issuer) == "" {
		writeError(w, http.StatusBadRequest, "ISSUER_REQUIRED", "issuer is required")
		return
	}
	if strings.TrimSpace(body.Subject) == "" {
		writeError(w, http.StatusBadRequest, "SUBJECT_REQUIRED", "subject is required")
		return
	}
	if strings.TrimSpace(body.HolderSubject) == "" {
		writeError(w, http.StatusBadRequest, "HOLDER_SUBJECT_REQUIRED", "holder_subject is required")
		return
	}
	if body.Subject == body.HolderSubject {
		writeError(w, http.StatusBadRequest, "SUBJECT_IS_HOLDER", "subject and holder_subject name one account: the account that no longer holds the address and the one that does are two")
		return
	}
	if body.SessionGeneration == nil {
		writeError(w, http.StatusBadRequest, "SESSION_GENERATION_REQUIRED", "session_generation is required")
		return
	}
	person := record.CanonicalLogin(body.Person)
	issuer := oidc.CanonicalIssuer(body.Issuer)
	revoked, err := s.deps.Enroll.RevokePersonSubject(r.Context(), person, issuer, body.Subject, body.HolderSubject, *body.SessionGeneration)
	if err != nil {
		writeInternal(w, "revoke person subject", err)
		return
	}
	// The accounts are the caller's word, of any length, so each line cuts them (answer.ClipFields).
	if revoked.SupersededBy != nil {
		slog.Warn("person subject revocation superseded: a later replacement at the address has been applied, so this one changed nothing",
			answer.ClipFields([]any{"caller", callerName, "person", person, "issuer", issuer, "subject", body.Subject,
				"holder_subject", body.HolderSubject, "session_generation", *body.SessionGeneration, "superseded_by", *revoked.SupersededBy})...)
		writeJSON(w, http.StatusOK, personSubjectRevokedResponse{State: "superseded", SupersededBy: revoked.SupersededBy})
		return
	}
	slog.Info("person subject revocation relayed", answer.ClipFields([]any{"caller", callerName, "person", person, "issuer", issuer,
		"subject", body.Subject, "holder_subject", body.HolderSubject, "session_generation", *body.SessionGeneration,
		"signins", revoked.SignIns, "machine_logins", revoked.MachineLogins, "tokens_ended", revoked.Tokens,
		"approval_sessions", revoked.ApprovalSessions})...)
	writeJSON(w, http.StatusOK, personSubjectRevokedResponse{
		State: "revoked", SignIns: revoked.SignIns, MachineLogins: revoked.MachineLogins, TokensEnded: revoked.Tokens,
		ApprovalSessions: revoked.ApprovalSessions,
	})
}
