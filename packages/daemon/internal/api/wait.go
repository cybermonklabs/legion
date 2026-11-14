package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// WaitRequest is the body of POST /legion/v1/claims/wait: the claim the declaring session holds,
// that session, the claim's secret, what the worker waits on, and when the wait's time comes, an
// RFC 3339 instant (dispatch://INFRA-2040 R2).
type WaitRequest struct {
	ClaimToken claim.Token `json:"claimToken"`
	SessionID  string      `json:"sessionId"`
	Secret     string      `json:"secret"`
	WaitingOn  string      `json:"waitingOn"`
	WakeAt     string      `json:"wakeAt"`
}

// WaitView is one wait as the daemon recorded it: its id, what it waits on, when its time comes
// and when the daemon recorded the declaration.
type WaitView struct {
	ID         string    `json:"id"`
	WaitingOn  string    `json:"waitingOn"`
	WakeAt     time.Time `json:"wakeAt"`
	DeclaredAt time.Time `json:"declaredAt"`
}

// WaitResponse is the body POST /legion/v1/claims/wait answers once the wait is recorded.
type WaitResponse struct {
	Wait WaitView `json:"wait"`
}

// wait is a phase worker declaring that it waits on something outside its own work — a person's
// answer, a CI run, another issue — until a time it names, so the daemon's own wakes leave it be
// until then (dispatch://INFRA-2040 R2). The route authenticates the claim as activity does — a
// blank field refused by its name, a claim this daemon does not hold or a wrong secret with
// claim.InvalidSecret, a stale plugin with the stale-contract 409 — and holds the call to the
// session the claim records, as ready-for-update's fence does (claim.SameAgentRefusal), since a
// wait is the declaring session's own.
//
// The claim is read once, with the secret (authenticated), and every claim field of the fact
// comes from that read: the issue and role, the run the worker is on (supervise.Claim.ServingRun)
// and the entry of the task it had taken (takenEntry), so no two fields straddle a decision. The
// route mints the wait's id and applies intake.WaitDeclared under `wait-declared:<id>`, through
// intake.ApplyFact, so the declaration takes the lock every fact applies under, the design gate's
// closing among them. The engine decides it (workflow's waitDeclared): a refusal is written as
// the workflow routes write theirs, its code and sentence (400 for the text and the two times, 409
// for the record's), and a recorded wait answers 200 with its id, its text, its time and its
// declaration time. Both times are kept to the microsecond the record keeps, so the answer is the
// record.
func (s *server) wait(w http.ResponseWriter, r *http.Request) {
	var req WaitRequest
	if !s.readBody(w, r, &req, s.claimSecretCheck) || !requireFields(w,
		field{"claimToken", string(req.ClaimToken)}, field{"sessionId", req.SessionID}, field{"secret", req.Secret},
		field{"wakeAt", req.WakeAt},
	) {
		return
	}
	wakeAt, err := time.Parse(time.RFC3339, req.WakeAt)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("wakeAt must be an RFC 3339 time: "+err.Error()))
		return
	}
	_, held, ok := s.authenticated(req.ClaimToken, req.Secret)
	if !ok {
		writeJSON(w, claim.InvalidSecret.Status, claim.InvalidSecret)
		return
	}
	if s.refuseIfStaleClaim(w, r, held) {
		return
	}
	if req.SessionID != held.Session {
		s.log.Info("api: refused a claim request", "request", "wait", "claim", req.ClaimToken,
			"reason", "the claim resumes another session", "session", req.SessionID, "recorded", held.Session)
		writeJSON(w, claim.SameAgentRefusal.Status, claim.SameAgentRefusal)
		return
	}
	if s.pool == nil || len(s.handlers) == 0 {
		writeFailure(w, http.StatusInternalServerError, "FACTS_UNAVAILABLE", "this daemon runs no workflow, so it records no wait")
		return
	}
	fact := intake.WaitDeclared{
		Token: req.ClaimToken, Issue: held.Issue, Role: held.Role, Session: req.SessionID,
		Generation: held.ServingRun(), WaitID: uuid.NewString(), WaitingOn: strings.TrimSpace(req.WaitingOn),
		WakeAt: wakeAt.UTC().Truncate(time.Microsecond), TakenEntry: takenEntry(held),
		At: s.now().UTC().Truncate(time.Microsecond),
	}
	s.applyFact(w, r, "wait-declared:"+fact.WaitID, fact, WaitResponse{Wait: WaitView{
		ID: fact.WaitID, WaitingOn: fact.WaitingOn, WakeAt: fact.WakeAt, DeclaredAt: fact.At,
	}})
}

// takenEntry is the phase entry of the task c's worker had taken (dispatch://INFRA-2040 D4): the
// entry of the confirmed delivery the claim holds, when that delivery carries one, else the entry
// of the last confirmed delivery it retired (supervise.Claim.ServedEntry). It is not
// Claim.ServingEntry, which for a claim that has retired no confirmed task of an entry answers
// the entry of a held task its worker never confirmed (supervise's serving), a task it had not
// taken.
func takenEntry(c supervise.Claim) int64 {
	if p := c.Pending; p != nil && !p.ConfirmedAt.IsZero() && p.Entry != 0 {
		return p.Entry
	}
	return c.ServedEntry
}
