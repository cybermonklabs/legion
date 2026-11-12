package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// OwedRequest is the body of POST /legion/v1/claims/owed: the claim the asking session holds,
// that session, and the secret the claim's registration issued it — the three fields
// claims/activity carries, and no generation, since the question changes nothing.
type OwedRequest struct {
	ClaimToken claim.Token `json:"claimToken"`
	SessionID  string      `json:"sessionId"`
	Secret     string      `json:"secret"`
}

// OwedResponse is the body POST /legion/v1/claims/owed answers: whether the claim owes work now,
// and what that work is called, as a person reads it in a reminder — "the testing phase of
// LEGION-208", "the production check of LEGION-208". Work is empty exactly when Owes is false, so
// a reminder renders the phrase it is given and never one of its own.
type OwedResponse struct {
	Owes bool   `json:"owes"`
	Work string `json:"work"`
}

// claimOwed is a live role's session asking, at the end of its turn, what its claim owes now. It
// is what the end-of-turn reminder renders in both harnesses, so the session keeps no model of
// the issue's phase: a process restarted or replaced asks again and reads the same answer, which
// a reminder state held in the process cannot give (dispatch://INFRA-2746).
//
// It authenticates as claims/activity does — a blank field by its name with 400, a claim this
// daemon does not hold or a wrong secret with claim.InvalidSecret, a stale plugin with the
// stale-contract 409 — and holds the session to the one the claim records as well, which activity
// reads no further than its field. A session the claim does not record gets claim.InvalidSecret
// too: the answer names the claim's work, and the route tells it to no process but the one whose
// turn it is for, nor to a caller that cannot prove it holds the claim.
//
// A daemon that reads no issue record, or does not know which role works a phase
// (Options.Pool, Options.RoleFor), answers 500 rather than "nothing owed": the reminder that
// reads a failure still nudges and says it could not read the record, where an empty answer
// would silence it on a guess.
func (s *server) claimOwed(w http.ResponseWriter, r *http.Request) {
	var req OwedRequest
	if !s.readBody(w, r, &req, s.claimSecretCheck) || !requireFields(w,
		field{"claimToken", string(req.ClaimToken)}, field{"sessionId", req.SessionID}, field{"secret", req.Secret},
	) {
		return
	}
	m, ok := s.authenticated(req.ClaimToken, req.Secret)
	if !ok {
		writeJSON(w, claim.InvalidSecret.Status, claim.InvalidSecret)
		return
	}
	// The claim is read once: the session it is held to, the stale check and the answer all come
	// from the one decision Claim() waited for.
	c := m.Claim()
	if c.Session != "" && c.Session != req.SessionID {
		writeJSON(w, claim.InvalidSecret.Status, claim.InvalidSecret)
		return
	}
	if s.refuseIfStaleClaim(w, r, c) {
		return
	}
	// The architect and the controller work no phase of their own, so they owe no phase's work
	// and no look of a check: nothing is read for them.
	if !isPhaseWorker(c.Role) {
		writeJSON(w, http.StatusOK, OwedResponse{})
		return
	}
	if s.pool == nil || s.roleFor == nil {
		writeJSON(w, http.StatusInternalServerError, errorBody("the daemon reads no issue record, so it cannot say what this claim owes"))
		return
	}
	position, err := s.handoffPosition(r.Context(), c.Issue, c.Role)
	if err != nil {
		s.logRequestFailure(r, err, "api: the record of a claim's issue could not be read for what it owes", "claim", req.ClaimToken)
		writeJSON(w, http.StatusInternalServerError, errorBody("read the issue record: "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, owedBy(c, position, s.roleFor))
}

// owedBy is what claim c owes with its issue's record standing at position, roleFor naming the
// role that works each phase (workflow.RoleFor). Three sources can each seem to say: the claim's
// pending delivery and what it was left serving (supervise.Claim.Pending, ServingCheck), the
// issue's phase with the role's own row of it (record.PhaseRow.Completed), and the issue's
// production check. They disagree, and the rule is this.
//
// The record decides whether anything is owed. The pending delivery is a copy of the workflow's
// decision taken when the task was queued, and the process's memory of its turn is a copy taken
// later still; the record is what the workflow writes in the very transaction that takes a
// completion or moves a phase. So a role whose phase has finished owes nothing whatever it was
// handed or remembers: the issue has moved on to another role's phase (RoleFor names another
// role), to one no role works (awaiting_merge, awaiting_person, done, held), or its tree lingers,
// where a completion records nothing; or the phase is still the role's and its row records the
// completion the workflow took (a reviewer whose round waits for a review, a merger whose READY
// waits for a design approval). A role whose phase is open and whose row records no completion
// owes it, whether or not its task has reached the session yet: the task arrives as its next
// prompt, and the work is the role's either way.
//
// The claim decides one thing only, which kind of work its turn is on, read exactly as the
// completion route reads it (handoffComplete): a claim serving a look of a production check owes
// that look and nothing of a phase, and every other claim owes its phase or nothing. A look is
// owed while the record holds it open — the check open, its look open, the look the claim serves
// being the check's current one of this check instance, and the check the claim's role's — and a
// look the check has left owes nothing. A look the record holds open for the claim's role while
// the claim serves none — its task not yet taken, or a testing turn still running behind a
// merged close — is not owed yet: a report sent from that turn is refused as no report on
// production (CHECK_LOOK_NOT_SERVED), and the look's task comes as the next prompt.
func owedBy(c supervise.Claim, position completionPosition, roleFor func(phase.Phase) claim.Role) OwedResponse {
	issue := position.issue
	if issue == nil {
		return OwedResponse{}
	}
	if look := c.ServingCheck(); look.Look > 0 {
		check := position.check
		if check == nil || !check.Open() || !check.LookOpen || check.Looks != look.Look ||
			!check.NamedBy(look.OpenedAt) || check.Role != c.Role {
			return OwedResponse{}
		}
		return OwedResponse{Owes: true, Work: "the production check of " + issue.Key}
	}
	if position.lingers || position.completed || roleFor(issue.Phase) != c.Role {
		return OwedResponse{}
	}
	return OwedResponse{Owes: true, Work: fmt.Sprintf("the %s phase of %s", issue.Phase, issue.Key)}
}
